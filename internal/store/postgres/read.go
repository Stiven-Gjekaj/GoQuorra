package postgres

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/Stiven-Gjekaj/GoQuorra/internal/jobs"
	"github.com/Stiven-Gjekaj/GoQuorra/internal/store"
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

// Recent returns the newest jobs first.
func (s *Store) Recent(ctx context.Context, limit int) ([]*store.Job, error) {
	if limit <= 0 {
		return nil, nil
	}

	rows, err := s.pool.Query(ctx,
		`SELECT `+columns+` FROM jobs ORDER BY created_at DESC, seq DESC LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("postgres: cannot read the recent jobs: %w", err)
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
	return out, rows.Err()
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
	)

	err := r.Scan(
		&job.ID, &seq, &job.Type, &payload, &job.Queue, &job.Priority,
		&status, &job.Attempts, &job.MaxRetries, &job.LastError,
		&leaseID, &leasedBy, &expires,
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
