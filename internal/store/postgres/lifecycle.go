package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/Stiven-Gjekaj/GoQuorra/internal/jobs"
	"github.com/Stiven-Gjekaj/GoQuorra/internal/store"
	"github.com/jackc/pgx/v5"
)

// Cancel stops a job that has not finished.
//
// The row is locked first. Reading the status and writing the new one are two
// steps, and without the lock a worker can report between them, so a job
// reported as done comes back as cancelled or the other way round depending
// on which write lands last.
func (s *Store) Cancel(ctx context.Context, id, actor string) (*store.Job, error) {
	return s.transition(ctx, id, actor, func(current jobs.Status) (jobs.Status, error) {
		if current.Terminal() {
			return "", fmt.Errorf("%w: the job is %s and has already finished", store.ErrWrongState, current)
		}
		return jobs.Cancelled, nil
	}, keepAttempts)
}

// Revive puts a dead or cancelled job back in the queue.
func (s *Store) Revive(ctx context.Context, id, actor string) (*store.Job, error) {
	return s.transition(ctx, id, actor, func(current jobs.Status) (jobs.Status, error) {
		if current != jobs.Dead && current != jobs.Cancelled {
			return "", fmt.Errorf(
				"%w: the job is %s, and only a dead or cancelled job can be revived", store.ErrWrongState, current)
		}
		return jobs.Pending, nil
	}, resetAttempts)
}

// ExtendLease pushes the expiry of a lease further out.
//
// One statement, and the lease check is in its WHERE clause rather than in a
// read before it. That makes the check and the write the same operation, so
// the reclaimer cannot take the job between them.
func (s *Store) ExtendLease(ctx context.Context, jobID, leaseID string, by time.Duration) (*store.Job, error) {
	if by <= 0 {
		return nil, fmt.Errorf("store: cannot extend a lease by %s", by)
	}
	if _, err := parseID(jobID); err != nil {
		return nil, store.ErrNotFound
	}
	// An identifier that is not a UUID matches no lease. Sending it to the
	// database gives a type conversion error instead of the refusal that a
	// worker knows how to act on.
	if _, err := parseID(leaseID); err != nil {
		return nil, store.ErrLeaseNotValid
	}

	now := s.opts.Now()

	row := s.pool.QueryRow(ctx, `
		UPDATE jobs SET lease_expires_at = $1, updated_at = $1
		WHERE id = $2 AND lease_id = $3 AND status = 'leased'
		RETURNING `+columns,
		now.Add(by), jobID, leaseID,
	)

	job, _, err := scanJob(row)
	if errors.Is(err, pgx.ErrNoRows) {
		// Nothing matched. Either the job is gone or it no longer holds that
		// lease, and the two are told apart by a second read so that a
		// worker gets the answer it can act on.
		var exists bool
		if err := s.pool.QueryRow(ctx, `SELECT TRUE FROM jobs WHERE id = $1`, jobID).Scan(&exists); err != nil {
			return nil, store.ErrNotFound
		}
		return nil, store.ErrLeaseNotValid
	}
	if err != nil {
		return nil, fmt.Errorf("postgres: cannot extend the lease: %w", err)
	}
	return job, nil
}

// DeleteFinished removes finished jobs that stopped moving before a time.
//
// A bounded batch, chosen by seq so the oldest go first. One statement that
// deleted a month of rows would hold locks and write a write-ahead log entry
// for every one of them, and a queue that stalls at four in the morning
// because its own housekeeping is running is worse than a large table.
func (s *Store) DeleteFinished(ctx context.Context, status jobs.Status, before time.Time, limit int) (int, error) {
	if !status.Terminal() {
		return 0, fmt.Errorf("store: %q is not a finished state, and removing a job in it would lose work", status)
	}
	if limit <= 0 {
		return 0, nil
	}

	tag, err := s.pool.Exec(ctx, `
		DELETE FROM jobs WHERE id IN (
			SELECT id FROM jobs
			WHERE status = $1 AND updated_at < $2
			ORDER BY seq
			LIMIT $3
		)`,
		string(status), before, limit,
	)
	if err != nil {
		return 0, fmt.Errorf("postgres: cannot remove finished jobs: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

// attempts says what happens to the attempt count during a transition.
type attempts int

const (
	keepAttempts  attempts = iota
	resetAttempts          // Used by Revive, which gives a job a fresh set.
)

// transition moves a job to a new state, chosen by a function that sees the
// state it is in.
//
// Cancel and Revive differ in one line each, and both need the same lock, the
// same missing job check, and the same clearing of the lease. Writing that
// twice is how the two of them drift apart.
func (s *Store) transition(
	ctx context.Context,
	id string,
	actor string,
	next func(current jobs.Status) (jobs.Status, error),
	count attempts,
) (*store.Job, error) {
	if _, err := parseID(id); err != nil {
		return nil, store.ErrNotFound
	}

	now := s.opts.Now()

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("postgres: cannot begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var current string
	err = tx.QueryRow(ctx, `SELECT status FROM jobs WHERE id = $1 FOR UPDATE`, id).Scan(&current)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("postgres: cannot read the job: %w", err)
	}

	status, err := jobs.ParseStatus(current)
	if err != nil {
		return nil, fmt.Errorf("postgres: the table holds %w", err)
	}

	wanted, err := next(status)
	if err != nil {
		return nil, err
	}

	// The lease is cleared whatever the transition. A job that is no longer
	// leased must hold none of the three lease columns, and the constraint in
	// the schema enforces it.
	setAttempts := "attempts"
	if count == resetAttempts {
		setAttempts = "0"
	}

	// A name and a moment, or neither of them. The columns hold the last
	// action and not a history, so a caller that does not name itself clears
	// them: leaving the previous name there would say that somebody cancelled
	// this job who did not.
	var who, when any
	if actor != "" {
		who, when = actor, now
	}

	row := tx.QueryRow(ctx, `
		UPDATE jobs SET
			status = $1,
			attempts = `+setAttempts+`,
			run_at = $2,
			updated_at = $2,
			lease_id = NULL,
			leased_by = NULL,
			lease_expires_at = NULL,
			acted_by = $4,
			acted_at = $5
		WHERE id = $3
		RETURNING `+columns,
		string(wanted), now, id, who, when,
	)

	job, _, err := scanJob(row)
	if err != nil {
		return nil, fmt.Errorf("postgres: cannot write the job: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("postgres: cannot commit: %w", err)
	}
	return job, nil
}
