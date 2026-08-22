// Package memory keeps jobs in a map.
//
// It exists so that the contract suite runs with nothing installed, and so
// that the API tests, the gRPC tests and a developer trying the server for the
// first time do not need a database. It passes the same suite as the
// PostgreSQL store, which is the only reason it can be trusted to stand in
// for one.
//
// It is not a deployment target. Nothing here survives a restart, and the
// server says so at startup when it is asked to use this store.
package memory

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/Stiven-Gjekaj/GoQuorra/internal/jobs"
	"github.com/Stiven-Gjekaj/GoQuorra/internal/store"
	"github.com/google/uuid"
)

// Store holds jobs in a map behind one mutex.
//
// One mutex rather than a finer lock, because every operation is short and the
// contended one is Lease, which has to serialise anyway to keep two workers
// from taking the same job. That is the same promise SKIP LOCKED gives in
// PostgreSQL, arrived at more cheaply.
type Store struct {
	opts store.Options

	mu      sync.Mutex
	records map[string]*record
	next    uint64
}

// record is a job and the order it arrived in.
//
// The sequence breaks a tie that a wall clock cannot. A test drives the clock
// by hand, so two jobs created in one step carry the same timestamp to the
// nanosecond, and an order that depends on the timestamp alone comes out
// differently between runs.
type record struct {
	job store.Job
	seq uint64
}

// New makes an empty store.
func New(opts store.Options) *Store {
	return &Store{
		opts:    opts.WithDefaults(),
		records: make(map[string]*record),
	}
}

// Create stores a new job.
func (s *Store) Create(ctx context.Context, n store.NewJob) (*store.Job, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := n.Validate(); err != nil {
		return nil, err
	}

	job := s.opts.Prepare(n, uuid.NewString(), s.opts.Now())

	s.mu.Lock()
	defer s.mu.Unlock()
	s.next++
	s.records[job.ID] = &record{job: *job, seq: s.next}

	return clone(job), nil
}

// Get returns one job.
func (s *Store) Get(ctx context.Context, id string) (*store.Job, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	rec, found := s.records[id]
	if !found {
		return nil, store.ErrNotFound
	}
	return clone(&rec.job), nil
}

// Lease hands ready jobs to a worker.
func (s *Store) Lease(ctx context.Context, req store.LeaseRequest) ([]*store.Job, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if req.Limit <= 0 {
		return nil, nil
	}

	now := s.opts.Now()

	s.mu.Lock()
	defer s.mu.Unlock()

	ready := make([]*record, 0, len(s.records))
	for _, rec := range s.records {
		if rec.job.Status != jobs.Pending || rec.job.Queue != req.Queue {
			continue
		}
		// RunAt exactly now is ready. A job scheduled for this instant has
		// arrived, and the test that leases one second early depends on the
		// comparison being written this way round.
		if rec.job.RunAt.After(now) {
			continue
		}
		ready = append(ready, rec)
	}

	sort.Slice(ready, func(i, j int) bool {
		a, b := ready[i], ready[j]
		if a.job.Priority != b.job.Priority {
			return a.job.Priority > b.job.Priority
		}
		if !a.job.RunAt.Equal(b.job.RunAt) {
			return a.job.RunAt.Before(b.job.RunAt)
		}
		return a.seq < b.seq
	})

	if len(ready) > req.Limit {
		ready = ready[:req.Limit]
	}

	expires := now.Add(req.TTL)
	leased := make([]*store.Job, 0, len(ready))
	for _, rec := range ready {
		// One lease identifier for each job rather than one for the batch.
		// A batch identifier lets a worker that finished one job send a
		// report that matches every other job in the same batch.
		rec.job.Status = jobs.Leased
		rec.job.LeaseID = uuid.NewString()
		rec.job.LeasedBy = req.WorkerID
		rec.job.LeaseExpiresAt = &expires
		rec.job.Attempts++
		rec.job.UpdatedAt = now

		leased = append(leased, clone(&rec.job))
	}

	return leased, nil
}

// Report records what happened to a leased job.
func (s *Store) Report(ctx context.Context, rep store.Report) (*store.Job, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	now := s.opts.Now()

	s.mu.Lock()
	defer s.mu.Unlock()

	rec, found := s.records[rep.JobID]
	if !found {
		return nil, store.ErrNotFound
	}

	// An unleased job carries an empty lease. Comparing the two strings alone
	// would let a report with no lease identifier match it, and any caller
	// could then retire any waiting job in the table.
	if rec.job.LeaseID == "" || rec.job.LeaseID != rep.LeaseID {
		return nil, store.ErrLeaseNotValid
	}

	s.apply(rec, rep.Outcome, rep.Error, now)
	return clone(&rec.job), nil
}

// Cancel stops a job that has not finished.
func (s *Store) Cancel(ctx context.Context, id string) (*store.Job, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	now := s.opts.Now()

	s.mu.Lock()
	defer s.mu.Unlock()

	rec, found := s.records[id]
	if !found {
		return nil, store.ErrNotFound
	}
	if rec.job.Status.Terminal() {
		return nil, fmt.Errorf("%w: the job is %s and has already finished", store.ErrWrongState, rec.job.Status)
	}

	// The lease goes with it. A worker still running this job reports later
	// and is refused, which is the same path a reclaimed job takes.
	rec.job.Status = jobs.Cancelled
	rec.job.LeaseID = ""
	rec.job.LeasedBy = ""
	rec.job.LeaseExpiresAt = nil
	rec.job.UpdatedAt = now

	return clone(&rec.job), nil
}

// Revive puts a dead or cancelled job back in the queue.
func (s *Store) Revive(ctx context.Context, id string) (*store.Job, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	now := s.opts.Now()

	s.mu.Lock()
	defer s.mu.Unlock()

	rec, found := s.records[id]
	if !found {
		return nil, store.ErrNotFound
	}
	if rec.job.Status != jobs.Dead && rec.job.Status != jobs.Cancelled {
		return nil, fmt.Errorf(
			"%w: the job is %s, and only a dead or cancelled job can be revived", store.ErrWrongState, rec.job.Status)
	}

	// A fresh set of attempts, and ready now. The last error stays on the
	// row, because the thing that went wrong before is what somebody looking
	// at the job afterwards wants to see.
	rec.job.Status = jobs.Pending
	rec.job.Attempts = 0
	rec.job.RunAt = now
	rec.job.UpdatedAt = now
	rec.job.LeaseID = ""
	rec.job.LeasedBy = ""
	rec.job.LeaseExpiresAt = nil

	return clone(&rec.job), nil
}

// ReclaimExpired returns jobs whose lease has run out.
func (s *Store) ReclaimExpired(ctx context.Context, limit int) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if limit <= 0 {
		return 0, nil
	}

	now := s.opts.Now()

	s.mu.Lock()
	defer s.mu.Unlock()

	expired := make([]*record, 0)
	for _, rec := range s.records {
		if rec.job.Status != jobs.Leased || rec.job.LeaseExpiresAt == nil {
			continue
		}
		if rec.job.LeaseExpiresAt.After(now) {
			continue
		}
		expired = append(expired, rec)
	}

	sort.Slice(expired, func(i, j int) bool { return expired[i].seq < expired[j].seq })
	if len(expired) > limit {
		expired = expired[:limit]
	}

	for _, rec := range expired {
		s.apply(rec, jobs.OutcomeExpired, expiryMessage(rec.job.LeasedBy), now)
	}

	return len(expired), nil
}

// apply writes what the domain decided. Both a reported outcome and an expiry
// come through here, so the two age a job identically.
func (s *Store) apply(rec *record, outcome jobs.Outcome, message string, now time.Time) {
	decision := s.opts.PolicyFor(rec.job.MaxRetries).Decide(rec.job.Attempts, outcome, now, s.opts.Jitter())

	rec.job.Status = decision.Status
	rec.job.Attempts = decision.Attempts
	rec.job.RunAt = decision.RunAt
	rec.job.UpdatedAt = now
	rec.job.LeaseID = ""
	rec.job.LeasedBy = ""
	rec.job.LeaseExpiresAt = nil

	if outcome != jobs.OutcomeDone {
		rec.job.LastError = message
	}
}

// QueueStats counts the jobs by queue and by status.
func (s *Store) QueueStats(ctx context.Context) ([]store.QueueStat, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	counts := map[store.QueueStat]int{}
	for _, rec := range s.records {
		counts[store.QueueStat{Queue: rec.job.Queue, Status: rec.job.Status}]++
	}

	out := make([]store.QueueStat, 0, len(counts))
	for key, n := range counts {
		key.Count = n
		out = append(out, key)
	}

	sort.Slice(out, func(i, j int) bool {
		if out[i].Queue != out[j].Queue {
			return out[i].Queue < out[j].Queue
		}
		return out[i].Status < out[j].Status
	})

	return out, nil
}

// List returns matching jobs, newest first.
func (s *Store) List(ctx context.Context, f store.Filter) ([]*store.Job, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := f.Validate(); err != nil {
		return nil, err
	}
	if f.Limit <= 0 {
		return nil, nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	// The cursor names a job, and what is wanted is its place in the order.
	// A cursor naming a job that has been removed leaves the page start
	// undefined, so it is refused rather than quietly treated as the start.
	var before uint64
	if f.Before != "" {
		rec, found := s.records[f.Before]
		if !found {
			return nil, store.ErrNotFound
		}
		before = rec.seq
	}

	matching := make([]*record, 0, len(s.records))
	for _, rec := range s.records {
		if f.Queue != "" && rec.job.Queue != f.Queue {
			continue
		}
		if f.Status != "" && rec.job.Status != f.Status {
			continue
		}
		if f.Type != "" && rec.job.Type != f.Type {
			continue
		}
		if f.Before != "" && rec.seq >= before {
			continue
		}
		matching = append(matching, rec)
	}

	sort.Slice(matching, func(i, j int) bool { return matching[i].seq > matching[j].seq })

	if len(matching) > f.Limit {
		matching = matching[:f.Limit]
	}

	out := make([]*store.Job, len(matching))
	for i, rec := range matching {
		out[i] = clone(&rec.job)
	}
	return out, nil
}

// Close releases nothing, and exists so that a caller can treat every store
// the same way.
func (s *Store) Close() error { return nil }

func expiryMessage(worker string) string {
	if worker == "" {
		return "the lease ran out before any worker reported"
	}
	return "the lease held by " + worker + " ran out before it reported"
}

// clone returns a copy that shares nothing with the stored job.
//
// Returning the stored value directly hands the caller a pointer into the map.
// A handler that then edits the payload it received, or that holds the job
// while another goroutine leases it, changes what the store believes.
func clone(job *store.Job) *store.Job {
	out := *job

	if job.Payload != nil {
		out.Payload = append(json.RawMessage(nil), job.Payload...)
	}
	if job.LeaseExpiresAt != nil {
		expires := *job.LeaseExpiresAt
		out.LeaseExpiresAt = &expires
	}

	return &out
}
