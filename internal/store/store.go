// Package store holds the jobs.
//
// One interface, and two implementations that pass the same contract suite in
// internal/store/storetest. The in-memory one needs nothing installed, so the
// suite runs on any machine. The PostgreSQL one runs when a database is
// configured. Testing both against one suite is what stops the memory store
// becoming a convenient fiction that agrees with no real database.
//
// No query in this package calls NOW(). Every time comes from Options.Now and
// travels as a parameter. That costs one field and buys two things: the
// database clock and the server clock cannot disagree, and a test can move
// time forward and watch a lease expire without waiting for it.
package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/Stiven-Gjekaj/GoQuorra/internal/jobs"
)

// Errors that a caller is expected to tell apart.
//
// The old code returned fmt.Errorf("job not found"), so the API layer could
// not separate a missing job from a database that had fallen over. It
// answered 404 to both, and a reader of the logs learned nothing about which
// had happened.
var (
	// ErrNotFound means no job carries that identifier.
	ErrNotFound = errors.New("store: no such job")

	// ErrLeaseNotValid means the lease named in a report is not the lease the
	// job is holding. The usual cause is a worker that stalled long enough
	// for the reclaimer to take the job back and give it to somebody else.
	ErrLeaseNotValid = errors.New("store: lease is not valid")
)

// Job is one job, as it is stored.
type Job struct {
	ID         string          `json:"id"`
	Type       string          `json:"type"`
	Payload    json.RawMessage `json:"payload"`
	Queue      string          `json:"queue"`
	Priority   int             `json:"priority"`
	Status     jobs.Status     `json:"status"`
	Attempts   int             `json:"attempts"`
	MaxRetries int             `json:"max_retries"`
	LastError  string          `json:"last_error,omitempty"`

	// The lease fields are set together or not at all. A job that is not
	// leased carries none of them.
	LeaseID        string     `json:"lease_id,omitempty"`
	LeasedBy       string     `json:"leased_by,omitempty"`
	LeaseExpiresAt *time.Time `json:"lease_expires_at,omitempty"`

	RunAt     time.Time `json:"run_at"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// NewJob is a job that does not exist yet.
type NewJob struct {
	Type     string
	Payload  json.RawMessage
	Queue    string
	Priority int

	// Delay holds the job back. A delay of zero means the job is ready now.
	Delay time.Duration

	// MaxRetries counts the retries after the first attempt. A nil value
	// takes the default from the store.
	MaxRetries *int
}

// Validate refuses a job that cannot be stored.
func (n NewJob) Validate() error {
	if n.Type == "" {
		return errors.New("store: a job needs a type")
	}
	if len(n.Type) > 255 {
		return fmt.Errorf("store: the type is %d characters, and the column holds 255", len(n.Type))
	}
	if len(n.Queue) > 255 {
		return fmt.Errorf("store: the queue name is %d characters, and the column holds 255", len(n.Queue))
	}
	if n.Delay < 0 {
		return fmt.Errorf("store: the delay is %s, which puts the job in the past", n.Delay)
	}
	if n.MaxRetries != nil && *n.MaxRetries < 0 {
		return fmt.Errorf("store: max retries is %d, and it cannot be negative", *n.MaxRetries)
	}
	// An empty payload is allowed and becomes {}. Text that is not JSON is
	// not, because the column is JSONB and the database would refuse it
	// later, with an error naming a constraint rather than the field.
	if len(n.Payload) > 0 && !json.Valid(n.Payload) {
		return errors.New("store: the payload is not JSON")
	}
	return nil
}

// LeaseRequest asks for work.
type LeaseRequest struct {
	Queue    string
	WorkerID string

	// Limit is the most jobs to hand over at once.
	Limit int

	// TTL is how long the worker has before the reclaimer takes the jobs
	// back. It is stored as an expiry time, which is the part that was
	// missing before: the old schema recorded when a lease started and never
	// when it ended, so nothing could tell that one had run out.
	TTL time.Duration
}

// Report is what a worker says about a job it held.
type Report struct {
	JobID   string
	LeaseID string
	Outcome jobs.Outcome

	// Error is the message to keep on a failure. It is ignored on a success.
	Error string
}

// QueueStat is one row of the queue statistics.
type QueueStat struct {
	Queue  string      `json:"queue"`
	Status jobs.Status `json:"status"`
	Count  int         `json:"count"`
}

// Store keeps the jobs and moves them between states.
//
// Every method takes a context and honours it. Every method is safe to call
// from several goroutines at once, because the server does.
type Store interface {
	// Create stores a new job and returns it as stored.
	Create(ctx context.Context, n NewJob) (*Job, error)

	// Get returns one job, or ErrNotFound.
	Get(ctx context.Context, id string) (*Job, error)

	// Lease hands ready jobs to a worker and raises the attempt count of
	// each. Two callers asking at once never receive the same job.
	Lease(ctx context.Context, req LeaseRequest) ([]*Job, error)

	// Report records what happened to a leased job and returns it as stored.
	// It returns ErrLeaseNotValid when the lease named is not the lease the
	// job holds, and changes nothing in that case.
	Report(ctx context.Context, rep Report) (*Job, error)

	// ReclaimExpired returns jobs whose lease has run out, and reports how
	// many it moved. A job that has no attempts left is buried instead.
	ReclaimExpired(ctx context.Context, limit int) (int, error)

	// QueueStats counts the jobs by queue and by status.
	QueueStats(ctx context.Context) ([]QueueStat, error)

	// Recent returns the newest jobs first.
	Recent(ctx context.Context, limit int) ([]*Job, error)

	// Close releases whatever the store holds.
	Close() error
}

// Options configure a store.
type Options struct {
	// Policy holds the retry rules. Its MaxRetries is the default for a job
	// that names none of its own.
	Policy jobs.Policy

	// Now reads the clock. A test replaces it and moves time forward.
	Now func() time.Time

	// Jitter draws a number between 0 and 1 for the backoff. A test replaces
	// it with a constant and then states the wait it expects.
	Jitter func() float64
}

// withDefaults fills in what the caller left out, so that a zero Options is
// usable and a partly filled one does not panic on a nil function.
func (o Options) withDefaults() Options {
	if o.Policy.Base == 0 && o.Policy.Max == 0 && o.Policy.MaxRetries == 0 {
		o.Policy = jobs.DefaultPolicy()
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Jitter == nil {
		o.Jitter = defaultJitter
	}
	return o
}

// policyFor returns the retry rules for one job. The waits come from the
// store and the retry count comes from the job, because the caller who
// submitted it chose that number.
func (o Options) policyFor(maxRetries int) jobs.Policy {
	p := o.Policy
	p.MaxRetries = maxRetries
	return p
}
