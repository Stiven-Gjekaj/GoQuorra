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

// Report records what happened to a leased job.
//
// It runs in a transaction that locks the row first. Reading the attempt
// count, deciding, and writing the answer are three steps, and without the
// lock the reclaimer can take the job back between the first and the third.
// The job would then be given to another worker and retired by this one.
func (s *Store) Report(ctx context.Context, rep store.Report) (*store.Job, error) {
	now := s.opts.Now()

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("postgres: cannot begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var (
		attempts   int
		maxRetries int
		leaseID    *string
	)
	err = tx.QueryRow(ctx,
		`SELECT attempts, max_retries, lease_id::text FROM jobs WHERE id = $1 FOR UPDATE`,
		rep.JobID,
	).Scan(&attempts, &maxRetries, &leaseID)

	if errors.Is(err, pgx.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		// A job identifier that is not a UUID reaches the database as one, so
		// the failure is a type conversion rather than a missing row. The
		// caller asked for something that cannot exist either way.
		if isInvalidUUID(err) {
			return nil, store.ErrNotFound
		}
		return nil, fmt.Errorf("postgres: cannot read the job: %w", err)
	}

	// An unleased job holds no identifier. Comparing what the report carries
	// against nothing would let a report with an empty lease match it, and
	// any caller could then retire any waiting job in the table.
	if leaseID == nil || *leaseID != rep.LeaseID {
		return nil, store.ErrLeaseNotValid
	}

	job, err := s.applyDecision(ctx, tx, rep.JobID, attempts, maxRetries, rep.Outcome, rep.Error, now)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("postgres: cannot commit: %w", err)
	}
	return job, nil
}

// ReclaimExpired returns jobs whose lease has run out.
//
// SKIP LOCKED again, so that two servers running the reclaimer at once share
// the work instead of one waiting behind the other. A row already being
// reported on by its worker is locked, and stepping over it is exactly right:
// that worker is answering for the job right now.
func (s *Store) ReclaimExpired(ctx context.Context, limit int) (int, error) {
	if limit <= 0 {
		return 0, nil
	}

	now := s.opts.Now()

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("postgres: cannot begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	rows, err := tx.Query(ctx, `
		SELECT id::text, attempts, max_retries, leased_by FROM jobs
		WHERE status = 'leased' AND lease_expires_at <= $1
		ORDER BY lease_expires_at, seq
		LIMIT $2
		FOR UPDATE SKIP LOCKED`,
		now, limit,
	)
	if err != nil {
		return 0, fmt.Errorf("postgres: cannot look for expired leases: %w", err)
	}

	type expired struct {
		id         string
		attempts   int
		maxRetries int
		worker     string
	}
	var found []expired

	for rows.Next() {
		var e expired
		var worker *string
		if err := rows.Scan(&e.id, &e.attempts, &e.maxRetries, &worker); err != nil {
			rows.Close()
			return 0, fmt.Errorf("postgres: cannot read an expired lease: %w", err)
		}
		if worker != nil {
			e.worker = *worker
		}
		found = append(found, e)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("postgres: cannot read the expired leases: %w", err)
	}

	for _, e := range found {
		if _, err := s.applyDecision(ctx, tx, e.id, e.attempts, e.maxRetries,
			jobs.OutcomeExpired, expiryMessage(e.worker), now); err != nil {
			return 0, err
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("postgres: cannot commit: %w", err)
	}
	return len(found), nil
}

// applyDecision writes what the domain decided.
//
// Both a reported outcome and an expired lease come through here, so the two
// age a job in exactly the same way. When they were separate, only one of
// them aged a job at all.
func (s *Store) applyDecision(
	ctx context.Context,
	tx pgx.Tx,
	id string,
	attempts, maxRetries int,
	outcome jobs.Outcome,
	message string,
	now time.Time,
) (*store.Job, error) {
	decision := s.opts.PolicyFor(maxRetries).Decide(attempts, outcome, now, s.opts.Jitter())

	// A success leaves the previous error in place rather than clearing it.
	// The row then still says what went wrong on the attempt before the one
	// that worked, which is the thing somebody reading a retried job wants.
	var lastError *string
	if outcome != jobs.OutcomeDone {
		lastError = &message
	}

	row := tx.QueryRow(ctx, `
		UPDATE jobs SET
			status = $1,
			attempts = $2,
			run_at = $3,
			updated_at = $4,
			last_error = COALESCE($5, last_error),
			lease_id = NULL,
			leased_by = NULL,
			lease_expires_at = NULL
		WHERE id = $6
		RETURNING `+columns,
		string(decision.Status), decision.Attempts, decision.RunAt, now, lastError, id,
	)

	job, _, err := scanJob(row)
	if err != nil {
		return nil, fmt.Errorf("postgres: cannot write the job: %w", err)
	}
	return job, nil
}

func expiryMessage(worker string) string {
	if worker == "" {
		return "the lease ran out before any worker reported"
	}
	return "the lease held by " + worker + " ran out before it reported"
}
