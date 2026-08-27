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

	// workers holds every worker the store has heard from, keyed by the
	// worker and the queue it asked about.
	workers map[workerKey]store.Worker

	next uint64
}

// workerKey names one worker asking about one queue.
type workerKey struct {
	id    string
	queue string
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

	// attempts is what happened on each finished run, oldest first.
	//
	// A slice and not a map keyed by the attempt number. Reviving a job sets
	// its count back to zero, so a job that was buried and revived holds two
	// runs numbered 1, and only the order they were appended in says which
	// came first.
	attempts []store.Attempt
}

// New makes an empty store.
func New(opts store.Options) *Store {
	return &Store{
		opts:    opts.WithDefaults(),
		records: make(map[string]*record),
		byKey:   make(map[string]string),
		workers: make(map[workerKey]store.Worker),
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

	// Every job it waits for has to be here already. That is what makes a
	// cycle impossible, and it is also the honest answer to a caller that
	// names a job that never existed: the queue cannot say when a job it has
	// never heard of will succeed.
	parents := make([]jobs.Status, 0, len(n.After))
	for _, id := range n.After {
		parent, found := s.records[id]
		if !found {
			return nil, false, fmt.Errorf(
				"%w: the job waits for %s, and there is no such job", store.ErrNotFound, id)
		}
		parents = append(parents, parent.job.Status)
	}

	job.After = append([]string(nil), n.After...)
	job.Status = jobs.AfterState(parents)
	if job.Status == jobs.Cancelled {
		job.LastError = afterMessage(n.After, s.records)
	}

	s.next++
	s.records[job.ID] = &record{job: *job, seq: s.next}

	return clone(job), true, nil
}

// afterMessage says which of the jobs a job waited for stopped it.
//
// The identifier and the state, because "a job it waited for failed" sends
// somebody to read every one of them. Only the first is named: one parent
// that cannot succeed is the whole reason, and listing the rest would suggest
// they all have to be fixed.
func afterMessage(after []string, records map[string]*record) string {
	for _, id := range after {
		parent, found := records[id]
		if found && parent.job.Status.Terminal() && parent.job.Status != jobs.Succeeded {
			return "the job it waits for, " + id + ", is " + parent.job.Status.String()
		}
	}
	return "a job it waits for cannot succeed"
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

	// Recorded before the jobs are looked for, and whether or not any come
	// back. An ask that finds nothing is the ask that matters: it is the only
	// sign a fleet with no work is still there.
	if req.WorkerID != "" {
		key := workerKey{id: req.WorkerID, queue: req.Queue}
		seen, found := s.workers[key]
		if !found {
			seen = store.Worker{ID: req.WorkerID, Queue: req.Queue, FirstSeenAt: now}
		}
		seen.LastSeenAt = now
		s.workers[key] = seen
	}

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
	leasedAt := now
	leased := make([]*store.Job, 0, len(ready))
	for _, rec := range ready {
		// One lease identifier for each job rather than one for the batch.
		// A batch identifier lets a worker that finished one job send a
		// report that matches every other job in the same batch.
		rec.job.Status = jobs.Leased
		rec.job.LeaseID = uuid.NewString()
		rec.job.LeasedBy = req.WorkerID
		rec.job.LeaseExpiresAt = &expires
		rec.job.LeasedAt = &leasedAt
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
	rec.job.LeasedAt = nil
	rec.job.UpdatedAt = now
	recordAction(&rec.job, actor, now)

	// What was waiting for this job will never run either.
	s.settleAfter(rec.job.ID, now)

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
	// A revived job goes back to waiting when it still waits.
	//
	// Sending it to pending would run it before the parent it was submitted
	// to follow, which is the one thing the whole feature exists to stop. A
	// job whose parents have since succeeded is pending, and one whose parent
	// is still dead cannot be revived until that parent is.
	parents := make([]jobs.Status, 0, len(rec.job.After))
	for _, id := range rec.job.After {
		if parent, found := s.records[id]; found {
			parents = append(parents, parent.job.Status)
			continue
		}
		parents = append(parents, jobs.Succeeded)
	}
	back := jobs.AfterState(parents)
	if back == jobs.Cancelled {
		return nil, fmt.Errorf(
			"%w: %s", store.ErrWrongState, afterMessage(rec.job.After, s.records))
	}

	rec.job.Status = back
	rec.job.Attempts = 0
	rec.job.RunAt = now
	rec.job.UpdatedAt = now
	rec.job.LeaseID = ""
	rec.job.LeasedBy = ""
	rec.job.LeaseExpiresAt = nil
	rec.job.LeasedAt = nil
	recordAction(&rec.job, actor, now)

	// A job that was itself a parent releases what waited for it, because a
	// revive can take a chain out of cancelled.
	s.settleAfter(rec.job.ID, now)

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

	// Recorded before the job moves, from the run that ended and not from the
	// decision. Decide leaves the count alone today, so the two hold the same
	// number and no test can tell them apart. That is the reason to be
	// careful here rather than a reason not to be: a policy that ever moved
	// the count would renumber this row to describe the run that comes next.
	//
	// A run that finished carries no error, whatever the job carried before
	// it. The job keeps its last error on purpose, and copying it here would
	// put an old failure on the row of the attempt that worked.
	attempt := store.Attempt{
		JobID:      rec.job.ID,
		Number:     rec.job.Attempts,
		Worker:     rec.job.LeasedBy,
		Outcome:    outcome,
		FinishedAt: now,
	}
	if outcome != jobs.OutcomeDone {
		attempt.Error = message
	}
	if rec.job.LeasedAt != nil {
		at := *rec.job.LeasedAt
		attempt.StartedAt = &at
	}
	rec.attempts = append(rec.attempts, attempt)

	rec.job.Status = decision.Status
	rec.job.Attempts = decision.Attempts
	rec.job.RunAt = decision.RunAt
	rec.job.UpdatedAt = now
	rec.job.LeaseID = ""
	rec.job.LeasedBy = ""
	rec.job.LeaseExpiresAt = nil
	rec.job.LeasedAt = nil

	if outcome != jobs.OutcomeDone {
		rec.job.LastError = message
	}

	s.settleAfter(rec.job.ID, now)
}

// settleAfter moves the jobs that were waiting for one that has stopped.
//
// Called from every path that puts a job into a state it will not leave:
// reporting, a lease running out, a cancel and a revive. A revive is in the
// list because it takes a job out of a terminal state, and a child cancelled
// because its parent died has to be able to come back the same way.
//
// It walks the whole table. A store that holds a hundred thousand jobs in
// memory has already chosen this trade, and the store that does not is the
// one behind PostgreSQL, which answers the same question with an index.
func (s *Store) settleAfter(parentID string, now time.Time) {
	for _, rec := range s.records {
		if rec.job.Status != jobs.Blocked {
			continue
		}
		if !waitsFor(rec.job.After, parentID) {
			continue
		}

		parents := make([]jobs.Status, 0, len(rec.job.After))
		for _, id := range rec.job.After {
			if parent, found := s.records[id]; found {
				parents = append(parents, parent.job.Status)
				continue
			}
			// A parent the retention sweep removed succeeded long enough ago
			// to be forgotten. Treating it as still waiting would hold the
			// child for ever with nothing to explain it.
			parents = append(parents, jobs.Succeeded)
		}

		wanted := jobs.AfterState(parents)
		if wanted == jobs.Blocked {
			continue
		}

		rec.job.Status = wanted
		rec.job.UpdatedAt = now
		if wanted == jobs.Pending {
			// Ready now and not at the time it was submitted. A job held for
			// an hour by its parent is not an hour late.
			rec.job.RunAt = now
		} else {
			rec.job.LastError = afterMessage(rec.job.After, s.records)
		}

		// The children of this one, if it was itself a parent. A chain of
		// three jobs where the first dies has to cancel both of the others,
		// and only the second is reached by the loop above.
		s.settleAfter(rec.job.ID, now)
	}
}

// waitsFor reports whether a job waits for one named job.
func waitsFor(after []string, id string) bool {
	for _, one := range after {
		if one == id {
			return true
		}
	}
	return false
}

// Attempts lists the finished runs of one job, oldest run first.
func (s *Store) Attempts(ctx context.Context, jobID string) ([]store.Attempt, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	rec, found := s.records[jobID]
	if !found {
		// An empty list means a job that has not finished a run. A job that
		// is not there is a different answer, and the caller above turns the
		// two into a 200 and a 404.
		return nil, store.ErrNotFound
	}
	if len(rec.attempts) == 0 {
		return nil, nil
	}

	// A copy, so that a caller holding the answer cannot change what the
	// store believes, and so that the next run appending to the slice does
	// not write into an array the caller is reading.
	out := make([]store.Attempt, len(rec.attempts))
	for i, a := range rec.attempts {
		out[i] = a
		if a.StartedAt != nil {
			at := *a.StartedAt
			out[i].StartedAt = &at
		}
	}
	return out, nil
}

// Workers lists the workers that have asked for work, most recently first.
func (s *Store) Workers(ctx context.Context) ([]store.Worker, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if len(s.workers) == 0 {
		return nil, nil
	}

	out := make([]store.Worker, 0, len(s.workers))
	for _, w := range s.workers {
		out = append(out, w)
	}

	// Most recently seen first, and then by name, so that two workers seen in
	// the same step of a driven clock come out in the same order every run.
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if !a.LastSeenAt.Equal(b.LastSeenAt) {
			return a.LastSeenAt.After(b.LastSeenAt)
		}
		if a.ID != b.ID {
			return a.ID < b.ID
		}
		return a.Queue < b.Queue
	})
	return out, nil
}

// DeleteStaleWorkers removes workers last seen before a time.
func (s *Store) DeleteStaleWorkers(ctx context.Context, before time.Time, limit int) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if limit <= 0 {
		return 0, nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	// Oldest first, so a backlog is worked through from the far end rather
	// than the same rows being looked at on every sweep.
	var doomed []workerKey
	for key, w := range s.workers {
		if w.LastSeenAt.Before(before) {
			doomed = append(doomed, key)
		}
	}
	sort.Slice(doomed, func(i, j int) bool {
		a, b := s.workers[doomed[i]], s.workers[doomed[j]]
		if !a.LastSeenAt.Equal(b.LastSeenAt) {
			return a.LastSeenAt.Before(b.LastSeenAt)
		}
		return a.ID < b.ID
	})
	if len(doomed) > limit {
		doomed = doomed[:limit]
	}

	for _, key := range doomed {
		delete(s.workers, key)
	}
	return len(doomed), nil
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
	if job.LeasedAt != nil {
		at := *job.LeasedAt
		out.LeasedAt = &at
	}
	if job.After != nil {
		out.After = append([]string(nil), job.After...)
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
