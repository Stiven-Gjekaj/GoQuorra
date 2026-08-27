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

	// byKey points an idempotency key at the job that claimed it.
	byKey map[string]string

	next uint64
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
		byKey:   make(map[string]string),
	}
}

// Create stores a new job.
func (s *Store) Create(ctx context.Context, n store.NewJob) (*store.Job, bool, error) {
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	if err := n.Validate(); err != nil {
		return nil, false, err
	}

	job := s.opts.Prepare(n, uuid.NewString(), s.opts.Now())

	s.mu.Lock()
	defer s.mu.Unlock()

	// The check and the write are both inside the lock, because two
	// submissions carrying one key arriving together is the case this exists
	// for. Checking first and writing after would let both through.
	if n.IdempotencyKey != "" {
		if id, taken := s.byKey[n.IdempotencyKey]; taken {
			return clone(&s.records[id].job), false, nil
		}
		s.byKey[n.IdempotencyKey] = job.ID
	}

	s.next++
	s.records[job.ID] = &record{job: *job, seq: s.next}

	return clone(job), true, nil
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
	if err := rep.Validate(); err != nil {
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

	// Only on a success. The output of an attempt that failed is not an
	// output, and keeping it would leave the value from a failed run sitting
	// on a job that later succeeded with a different one.
	if rep.Outcome == jobs.OutcomeDone && len(rep.Result) > 0 {
		rec.job.Result = append(json.RawMessage(nil), rep.Result...)
	}

	return clone(&rec.job), nil
}

// ExtendLease pushes the expiry of a lease further out.
func (s *Store) ExtendLease(ctx context.Context, jobID, leaseID string, by time.Duration) (*store.Job, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if by <= 0 {
		return nil, fmt.Errorf("store: cannot extend a lease by %s", by)
	}

	now := s.opts.Now()

	s.mu.Lock()
	defer s.mu.Unlock()

	rec, found := s.records[jobID]
	if !found {
		return nil, store.ErrNotFound
	}

	// An unleased job holds an empty identifier, so the comparison has to
	// refuse that before it compares, or a request carrying none matches.
	if rec.job.Status != jobs.Leased || rec.job.LeaseID == "" || rec.job.LeaseID != leaseID {
		return nil, store.ErrLeaseNotValid
	}

	// From now, not from the old expiry. A worker that heartbeats late
	// should get its full extension from the moment it asked, and adding to
	// an expiry already in the past would hand back a lease that has already
	// run out.
	expires := now.Add(by)
	rec.job.LeaseExpiresAt = &expires
	rec.job.UpdatedAt = now

	return clone(&rec.job), nil
}

// Cancel stops a job that has not finished.
func (s *Store) Cancel(ctx context.Context, id, actor string) (*store.Job, error) {
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
	recordAction(&rec.job, actor, now)

	return clone(&rec.job), nil
}

// Revive puts a dead or cancelled job back in the queue.
func (s *Store) Revive(ctx context.Context, id, actor string) (*store.Job, error) {
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
	recordAction(&rec.job, actor, now)

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

// DeleteFinished removes finished jobs that stopped moving before a time.
func (s *Store) DeleteFinished(ctx context.Context, status jobs.Status, before time.Time, limit int) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if !status.Terminal() {
		return 0, fmt.Errorf("store: %q is not a finished state, and removing a job in it would lose work", status)
	}
	if limit <= 0 {
		return 0, nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	// Oldest first, so a backlog is worked through from the far end rather
	// than the same recent rows being looked at on every sweep.
	var doomed []*record
	for _, rec := range s.records {
		if rec.job.Status == status && rec.job.UpdatedAt.Before(before) {
			doomed = append(doomed, rec)
		}
	}
	sort.Slice(doomed, func(i, j int) bool { return doomed[i].seq < doomed[j].seq })
	if len(doomed) > limit {
		doomed = doomed[:limit]
	}

	for _, rec := range doomed {
		delete(s.records, rec.job.ID)
		// The key goes with the job. Leaving it would refuse a submission
		// for ever on behalf of a job nobody can look at any more.
		if rec.job.IdempotencyKey != "" {
			delete(s.byKey, rec.job.IdempotencyKey)
		}
	}

	return len(doomed), nil
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
	var beforeRunAt time.Time
	if f.Before != "" {
		rec, found := s.records[f.Before]
		if !found {
			return nil, store.ErrNotFound
		}
		before = rec.seq
		beforeRunAt = rec.job.RunAt
	}

	// after says whether a record falls on the far side of the cursor, in
	// whatever order was asked for.
	//
	// In the soonest order the comparison is on the pair and not on run_at
	// alone, because run_at is not unique: a burst of submissions shares one
	// value and every job a reclaim sweep returns shares one. seq is unique,
	// so the pair is, so the order is total and the cursor lands between two
	// rows rather than in the middle of a group.
	after := func(rec *record) bool {
		if f.Order == store.Soonest {
			if rec.job.RunAt.Equal(beforeRunAt) {
				return rec.seq > before
			}
			return rec.job.RunAt.After(beforeRunAt)
		}
		return rec.seq < before
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
		if f.Worker != "" && rec.job.LeasedBy != f.Worker {
			continue
		}
		if !f.DueBy.IsZero() && rec.job.RunAt.After(f.DueBy) {
			continue
		}
		if f.Before != "" && !after(rec) {
			continue
		}
		matching = append(matching, rec)
	}

	sort.Slice(matching, func(i, j int) bool {
		if f.Order == store.Soonest {
			if !matching[i].job.RunAt.Equal(matching[j].job.RunAt) {
				return matching[i].job.RunAt.Before(matching[j].job.RunAt)
			}
			return matching[i].seq < matching[j].seq
		}
		return matching[i].seq > matching[j].seq
	})

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
	if job.Result != nil {
		out.Result = append(json.RawMessage(nil), job.Result...)
	}
	if job.LeaseExpiresAt != nil {
		expires := *job.LeaseExpiresAt
		out.LeaseExpiresAt = &expires
	}
	if job.ActedAt != nil {
		at := *job.ActedAt
		out.ActedAt = &at
	}

	return &out
}

// recordAction writes the caller that cancelled or revived a job.
//
// A name and a moment, or neither of them. The two fields hold the last
// action and not a history, so a caller that does not name itself clears
// them: leaving the previous name there would say that somebody cancelled
// this job who did not.
func recordAction(job *store.Job, actor string, now time.Time) {
	if actor == "" {
		job.ActedBy = ""
		job.ActedAt = nil
		return
	}
	at := now.UTC()
	job.ActedBy = actor
	job.ActedAt = &at
}
