package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Stiven-Gjekaj/GoQuorra/internal/jobs"
	"github.com/Stiven-Gjekaj/GoQuorra/internal/store"
	"github.com/jackc/pgx/v5"
)

// QueueStats counts the jobs by queue and by status.
//
// A plain GROUP BY, and not the view the old schema carried. The view added a
// second place for the shape of this answer to live, and it had to be dropped
// and rebuilt whenever a column changed.
func (s *Store) QueueStats(ctx context.Context) ([]store.QueueStat, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT queue, status, COUNT(*) FROM jobs
		GROUP BY queue, status
		ORDER BY queue, status`)
	if err != nil {
		return nil, fmt.Errorf("postgres: cannot count the queues: %w", err)
	}
	defer rows.Close()

	var out []store.QueueStat
	for rows.Next() {
		var stat store.QueueStat
		var status string
		if err := rows.Scan(&stat.Queue, &status, &stat.Count); err != nil {
			return nil, fmt.Errorf("postgres: cannot read a count: %w", err)
		}

		parsed, err := jobs.ParseStatus(status)
		if err != nil {
			return nil, fmt.Errorf("postgres: the table holds %w", err)
		}
		stat.Status = parsed
		out = append(out, stat)
	}
	return out, rows.Err()
}

// List returns matching jobs, newest first.
//
// The ordering is by seq and not by created_at. seq is unique and monotonic,
// so it gives every row a distinct place, which is what a cursor needs: two
// jobs written in the same microsecond share a created_at, and a page break
// between them would show one of them on both pages or on neither.
func (s *Store) List(ctx context.Context, f store.Filter) ([]*store.Job, error) {
	if err := f.Validate(); err != nil {
		return nil, err
	}
	if f.Limit <= 0 {
		return nil, nil
	}

	where, args := conditions(f)
	add := func(clause string, value any) {
		args = append(args, value)
		where = append(where, fmt.Sprintf(clause, len(args)))
	}

	if f.Before != "" {
		if _, err := parseID(f.Before); err != nil {
			return nil, store.ErrNotFound
		}
		// A cursor naming a job that is gone leaves the page start
		// undefined, so it is refused rather than quietly read as the start
		// of the list, which would send the reader back to page one.
		var seq int64
		var runAt time.Time
		err := s.pool.QueryRow(ctx, `SELECT seq, run_at FROM jobs WHERE id = $1`, f.Before).Scan(&seq, &runAt)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, store.ErrNotFound
		}
		if err != nil {
			return nil, fmt.Errorf("postgres: cannot read the cursor: %w", err)
		}

		if f.Order == store.Soonest {
			// The pair and not run_at alone. run_at is not unique: a burst of
			// submissions shares one value, and every job a reclaim sweep
			// returns shares one. Comparing run_at alone would hand back the
			// whole group again on the next page, or skip the rest of it,
			// and which of the two would depend on how many jobs happened to
			// share a moment.
			//
			// Written as a row comparison and not as the OR it stands for.
			// PostgreSQL turns this form into an index condition on
			// jobs_due_idx and seeks; it cannot do that with the OR, and
			// measuring both showed the OR reading 955 rows to return 25.
			args = append(args, runAt, seq)
			where = append(where, fmt.Sprintf("(run_at, seq) > ($%d, $%d)", len(args)-1, len(args)))
		} else {
			add("seq < $%d", seq)
		}
	}

	order := `seq DESC`
	if f.Order == store.Soonest {
		order = `run_at, seq`
	}

	args = append(args, f.Limit)
	query := `SELECT ` + columns + ` FROM jobs WHERE ` + strings.Join(where, " AND ") +
		fmt.Sprintf(` ORDER BY %s LIMIT $%d`, order, len(args))

	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("postgres: cannot list the jobs: %w", err)
	}
	defer rows.Close()

	var out []*store.Job
	for rows.Next() {
		job, _, err := scanJob(rows)
		if err != nil {
			return nil, fmt.Errorf("postgres: cannot read a job: %w", err)
		}
		out = append(out, job)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// The list each job waits for, after the rows are read rather than during.
	// pgx holds one query open on a connection at a time, so asking inside the
	// loop above would need a second connection for every page.
	//
	// Only for the jobs that are waiting. Every other job waits for nothing in
	// almost every case, and a listing of fifty would otherwise be fifty one
	// queries to fill a field that is empty on all of them.
	for _, job := range out {
		if job.Status != jobs.Blocked {
			continue
		}
		job.After, err = afterOf(ctx, s.pool, job.ID)
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

// Attempts lists the finished runs of one job, oldest run first.
//
// Ordered by id and not by the attempt number. Reviving a job sets its count
// back to zero, so a job that was buried and revived holds two runs numbered
// 1, and only the order they were written in says which came first.
func (s *Store) Attempts(ctx context.Context, jobID string) ([]store.Attempt, error) {
	if _, err := parseID(jobID); err != nil {
		return nil, store.ErrNotFound
	}

	// The job is looked for first. An empty list means a job that has not
	// finished a run, and without this it would also mean a job that is not
	// there, which are different answers to the caller above.
	var exists bool
	err := s.pool.QueryRow(ctx, `SELECT TRUE FROM jobs WHERE id = $1`, jobID).Scan(&exists)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("postgres: cannot read the job: %w", err)
	}

	rows, err := s.pool.Query(ctx, `
		SELECT job_id::text, attempt, worker, outcome, error, started_at, finished_at
		FROM job_attempts WHERE job_id = $1 ORDER BY id`, jobID)
	if err != nil {
		return nil, fmt.Errorf("postgres: cannot read the attempts: %w", err)
	}
	defer rows.Close()

	var out []store.Attempt
	for rows.Next() {
		var (
			a       store.Attempt
			outcome string
			started *time.Time
		)
		if err := rows.Scan(
			&a.JobID, &a.Number, &a.Worker, &outcome, &a.Error, &started, &a.FinishedAt,
		); err != nil {
			return nil, fmt.Errorf("postgres: cannot read an attempt: %w", err)
		}

		parsed, err := jobs.ParseOutcome(outcome)
		if err != nil {
			return nil, fmt.Errorf("postgres: the table holds %w", err)
		}
		a.Outcome = parsed
		if started != nil {
			at := started.UTC()
			a.StartedAt = &at
		}
		a.FinishedAt = a.FinishedAt.UTC()
		out = append(out, a)
	}
	return out, rows.Err()
}

// after reads what one job waits for, oldest first.
//
// A separate read and not a join on the job query. A job waits for nothing in
// almost every case, so joining would put an outer join and a group on every
// listing to carry a column that is empty on nearly every row.
func afterOf(ctx context.Context, q querier, jobID string) ([]string, error) {
	rows, err := q.Query(ctx,
		`SELECT after_id::text FROM job_after WHERE job_id = $1 ORDER BY after_id`, jobID)
	if err != nil {
		return nil, fmt.Errorf("postgres: cannot read what the job waits for: %w", err)
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("postgres: cannot read a job it waits for: %w", err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// Workers lists the workers that have asked for work, most recently first.
func (s *Store) Workers(ctx context.Context) ([]store.Worker, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, queue, first_seen_at, last_seen_at FROM workers
		ORDER BY last_seen_at DESC, id, queue`)
	if err != nil {
		return nil, fmt.Errorf("postgres: cannot list the workers: %w", err)
	}
	defer rows.Close()

	var out []store.Worker
	for rows.Next() {
		var w store.Worker
		if err := rows.Scan(&w.ID, &w.Queue, &w.FirstSeenAt, &w.LastSeenAt); err != nil {
			return nil, fmt.Errorf("postgres: cannot read a worker: %w", err)
		}
		w.FirstSeenAt = w.FirstSeenAt.UTC()
		w.LastSeenAt = w.LastSeenAt.UTC()
		out = append(out, w)
	}
	return out, rows.Err()
}

// conditions turns the fields of a filter into a WHERE clause.
//
// Built with placeholders and never with the values. Three of the fields here
// come straight from a query string.
//
// The cursor is not one of them. A cursor says where a page starts and not
// which jobs belong in it, and a bulk action has no page.
//
// One function, used by the listing and by the bulk actions, so that the jobs
// a bulk cancel stops are exactly the jobs a listing with the same filter
// shows. Written twice, the two would answer differently on the first field
// added to one of them.
func conditions(f store.Filter) ([]string, []any) {
	where := []string{"TRUE"}
	args := []any{}
	add := func(clause string, value any) {
		args = append(args, value)
		where = append(where, fmt.Sprintf(clause, len(args)))
	}

	if f.Queue != "" {
		add("queue = $%d", f.Queue)
	}
	if len(f.Queues) > 0 {
		// ANY of an array rather than an IN list built by hand. The list is
		// as long as the queues a key holds, and building the placeholders
		// puts a loop between the caller and the query for no gain.
		add("queue = ANY($%d)", f.Queues)
	}
	if f.Status != "" {
		add("status = $%d", string(f.Status))
	}
	if f.Type != "" {
		add("type = $%d", f.Type)
	}
	if f.Worker != "" {
		add("leased_by = $%d", f.Worker)
	}
	if !f.DueBy.IsZero() {
		add("run_at <= $%d", f.DueBy)
	}
	return where, args
}

// Close releases the pool.
func (s *Store) Close() error {
	s.pool.Close()
	return nil
}

// row is what both pgx.Row and pgx.Rows offer.
type row interface {
	Scan(dest ...any) error
}

// querier is what both the pool and a transaction offer.
//
// A read inside an open transaction has to go through that transaction: the
// pool would hand out a second connection, which cannot see the writes the
// transaction has not committed.
type querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// scanJob builds a job from one row, and returns its sequence number for the
// caller that needs to sort.
//
// The nullable columns are read into pointers. A job that nobody holds has
// three NULLs, and scanning those straight into strings fails at run time
// rather than at compile time, on the first job that reaches this state.
func scanJob(r row) (*store.Job, int64, error) {
	var (
		job      store.Job
		seq      int64
		status   string
		payload  []byte
		leaseID  *string
		leasedBy *string
		expires  *time.Time
		key      *string
		result   []byte
		actedBy  *string
		actedAt  *time.Time
		leasedAt *time.Time
		schedule *string
	)

	err := r.Scan(
		&job.ID, &seq, &job.Type, &payload, &job.Queue, &job.Priority,
		&status, &job.Attempts, &job.MaxRetries, &job.LastError,
		&leaseID, &leasedBy, &expires, &key, &result,
		&actedBy, &actedAt, &leasedAt, &schedule,
		&job.RunAt, &job.CreatedAt, &job.UpdatedAt,
	)
	if err != nil {
		return nil, 0, err
	}

	parsed, err := jobs.ParseStatus(status)
	if err != nil {
		return nil, 0, fmt.Errorf("the table holds %w", err)
	}
	job.Status = parsed
	job.Payload = json.RawMessage(payload)

	if leaseID != nil {
		job.LeaseID = *leaseID
	}
	if leasedBy != nil {
		job.LeasedBy = *leasedBy
	}
	if key != nil {
		job.IdempotencyKey = *key
	}
	if actedBy != nil {
		job.ActedBy = *actedBy
	}
	if actedAt != nil {
		at := actedAt.UTC()
		job.ActedAt = &at
	}
	if leasedAt != nil {
		at := leasedAt.UTC()
		job.LeasedAt = &at
	}
	if schedule != nil {
		job.ScheduleID = *schedule
	}
	if len(result) > 0 {
		job.Result = json.RawMessage(result)
	}
	if expires != nil {
		// UTC, so that a time from the database compares against a time from
		// the server without either carrying a location the other does not.
		at := expires.UTC()
		job.LeaseExpiresAt = &at
	}

	job.RunAt = job.RunAt.UTC()
	job.CreatedAt = job.CreatedAt.UTC()
	job.UpdatedAt = job.UpdatedAt.UTC()

	return &job, seq, nil
}

// isInvalidUUID reports whether an error is PostgreSQL refusing to read text
// as a UUID. The caller turns that into ErrNotFound, because a job whose
// identifier is not a UUID cannot be in a table keyed by one.
func isInvalidUUID(err error) bool {
	return strings.Contains(err.Error(), "invalid input syntax for type uuid")
}

var _ store.Store = (*Store)(nil)
