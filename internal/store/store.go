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
	"math"
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

	// ErrWrongState means the job exists and the operation does not apply to
	// a job in the state it is in. Cancelling a job that has already finished
	// is the common one.
	//
	// It is separate from ErrNotFound because the answer to a caller is
	// different: a missing job will never be there, and a job in the wrong
	// state may be in the right one later. The HTTP layer answers 404 to the
	// first and 409 to the second.
	ErrWrongState = errors.New("store: the job is in the wrong state")

	// ErrNotJSON means a worker handed back a result that is not JSON.
	//
	// It is a sentinel because the layer above has to tell it apart: this is
	// the worker's mistake and answers 400, while every other failure from
	// this package is the server's and answers 500. That layer used to decide
	// by searching the message for the words "not JSON", so rewording this
	// sentence would have quietly moved every one of these to a 500.
	ErrNotJSON = errors.New("store: the result is not JSON")
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

	// IdempotencyKey is what the client called this piece of work, if it
	// named it at all.
	IdempotencyKey string `json:"idempotency_key,omitempty"`

	// Result is what the worker produced, if it produced anything. It is
	// whatever JSON the handler returned, and this project does not look
	// inside it.
	Result json.RawMessage `json:"result,omitempty"`

	// ActedBy and ActedAt record the caller that last cancelled or revived
	// this job, and when. They are set together or not at all, and a job that
	// nobody has acted on carries neither.
	//
	// Only a person acting is recorded here. A job that failed, was retried
	// or was reclaimed was moved by the queue itself, and writing the queue's
	// own name into this pair would bury the entries that matter.
	ActedBy string     `json:"acted_by,omitempty"`
	ActedAt *time.Time `json:"acted_at,omitempty"`

	// LeasedAt is when the worker now holding this job took it, and is nil
	// when nobody holds it.
	//
	// LeaseExpiresAt says when the queue gives up on the run. This says how
	// long the run has already been going, which is the question somebody
	// looking for a job that is stuck actually asks. It is also what the
	// attempt row records as the start of the run.
	LeasedAt *time.Time `json:"leased_at,omitempty"`

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

	// IdempotencyKey makes the submission safe to repeat. A second Create
	// carrying a key that has been used gives back the job that already
	// exists and stores nothing.
	//
	// An empty key means the caller is not asking for that, and two jobs
	// submitted without one are two jobs.
	IdempotencyKey string
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
	// Priority and max retries are bounded by the column and not only by
	// their sign.
	//
	// Both columns are INTEGER, which holds a 32 bit value, and both fields
	// are a Go int, which holds a 64 bit one. A number between the two sizes
	// passed every check here and reached PostgreSQL, which refused it with
	// "integer out of range". That message carries no "store: " prefix, so
	// the layer above read it as the server's fault and answered 500 to what
	// the client had plainly got wrong.
	//
	// The in-memory store has no such column and stored the same value
	// happily, so the two stores disagreed about the same job while the whole
	// contract suite passed. That is the part worth fixing: the suite exists
	// so that a store cannot agree only with itself.
	if n.Priority > math.MaxInt32 || n.Priority < math.MinInt32 {
		return fmt.Errorf("store: the priority is %d, and the column holds a number between %d and %d",
			n.Priority, math.MinInt32, math.MaxInt32)
	}
	if n.MaxRetries != nil && *n.MaxRetries < 0 {
		return fmt.Errorf("store: max retries is %d, and it cannot be negative", *n.MaxRetries)
	}
	if n.MaxRetries != nil && *n.MaxRetries > math.MaxInt32 {
		return fmt.Errorf("store: max retries is %d, and the column holds no more than %d",
			*n.MaxRetries, math.MaxInt32)
	}
	if len(n.IdempotencyKey) > 255 {
		return fmt.Errorf("store: the idempotency key is %d characters, and the column holds 255", len(n.IdempotencyKey))
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

	// Result is what the job produced. It is kept only on a success, because
	// the output of an attempt that failed is not an output.
	Result json.RawMessage
}

// Validate refuses a report that cannot be stored.
func (r Report) Validate() error {
	if len(r.Result) > 0 && !json.Valid(r.Result) {
		return ErrNotJSON
	}
	return nil
}

// Filter narrows a listing.
//
// A zero Filter means every job, newest first, which is what the dashboard
// asks for.
type Filter struct {
	// Queue, Status and Type each narrow the list when they are set. An empty
	// value means the field is not being filtered on, which is why Status is
	// a string here rather than a jobs.Status: the empty status is not one.
	Queue  string
	Status jobs.Status
	Type   string

	// Limit is how many to return.
	Limit int

	// Before is the identifier of the last job on the page already seen. The
	// next page carries on from it, in whatever order was asked for.
	//
	// A cursor and not an offset. An offset re-reads and skips every row
	// before the page, so page five hundred costs five hundred pages of work
	// and jobs submitted meanwhile shift every later page by one, which shows
	// the reader a row twice and hides another entirely.
	Before string

	// Order is the order the jobs come back in. The zero value is Newest.
	Order Order

	// DueBy keeps only the jobs that run at or before this moment. The zero
	// value keeps every job.
	//
	// The moment comes from the caller and is not read here. A store that
	// read its own clock could not be asked what the queue looked like at a
	// stated time, and its tests would wait for a moment to arrive instead of
	// stating it. This is the same rule jobs.Decide follows.
	DueBy time.Time

	// Worker keeps only the jobs that this worker holds.
	//
	// It matches leased_by, which is set while a lease is held and cleared
	// when the job ends, so this only ever answers about work in flight. A
	// finished job belongs to nobody, whoever ran it.
	Worker string
}

// Order is the order a listing comes back in.
type Order int

const (
	// Newest gives the most recently submitted job first.
	//
	// This is the zero value, so a caller who does not care gets what the
	// dashboard shows, which is what somebody watching a queue wants.
	Newest Order = iota

	// Soonest gives the job that runs first, first. This is the order the
	// queue itself works in, so it is the order that answers what is at the
	// front of the line and what is waiting behind a backoff.
	Soonest
)

// Valid says whether an order is one this package knows.
func (o Order) Valid() bool { return o == Newest || o == Soonest }

func (o Order) String() string {
	switch o {
	case Newest:
		return "newest"
	case Soonest:
		return "soonest"
	default:
		return fmt.Sprintf("Order(%d)", int(o))
	}
}

// Validate refuses a filter that cannot be answered.
func (f Filter) Validate() error {
	if f.Status != "" && !f.Status.Valid() {
		return fmt.Errorf("store: %q is not a status", f.Status)
	}
	if f.Limit < 0 {
		return fmt.Errorf("store: the limit is %d", f.Limit)
	}
	if !f.Order.Valid() {
		return fmt.Errorf("store: %s is not an order", f.Order)
	}
	return nil
}

// Attempt is one finished run of a job.
//
// A row is written where the run ends and not where it starts. A running
// attempt is already fully visible on the job itself, which names the worker
// holding it and says when the lease ends, so writing a row at lease time
// would be a second write on the busiest statement in the system to record
// what the first one already said.
type Attempt struct {
	JobID string `json:"job_id"`

	// Number counts from one. It is the attempt count the job carried while
	// this run was happening.
	Number int `json:"attempt"`

	// Worker is the identifier the worker gave when it took the lease. It is
	// empty for a lease that ran out with no worker named.
	Worker string `json:"worker,omitempty"`

	// Outcome is done, failed, expired or refused.
	Outcome jobs.Outcome `json:"outcome"`

	// Error is what went wrong, and is empty for a run that finished.
	Error string `json:"error,omitempty"`

	// StartedAt is when the worker took the lease, and is nil when that is
	// not known. A job leased by a build older than this table has no start
	// to record, and an invented one would put a wrong duration in the
	// history this type exists to keep.
	StartedAt *time.Time `json:"started_at,omitempty"`

	FinishedAt time.Time `json:"finished_at"`
}

// Took gives how long the run took, and whether that is known.
func (a Attempt) Took() (time.Duration, bool) {
	if a.StartedAt == nil {
		return 0, false
	}
	return a.FinishedAt.Sub(*a.StartedAt), true
}

// Worker is one worker, asking about one queue.
//
// A row for each pair and not one for each worker. A worker asks for one
// queue at a time, so a row for the worker alone would hold whichever queue
// it asked about last, which reads like a worker moving between queues.
type Worker struct {
	ID    string `json:"id"`
	Queue string `json:"queue"`

	FirstSeenAt time.Time `json:"first_seen_at"`
	LastSeenAt  time.Time `json:"last_seen_at"`
}

// Idle reports how long ago this worker last asked for work.
func (w Worker) Idle(now time.Time) time.Duration { return now.Sub(w.LastSeenAt) }

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
	//
	// The second value says whether anything was stored. It is false when the
	// job carried an idempotency key that had been used before, in which case
	// the job returned is the one that already existed. A caller answering
	// HTTP uses it to choose between 201 and 200.
	Create(ctx context.Context, n NewJob) (*Job, bool, error)

	// Get returns one job, or ErrNotFound.
	Get(ctx context.Context, id string) (*Job, error)

	// Lease hands ready jobs to a worker and raises the attempt count of
	// each. Two callers asking at once never receive the same job.
	Lease(ctx context.Context, req LeaseRequest) ([]*Job, error)

	// Report records what happened to a leased job and returns it as stored.
	// It returns ErrLeaseNotValid when the lease named is not the lease the
	// job holds, and changes nothing in that case.
	Report(ctx context.Context, rep Report) (*Job, error)

	// ExtendLease pushes the expiry of a lease further out, and returns the
	// job as stored.
	//
	// It is what lets a handler run for longer than the lease it was given
	// without the queue deciding it has died. The alternative is asking every
	// caller to guess the slowest their work will ever be and set the lease
	// to that, which means every genuinely dead worker then holds its jobs
	// for exactly as long as the slowest job in the system.
	//
	// It returns ErrLeaseNotValid when the lease named is not the one the job
	// holds, which covers a job that was reclaimed and a job that was
	// cancelled. A worker learns both from the refusal.
	ExtendLease(ctx context.Context, jobID, leaseID string, by time.Duration) (*Job, error)

	// Cancel stops a job that has not finished, and returns it as stored.
	//
	// A job a worker is holding can be cancelled. Its lease is cleared, so
	// the report that worker sends later is refused in exactly the way a
	// reclaimed job's is. Nothing here reaches into the worker: a handler
	// already running goes on running, and the queue simply stops caring what
	// it says.
	//
	// It returns ErrWrongState for a job that has already finished.
	Cancel(ctx context.Context, id, actor string) (*Job, error)

	// Revive puts a dead or cancelled job back in the queue, and returns it
	// as stored.
	//
	// The attempt count goes back to zero, so the job gets the full set of
	// tries again. That is what somebody clearing a dead letter queue after
	// fixing the thing that broke actually wants: leaving the count where it
	// was would give the job one more try and send it straight back.
	//
	// A job that succeeded cannot be revived. Running it again is a new piece
	// of work and deserves a new job, with its own identifier that the caller
	// can follow.
	//
	// It returns ErrWrongState for any other state.
	Revive(ctx context.Context, id, actor string) (*Job, error)

	// ReclaimExpired returns jobs whose lease has run out, and reports how
	// many it moved. A job that has no attempts left is buried instead.
	ReclaimExpired(ctx context.Context, limit int) (int, error)

	// DeleteFinished removes jobs in one finished state that stopped moving
	// before a time, and reports how many it removed.
	//
	// Only a terminal state is accepted. Deleting a job that is waiting or
	// that a worker is holding would lose work, and a sweeper is exactly the
	// place where a wrong status is not noticed for a month.
	DeleteFinished(ctx context.Context, status jobs.Status, before time.Time, limit int) (int, error)

	// Workers lists the workers that have asked for work, most recently
	// seen first.
	//
	// A worker is recorded where it asks, so this holds every worker the
	// queue has heard from and not only the ones holding a job. An empty
	// queue and a fleet that has stopped look the same in every other
	// answer, and the second one is an outage.
	Workers(ctx context.Context) ([]Worker, error)

	// DeleteStaleWorkers removes workers last seen before a time, and reports
	// how many it removed.
	//
	// A worker identifier is usually the name of a container, so a
	// deployment retires every row in this table and writes a new set.
	// Without this the table grows once for each worker on each release, for
	// ever.
	DeleteStaleWorkers(ctx context.Context, before time.Time, limit int) (int, error)

	// Attempts lists the finished runs of one job, oldest run first.
	//
	// An empty list means the job has not finished a run yet, and is not the
	// same as a job that is not there, which is ErrNotFound.
	Attempts(ctx context.Context, jobID string) ([]Attempt, error)

	// QueueStats counts the jobs by queue and by status.
	QueueStats(ctx context.Context) ([]QueueStat, error)

	// List returns matching jobs, newest first.
	//
	// A caller pages by passing the identifier of the last job it received as
	// the next Before. An empty result means the end.
	List(ctx context.Context, f Filter) ([]*Job, error)

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

// WithDefaults fills in what the caller left out, so that a zero Options is
// usable and a partly filled one does not panic on a nil function.
func (o Options) WithDefaults() Options {
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

// PolicyFor returns the retry rules for one job. The waits come from the
// store and the retry count comes from the job, because the caller who
// submitted it chose that number.
func (o Options) PolicyFor(maxRetries int) jobs.Policy {
	p := o.Policy
	p.MaxRetries = maxRetries
	return p
}
