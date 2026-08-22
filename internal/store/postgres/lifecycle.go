package postgres

import (
	"context"
	"errors"
	"fmt"

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
func (s *Store) Cancel(ctx context.Context, id string) (*store.Job, error) {
	return s.transition(ctx, id, func(current jobs.Status) (jobs.Status, error) {
		if current.Terminal() {
			return "", fmt.Errorf("%w: the job is %s and has already finished", store.ErrWrongState, current)
		}
		return jobs.Cancelled, nil
	}, keepAttempts)
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

	row := tx.QueryRow(ctx, `
		UPDATE jobs SET
			status = $1,
			attempts = `+setAttempts+`,
			run_at = $2,
			updated_at = $2,
			lease_id = NULL,
			leased_by = NULL,
			lease_expires_at = NULL
		WHERE id = $3
		RETURNING `+columns,
		string(wanted), now, id,
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
