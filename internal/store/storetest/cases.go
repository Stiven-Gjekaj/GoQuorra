package storetest

import (
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/Stiven-Gjekaj/GoQuorra/internal/jobs"
	"github.com/Stiven-Gjekaj/GoQuorra/internal/store"
)

var cases = []testCase{
	{"a stored job comes back as it went in", func(t *testing.T, s store.Store, clock *Clock) {
		retries := 5
		payload := json.RawMessage(`{"to":"a@b.c","tries":2,"nested":{"x":[1,2,3]}}`)
		made := create(t, s, store.NewJob{
			Type:       "email",
			Payload:    payload,
			Queue:      "mail",
			Priority:   7,
			MaxRetries: &retries,
		})

		got, err := s.Get(ctx(), made.ID)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}

		if got.ID != made.ID {
			t.Errorf("id = %q, want %q", got.ID, made.ID)
		}
		if got.Type != "email" {
			t.Errorf("type = %q", got.Type)
		}
		if got.Queue != "mail" {
			t.Errorf("queue = %q", got.Queue)
		}
		if got.Priority != 7 {
			t.Errorf("priority = %d", got.Priority)
		}
		if got.MaxRetries != 5 {
			t.Errorf("max retries = %d", got.MaxRetries)
		}
		if got.Status != jobs.Pending {
			t.Errorf("status = %q", got.Status)
		}
		if got.Attempts != 0 {
			t.Errorf("attempts = %d, want 0 before anything leases it", got.Attempts)
		}
		samePayload(t, got.Payload, payload)
		requireTime(t, "run at", got.RunAt, Start)
		requireTime(t, "created at", got.CreatedAt, Start)

		// A job nobody holds carries no lease. The three fields go together
		// or not at all, and a stale one of them is how a reclaimer decides
		// to take a job that nobody has.
		if got.LeaseID != "" || got.LeasedBy != "" || got.LeaseExpiresAt != nil {
			t.Errorf("an unleased job carries lease fields: %+v", got)
		}
	}},

	{"an unknown job is reported as missing and not as a failure", func(t *testing.T, s store.Store, clock *Clock) {
		_, err := s.Get(ctx(), "8de1a3d0-0000-0000-0000-000000000000")
		if !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("Get of an unknown job gave %v, want ErrNotFound", err)
		}
	}},

	{"the defaults are filled in", func(t *testing.T, s store.Store, clock *Clock) {
		got := create(t, s, store.NewJob{Type: "bare"})

		if got.Queue != store.DefaultQueue {
			t.Errorf("queue = %q, want %q", got.Queue, store.DefaultQueue)
		}
		if got.MaxRetries != Policy.MaxRetries {
			t.Errorf("max retries = %d, want the store default of %d", got.MaxRetries, Policy.MaxRetries)
		}
		samePayload(t, got.Payload, json.RawMessage(`{}`))
	}},

	{"a delayed job is held back until its time comes", func(t *testing.T, s store.Store, clock *Clock) {
		made := create(t, s, store.NewJob{Type: "later", Delay: time.Minute})
		requireTime(t, "run at", made.RunAt, Start.Add(time.Minute))

		if got := lease(t, s, store.LeaseRequest{}); len(got) != 0 {
			t.Fatalf("a delayed job was handed out %s early: %v", time.Minute, ids(got))
		}

		// One second short of the time is still too early. Testing at the
		// boundary is the only way to see an inequality written the wrong way
		// round, which passes every test that jumps an hour.
		clock.Advance(time.Minute - time.Second)
		if got := lease(t, s, store.LeaseRequest{}); len(got) != 0 {
			t.Fatalf("a delayed job was handed out one second early: %v", ids(got))
		}

		clock.Advance(time.Second)
		got := lease(t, s, store.LeaseRequest{})
		if len(got) != 1 || got[0].ID != made.ID {
			t.Fatalf("the job was not handed out when its time came: %v", ids(got))
		}
	}},

	{"a lease takes only from the queue it names", func(t *testing.T, s store.Store, clock *Clock) {
		mine := create(t, s, store.NewJob{Type: "a", Queue: "mine"})
		create(t, s, store.NewJob{Type: "b", Queue: "yours"})

		got := lease(t, s, store.LeaseRequest{Queue: "mine"})
		if len(got) != 1 || got[0].ID != mine.ID {
			t.Fatalf("leasing from \"mine\" gave %v", ids(got))
		}
	}},

	{"a lease takes the most urgent job first", func(t *testing.T, s store.Store, clock *Clock) {
		// Created oldest first, so that ordering by age alone would give the
		// reverse of what priority asks for. Without the second key an
		// implementation can pass by accident.
		low := create(t, s, store.NewJob{Type: "low", Priority: 1})
		clock.Advance(time.Second)
		high := create(t, s, store.NewJob{Type: "high", Priority: 9})
		clock.Advance(time.Second)
		middle := create(t, s, store.NewJob{Type: "middle", Priority: 5})

		got := lease(t, s, store.LeaseRequest{Limit: 3})
		want := []string{high.ID, middle.ID, low.ID}
		if fmt.Sprint(ids(got)) != fmt.Sprint(want) {
			t.Fatalf("order = %v, want highest priority first", ids(got))
		}
	}},

	{"two jobs of one priority go oldest first", func(t *testing.T, s store.Store, clock *Clock) {
		first := create(t, s, store.NewJob{Type: "a", Priority: 3})
		clock.Advance(time.Second)
		second := create(t, s, store.NewJob{Type: "b", Priority: 3})

		got := lease(t, s, store.LeaseRequest{Limit: 2})
		want := []string{first.ID, second.ID}
		if fmt.Sprint(ids(got)) != fmt.Sprint(want) {
			t.Fatalf("order = %v, want %v", ids(got), want)
		}
	}},

	{"a lease marks the job and raises its attempt count", func(t *testing.T, s store.Store, clock *Clock) {
		made := create(t, s, store.NewJob{Type: "work"})

		got := lease(t, s, store.LeaseRequest{WorkerID: "worker-7", TTL: 45 * time.Second})
		if len(got) != 1 {
			t.Fatalf("leased %d jobs, want 1", len(got))
		}
		leased := got[0]

		if leased.Status != jobs.Leased {
			t.Errorf("status = %q, want %q", leased.Status, jobs.Leased)
		}
		if leased.LeasedBy != "worker-7" {
			t.Errorf("leased by = %q", leased.LeasedBy)
		}
		if leased.LeaseID == "" {
			t.Error("the lease carries no identifier, so no report can be checked against it")
		}
		if leased.LeaseExpiresAt == nil {
			t.Fatal("the lease carries no expiry, so nothing can tell that it has run out")
		}
		requireTime(t, "lease expiry", *leased.LeaseExpiresAt, Start.Add(45*time.Second))

		// The count rises when the job is handed out, not when a worker
		// reports back. A worker that crashes reports nothing, and a count
		// kept at report time would never move, so the job would retry for
		// ever.
		if leased.Attempts != 1 {
			t.Errorf("attempts = %d, want 1", leased.Attempts)
		}

		stored, err := s.Get(ctx(), made.ID)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if stored.Attempts != 1 || stored.Status != jobs.Leased {
			t.Errorf("the stored job disagrees with the leased one: %+v", stored)
		}
	}},

	{"a leased job is not handed out again", func(t *testing.T, s store.Store, clock *Clock) {
		create(t, s, store.NewJob{Type: "work"})

		if got := lease(t, s, store.LeaseRequest{WorkerID: "one"}); len(got) != 1 {
			t.Fatalf("the first lease gave %d jobs", len(got))
		}
		if got := lease(t, s, store.LeaseRequest{WorkerID: "two"}); len(got) != 0 {
			t.Fatalf("a second worker was given a job that the first one holds: %v", ids(got))
		}
	}},

	{"a finished job is never handed out", func(t *testing.T, s store.Store, clock *Clock) {
		made := create(t, s, store.NewJob{Type: "work"})
		held := lease(t, s, store.LeaseRequest{})[0]

		if _, err := s.Report(ctx(), store.Report{JobID: made.ID, LeaseID: held.LeaseID, Outcome: jobs.OutcomeDone}); err != nil {
			t.Fatalf("Report: %v", err)
		}

		clock.Advance(time.Hour)
		if got := lease(t, s, store.LeaseRequest{}); len(got) != 0 {
			t.Fatalf("a finished job was handed out again: %v", ids(got))
		}
	}},

	{"a lease gives no more than it is asked for", func(t *testing.T, s store.Store, clock *Clock) {
		for i := 0; i < 5; i++ {
			create(t, s, store.NewJob{Type: fmt.Sprintf("work-%d", i)})
		}

		got := lease(t, s, store.LeaseRequest{Limit: 2})
		if len(got) != 2 {
			t.Fatalf("leased %d jobs, want 2", len(got))
		}
	}},

	{"a report of success finishes the job", func(t *testing.T, s store.Store, clock *Clock) {
		made := create(t, s, store.NewJob{Type: "work"})
		held := lease(t, s, store.LeaseRequest{})[0]

		clock.Advance(3 * time.Second)
		got, err := s.Report(ctx(), store.Report{JobID: made.ID, LeaseID: held.LeaseID, Outcome: jobs.OutcomeDone})
		if err != nil {
			t.Fatalf("Report: %v", err)
		}

		if got.Status != jobs.Succeeded {
			t.Errorf("status = %q, want %q", got.Status, jobs.Succeeded)
		}
		if got.LeaseID != "" || got.LeaseExpiresAt != nil || got.LeasedBy != "" {
			t.Errorf("a finished job still carries its lease: %+v", got)
		}
		requireTime(t, "updated at", got.UpdatedAt, Start.Add(3*time.Second))
	}},

	{"a report carrying the wrong lease changes nothing", func(t *testing.T, s store.Store, clock *Clock) {
		made := create(t, s, store.NewJob{Type: "work"})
		lease(t, s, store.LeaseRequest{})

		_, err := s.Report(ctx(), store.Report{
			JobID:   made.ID,
			LeaseID: "6f1c0c64-0000-0000-0000-000000000000",
			Outcome: jobs.OutcomeDone,
		})
		if !errors.Is(err, store.ErrLeaseNotValid) {
			t.Fatalf("Report with a stale lease gave %v, want ErrLeaseNotValid", err)
		}

		stored, err := s.Get(ctx(), made.ID)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if stored.Status != jobs.Leased {
			t.Errorf("status = %q, want the job left alone at %q", stored.Status, jobs.Leased)
		}
	}},

	// A worker that sends no lease identifier must not be able to finish a
	// job that nobody holds. An unleased job carries an empty lease, so a
	// straight comparison of two empty strings says they match, and any
	// caller can then retire any pending job in the table.
	{"an empty lease matches nothing", func(t *testing.T, s store.Store, clock *Clock) {
		made := create(t, s, store.NewJob{Type: "work"})

		_, err := s.Report(ctx(), store.Report{JobID: made.ID, LeaseID: "", Outcome: jobs.OutcomeDone})
		if !errors.Is(err, store.ErrLeaseNotValid) {
			t.Fatalf("an empty lease on an unleased job gave %v, want ErrLeaseNotValid", err)
		}

		stored, _ := s.Get(ctx(), made.ID)
		if stored.Status != jobs.Pending {
			t.Errorf("status = %q, want the job still waiting at %q", stored.Status, jobs.Pending)
		}
	}},

	{"a report about an unknown job is reported as missing", func(t *testing.T, s store.Store, clock *Clock) {
		_, err := s.Report(ctx(), store.Report{
			JobID:   "8de1a3d0-0000-0000-0000-000000000000",
			LeaseID: "anything",
			Outcome: jobs.OutcomeDone,
		})
		if !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("Report about an unknown job gave %v, want ErrNotFound", err)
		}
	}},

	{"a failure sends the job back with a wait", func(t *testing.T, s store.Store, clock *Clock) {
		made := create(t, s, store.NewJob{Type: "work"})
		held := lease(t, s, store.LeaseRequest{})[0]

		got, err := s.Report(ctx(), store.Report{
			JobID:   made.ID,
			LeaseID: held.LeaseID,
			Outcome: jobs.OutcomeFailed,
			Error:   "the host refused the connection",
		})
		if err != nil {
			t.Fatalf("Report: %v", err)
		}

		if got.Status != jobs.Pending {
			t.Errorf("status = %q, want %q", got.Status, jobs.Pending)
		}
		if got.LastError != "the host refused the connection" {
			t.Errorf("last error = %q", got.LastError)
		}
		if got.LeaseID != "" || got.LeaseExpiresAt != nil {
			t.Errorf("a job sent back still carries its lease: %+v", got)
		}
		if got.Attempts != 1 {
			t.Errorf("attempts = %d, want the one it used", got.Attempts)
		}

		// Jitter is fixed at zero for the suite, so the wait is half the
		// plain one. Base is 10s and this is the first retry.
		requireTime(t, "run at", got.RunAt, Start.Add(5*time.Second))

		// And the job is really held back, rather than merely carrying a
		// future date that the lease query ignores.
		if handed := lease(t, s, store.LeaseRequest{}); len(handed) != 0 {
			t.Fatalf("a job waiting out its backoff was handed straight back: %v", ids(handed))
		}
		clock.Advance(5 * time.Second)
		if handed := lease(t, s, store.LeaseRequest{}); len(handed) != 1 {
			t.Fatalf("the job did not return when its wait ended")
		}
	}},

	{"a job that runs out of attempts is buried", func(t *testing.T, s store.Store, clock *Clock) {
		made := create(t, s, store.NewJob{Type: "work"})

		// Policy.MaxRetries is 2, so the job runs three times.
		runs := 0
		for {
			handed := lease(t, s, store.LeaseRequest{})
			if len(handed) == 0 {
				break
			}
			runs++
			got, err := s.Report(ctx(), store.Report{
				JobID:   made.ID,
				LeaseID: handed[0].LeaseID,
				Outcome: jobs.OutcomeFailed,
				Error:   fmt.Sprintf("attempt %d failed", runs),
			})
			if err != nil {
				t.Fatalf("Report: %v", err)
			}
			if got.Status == jobs.Dead {
				break
			}
			clock.Advance(time.Hour)
			if runs > 10 {
				t.Fatal("the job never reached the dead letter queue")
			}
		}

		if want := Policy.MaxRetries + 1; runs != want {
			t.Errorf("the job ran %d times, want %d", runs, want)
		}

		stored, err := s.Get(ctx(), made.ID)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if stored.Status != jobs.Dead {
			t.Errorf("status = %q, want %q", stored.Status, jobs.Dead)
		}
		if stored.LastError != fmt.Sprintf("attempt %d failed", runs) {
			t.Errorf("last error = %q, want the message from the last attempt", stored.LastError)
		}
	}},

	{"a live lease is left alone", func(t *testing.T, s store.Store, clock *Clock) {
		create(t, s, store.NewJob{Type: "work"})
		lease(t, s, store.LeaseRequest{TTL: time.Minute})

		clock.Advance(59 * time.Second)
		moved, err := s.ReclaimExpired(ctx(), 100)
		if err != nil {
			t.Fatalf("ReclaimExpired: %v", err)
		}
		if moved != 0 {
			t.Errorf("reclaimed %d jobs one second before the lease ends", moved)
		}
	}},

	{"a lease that runs out is taken back", func(t *testing.T, s store.Store, clock *Clock) {
		made := create(t, s, store.NewJob{Type: "work"})
		lease(t, s, store.LeaseRequest{TTL: time.Minute})

		clock.Advance(time.Minute + time.Second)
		moved, err := s.ReclaimExpired(ctx(), 100)
		if err != nil {
			t.Fatalf("ReclaimExpired: %v", err)
		}
		if moved != 1 {
			t.Fatalf("reclaimed %d jobs, want 1", moved)
		}

		stored, err := s.Get(ctx(), made.ID)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if stored.Status != jobs.Pending {
			t.Errorf("status = %q, want %q", stored.Status, jobs.Pending)
		}
		if stored.LeaseID != "" || stored.LeaseExpiresAt != nil || stored.LeasedBy != "" {
			t.Errorf("a reclaimed job still carries its lease: %+v", stored)
		}
		if stored.Attempts != 1 {
			t.Errorf("attempts = %d, want the one the crashed worker used", stored.Attempts)
		}
		if stored.LastError == "" {
			t.Error("a reclaimed job records no reason, so nobody can tell it from a job that failed cleanly")
		}
	}},

	// The test that matters most about reclaiming.
	//
	// A worker that stalls past its lease and then wakes up still believes it
	// holds the job. By then the reclaimer has taken it back and another
	// worker may be running it. The old lease must be refused, or the slow
	// worker retires a job that somebody else is still doing.
	{"a worker that wakes up late cannot finish a job it lost", func(t *testing.T, s store.Store, clock *Clock) {
		made := create(t, s, store.NewJob{Type: "work"})
		slow := lease(t, s, store.LeaseRequest{WorkerID: "slow", TTL: time.Minute})[0]

		clock.Advance(2 * time.Minute)
		if _, err := s.ReclaimExpired(ctx(), 100); err != nil {
			t.Fatalf("ReclaimExpired: %v", err)
		}

		_, err := s.Report(ctx(), store.Report{
			JobID:   made.ID,
			LeaseID: slow.LeaseID,
			Outcome: jobs.OutcomeDone,
		})
		if !errors.Is(err, store.ErrLeaseNotValid) {
			t.Fatalf("the lost lease was accepted, giving %v", err)
		}

		stored, _ := s.Get(ctx(), made.ID)
		if stored.Status == jobs.Succeeded {
			t.Error("a worker finished a job that had been taken away from it")
		}
	}},

	{"a reclaimed job with no attempts left is buried", func(t *testing.T, s store.Store, clock *Clock) {
		made := create(t, s, store.NewJob{Type: "work"})

		// Use every attempt by letting each lease run out, which is the path
		// a repeatedly crashing worker takes.
		for i := 0; i <= Policy.MaxRetries; i++ {
			handed := lease(t, s, store.LeaseRequest{TTL: time.Minute})
			if len(handed) != 1 {
				t.Fatalf("run %d: leased %d jobs", i, len(handed))
			}
			clock.Advance(2 * time.Minute)
			if _, err := s.ReclaimExpired(ctx(), 100); err != nil {
				t.Fatalf("ReclaimExpired: %v", err)
			}
			clock.Advance(time.Hour)
		}

		stored, err := s.Get(ctx(), made.ID)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if stored.Status != jobs.Dead {
			t.Errorf("status = %q, want %q after %d expiries", stored.Status, jobs.Dead, Policy.MaxRetries+1)
		}
	}},

	{"a waiting job can be cancelled", func(t *testing.T, s store.Store, clock *Clock) {
		made := create(t, s, store.NewJob{Type: "work"})

		clock.Advance(time.Second)
		got, err := s.Cancel(ctx(), made.ID)
		if err != nil {
			t.Fatalf("Cancel: %v", err)
		}
		if got.Status != jobs.Cancelled {
			t.Errorf("status = %q, want %q", got.Status, jobs.Cancelled)
		}
		requireTime(t, "updated at", got.UpdatedAt, Start.Add(time.Second))

		// And it is really gone from the queue, rather than merely marked.
		if handed := lease(t, s, store.LeaseRequest{}); len(handed) != 0 {
			t.Errorf("a cancelled job was handed to a worker: %v", ids(handed))
		}
	}},

	// Cancelling a job a worker holds is the case that matters.
	//
	// Nothing here reaches into the worker, and nothing can: a handler
	// already running goes on running. What the queue can do is stop caring
	// what that handler says, and it does it by clearing the lease, so the
	// report arrives against a lease the job no longer holds and is refused
	// on exactly the path a reclaimed job uses.
	{"cancelling a job a worker holds refuses that worker's report", func(t *testing.T, s store.Store, clock *Clock) {
		made := create(t, s, store.NewJob{Type: "work"})
		held := lease(t, s, store.LeaseRequest{WorkerID: "busy"})[0]

		if _, err := s.Cancel(ctx(), made.ID); err != nil {
			t.Fatalf("Cancel: %v", err)
		}

		_, err := s.Report(ctx(), store.Report{
			JobID:   made.ID,
			LeaseID: held.LeaseID,
			Outcome: jobs.OutcomeDone,
		})
		if !errors.Is(err, store.ErrLeaseNotValid) {
			t.Fatalf("the report from the cancelled worker gave %v, want ErrLeaseNotValid", err)
		}

		stored, _ := s.Get(ctx(), made.ID)
		if stored.Status != jobs.Cancelled {
			t.Errorf("status = %q, want the job to stay cancelled", stored.Status)
		}
	}},

	{"a job that has finished cannot be cancelled", func(t *testing.T, s store.Store, clock *Clock) {
		made := create(t, s, store.NewJob{Type: "work"})
		held := lease(t, s, store.LeaseRequest{})[0]
		if _, err := s.Report(ctx(), store.Report{JobID: made.ID, LeaseID: held.LeaseID, Outcome: jobs.OutcomeDone}); err != nil {
			t.Fatalf("Report: %v", err)
		}

		_, err := s.Cancel(ctx(), made.ID)
		if !errors.Is(err, store.ErrWrongState) {
			t.Fatalf("Cancel of a finished job gave %v, want ErrWrongState", err)
		}

		// And it is still succeeded, not quietly overwritten.
		stored, _ := s.Get(ctx(), made.ID)
		if stored.Status != jobs.Succeeded {
			t.Errorf("status = %q, want %q", stored.Status, jobs.Succeeded)
		}
	}},

	{"cancelling twice is refused the second time", func(t *testing.T, s store.Store, clock *Clock) {
		made := create(t, s, store.NewJob{Type: "work"})

		if _, err := s.Cancel(ctx(), made.ID); err != nil {
			t.Fatalf("the first Cancel: %v", err)
		}
		if _, err := s.Cancel(ctx(), made.ID); !errors.Is(err, store.ErrWrongState) {
			t.Fatalf("the second Cancel gave %v, want ErrWrongState", err)
		}
	}},

	{"cancelling an unknown job is reported as missing", func(t *testing.T, s store.Store, clock *Clock) {
		_, err := s.Cancel(ctx(), "8de1a3d0-0000-0000-0000-000000000000")
		if !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("Cancel of an unknown job gave %v, want ErrNotFound", err)
		}

		// Text that is not an identifier at all is also a missing job.
		if _, err := s.Cancel(ctx(), "not-a-uuid"); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("Cancel of a malformed identifier gave %v, want ErrNotFound", err)
		}
	}},

	// The test this feature exists for.
	//
	// Somebody clears a dead letter queue after fixing the thing that broke.
	// A revived job has to get the full set of tries again, and the way to
	// see that is to run its whole life a second time and count the runs.
	// Leaving the attempt count where it was would give the job one more try
	// and send it straight back, which looks like it worked until the queue
	// fills up again an hour later.
	{"a revived job gets a full set of attempts", func(t *testing.T, s store.Store, clock *Clock) {
		made := create(t, s, store.NewJob{Type: "work"})

		runs := failUntilBuried(t, s, clock, made.ID)
		if want := Policy.MaxRetries + 1; runs != want {
			t.Fatalf("the job ran %d times before it was buried, want %d", runs, want)
		}

		got, err := s.Revive(ctx(), made.ID)
		if err != nil {
			t.Fatalf("Revive: %v", err)
		}
		if got.Status != jobs.Pending {
			t.Errorf("status = %q, want %q", got.Status, jobs.Pending)
		}
		if got.Attempts != 0 {
			t.Errorf("attempts = %d, want a fresh set", got.Attempts)
		}

		again := failUntilBuried(t, s, clock, made.ID)
		if want := Policy.MaxRetries + 1; again != want {
			t.Errorf("the revived job ran %d times, want %d", again, want)
		}
	}},

	{"a cancelled job can be revived", func(t *testing.T, s store.Store, clock *Clock) {
		made := create(t, s, store.NewJob{Type: "work"})
		if _, err := s.Cancel(ctx(), made.ID); err != nil {
			t.Fatalf("Cancel: %v", err)
		}

		got, err := s.Revive(ctx(), made.ID)
		if err != nil {
			t.Fatalf("Revive: %v", err)
		}
		if got.Status != jobs.Pending {
			t.Fatalf("status = %q, want %q", got.Status, jobs.Pending)
		}

		if handed := lease(t, s, store.LeaseRequest{}); len(handed) != 1 {
			t.Errorf("the revived job was not handed to a worker")
		}
	}},

	// Running a job that already worked is a new piece of work, and deserves
	// a new identifier that the caller can follow. Reviving in place would
	// hide the second run behind the record of the first.
	{"a job that succeeded cannot be revived", func(t *testing.T, s store.Store, clock *Clock) {
		made := create(t, s, store.NewJob{Type: "work"})
		held := lease(t, s, store.LeaseRequest{})[0]
		if _, err := s.Report(ctx(), store.Report{JobID: made.ID, LeaseID: held.LeaseID, Outcome: jobs.OutcomeDone}); err != nil {
			t.Fatalf("Report: %v", err)
		}

		if _, err := s.Revive(ctx(), made.ID); !errors.Is(err, store.ErrWrongState) {
			t.Fatalf("Revive of a finished job gave %v, want ErrWrongState", err)
		}
	}},

	{"a job already in the queue cannot be revived", func(t *testing.T, s store.Store, clock *Clock) {
		made := create(t, s, store.NewJob{Type: "work"})

		if _, err := s.Revive(ctx(), made.ID); !errors.Is(err, store.ErrWrongState) {
			t.Fatalf("Revive of a waiting job gave %v, want ErrWrongState", err)
		}

		lease(t, s, store.LeaseRequest{})
		if _, err := s.Revive(ctx(), made.ID); !errors.Is(err, store.ErrWrongState) {
			t.Fatalf("Revive of a leased job gave %v, want ErrWrongState", err)
		}
	}},

	{"reviving an unknown job is reported as missing", func(t *testing.T, s store.Store, clock *Clock) {
		if _, err := s.Revive(ctx(), "8de1a3d0-0000-0000-0000-000000000000"); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("Revive of an unknown job gave %v, want ErrNotFound", err)
		}
	}},

	{"the statistics count by queue and by status", func(t *testing.T, s store.Store, clock *Clock) {
		create(t, s, store.NewJob{Type: "a", Queue: "one"})
		create(t, s, store.NewJob{Type: "b", Queue: "one"})
		made := create(t, s, store.NewJob{Type: "c", Queue: "two"})

		held := lease(t, s, store.LeaseRequest{Queue: "two"})[0]
		if _, err := s.Report(ctx(), store.Report{JobID: made.ID, LeaseID: held.LeaseID, Outcome: jobs.OutcomeDone}); err != nil {
			t.Fatalf("Report: %v", err)
		}

		stats, err := s.QueueStats(ctx())
		if err != nil {
			t.Fatalf("QueueStats: %v", err)
		}

		counted := map[string]int{}
		for _, row := range stats {
			if !row.Status.Valid() {
				t.Errorf("the statistics name %q, which is not a status", row.Status)
			}
			counted[row.Queue+"/"+row.Status.String()] = row.Count
		}

		want := map[string]int{"one/pending": 2, "two/succeeded": 1}
		for key, n := range want {
			if counted[key] != n {
				t.Errorf("%s = %d, want %d (all: %v)", key, counted[key], n, counted)
			}
		}
		if _, present := counted["two/pending"]; present {
			t.Errorf("a queue with no pending jobs is counted anyway: %v", counted)
		}
	}},

	{"the recent list gives the newest first", func(t *testing.T, s store.Store, clock *Clock) {
		var made []string
		for i := 0; i < 4; i++ {
			made = append(made, create(t, s, store.NewJob{Type: fmt.Sprintf("job-%d", i)}).ID)
			clock.Advance(time.Second)
		}

		got, err := s.Recent(ctx(), 3)
		if err != nil {
			t.Fatalf("Recent: %v", err)
		}
		if len(got) != 3 {
			t.Fatalf("Recent(3) gave %d jobs", len(got))
		}

		want := []string{made[3], made[2], made[1]}
		if fmt.Sprint(ids(got)) != fmt.Sprint(want) {
			t.Errorf("order = %v, want the newest first", ids(got))
		}
	}},

	// Two workers asking at the same instant must never receive the same job.
	//
	// This is the property the whole design rests on, and it is the one that
	// a single threaded test cannot see. Against PostgreSQL it exercises the
	// SKIP LOCKED clause. Against the memory store it exercises the mutex.
	{"two workers asking at once never receive the same job", func(t *testing.T, s store.Store, clock *Clock) {
		const total = 40
		for i := 0; i < total; i++ {
			create(t, s, store.NewJob{Type: fmt.Sprintf("job-%d", i)})
		}

		const workers = 8
		var wg sync.WaitGroup
		var mu sync.Mutex
		seen := map[string]string{}
		duplicates := []string{}

		for w := 0; w < workers; w++ {
			wg.Add(1)
			go func(w int) {
				defer wg.Done()
				name := fmt.Sprintf("worker-%d", w)
				for taken := 0; taken < total; {
					got, err := s.Lease(ctx(), store.LeaseRequest{
						Queue:    store.DefaultQueue,
						WorkerID: name,
						Limit:    3,
						TTL:      time.Minute,
					})
					if err != nil {
						t.Errorf("%s: Lease: %v", name, err)
						return
					}
					if len(got) == 0 {
						return
					}
					taken += len(got)

					mu.Lock()
					for _, job := range got {
						if other, already := seen[job.ID]; already {
							duplicates = append(duplicates, fmt.Sprintf("%s went to %s and to %s", job.ID, other, name))
						}
						seen[job.ID] = name
					}
					mu.Unlock()
				}
			}(w)
		}
		wg.Wait()

		if len(duplicates) > 0 {
			t.Errorf("the same job was leased twice: %v", duplicates)
		}
		if len(seen) != total {
			t.Errorf("%d of %d jobs were leased", len(seen), total)
		}
	}},
}
