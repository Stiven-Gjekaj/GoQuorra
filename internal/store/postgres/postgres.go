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

	"github.com/Stiven-Gjekaj/GoQuorra/internal/jobs"
	"github.com/Stiven-Gjekaj/GoQuorra/internal/store"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// columns names every field a job is built from, in one place, so that the
// six queries that read a job cannot fall out of step with the scanner.
const columns = `id, seq, type, payload, queue, priority, status, attempts,
	max_retries, last_error, lease_id, leased_by, lease_expires_at,
	idempotency_key, result, acted_by, acted_at, leased_at, schedule_id,
	run_at, created_at, updated_at`

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
	var schedule *string
	if n.ScheduleID != "" {
		schedule = &n.ScheduleID
	}

	// A transaction, because a job that waits for another is two writes: the
	// row and the list of what it waits for. A job stored without its list
	// would run at once, which is the one thing the feature exists to stop.
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, false, fmt.Errorf("postgres: cannot begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// The parents are read and locked first. Locking them is what stops a
	// parent succeeding between the read and the write, which would leave
	// this job blocked for ever on a job that is already done.
	//
	// ORDER BY id, so that two submissions naming the same parents take the
	// rows in the same order and cannot hold one each.
	if len(n.After) > 0 {
		rows, err := tx.Query(ctx,
			`SELECT id::text, status FROM jobs WHERE id = ANY($1) ORDER BY id FOR UPDATE`,
			n.After)
		if err != nil {
			return nil, false, notAJobIdentifier(err)
		}

		found := map[string]jobs.Status{}
		for rows.Next() {
			var id, status string
			if err := rows.Scan(&id, &status); err != nil {
				rows.Close()
				return nil, false, fmt.Errorf("postgres: cannot read a job it waits for: %w", err)
			}
			parsed, err := jobs.ParseStatus(status)
			if err != nil {
				rows.Close()
				return nil, false, fmt.Errorf("postgres: the table holds %w", err)
			}
			found[id] = parsed
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			// The type conversion arrives here rather than from Query,
			// because pgx does not send the statement until the rows are
			// read. Both places go through one function so that neither can
			// answer differently.
			return nil, false, notAJobIdentifier(err)
		}

		// Every one has to be here already. That is what makes a cycle
		// impossible, and it is the honest answer to a caller naming a job
		// that never existed: the queue cannot say when a job it has never
		// heard of will succeed.
		parents := make([]jobs.Status, 0, len(n.After))
		for _, id := range n.After {
			status, here := found[id]
			if !here {
				return nil, false, fmt.Errorf(
					"%w: the job waits for %s, and there is no such job", store.ErrNotFound, id)
			}
			parents = append(parents, status)
		}

		job.After = append([]string(nil), n.After...)
		job.Status = jobs.AfterState(parents)
		if job.Status == jobs.Cancelled {
			job.LastError = afterMessage(n.After, found)
		}
	}

	row := tx.QueryRow(ctx, `
		INSERT INTO jobs (id, type, payload, queue, priority, status,
		                  attempts, max_retries, last_error,
		                  idempotency_key, schedule_id, run_at, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)
		ON CONFLICT (idempotency_key) WHERE idempotency_key IS NOT NULL DO NOTHING
		RETURNING `+columns,
		job.ID, job.Type, []byte(job.Payload), job.Queue, job.Priority,
		string(job.Status), job.Attempts, job.MaxRetries, job.LastError,
		key, schedule, job.RunAt, job.CreatedAt, job.UpdatedAt,
	)

	stored, _, err := scanJob(row)
	if errors.Is(err, pgx.ErrNoRows) {
		// DO NOTHING returns no row, which here can only mean the key was
		// already claimed.
		if err := tx.Rollback(ctx); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
			return nil, false, fmt.Errorf("postgres: cannot roll back: %w", err)
		}
		existing, err := s.byIdempotencyKey(ctx, n.IdempotencyKey)
		if err != nil {
			return nil, false, err
		}
		return existing, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("postgres: cannot store the job: %w", err)
	}

	for _, id := range n.After {
		if _, err := tx.Exec(ctx,
			`INSERT INTO job_after (job_id, after_id) VALUES ($1, $2)
				ON CONFLICT DO NOTHING`,
			job.ID, id,
		); err != nil {
			return nil, false, fmt.Errorf("postgres: cannot record what the job waits for: %w", err)
		}
	}
	stored.After = append([]string(nil), n.After...)

	if err := tx.Commit(ctx); err != nil {
		return nil, false, fmt.Errorf("postgres: cannot commit: %w", err)
	}
	return stored, true, nil
}

// notAJobIdentifier turns PostgreSQL refusing to read text as a UUID into a
// missing job.
//
// A job cannot wait for something that is not a job identifier, and that is
// the caller's mistake rather than a fault underneath. Passing the type
// conversion on gives an error naming a database type, which sends the reader
// to the wrong place.
func notAJobIdentifier(err error) error {
	if isInvalidUUID(err) {
		return fmt.Errorf(
			"%w: the job waits for something that is not a job identifier", store.ErrNotFound)
	}
	return fmt.Errorf("postgres: cannot read the jobs it waits for: %w", err)
}

// afterMessage says which of the jobs a job waited for stopped it.
//
// The identifier and the state, because "a job it waited for failed" sends
// somebody to read every one of them. Only the first is named: one parent
// that cannot succeed is the whole reason, and listing the rest would suggest
// they all have to be fixed.
func afterMessage(after []string, parents map[string]jobs.Status) string {
	for _, id := range after {
		status, here := parents[id]
		if here && status.Terminal() && status != jobs.Succeeded {
			return "the job it waits for, " + id + ", is " + status.String()
		}
	}
	return "a job it waits for cannot succeed"
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

	// Only for a job that is waiting. Reading one job is the path a person
	// takes to find out why it has not run, and every other status waits for
	// nothing that still matters.
	if job.Status == jobs.Blocked {
		job.After, err = afterOf(ctx, s.pool, id)
		if err != nil {
			return nil, err
		}
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

	// The worker is recorded before the jobs are looked for, and whether or
	// not any come back. An ask that finds nothing is the ask that matters:
	// it is the only sign a fleet with no work is still there.
	//
	// Outside the lease statement rather than in a transaction with it. A row
	// saying a worker asked, on an ask that then failed, is true. Two round
	// trips to make it exactly consistent would buy nothing.
	if req.WorkerID != "" {
		if _, err := s.pool.Exec(ctx, `
			INSERT INTO workers (id, queue, first_seen_at, last_seen_at)
			VALUES ($1, $2, $3, $3)
			ON CONFLICT (id, queue) DO UPDATE SET last_seen_at = $3`,
			req.WorkerID, req.Queue, now,
		); err != nil {
			return nil, fmt.Errorf("postgres: cannot record the worker: %w", err)
		}
	}

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
