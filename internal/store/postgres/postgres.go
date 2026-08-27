// Package postgres keeps jobs in PostgreSQL.
//
// It passes the same contract suite as the in-memory store, in
// internal/store/storetest, which is what says the two behave alike.
//
// The driver is pgx rather than lib/pq. lib/pq says in its own README that it
// is in maintenance mode and points at this one. pgx also cancels a query on
// the wire when the context ends, where database/sql can only stop waiting
// for it, and that difference decides whether a slow query keeps holding a
// row lock after the client has gone.
package postgres

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/Stiven-Gjekaj/GoQuorra/internal/store"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// columns names every field a job is built from, in one place, so that the
// six queries that read a job cannot fall out of step with the scanner.
const columns = `id, seq, type, payload, queue, priority, status, attempts,
	max_retries, last_error, lease_id, leased_by, lease_expires_at,
	idempotency_key, result, acted_by, acted_at, leased_at, run_at, created_at, updated_at`

// Store keeps jobs in PostgreSQL.
type Store struct {
	pool *pgxpool.Pool
	opts store.Options
}

// Open connects to the database named in the URL.
func Open(ctx context.Context, databaseURL string, opts store.Options) (*Store, error) {
	config, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("postgres: the database URL is not usable: %w", err)
	}

	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		return nil, fmt.Errorf("postgres: cannot build the connection pool: %w", err)
	}

	// Reach the database now rather than on the first job. A server that
	// starts and then fails every request looks healthy to whatever is
	// watching it, and the fault is reported by the caller instead of by the
	// process that has the problem.
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("postgres: cannot reach the database: %w", err)
	}

	return &Store{pool: pool, opts: opts.WithDefaults()}, nil
}

// NewWithPool wraps a pool the caller already holds. The tests use it.
func NewWithPool(pool *pgxpool.Pool, opts store.Options) *Store {
	return &Store{pool: pool, opts: opts.WithDefaults()}
}

// Pool gives the underlying pool, for a health check or for applying the
// schema.
func (s *Store) Pool() *pgxpool.Pool { return s.pool }

// Create stores a new job.
//
// The idempotency check is the database's and not this code's. Reading for an
// existing key and inserting afterwards lets two submissions carrying one key
// both find nothing and both insert, which is the exact case the key exists
// to prevent. ON CONFLICT makes the check and the write one statement, and the
// unique index is what decides.
func (s *Store) Create(ctx context.Context, n store.NewJob) (*store.Job, bool, error) {
	if err := n.Validate(); err != nil {
		return nil, false, err
	}

	job := s.opts.Prepare(n, uuid.NewString(), s.opts.Now())

	var key *string
	if n.IdempotencyKey != "" {
		key = &n.IdempotencyKey
	}

	row := s.pool.QueryRow(ctx, `
		INSERT INTO jobs (id, type, payload, queue, priority, status,
		                  attempts, max_retries, last_error,
		                  idempotency_key, run_at, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)
		ON CONFLICT (idempotency_key) WHERE idempotency_key IS NOT NULL DO NOTHING
		RETURNING `+columns,
		job.ID, job.Type, []byte(job.Payload), job.Queue, job.Priority,
		string(job.Status), job.Attempts, job.MaxRetries, job.LastError,
		key, job.RunAt, job.CreatedAt, job.UpdatedAt,
	)

	stored, _, err := scanJob(row)
	if errors.Is(err, pgx.ErrNoRows) {
		// DO NOTHING returns no row, which here can only mean the key was
		// already claimed.
		existing, err := s.byIdempotencyKey(ctx, n.IdempotencyKey)
		if err != nil {
			return nil, false, err
		}
		return existing, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("postgres: cannot store the job: %w", err)
	}
	return stored, true, nil
}

// byIdempotencyKey reads the job that claimed a key.
func (s *Store) byIdempotencyKey(ctx context.Context, key string) (*store.Job, error) {
	row := s.pool.QueryRow(ctx, `SELECT `+columns+` FROM jobs WHERE idempotency_key = $1`, key)

	job, _, err := scanJob(row)
	if err != nil {
		// The row was there a moment ago, so this is a real fault rather than
		// a job that never existed.
		return nil, fmt.Errorf("postgres: the idempotency key %q is taken and its job cannot be read: %w", key, err)
	}
	return job, nil
}

// parseID checks that an identifier could name a row.
//
// The column is a UUID, so text that is not one is not a job that exists.
// Passing it on gives an error naming a type conversion, which sends the
// reader to the database when the answer is that the caller asked for
// something that cannot be there.
func parseID(id string) (uuid.UUID, error) { return uuid.Parse(id) }

// Get returns one job.
func (s *Store) Get(ctx context.Context, id string) (*store.Job, error) {
	if _, err := parseID(id); err != nil {
		return nil, store.ErrNotFound
	}

	row := s.pool.QueryRow(ctx, `SELECT `+columns+` FROM jobs WHERE id = $1`, id)

	job, _, err := scanJob(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("postgres: cannot read the job: %w", err)
	}
	return job, nil
}

// Lease hands ready jobs to a worker.
//
// FOR UPDATE SKIP LOCKED is what lets several workers ask at the same instant
// and receive different jobs. The rows one worker has locked are stepped over
// rather than waited for, so the second worker takes the next ones and
// neither blocks.
func (s *Store) Lease(ctx context.Context, req store.LeaseRequest) ([]*store.Job, error) {
	if req.Limit <= 0 {
		return nil, nil
	}

	now := s.opts.Now()

	rows, err := s.pool.Query(ctx, `
		WITH ready AS (
			-- Named ready_id rather than id. The RETURNING clause below
			-- lists the job columns unqualified, and two relations in scope
			-- carrying "id" makes every one of them ambiguous.
			SELECT id AS ready_id FROM jobs
			WHERE queue = $1 AND status = 'pending' AND run_at <= $2
			ORDER BY priority DESC, run_at, seq
			LIMIT $3
			FOR UPDATE SKIP LOCKED
		)
		UPDATE jobs SET
			status = 'leased',
			-- One identifier for each job and not one for the batch. A batch
			-- identifier lets a worker that finished one job send a report
			-- that matches every other job it was handed.
			lease_id = gen_random_uuid(),
			leased_by = $4,
			lease_expires_at = $5,
			leased_at = $2,
			attempts = attempts + 1,
			updated_at = $2
		FROM ready
		WHERE jobs.id = ready.ready_id
		RETURNING `+columns,
		req.Queue, now, req.Limit, req.WorkerID, now.Add(req.TTL),
	)
	if err != nil {
		return nil, fmt.Errorf("postgres: cannot lease jobs: %w", err)
	}
	defer rows.Close()

	type ordered struct {
		job *store.Job
		seq int64
	}
	var found []ordered

	for rows.Next() {
		job, seq, err := scanJob(rows)
		if err != nil {
			return nil, fmt.Errorf("postgres: cannot read a leased job: %w", err)
		}
		found = append(found, ordered{job: job, seq: seq})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("postgres: cannot read the leased jobs: %w", err)
	}

	// RETURNING has no defined order, whatever the subquery sorted by. The
	// caller is promised the most urgent job first, so the sort happens here
	// rather than being left to whatever order the update touched the rows
	// in. That order is stable enough to look correct in a test and to change
	// under load.
	sort.SliceStable(found, func(i, j int) bool {
		a, b := found[i], found[j]
		if a.job.Priority != b.job.Priority {
			return a.job.Priority > b.job.Priority
		}
		if !a.job.RunAt.Equal(b.job.RunAt) {
			return a.job.RunAt.Before(b.job.RunAt)
		}
		return a.seq < b.seq
	})

	leased := make([]*store.Job, len(found))
	for i, row := range found {
		leased[i] = row.job
	}
	return leased, nil
}
