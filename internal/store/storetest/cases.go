package storetest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
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

	// The rule this feature exists for.
	//
	// A handler slower than the lease it was given used to be doomed: the
	// reclaimer took the job at the expiry and gave it to somebody else while
	// the first worker was still running it. Extending the lease is what
	// lets a slow job finish, and the way to see that it works is to push the
	// expiry out and then sweep past the moment the job would have been
	// taken.
	{"an extended lease survives the moment it would have expired", func(t *testing.T, s store.Store, clock *Clock) {
		made := create(t, s, store.NewJob{Type: "slow"})
		held := lease(t, s, store.LeaseRequest{WorkerID: "patient", TTL: time.Minute})[0]

		clock.Advance(50 * time.Second)
		got, err := s.ExtendLease(ctx(), made.ID, held.LeaseID, time.Minute)
		if err != nil {
			t.Fatalf("ExtendLease: %v", err)
		}

		// From the moment it was asked, and not from the old expiry. A worker
		// that heartbeats late gets its full extension, and adding to an
		// expiry already in the past would hand back a lease that has already
		// run out.
		requireTime(t, "lease expiry", *got.LeaseExpiresAt, Start.Add(110*time.Second))

		// Past the original expiry, and the reclaimer leaves it alone.
		clock.Advance(20 * time.Second)
		moved, err := s.ReclaimExpired(ctx(), 10)
		if err != nil {
			t.Fatalf("ReclaimExpired: %v", err)
		}
		if moved != 0 {
			t.Fatalf("the reclaimer took %d jobs past an expiry that had been pushed out", moved)
		}

		// And the worker can still report, which is the whole point.
		done, err := s.Report(ctx(), store.Report{JobID: made.ID, LeaseID: held.LeaseID, Outcome: jobs.OutcomeDone})
		if err != nil {
			t.Fatalf("Report after extending: %v", err)
		}
		if done.Status != jobs.Succeeded {
			t.Errorf("status = %q", done.Status)
		}
	}},

	{"a lease that is not held cannot be extended", func(t *testing.T, s store.Store, clock *Clock) {
		made := create(t, s, store.NewJob{Type: "work"})

		// Waiting, so nobody holds it. An empty lease must not match.
		if _, err := s.ExtendLease(ctx(), made.ID, "", time.Minute); !errors.Is(err, store.ErrLeaseNotValid) {
			t.Errorf("an empty lease on a waiting job gave %v, want ErrLeaseNotValid", err)
		}

		held := lease(t, s, store.LeaseRequest{})[0]
		if _, err := s.ExtendLease(ctx(), made.ID, "6f1c0c64-0000-0000-0000-000000000000", time.Minute); !errors.Is(err, store.ErrLeaseNotValid) {
			t.Errorf("somebody else's lease gave %v, want ErrLeaseNotValid", err)
		}
		_ = held
	}},

	// A worker learns that its job was cancelled by being refused the
	// extension. Nothing has to reach into the handler: the next heartbeat
	// simply fails, and the worker package stops the job on that.
	{"a cancelled job refuses to extend its lease", func(t *testing.T, s store.Store, clock *Clock) {
		made := create(t, s, store.NewJob{Type: "work"})
		held := lease(t, s, store.LeaseRequest{TTL: time.Minute})[0]

		if _, err := s.Cancel(ctx(), made.ID, ""); err != nil {
			t.Fatalf("Cancel: %v", err)
		}

		if _, err := s.ExtendLease(ctx(), made.ID, held.LeaseID, time.Minute); !errors.Is(err, store.ErrLeaseNotValid) {
			t.Fatalf("extending a cancelled job gave %v, want ErrLeaseNotValid", err)
		}
	}},

	{"a reclaimed job refuses to extend its lease", func(t *testing.T, s store.Store, clock *Clock) {
		made := create(t, s, store.NewJob{Type: "work"})
		held := lease(t, s, store.LeaseRequest{TTL: time.Minute})[0]

		clock.Advance(2 * time.Minute)
		if _, err := s.ReclaimExpired(ctx(), 10); err != nil {
			t.Fatalf("ReclaimExpired: %v", err)
		}

		if _, err := s.ExtendLease(ctx(), made.ID, held.LeaseID, time.Minute); !errors.Is(err, store.ErrLeaseNotValid) {
			t.Fatalf("extending a reclaimed job gave %v, want ErrLeaseNotValid", err)
		}
	}},

	{"extending an unknown job is reported as missing", func(t *testing.T, s store.Store, clock *Clock) {
		_, err := s.ExtendLease(ctx(), "8de1a3d0-0000-0000-0000-000000000000",
			"6f1c0c64-0000-0000-0000-000000000000", time.Minute)
		if !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("extending an unknown job gave %v, want ErrNotFound", err)
		}
	}},

	{"a lease cannot be extended by nothing", func(t *testing.T, s store.Store, clock *Clock) {
		made := create(t, s, store.NewJob{Type: "work"})
		held := lease(t, s, store.LeaseRequest{})[0]

		for _, by := range []time.Duration{0, -time.Minute} {
			if _, err := s.ExtendLease(ctx(), made.ID, held.LeaseID, by); err == nil {
				t.Errorf("extending by %s was accepted", by)
			}
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
		got, err := s.Cancel(ctx(), made.ID, "")
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

		if _, err := s.Cancel(ctx(), made.ID, ""); err != nil {
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

		_, err := s.Cancel(ctx(), made.ID, "")
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

		if _, err := s.Cancel(ctx(), made.ID, ""); err != nil {
			t.Fatalf("the first Cancel: %v", err)
		}
		if _, err := s.Cancel(ctx(), made.ID, ""); !errors.Is(err, store.ErrWrongState) {
			t.Fatalf("the second Cancel gave %v, want ErrWrongState", err)
		}
	}},

	{"cancelling an unknown job is reported as missing", func(t *testing.T, s store.Store, clock *Clock) {
		_, err := s.Cancel(ctx(), "8de1a3d0-0000-0000-0000-000000000000", "")
		if !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("Cancel of an unknown job gave %v, want ErrNotFound", err)
		}

		// Text that is not an identifier at all is also a missing job.
		if _, err := s.Cancel(ctx(), "not-a-uuid", ""); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("Cancel of a malformed identifier gave %v, want ErrNotFound", err)
		}
	}},

	// A submission tells a watcher that a queue may have work.
	//
	// A hint and never a promise: a worker still polls, so one that is lost
	// costs latency and nothing else. That is what makes this safe to add to
	// a protocol whose correctness already worked without it.
	{"a job that is ready now tells a watcher", func(t *testing.T, s store.Store, clock *Clock) {
		watching, cancel := context.WithCancel(ctx())
		defer cancel()

		hints, err := s.Watch(watching)
		if err != nil {
			t.Fatalf("Watch: %v", err)
		}

		create(t, s, store.NewJob{Type: "work", Queue: "mail"})

		select {
		case queue := <-hints:
			if queue != "mail" {
				t.Errorf("the hint names %q, want the queue the job went into", queue)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("a job was submitted and no hint arrived")
		}
	}},

	// A job that is not ready yet tells nobody.
	//
	// A job with a delay, or one waiting out a backoff, is deliberately not
	// urgent. A hint for it would wake every worker to find nothing.
	{"a job that is not ready yet tells nobody", func(t *testing.T, s store.Store, clock *Clock) {
		watching, cancel := context.WithCancel(ctx())
		defer cancel()

		hints, err := s.Watch(watching)
		if err != nil {
			t.Fatalf("Watch: %v", err)
		}

		create(t, s, store.NewJob{Type: "work", Queue: "mail", Delay: time.Hour})

		select {
		case queue := <-hints:
			t.Errorf("a delayed job hinted about %q", queue)
		case <-time.After(300 * time.Millisecond):
			// Nothing, which is the answer.
		}
	}},

	// Reviving a job tells a watcher, because the job is ready at once.
	{"reviving a job tells a watcher", func(t *testing.T, s store.Store, clock *Clock) {
		made := create(t, s, store.NewJob{Type: "work", Queue: "mail"})
		if _, err := s.Cancel(ctx(), made.ID, "ops"); err != nil {
			t.Fatalf("Cancel: %v", err)
		}

		watching, cancel := context.WithCancel(ctx())
		defer cancel()

		hints, err := s.Watch(watching)
		if err != nil {
			t.Fatalf("Watch: %v", err)
		}

		if _, err := s.Revive(ctx(), made.ID, "ops"); err != nil {
			t.Fatalf("Revive: %v", err)
		}

		select {
		case queue := <-hints:
			if queue != "mail" {
				t.Errorf("the hint names %q", queue)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("a job was revived and no hint arrived")
		}
	}},

	// The channel closes when the caller stops watching.
	//
	// A watcher that ended and left a channel open is a goroutine and a
	// database connection that nothing will ever close.
	{"a watcher that stops is closed", func(t *testing.T, s store.Store, clock *Clock) {
		watching, cancel := context.WithCancel(ctx())

		hints, err := s.Watch(watching)
		if err != nil {
			t.Fatalf("Watch: %v", err)
		}
		cancel()

		select {
		case _, open := <-hints:
			if open {
				// A hint that was already in flight. The next read is the
				// close.
				select {
				case _, open := <-hints:
					if open {
						t.Error("the channel is still open after the watcher stopped")
					}
				case <-time.After(5 * time.Second):
					t.Error("the channel was not closed")
				}
			}
		case <-time.After(5 * time.Second):
			t.Error("the channel was not closed when the watcher stopped")
		}
	}},

	// A schedule is stored and read back as it was written.
	{"a schedule is stored and read back", func(t *testing.T, s store.Store, clock *Clock) {
		made, err := s.CreateSchedule(ctx(), store.NewSchedule{
			Name: "nightly", Cron: "0 3 * * *", Timezone: "Europe/Berlin",
			CatchUp: jobs.CatchUpSkip, Type: "report", Queue: "reports", Priority: 5,
		})
		if err != nil {
			t.Fatalf("CreateSchedule: %v", err)
		}
		if made.ID == "" {
			t.Error("the schedule has no identifier")
		}
		if !made.Enabled {
			t.Error("a new schedule is switched off, and nothing asked for that")
		}
		if made.LastFiredAt != nil {
			t.Errorf("a new schedule claims it last fired at %s", made.LastFiredAt)
		}

		read, err := s.Schedule(ctx(), "nightly")
		if err != nil {
			t.Fatalf("Schedule: %v", err)
		}
		if read.Cron != "0 3 * * *" || read.Timezone != "Europe/Berlin" {
			t.Errorf("the schedule came back as %+v", read)
		}
		if read.CatchUp != jobs.CatchUpSkip || read.Type != "report" || read.Queue != "reports" {
			t.Errorf("the schedule came back as %+v", read)
		}
		if read.Priority != 5 {
			t.Errorf("the priority came back as %d", read.Priority)
		}

		// An absent payload is an empty object, the same way a job's is.
		if string(read.Payload) != "{}" {
			t.Errorf("the payload came back as %s", read.Payload)
		}
	}},

	// A filter that names several queues keeps those and nothing else.
	//
	// It narrows on top of Queue rather than replacing it, because the two
	// answer different questions: one is what a caller asked for and the
	// other is what a caller is allowed.
	//
	// The bulk actions are checked with the same filter, because in both
	// stores one piece of code turns a filter into a condition. If that ever
	// stops being true, a listing and a cancel would disagree about which
	// jobs a filter names, and the cancel is the one that cannot be undone.
	{"a filter that names several queues keeps those and nothing else", func(t *testing.T, s store.Store, clock *Clock) {
		for _, queue := range []string{"invoices", "receipts", "payroll"} {
			if _, _, err := s.Create(ctx(), store.NewJob{Type: "work", Queue: queue}); err != nil {
				t.Fatalf("Create in %s: %v", queue, err)
			}
		}

		held := store.Filter{Queues: []string{"invoices", "receipts"}, Limit: 50}
		listed, err := s.List(ctx(), held)
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if len(listed) != 2 {
			t.Fatalf("the listing holds %d jobs, want the two in the named queues", len(listed))
		}
		for _, job := range listed {
			if job.Queue == "payroll" {
				t.Errorf("the listing holds a job from %s", job.Queue)
			}
		}

		// Queue and Queues together, and not one replacing the other.
		both, err := s.List(ctx(), store.Filter{
			Queue: "payroll", Queues: []string{"invoices", "receipts"}, Limit: 50,
		})
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if len(both) != 0 {
			t.Errorf("asking for payroll while holding two other queues gave %d jobs", len(both))
		}

		// The same filter, through the action that cannot be undone.
		stopped, err := s.CancelMatching(ctx(), held, "ops")
		if err != nil {
			t.Fatalf("CancelMatching: %v", err)
		}
		if stopped != 2 {
			t.Errorf("the bulk cancel stopped %d jobs, want the two in the named queues", stopped)
		}

		left, err := s.List(ctx(), store.Filter{Queue: "payroll", Limit: 50})
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if len(left) != 1 || left[0].Status != jobs.Pending {
			t.Errorf("the job outside the named queues came back as %+v", left)
		}
	}},

	// A schedule that crossed the day the clock goes back fires once.
	//
	// The rule about which moment a reading names lives in internal/jobs and
	// is table tested there. What this asks is whether the mark survives the
	// round trip, because that is the part the two stores could differ on: a
	// timestamp goes into PostgreSQL with a zone and comes back in another
	// one, and the memory store keeps the Go value it was given.
	//
	// 25 October 2026 in Europe/Berlin: 03:00 CEST becomes 02:00 CET, so a
	// clock reads 02:00 twice, an hour apart. A daily schedule at 02:00 has
	// one window that day and not two.
	{"a schedule marked at a reading that happens twice does not fire again", func(t *testing.T, s store.Store, clock *Clock) {
		made, err := s.CreateSchedule(ctx(), store.NewSchedule{
			Name: "nightly", Cron: "0 2 * * *", Timezone: "Europe/Berlin",
			CatchUp: jobs.CatchUpAll, Type: "report", Queue: "reports",
		})
		if err != nil {
			t.Fatalf("CreateSchedule: %v", err)
		}

		// The first of the two readings, which is the one a schedule means.
		first := time.Date(2026, 10, 25, 0, 0, 0, 0, time.UTC)
		if err := s.MarkScheduleFired(ctx(), made.ID, first); err != nil {
			t.Fatalf("MarkScheduleFired: %v", err)
		}

		read, err := s.Schedule(ctx(), "nightly")
		if err != nil {
			t.Fatalf("Schedule: %v", err)
		}
		if read.LastFiredAt == nil {
			t.Fatal("the schedule came back with no last firing")
		}
		if !read.LastFiredAt.Equal(first) {
			t.Errorf("the mark came back as %s, want %s", read.LastFiredAt.UTC(), first)
		}

		// Noon on the same day, which is after both readings.
		windows, mark, dropped, err := read.Due(time.Date(2026, 10, 25, 11, 0, 0, 0, time.UTC))
		if err != nil {
			t.Fatalf("Due: %v", err)
		}
		if dropped != 0 {
			t.Errorf("%d windows were dropped, and the policy is to catch every one up", dropped)
		}
		if len(windows) != 0 {
			t.Errorf("the schedule fired again for %s, and 02:00 happened once", windows)
		}
		if !mark.Equal(first) {
			t.Errorf("the mark moved to %s with nothing produced", mark.UTC())
		}
	}},

	// A name that is taken is refused rather than replacing what is there.
	//
	// A schedule is something somebody refers to by name in a change request,
	// and quietly replacing one is how a rule nobody agreed to starts
	// producing jobs.
	{"a schedule name cannot be taken twice", func(t *testing.T, s store.Store, clock *Clock) {
		first := store.NewSchedule{
			Name: "nightly", Cron: "0 3 * * *", CatchUp: jobs.CatchUpSkip, Type: "report",
		}
		if _, err := s.CreateSchedule(ctx(), first); err != nil {
			t.Fatalf("CreateSchedule: %v", err)
		}

		second := first
		second.Cron = "0 4 * * *"
		if _, err := s.CreateSchedule(ctx(), second); err == nil {
			t.Fatal("a second schedule took a name that was in use")
		}

		// And the first one is untouched, which is the point.
		read, err := s.Schedule(ctx(), "nightly")
		if err != nil {
			t.Fatalf("Schedule: %v", err)
		}
		if read.Cron != "0 3 * * *" {
			t.Errorf("the stored rule is %q, and the refused one replaced it", read.Cron)
		}
	}},

	// A schedule the store cannot run is refused where it is written.
	{"a schedule that cannot work is refused", func(t *testing.T, s store.Store, clock *Clock) {
		good := store.NewSchedule{
			Name: "ok", Cron: "0 3 * * *", CatchUp: jobs.CatchUpSkip, Type: "report",
		}
		bad := map[string]func(store.NewSchedule) store.NewSchedule{
			"no name":                   func(n store.NewSchedule) store.NewSchedule { n.Name = ""; return n },
			"a rule that is not a rule": func(n store.NewSchedule) store.NewSchedule { n.Cron = "every night"; return n },
			"a rule with four fields":   func(n store.NewSchedule) store.NewSchedule { n.Cron = "0 3 * *"; return n },
			"a zone that is not a zone": func(n store.NewSchedule) store.NewSchedule { n.Timezone = "Mars/Olympus"; return n },
			"a catch up nobody knows":   func(n store.NewSchedule) store.NewSchedule { n.CatchUp = "maybe"; return n },
			"no catch up at all":        func(n store.NewSchedule) store.NewSchedule { n.CatchUp = ""; return n },
			"no job type":               func(n store.NewSchedule) store.NewSchedule { n.Type = ""; return n },
		}

		for name, change := range bad {
			_, err := s.CreateSchedule(ctx(), change(good))
			if err == nil {
				t.Errorf("%s was accepted", name)
				continue
			}
			// Refused, and refused as the caller's mistake. Without this the
			// rule passes against a store that answers "connection refused",
			// and the layer above answers 500 to a cron rule somebody
			// mistyped.
			if !errors.Is(err, store.ErrInvalid) {
				t.Errorf("%s gave %q, which does not answer to ErrInvalid", name, err)
			}
		}

		// And the good one is stored, so the refusals are not simply always
		// the answer.
		if _, err := s.CreateSchedule(ctx(), good); err != nil {
			t.Errorf("a good schedule was refused: %v", err)
		}
	}},

	// A schedule is switched off rather than deleted, and it keeps what it
	// knows.
	{"a schedule can be switched off and on", func(t *testing.T, s store.Store, clock *Clock) {
		made, err := s.CreateSchedule(ctx(), store.NewSchedule{
			Name: "nightly", Cron: "0 3 * * *", CatchUp: jobs.CatchUpSkip, Type: "report",
		})
		if err != nil {
			t.Fatalf("CreateSchedule: %v", err)
		}

		off, err := s.SetScheduleEnabled(ctx(), "nightly", false)
		if err != nil {
			t.Fatalf("SetScheduleEnabled: %v", err)
		}
		if off.Enabled {
			t.Error("the schedule is still on")
		}

		// The producing loop asks for the ones that are on, and does not see
		// it.
		running, err := s.Schedules(ctx(), true, 100)
		if err != nil {
			t.Fatalf("Schedules: %v", err)
		}
		if len(running) != 0 {
			t.Errorf("a schedule that is off is offered to the producing loop: %+v", running)
		}

		// Everything else still sees it, because a schedule that is off is
		// still one somebody wants to look at.
		all, err := s.Schedules(ctx(), false, 100)
		if err != nil {
			t.Fatalf("Schedules: %v", err)
		}
		if len(all) != 1 || all[0].ID != made.ID {
			t.Errorf("a schedule that is off has disappeared: %+v", all)
		}

		if on, err := s.SetScheduleEnabled(ctx(), "nightly", true); err != nil || !on.Enabled {
			t.Errorf("the schedule could not be switched back on: %v", err)
		}
	}},

	// The window a schedule has fired up to only ever moves forward.
	//
	// Two servers running the producing loop can mark the same schedule, and
	// the later window is the true one. Moving it back would catch up windows
	// that have already been caught.
	{"the window a schedule fired up to never moves backwards", func(t *testing.T, s store.Store, clock *Clock) {
		made, err := s.CreateSchedule(ctx(), store.NewSchedule{
			Name: "hourly", Cron: "0 * * * *", CatchUp: jobs.CatchUpSkip, Type: "report",
		})
		if err != nil {
			t.Fatalf("CreateSchedule: %v", err)
		}

		later := Start.Add(3 * time.Hour)
		if err := s.MarkScheduleFired(ctx(), made.ID, later); err != nil {
			t.Fatalf("MarkScheduleFired: %v", err)
		}

		// An earlier window, which is the second server catching up.
		if err := s.MarkScheduleFired(ctx(), made.ID, Start.Add(time.Hour)); err != nil {
			t.Fatalf("MarkScheduleFired with an earlier window: %v", err)
		}

		read, err := s.Schedule(ctx(), "hourly")
		if err != nil {
			t.Fatalf("Schedule: %v", err)
		}
		if read.LastFiredAt == nil {
			t.Fatal("the schedule has no last window")
		}
		requireTime(t, "last fired at", *read.LastFiredAt, later)
	}},

	// Deleting a schedule leaves the jobs it produced.
	//
	// They are work that happened. The identifier on them is a record of
	// where they came from, and not a pointer to something that has to still
	// exist.
	{"deleting a schedule leaves the jobs it produced", func(t *testing.T, s store.Store, clock *Clock) {
		made, err := s.CreateSchedule(ctx(), store.NewSchedule{
			Name: "nightly", Cron: "0 3 * * *", CatchUp: jobs.CatchUpSkip, Type: "report",
		})
		if err != nil {
			t.Fatalf("CreateSchedule: %v", err)
		}

		produced := create(t, s, store.NewJob{Type: "report", ScheduleID: made.ID})
		if produced.ScheduleID != made.ID {
			t.Fatalf("the job says it came from %q, want the schedule", produced.ScheduleID)
		}

		if err := s.DeleteSchedule(ctx(), "nightly"); err != nil {
			t.Fatalf("DeleteSchedule: %v", err)
		}
		if _, err := s.Schedule(ctx(), "nightly"); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("the schedule is still there: %v", err)
		}

		left, err := s.Get(ctx(), produced.ID)
		if err != nil {
			t.Fatalf("the job the schedule produced went with it: %v", err)
		}
		if left.ScheduleID != made.ID {
			t.Errorf("the job forgot where it came from: %q", left.ScheduleID)
		}
	}},

	// A schedule that is not there is ErrNotFound and not an empty one.
	{"a schedule that is not there is reported as missing", func(t *testing.T, s store.Store, clock *Clock) {
		if _, err := s.Schedule(ctx(), "nothing"); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("Schedule of an unknown name gave %v, want ErrNotFound", err)
		}
		if _, err := s.SetScheduleEnabled(ctx(), "nothing", false); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("SetScheduleEnabled of an unknown name gave %v, want ErrNotFound", err)
		}
		if err := s.DeleteSchedule(ctx(), "nothing"); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("DeleteSchedule of an unknown name gave %v, want ErrNotFound", err)
		}
	}},

	// A dead letter queue is cleared in one request.
	//
	// The reason bulk exists. Recovering after fixing what broke is the most
	// common thing an operator does to a queue, and without this it is a
	// shell loop that leaves the queue half done if it stops.
	{"reviving by filter puts back every job it names", func(t *testing.T, s store.Store, clock *Clock) {
		var buried []string
		for i := 0; i < 3; i++ {
			made := create(t, s, store.NewJob{Type: "charge"})
			failUntilBuried(t, s, clock, made.ID)
			buried = append(buried, made.ID)
		}

		// One of another type, which must not move. A bulk action that
		// ignored the filter would pass every other check in this rule.
		other := create(t, s, store.NewJob{Type: "email"})
		failUntilBuried(t, s, clock, other.ID)

		clock.Advance(time.Minute)
		moved, err := s.ReviveMatching(ctx(), store.Filter{
			Status: jobs.Dead, Type: "charge", Limit: 100,
		}, "ops")
		if err != nil {
			t.Fatalf("ReviveMatching: %v", err)
		}
		if moved != 3 {
			t.Fatalf("the revive moved %d jobs, want 3", moved)
		}

		for _, id := range buried {
			got, err := s.Get(ctx(), id)
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			if got.Status != jobs.Pending {
				t.Errorf("a revived job is %q, want pending", got.Status)
			}
			if got.Attempts != 0 {
				t.Errorf("a revived job has %d attempts, want a fresh set", got.Attempts)
			}
			// Every one records who did it, the same way a single revive does.
			if got.ActedBy != "ops" {
				t.Errorf("a revived job says %q acted on it, want ops", got.ActedBy)
			}
		}

		if got, _ := s.Get(ctx(), other.ID); got.Status != jobs.Dead {
			t.Errorf("a job of another type is %q, and the filter did not name it", got.Status)
		}
	}},

	// The limit bounds it, and the oldest go first.
	//
	// A bulk action with no bound is one statement that can hold locks over a
	// year of history. Working from the far end is what lets two runs of the
	// same bounded command make progress rather than looking at the same rows.
	{"a bulk action stops at its limit and takes the oldest first", func(t *testing.T, s store.Store, clock *Clock) {
		var made []string
		for i := 0; i < 5; i++ {
			job := create(t, s, store.NewJob{Type: "work"})
			made = append(made, job.ID)
		}

		moved, err := s.CancelMatching(ctx(), store.Filter{Status: jobs.Pending, Limit: 2}, "ops")
		if err != nil {
			t.Fatalf("CancelMatching: %v", err)
		}
		if moved != 2 {
			t.Fatalf("the cancel moved %d jobs, want the 2 its limit allows", moved)
		}

		for i, id := range made {
			got, err := s.Get(ctx(), id)
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			want := jobs.Pending
			if i < 2 {
				want = jobs.Cancelled
			}
			if got.Status != want {
				t.Errorf("job %d is %q, want %q: the oldest two should go first", i, got.Status, want)
			}
		}
	}},

	// A job the filter names that the action does not apply to is skipped.
	//
	// A bulk action against a moving queue will always race something, and
	// failing the whole batch for it would make the operation useless.
	{"a bulk action skips what it cannot move", func(t *testing.T, s store.Store, clock *Clock) {
		open := create(t, s, store.NewJob{Type: "work"})
		done := create(t, s, store.NewJob{Type: "work"})

		held := lease(t, s, store.LeaseRequest{WorkerID: "w1", Limit: 10})
		for _, one := range held {
			if one.ID != done.ID {
				continue
			}
			if _, err := s.Report(ctx(), store.Report{
				JobID: one.ID, LeaseID: one.LeaseID, Outcome: jobs.OutcomeDone,
			}); err != nil {
				t.Fatalf("Report: %v", err)
			}
		}

		// No status in the filter, so it names both, and one of them has
		// finished.
		moved, err := s.CancelMatching(ctx(), store.Filter{Limit: 100}, "ops")
		if err != nil {
			t.Fatalf("CancelMatching: %v", err)
		}
		if moved != 1 {
			t.Fatalf("the cancel moved %d jobs, want only the one it could", moved)
		}

		if got, _ := s.Get(ctx(), done.ID); got.Status != jobs.Succeeded {
			t.Errorf("the finished job is %q, and a bulk cancel must not touch it", got.Status)
		}
		if got, _ := s.Get(ctx(), open.ID); got.Status != jobs.Cancelled {
			t.Errorf("the open job is %q, want cancelled", got.Status)
		}
	}},

	// A bulk cancel stops what waits for the jobs it cancels.
	//
	// The single path does it, so this one has to as well, or the two write
	// different things and which one ran decides what the queue holds.
	{"a bulk cancel stops what waits for it", func(t *testing.T, s store.Store, clock *Clock) {
		parent := create(t, s, store.NewJob{Type: "extract", Queue: "etl"})
		child, _, err := s.Create(ctx(), store.NewJob{
			Type: "load", Queue: "other", After: []string{parent.ID},
		})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}

		// The filter names only the parent's queue, so the child moves
		// because of the parent and not because it was matched.
		moved, err := s.CancelMatching(ctx(), store.Filter{Queue: "etl", Limit: 100}, "ops")
		if err != nil {
			t.Fatalf("CancelMatching: %v", err)
		}
		if moved != 1 {
			t.Fatalf("the cancel moved %d jobs, want 1", moved)
		}

		got, err := s.Get(ctx(), child.ID)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if got.Status != jobs.Cancelled {
			t.Errorf("the waiting job is %q after its parent was cancelled in a batch", got.Status)
		}
	}},

	// A filter that names nothing moves nothing, and says so rather than
	// failing.
	{"a bulk action over nothing moves nothing", func(t *testing.T, s store.Store, clock *Clock) {
		moved, err := s.ReviveMatching(ctx(), store.Filter{Status: jobs.Dead, Limit: 100}, "ops")
		if err != nil {
			t.Fatalf("ReviveMatching over an empty queue: %v", err)
		}
		if moved != 0 {
			t.Errorf("the revive moved %d jobs from an empty queue", moved)
		}

		// A limit of zero moves nothing rather than everything, which is the
		// mistake that costs the most.
		create(t, s, store.NewJob{Type: "work"})
		if moved, err := s.CancelMatching(ctx(), store.Filter{Limit: 0}, "ops"); err != nil || moved != 0 {
			t.Errorf("a limit of zero moved %d jobs: %v", moved, err)
		}
	}},

	// A job that waits for another does not run until that one succeeds.
	//
	// The whole feature. A job submitted to follow another is not pending,
	// because pending is a claim that the queue will hand it out, and this
	// job must not be handed out yet.
	{"a job that waits for another is not handed out", func(t *testing.T, s store.Store, clock *Clock) {
		first := create(t, s, store.NewJob{Type: "extract"})
		second, _, err := s.Create(ctx(), store.NewJob{Type: "load", After: []string{first.ID}})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}

		if second.Status != jobs.Blocked {
			t.Fatalf("the second job is %q, want blocked", second.Status)
		}
		if len(second.After) != 1 || second.After[0] != first.ID {
			t.Errorf("the job waits for %v, want the first job", second.After)
		}

		// The queue hands out the first and not the second, however long it
		// is asked.
		handed := lease(t, s, store.LeaseRequest{WorkerID: "w1", Limit: 10})
		if len(handed) != 1 || handed[0].ID != first.ID {
			t.Fatalf("the queue handed out %v, want only the first job", ids(handed))
		}

		// Finish the first, and the second is ready.
		clock.Advance(time.Minute)
		if _, err := s.Report(ctx(), store.Report{
			JobID: first.ID, LeaseID: handed[0].LeaseID, Outcome: jobs.OutcomeDone,
		}); err != nil {
			t.Fatalf("Report: %v", err)
		}

		released, err := s.Get(ctx(), second.ID)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if released.Status != jobs.Pending {
			t.Fatalf("the second job is %q after its parent succeeded, want pending", released.Status)
		}

		// Ready now and not at the moment it was submitted. A job held for a
		// minute by its parent is not a minute late.
		requireTime(t, "run at", released.RunAt, Start.Add(time.Minute))

		if after := lease(t, s, store.LeaseRequest{WorkerID: "w1", Limit: 10}); len(after) != 1 || after[0].ID != second.ID {
			t.Errorf("the queue handed out %v, want the second job", ids(after))
		}
	}},

	// A job that waits for a job that is already done is ready at once.
	//
	// A caller submitting a chain after the fact should not have to know
	// whether the parent has finished, and a job that waited for ever on a
	// job that had already succeeded would be the worst kind of stuck.
	{"a job that waits for a finished job is ready at once", func(t *testing.T, s store.Store, clock *Clock) {
		first := create(t, s, store.NewJob{Type: "extract"})
		held := lease(t, s, store.LeaseRequest{WorkerID: "w1"})[0]
		if _, err := s.Report(ctx(), store.Report{
			JobID: first.ID, LeaseID: held.LeaseID, Outcome: jobs.OutcomeDone,
		}); err != nil {
			t.Fatalf("Report: %v", err)
		}

		second, _, err := s.Create(ctx(), store.NewJob{Type: "load", After: []string{first.ID}})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		if second.Status != jobs.Pending {
			t.Errorf("the job is %q, and the job it waits for has already succeeded", second.Status)
		}
	}},

	// Every one of them, and not the first.
	{"a job waits for all of the jobs it names", func(t *testing.T, s store.Store, clock *Clock) {
		first := create(t, s, store.NewJob{Type: "a"})
		second := create(t, s, store.NewJob{Type: "b"})
		third, _, err := s.Create(ctx(), store.NewJob{
			Type: "c", After: []string{first.ID, second.ID},
		})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}

		// Finish the first only.
		for _, held := range lease(t, s, store.LeaseRequest{WorkerID: "w1", Limit: 1}) {
			if _, err := s.Report(ctx(), store.Report{
				JobID: held.ID, LeaseID: held.LeaseID, Outcome: jobs.OutcomeDone,
			}); err != nil {
				t.Fatalf("Report: %v", err)
			}
		}

		half, err := s.Get(ctx(), third.ID)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if half.Status != jobs.Blocked {
			t.Fatalf("the job is %q with one of two parents done, want blocked", half.Status)
		}

		// And the second.
		for _, held := range lease(t, s, store.LeaseRequest{WorkerID: "w1", Limit: 1}) {
			if _, err := s.Report(ctx(), store.Report{
				JobID: held.ID, LeaseID: held.LeaseID, Outcome: jobs.OutcomeDone,
			}); err != nil {
				t.Fatalf("Report: %v", err)
			}
		}

		whole, err := s.Get(ctx(), third.ID)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if whole.Status != jobs.Pending {
			t.Errorf("the job is %q with both parents done, want pending", whole.Status)
		}
	}},

	// A parent that will never succeed stops the job, and says which one.
	//
	// Cancelled and not dead: dead means the job used every attempt it had,
	// and this job used none. A person who fixes the parent revives the
	// child, which is a path they already know.
	{"a job whose parent dies is cancelled and told why", func(t *testing.T, s store.Store, clock *Clock) {
		first := create(t, s, store.NewJob{Type: "extract"})
		second, _, err := s.Create(ctx(), store.NewJob{Type: "load", After: []string{first.ID}})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}

		failUntilBuried(t, s, clock, first.ID)

		stopped, err := s.Get(ctx(), second.ID)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if stopped.Status != jobs.Cancelled {
			t.Fatalf("the job is %q after its parent died, want cancelled", stopped.Status)
		}
		if !strings.Contains(stopped.LastError, first.ID) {
			t.Errorf("the job says %q, and it does not name the job that stopped it", stopped.LastError)
		}

		// Nobody cancelled it, so nobody is recorded as having done so.
		if stopped.ActedBy != "" {
			t.Errorf("the queue recorded %q as the person who cancelled it", stopped.ActedBy)
		}
	}},

	// Cancelling a parent stops what waits for it, for the same reason.
	{"cancelling a job stops what waits for it", func(t *testing.T, s store.Store, clock *Clock) {
		first := create(t, s, store.NewJob{Type: "extract"})
		second, _, err := s.Create(ctx(), store.NewJob{Type: "load", After: []string{first.ID}})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}

		if _, err := s.Cancel(ctx(), first.ID, "ops"); err != nil {
			t.Fatalf("Cancel: %v", err)
		}

		stopped, err := s.Get(ctx(), second.ID)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if stopped.Status != jobs.Cancelled {
			t.Errorf("the job is %q after its parent was cancelled, want cancelled", stopped.Status)
		}
	}},

	// A chain of three, where the first one dies.
	//
	// Only the second waits for the first, so a store that looked one step
	// out would leave the third waiting for ever on a job that had already
	// been cancelled.
	{"a chain is stopped all the way down", func(t *testing.T, s store.Store, clock *Clock) {
		first := create(t, s, store.NewJob{Type: "a"})
		second, _, err := s.Create(ctx(), store.NewJob{Type: "b", After: []string{first.ID}})
		if err != nil {
			t.Fatalf("Create the second: %v", err)
		}
		third, _, err := s.Create(ctx(), store.NewJob{Type: "c", After: []string{second.ID}})
		if err != nil {
			t.Fatalf("Create the third: %v", err)
		}

		if _, err := s.Cancel(ctx(), first.ID, "ops"); err != nil {
			t.Fatalf("Cancel: %v", err)
		}

		for name, id := range map[string]string{"the second": second.ID, "the third": third.ID} {
			got, err := s.Get(ctx(), id)
			if err != nil {
				t.Fatalf("Get %s: %v", name, err)
			}
			if got.Status != jobs.Cancelled {
				t.Errorf("%s job is %q, want cancelled", name, got.Status)
			}
		}
	}},

	// A revived job goes back to waiting when it still waits.
	//
	// Sending it to pending would run it before the job it was submitted to
	// follow, which is the one thing this feature exists to stop.
	{"reviving a waiting job returns it to waiting", func(t *testing.T, s store.Store, clock *Clock) {
		first := create(t, s, store.NewJob{Type: "extract"})
		second, _, err := s.Create(ctx(), store.NewJob{Type: "load", After: []string{first.ID}})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}

		// A person cancels the waiting job, then changes their mind.
		if _, err := s.Cancel(ctx(), second.ID, "ops"); err != nil {
			t.Fatalf("Cancel: %v", err)
		}
		back, err := s.Revive(ctx(), second.ID, "ops")
		if err != nil {
			t.Fatalf("Revive: %v", err)
		}
		if back.Status != jobs.Blocked {
			t.Fatalf("the revived job is %q, and the job it waits for has not run", back.Status)
		}

		// And it is still not handed out.
		for _, handed := range lease(t, s, store.LeaseRequest{WorkerID: "w1", Limit: 10}) {
			if handed.ID == second.ID {
				t.Fatal("the revived job was handed to a worker before the job it waits for")
			}
		}
	}},

	// A job that waits for something that is not there is refused.
	//
	// This is what makes a cycle impossible: a job may only wait for one that
	// already exists, and a job cannot be created before itself. There is no
	// cycle check anywhere, and there is none to forget.
	{"a job cannot wait for a job that does not exist", func(t *testing.T, s store.Store, clock *Clock) {
		_, _, err := s.Create(ctx(), store.NewJob{
			Type: "load", After: []string{"8de1a3d0-0000-0000-0000-000000000000"},
		})
		if !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("Create gave %v, want ErrNotFound", err)
		}

		// Text that is not an identifier at all is the same answer.
		if _, _, err := s.Create(ctx(), store.NewJob{
			Type: "load", After: []string{"not-a-uuid"},
		}); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("Create with a malformed identifier gave %v, want ErrNotFound", err)
		}
	}},

	// A job that waits for nothing is what it always was.
	//
	// Every caller that exists was written before there was anything to wait
	// for, so the empty list has to keep meaning what those callers get.
	{"a job that waits for nothing is pending", func(t *testing.T, s store.Store, clock *Clock) {
		made := create(t, s, store.NewJob{Type: "work"})
		if made.Status != jobs.Pending {
			t.Errorf("a job with no After is %q, want pending", made.Status)
		}
		if len(made.After) != 0 {
			t.Errorf("a job with no After waits for %v", made.After)
		}
	}},

	// A worker that asks for work is remembered, even when there is none.
	//
	// This is the whole point. leased_by names the worker holding a job and
	// is cleared when the job ends, so a fleet with nothing to do left no
	// trace anywhere: an empty queue and a fleet that has stopped looked the
	// same from outside, and the second one is an outage.
	{"a worker that finds no work is still remembered", func(t *testing.T, s store.Store, clock *Clock) {
		// Nothing is in the queue, so this ask comes back empty.
		handed, err := s.Lease(ctx(), store.LeaseRequest{
			Queue: store.DefaultQueue, WorkerID: "idle-1", Limit: 5, TTL: time.Minute,
		})
		if err != nil {
			t.Fatalf("Lease: %v", err)
		}
		if len(handed) != 0 {
			t.Fatalf("an empty queue handed out %d jobs", len(handed))
		}

		seen, err := s.Workers(ctx())
		if err != nil {
			t.Fatalf("Workers: %v", err)
		}
		if len(seen) != 1 {
			t.Fatalf("the store remembers %d workers, want 1: %+v", len(seen), seen)
		}
		if seen[0].ID != "idle-1" || seen[0].Queue != store.DefaultQueue {
			t.Errorf("the worker is %+v", seen[0])
		}
		requireTime(t, "first seen at", seen[0].FirstSeenAt, Start)
		requireTime(t, "last seen at", seen[0].LastSeenAt, Start)
	}},

	// Asking again moves the last seen and leaves the first alone.
	//
	// The pair is what says "this worker has been here since Tuesday and was
	// here a second ago". Moving both would lose the first half, and moving
	// neither would make a worker that stopped an hour ago look present.
	{"asking again moves only the moment a worker was last seen", func(t *testing.T, s store.Store, clock *Clock) {
		ask := func() {
			if _, err := s.Lease(ctx(), store.LeaseRequest{
				Queue: store.DefaultQueue, WorkerID: "poller", Limit: 1, TTL: time.Minute,
			}); err != nil {
				t.Fatalf("Lease: %v", err)
			}
		}

		ask()
		clock.Advance(time.Hour)
		ask()

		seen, err := s.Workers(ctx())
		if err != nil {
			t.Fatalf("Workers: %v", err)
		}
		if len(seen) != 1 {
			t.Fatalf("the store remembers %d rows for one worker", len(seen))
		}
		requireTime(t, "first seen at", seen[0].FirstSeenAt, Start)
		requireTime(t, "last seen at", seen[0].LastSeenAt, Start.Add(time.Hour))

		if idle := seen[0].Idle(Start.Add(90 * time.Minute)); idle != 30*time.Minute {
			t.Errorf("the worker has been idle %s, want 30m", idle)
		}
	}},

	// One row for each worker and queue, and not one for each worker.
	//
	// A worker asks about one queue at a time, so a row for the worker alone
	// would hold whichever queue it asked about last and change on the next
	// ask, which reads like a worker moving between queues.
	{"a worker asking about two queues is two rows", func(t *testing.T, s store.Store, clock *Clock) {
		for _, queue := range []string{store.DefaultQueue, "mail"} {
			if _, err := s.Lease(ctx(), store.LeaseRequest{
				Queue: queue, WorkerID: "both", Limit: 1, TTL: time.Minute,
			}); err != nil {
				t.Fatalf("Lease from %s: %v", queue, err)
			}
		}

		seen, err := s.Workers(ctx())
		if err != nil {
			t.Fatalf("Workers: %v", err)
		}
		if len(seen) != 2 {
			t.Fatalf("the store remembers %d rows, want one for each queue: %+v", len(seen), seen)
		}

		queues := map[string]bool{}
		for _, w := range seen {
			if w.ID != "both" {
				t.Errorf("a row names %q", w.ID)
			}
			queues[w.Queue] = true
		}
		if !queues[store.DefaultQueue] || !queues["mail"] {
			t.Errorf("the rows cover %v, want both queues", queues)
		}
	}},

	// A worker nobody has seen for long enough goes.
	//
	// A worker identifier is usually the name of a container, so a
	// deployment retires every row in this table and writes a new set.
	// Without a sweep the table grows once for each worker on each release,
	// for ever.
	{"a worker nobody has seen is swept away", func(t *testing.T, s store.Store, clock *Clock) {
		if _, err := s.Lease(ctx(), store.LeaseRequest{
			Queue: store.DefaultQueue, WorkerID: "the-old-pod", Limit: 1, TTL: time.Minute,
		}); err != nil {
			t.Fatalf("Lease: %v", err)
		}

		clock.Advance(time.Hour)
		if _, err := s.Lease(ctx(), store.LeaseRequest{
			Queue: store.DefaultQueue, WorkerID: "the-new-pod", Limit: 1, TTL: time.Minute,
		}); err != nil {
			t.Fatalf("Lease: %v", err)
		}

		// A cutoff between the two, so the sweep has to choose rather than
		// removing everything or nothing.
		removed, err := s.DeleteStaleWorkers(ctx(), Start.Add(30*time.Minute), 10)
		if err != nil {
			t.Fatalf("DeleteStaleWorkers: %v", err)
		}
		if removed != 1 {
			t.Fatalf("the sweep removed %d workers, want 1", removed)
		}

		seen, err := s.Workers(ctx())
		if err != nil {
			t.Fatalf("Workers: %v", err)
		}
		if len(seen) != 1 || seen[0].ID != "the-new-pod" {
			t.Errorf("the store kept %+v, want only the new pod", seen)
		}
	}},

	// An ask with no worker named records nothing.
	//
	// A row with an empty identifier is not a worker. It is one row that
	// every unnamed caller shares, and its last seen would move whenever any
	// of them asked.
	{"an ask with no worker named records no worker", func(t *testing.T, s store.Store, clock *Clock) {
		if _, err := s.Lease(ctx(), store.LeaseRequest{
			Queue: store.DefaultQueue, Limit: 1, TTL: time.Minute,
		}); err != nil {
			t.Fatalf("Lease: %v", err)
		}

		seen, err := s.Workers(ctx())
		if err != nil {
			t.Fatalf("Workers: %v", err)
		}
		if len(seen) != 0 {
			t.Errorf("an unnamed ask left %+v behind", seen)
		}
	}},

	// The history of a job is one row per finished run.
	//
	// The jobs table holds one row per job, so a job that failed and then
	// worked carried one error, from whichever attempt wrote last, and no
	// record that the other runs happened. Nobody could answer which worker
	// was failing.
	{"every finished run leaves a row behind", func(t *testing.T, s store.Store, clock *Clock) {
		made := create(t, s, store.NewJob{Type: "work"})

		// Fail once, then succeed. The job carries the error from the first
		// run afterwards, on purpose, and this is what tells the two apart.
		held := lease(t, s, store.LeaseRequest{WorkerID: "w1"})[0]
		clock.Advance(2 * time.Second)
		if _, err := s.Report(ctx(), store.Report{
			JobID: made.ID, LeaseID: held.LeaseID, Outcome: jobs.OutcomeFailed, Error: "upstream said no",
		}); err != nil {
			t.Fatalf("the first Report: %v", err)
		}

		clock.Advance(time.Hour)
		again := lease(t, s, store.LeaseRequest{WorkerID: "w2"})[0]
		clock.Advance(time.Second)
		if _, err := s.Report(ctx(), store.Report{
			JobID: made.ID, LeaseID: again.LeaseID, Outcome: jobs.OutcomeDone,
		}); err != nil {
			t.Fatalf("the second Report: %v", err)
		}

		history, err := s.Attempts(ctx(), made.ID)
		if err != nil {
			t.Fatalf("Attempts: %v", err)
		}
		if len(history) != 2 {
			t.Fatalf("the job kept %d runs, want 2", len(history))
		}

		first, second := history[0], history[1]
		if first.Number != 1 || second.Number != 2 {
			t.Errorf("the runs are numbered %d and %d, want 1 and 2", first.Number, second.Number)
		}
		if first.Worker != "w1" || second.Worker != "w2" {
			t.Errorf("the runs name %q and %q, want w1 and w2", first.Worker, second.Worker)
		}
		if first.Outcome != jobs.OutcomeFailed || second.Outcome != jobs.OutcomeDone {
			t.Errorf("the outcomes are %s and %s, want failed and done", first.Outcome, second.Outcome)
		}
		if first.Error != "upstream said no" {
			t.Errorf("the failed run says %q", first.Error)
		}

		// The run that worked carries no error. The job keeps its last error
		// on purpose, and copying that here would put the old failure on the
		// row of the attempt that worked.
		if second.Error != "" {
			t.Errorf("the run that worked says %q went wrong", second.Error)
		}

		// How long each run took, which is the answer a single row per job
		// could never give.
		took, known := first.Took()
		if !known {
			t.Fatal("the first run has no duration, so its start was not recorded")
		}
		if took != 2*time.Second {
			t.Errorf("the first run took %s, want 2s", took)
		}
	}},

	// A lease that ran out is a finished run too.
	//
	// It is the one nobody reported, so it is the one a single row per job
	// could never describe: the worker is gone, and the row it would have
	// written is the row that says what happened to it.
	{"a lease that runs out leaves a row naming the worker", func(t *testing.T, s store.Store, clock *Clock) {
		made := create(t, s, store.NewJob{Type: "work"})
		lease(t, s, store.LeaseRequest{WorkerID: "the-one-that-died", TTL: time.Second})

		clock.Advance(2 * time.Second)
		if _, err := s.ReclaimExpired(ctx(), 10); err != nil {
			t.Fatalf("ReclaimExpired: %v", err)
		}

		history, err := s.Attempts(ctx(), made.ID)
		if err != nil {
			t.Fatalf("Attempts: %v", err)
		}
		if len(history) != 1 {
			t.Fatalf("the job kept %d runs, want 1", len(history))
		}
		if history[0].Outcome != jobs.OutcomeExpired {
			t.Errorf("the outcome is %s, want expired", history[0].Outcome)
		}
		if history[0].Worker != "the-one-that-died" {
			t.Errorf("the run names %q, want the worker that held it", history[0].Worker)
		}
		if history[0].Error == "" {
			t.Error("the run says nothing about why it ended")
		}
	}},

	// A refusal is recorded as a refusal.
	//
	// It reaches the same code as a failure and takes a different path
	// through the policy, so a store that wrote the outcome from the status
	// would record it as failed and lose the one thing that tells a bad
	// payload apart from a broken upstream.
	{"a refused run is recorded as refused", func(t *testing.T, s store.Store, clock *Clock) {
		made := create(t, s, store.NewJob{Type: "work"})
		held := lease(t, s, store.LeaseRequest{WorkerID: "w1"})[0]

		if _, err := s.Report(ctx(), store.Report{
			JobID: made.ID, LeaseID: held.LeaseID,
			Outcome: jobs.OutcomeRefused, Error: "the payload names no account",
		}); err != nil {
			t.Fatalf("Report: %v", err)
		}

		history, err := s.Attempts(ctx(), made.ID)
		if err != nil {
			t.Fatalf("Attempts: %v", err)
		}
		if len(history) != 1 || history[0].Outcome != jobs.OutcomeRefused {
			t.Fatalf("the history is %+v, want one refused run", history)
		}
	}},

	// A job that has run nothing has no history, and a job that is not there
	// is a different answer.
	{"a job with no runs is not a job that is missing", func(t *testing.T, s store.Store, clock *Clock) {
		made := create(t, s, store.NewJob{Type: "work"})

		history, err := s.Attempts(ctx(), made.ID)
		if err != nil {
			t.Fatalf("Attempts of a job that has not run: %v", err)
		}
		if len(history) != 0 {
			t.Errorf("a job that has not run kept %d runs", len(history))
		}

		if _, err := s.Attempts(ctx(), "8de1a3d0-0000-0000-0000-000000000000"); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("Attempts of an unknown job gave %v, want ErrNotFound", err)
		}
		if _, err := s.Attempts(ctx(), "not-a-uuid"); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("Attempts of a malformed identifier gave %v, want ErrNotFound", err)
		}
	}},

	// Reviving keeps what happened before it.
	//
	// The point of the history is that it survives. It also holds two runs
	// numbered 1, because reviving sets the count back to zero on purpose, so
	// the order they were written in is the only thing that says which came
	// first.
	{"reviving a job keeps the runs that came before", func(t *testing.T, s store.Store, clock *Clock) {
		made := create(t, s, store.NewJob{Type: "work"})
		runs := failUntilBuried(t, s, clock, made.ID)

		if _, err := s.Revive(ctx(), made.ID, "ops"); err != nil {
			t.Fatalf("Revive: %v", err)
		}
		held := lease(t, s, store.LeaseRequest{WorkerID: "after"})[0]
		if _, err := s.Report(ctx(), store.Report{
			JobID: made.ID, LeaseID: held.LeaseID, Outcome: jobs.OutcomeDone,
		}); err != nil {
			t.Fatalf("Report: %v", err)
		}

		history, err := s.Attempts(ctx(), made.ID)
		if err != nil {
			t.Fatalf("Attempts: %v", err)
		}
		if len(history) != runs+1 {
			t.Fatalf("the job kept %d runs, want the %d before the revive and the one after", len(history), runs+1)
		}

		last := history[len(history)-1]
		if last.Outcome != jobs.OutcomeDone || last.Worker != "after" {
			t.Errorf("the last run is %+v, want the one after the revive", last)
		}
		if last.Number != 1 {
			t.Errorf("the run after the revive is numbered %d, want 1", last.Number)
		}
	}},

	// The history goes when the job goes.
	//
	// Otherwise the retention sweep leaves the runs of every removed job
	// behind, and this side of the schema grows for ever while the side it
	// describes does not.
	{"removing a job removes what it kept", func(t *testing.T, s store.Store, clock *Clock) {
		made := create(t, s, store.NewJob{Type: "work"})
		held := lease(t, s, store.LeaseRequest{WorkerID: "w1"})[0]
		if _, err := s.Report(ctx(), store.Report{
			JobID: made.ID, LeaseID: held.LeaseID, Outcome: jobs.OutcomeDone,
		}); err != nil {
			t.Fatalf("Report: %v", err)
		}

		clock.Advance(48 * time.Hour)
		removed, err := s.DeleteFinished(ctx(), jobs.Succeeded, clock.Now(), 10)
		if err != nil {
			t.Fatalf("DeleteFinished: %v", err)
		}
		if removed != 1 {
			t.Fatalf("the sweep removed %d jobs, want 1", removed)
		}

		if _, err := s.Attempts(ctx(), made.ID); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("the runs of a removed job are still there: %v", err)
		}
	}},

	// A leased job says when the run began, not only when the queue gives up.
	//
	// lease_expires_at answers "how much longer will the queue wait". The
	// question somebody looking for a job that is stuck asks is "how long has
	// this been running", and nothing on the job answered it.
	{"a leased job carries the moment the worker took it", func(t *testing.T, s store.Store, clock *Clock) {
		create(t, s, store.NewJob{Type: "work"})

		clock.Advance(time.Minute)
		held := lease(t, s, store.LeaseRequest{WorkerID: "w1", TTL: 30 * time.Second})[0]

		if held.LeasedAt == nil {
			t.Fatal("a leased job carries no leased at")
		}
		requireTime(t, "leased at", *held.LeasedAt, Start.Add(time.Minute))

		// It is stored and not only returned.
		stored, err := s.Get(ctx(), held.ID)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if stored.LeasedAt == nil {
			t.Fatal("the stored job carries no leased at")
		}

		// And it is not the same value as the expiry, which is what a store
		// that wrote the wrong column would give.
		if stored.LeaseExpiresAt != nil && stored.LeasedAt.Equal(*stored.LeaseExpiresAt) {
			t.Error("leased at and the expiry are the same moment")
		}
	}},

	// A job nobody holds holds no start either.
	//
	// A stale value is worse than none: the attempt row copies it as the
	// moment that run began, so a run would be timed from a lease that ended
	// hours before.
	{"every path that ends a lease clears the moment it began", func(t *testing.T, s store.Store, clock *Clock) {
		for name, end := range map[string]func(t *testing.T, s store.Store, id, leaseID string){
			"a worker reporting": func(t *testing.T, s store.Store, id, leaseID string) {
				if _, err := s.Report(ctx(), store.Report{
					JobID: id, LeaseID: leaseID, Outcome: jobs.OutcomeDone,
				}); err != nil {
					t.Fatalf("Report: %v", err)
				}
			},
			"the lease running out": func(t *testing.T, s store.Store, id, leaseID string) {
				if _, err := s.ReclaimExpired(ctx(), 10); err != nil {
					t.Fatalf("ReclaimExpired: %v", err)
				}
			},
			"a cancel": func(t *testing.T, s store.Store, id, leaseID string) {
				if _, err := s.Cancel(ctx(), id, "ops"); err != nil {
					t.Fatalf("Cancel: %v", err)
				}
			},
		} {
			made := create(t, s, store.NewJob{Type: "work"})
			held := lease(t, s, store.LeaseRequest{WorkerID: "w1", TTL: time.Second})[0]
			clock.Advance(2 * time.Second)

			end(t, s, made.ID, held.LeaseID)

			stored, err := s.Get(ctx(), made.ID)
			if err != nil {
				t.Fatalf("%s: Get: %v", name, err)
			}
			if stored.LeasedAt != nil {
				t.Errorf("%s left leased at set to %s", name, stored.LeasedAt)
			}
		}
	}},

	// Who cancelled a job is part of the job, and both stores keep it the
	// same way.
	//
	// The counters say how many jobs were cancelled and the log line says
	// which one. Neither says whose hand it was, and on a queue that two
	// teams share that is the first question somebody asks.
	{"a cancel records the caller that asked for it", func(t *testing.T, s store.Store, clock *Clock) {
		made := create(t, s, store.NewJob{Type: "work"})
		clock.Advance(time.Second)

		got, err := s.Cancel(ctx(), made.ID, "ops")
		if err != nil {
			t.Fatalf("Cancel: %v", err)
		}
		if got.ActedBy != "ops" {
			t.Errorf("acted by = %q, want ops", got.ActedBy)
		}
		if got.ActedAt == nil {
			t.Fatal("acted at is not set, so the name has no moment beside it")
		}
		requireTime(t, "acted at", *got.ActedAt, Start.Add(time.Second))

		// And it is stored, not only returned.
		stored, err := s.Get(ctx(), made.ID)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if stored.ActedBy != "ops" {
			t.Errorf("the stored acted by = %q, want ops", stored.ActedBy)
		}
	}},

	// The two fields hold the last action and not a history.
	//
	// A job that ops cancelled and that billing then revived was last acted
	// on by billing. Keeping the first name would point an investigation at
	// the wrong team.
	{"a revive replaces the name the cancel left", func(t *testing.T, s store.Store, clock *Clock) {
		made := create(t, s, store.NewJob{Type: "work"})
		if _, err := s.Cancel(ctx(), made.ID, "ops"); err != nil {
			t.Fatalf("Cancel: %v", err)
		}

		clock.Advance(time.Minute)
		got, err := s.Revive(ctx(), made.ID, "billing")
		if err != nil {
			t.Fatalf("Revive: %v", err)
		}
		if got.ActedBy != "billing" {
			t.Errorf("acted by = %q, want billing, which acted last", got.ActedBy)
		}
		if got.ActedAt == nil {
			t.Fatal("acted at is not set")
		}
		requireTime(t, "acted at", *got.ActedAt, Start.Add(time.Minute))
	}},

	// A caller that names nobody records nobody.
	//
	// Leaving the previous name there would say that ops cancelled this job
	// when ops did not. No answer is better than a wrong one.
	{"an action with no caller clears the name", func(t *testing.T, s store.Store, clock *Clock) {
		made := create(t, s, store.NewJob{Type: "work"})
		if _, err := s.Cancel(ctx(), made.ID, "ops"); err != nil {
			t.Fatalf("Cancel: %v", err)
		}

		got, err := s.Revive(ctx(), made.ID, "")
		if err != nil {
			t.Fatalf("Revive: %v", err)
		}
		if got.ActedBy != "" {
			t.Errorf("acted by = %q, want nobody", got.ActedBy)
		}
		if got.ActedAt != nil {
			t.Errorf("acted at = %s, want nothing beside a name that is not there", got.ActedAt)
		}
	}},

	// A job nobody has acted on says so.
	{"a new job has no action on it", func(t *testing.T, s store.Store, clock *Clock) {
		made := create(t, s, store.NewJob{Type: "work"})
		if made.ActedBy != "" || made.ActedAt != nil {
			t.Errorf("a new job claims %q acted on it at %s", made.ActedBy, made.ActedAt)
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

		got, err := s.Revive(ctx(), made.ID, "")
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
		if _, err := s.Cancel(ctx(), made.ID, ""); err != nil {
			t.Fatalf("Cancel: %v", err)
		}

		got, err := s.Revive(ctx(), made.ID, "")
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

		if _, err := s.Revive(ctx(), made.ID, ""); !errors.Is(err, store.ErrWrongState) {
			t.Fatalf("Revive of a finished job gave %v, want ErrWrongState", err)
		}
	}},

	{"a job already in the queue cannot be revived", func(t *testing.T, s store.Store, clock *Clock) {
		made := create(t, s, store.NewJob{Type: "work"})

		if _, err := s.Revive(ctx(), made.ID, ""); !errors.Is(err, store.ErrWrongState) {
			t.Fatalf("Revive of a waiting job gave %v, want ErrWrongState", err)
		}

		lease(t, s, store.LeaseRequest{})
		if _, err := s.Revive(ctx(), made.ID, ""); !errors.Is(err, store.ErrWrongState) {
			t.Fatalf("Revive of a leased job gave %v, want ErrWrongState", err)
		}
	}},

	{"reviving an unknown job is reported as missing", func(t *testing.T, s store.Store, clock *Clock) {
		if _, err := s.Revive(ctx(), "8de1a3d0-0000-0000-0000-000000000000", ""); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("Revive of an unknown job gave %v, want ErrNotFound", err)
		}
	}},

	// A submission repeated with the same key stores one job.
	//
	// A client that sends a job and does not see the answer cannot tell
	// whether the server stored it. Retrying is the only thing it can do, and
	// the key is what stops that retry becoming a second job.
	{"a repeated submission with one key stores one job", func(t *testing.T, s store.Store, clock *Clock) {
		first, created, err := s.Create(ctx(), store.NewJob{Type: "charge", IdempotencyKey: "order-4471"})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		if !created {
			t.Fatal("the first submission stored nothing")
		}

		clock.Advance(time.Minute)
		second, created, err := s.Create(ctx(), store.NewJob{Type: "charge", IdempotencyKey: "order-4471"})
		if err != nil {
			t.Fatalf("the repeated Create: %v", err)
		}
		if created {
			t.Error("the repeated submission stored a second job")
		}
		if second.ID != first.ID {
			t.Errorf("the repeat gave job %s, want the first one at %s", second.ID, first.ID)
		}

		// And there really is only one.
		all, err := s.List(ctx(), store.Filter{Limit: 10})
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if len(all) != 1 {
			t.Errorf("the table holds %d jobs, want 1", len(all))
		}
	}},

	// The key is what matches, and nothing else about the job is compared.
	//
	// A client retrying is sending the same request, so the fields agree. A
	// client reusing a key for different work has made a mistake, and giving
	// it the first job back is the safe answer: storing the second under a
	// key that names the first is how one piece of work runs twice.
	{"a key that is taken wins over the rest of the job", func(t *testing.T, s store.Store, clock *Clock) {
		first, _, err := s.Create(ctx(), store.NewJob{Type: "charge", Queue: "money", IdempotencyKey: "k"})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}

		second, created, err := s.Create(ctx(), store.NewJob{Type: "refund", Queue: "other", IdempotencyKey: "k"})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		if created {
			t.Fatal("a different job under a used key was stored")
		}
		if second.Type != "charge" || second.Queue != "money" || second.ID != first.ID {
			t.Errorf("the answer is %+v, want the job that claimed the key", second)
		}
	}},

	{"jobs without a key are never merged", func(t *testing.T, s store.Store, clock *Clock) {
		first := create(t, s, store.NewJob{Type: "work"})
		second := create(t, s, store.NewJob{Type: "work"})

		if first.ID == second.ID {
			t.Fatal("two jobs with no key became one")
		}

		all, _ := s.List(ctx(), store.Filter{Limit: 10})
		if len(all) != 2 {
			t.Errorf("the table holds %d jobs, want 2", len(all))
		}
	}},

	{"the key is kept on the job", func(t *testing.T, s store.Store, clock *Clock) {
		made := create(t, s, store.NewJob{Type: "work", IdempotencyKey: "order-9"})

		got, err := s.Get(ctx(), made.ID)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if got.IdempotencyKey != "order-9" {
			t.Errorf("the key on the job is %q", got.IdempotencyKey)
		}
	}},

	// Two submissions carrying one key arriving together is the case the key
	// exists for, and a check followed by a write lets both through.
	{"one key survives two submissions at once", func(t *testing.T, s store.Store, clock *Clock) {
		const callers = 8

		var wg sync.WaitGroup
		var mu sync.Mutex
		var stored int
		seen := map[string]bool{}

		for i := 0; i < callers; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				job, created, err := s.Create(ctx(), store.NewJob{Type: "charge", IdempotencyKey: "once"})
				if err != nil {
					t.Errorf("Create: %v", err)
					return
				}
				mu.Lock()
				defer mu.Unlock()
				if created {
					stored++
				}
				seen[job.ID] = true
			}()
		}
		wg.Wait()

		if stored != 1 {
			t.Errorf("%d of %d submissions stored a job, want exactly 1", stored, callers)
		}
		if len(seen) != 1 {
			t.Errorf("the callers were given %d different jobs, want 1", len(seen))
		}
	}},

	{"finished jobs older than a time are removed", func(t *testing.T, s store.Store, clock *Clock) {
		old := create(t, s, store.NewJob{Type: "old"})
		held := lease(t, s, store.LeaseRequest{})[0]
		if _, err := s.Report(ctx(), store.Report{JobID: old.ID, LeaseID: held.LeaseID, Outcome: jobs.OutcomeDone}); err != nil {
			t.Fatalf("Report: %v", err)
		}

		clock.Advance(time.Hour)
		cutoff := clock.Now()
		clock.Advance(time.Minute)

		recent := create(t, s, store.NewJob{Type: "recent"})
		heldToo := lease(t, s, store.LeaseRequest{})[0]
		if _, err := s.Report(ctx(), store.Report{JobID: recent.ID, LeaseID: heldToo.LeaseID, Outcome: jobs.OutcomeDone}); err != nil {
			t.Fatalf("Report: %v", err)
		}

		removed, err := s.DeleteFinished(ctx(), jobs.Succeeded, cutoff, 100)
		if err != nil {
			t.Fatalf("DeleteFinished: %v", err)
		}
		if removed != 1 {
			t.Fatalf("removed %d jobs, want 1", removed)
		}

		if _, err := s.Get(ctx(), old.ID); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("the old job is still there")
		}
		if _, err := s.Get(ctx(), recent.ID); err != nil {
			t.Errorf("the job on the near side of the cutoff was removed: %v", err)
		}
	}},

	// A sweeper is exactly the place where a wrong status is not noticed for
	// a month, so it refuses to touch a job that has not finished.
	{"a job that has not finished is never removed", func(t *testing.T, s store.Store, clock *Clock) {
		waiting := create(t, s, store.NewJob{Type: "waiting"})
		clock.Advance(time.Hour)

		for _, status := range []jobs.Status{jobs.Pending, jobs.Leased} {
			if _, err := s.DeleteFinished(ctx(), status, clock.Now(), 100); err == nil {
				t.Errorf("removing %q jobs was accepted", status)
			}
		}

		if _, err := s.Get(ctx(), waiting.ID); err != nil {
			t.Errorf("the waiting job is gone: %v", err)
		}
	}},

	{"only the named status is removed", func(t *testing.T, s store.Store, clock *Clock) {
		buried := create(t, s, store.NewJob{Type: "buried"})
		failUntilBuried(t, s, clock, buried.ID)

		stopped := create(t, s, store.NewJob{Type: "stopped"})
		if _, err := s.Cancel(ctx(), stopped.ID, ""); err != nil {
			t.Fatalf("Cancel: %v", err)
		}

		clock.Advance(time.Hour)

		removed, err := s.DeleteFinished(ctx(), jobs.Cancelled, clock.Now(), 100)
		if err != nil {
			t.Fatalf("DeleteFinished: %v", err)
		}
		if removed != 1 {
			t.Errorf("removed %d jobs, want the one cancelled job", removed)
		}
		if _, err := s.Get(ctx(), buried.ID); err != nil {
			t.Errorf("a dead job was removed by a sweep for cancelled ones: %v", err)
		}
	}},

	{"a sweep removes no more than it is asked for", func(t *testing.T, s store.Store, clock *Clock) {
		for i := 0; i < 5; i++ {
			made := create(t, s, store.NewJob{Type: fmt.Sprintf("job-%d", i)})
			if _, err := s.Cancel(ctx(), made.ID, ""); err != nil {
				t.Fatalf("Cancel: %v", err)
			}
		}
		clock.Advance(time.Hour)

		removed, err := s.DeleteFinished(ctx(), jobs.Cancelled, clock.Now(), 2)
		if err != nil {
			t.Fatalf("DeleteFinished: %v", err)
		}
		if removed != 2 {
			t.Errorf("removed %d jobs, want the 2 asked for", removed)
		}

		left, _ := s.List(ctx(), store.Filter{Limit: 10})
		if len(left) != 3 {
			t.Errorf("%d jobs are left, want 3", len(left))
		}
	}},

	// The key goes with the job. Leaving it behind would refuse a submission
	// for ever on behalf of a job nobody can look at any more.
	{"removing a job frees its idempotency key", func(t *testing.T, s store.Store, clock *Clock) {
		made := create(t, s, store.NewJob{Type: "charge", IdempotencyKey: "order-1"})
		if _, err := s.Cancel(ctx(), made.ID, ""); err != nil {
			t.Fatalf("Cancel: %v", err)
		}

		clock.Advance(time.Hour)
		if _, err := s.DeleteFinished(ctx(), jobs.Cancelled, clock.Now(), 10); err != nil {
			t.Fatalf("DeleteFinished: %v", err)
		}

		again, created, err := s.Create(ctx(), store.NewJob{Type: "charge", IdempotencyKey: "order-1"})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		if !created {
			t.Error("the key of a removed job still refuses a submission")
		}
		if again.ID == made.ID {
			t.Error("the removed job came back")
		}
	}},

	{"a job keeps what it produced", func(t *testing.T, s store.Store, clock *Clock) {
		made := create(t, s, store.NewJob{Type: "count"})
		held := lease(t, s, store.LeaseRequest{})[0]

		got, err := s.Report(ctx(), store.Report{
			JobID:   made.ID,
			LeaseID: held.LeaseID,
			Outcome: jobs.OutcomeDone,
			Result:  []byte(`{"rows":41,"skipped":1}`),
		})
		if err != nil {
			t.Fatalf("Report: %v", err)
		}
		samePayload(t, got.Result, []byte(`{"rows":41,"skipped":1}`))

		stored, err := s.Get(ctx(), made.ID)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		samePayload(t, stored.Result, []byte(`{"rows":41,"skipped":1}`))
	}},

	// The output of an attempt that failed is not an output. Keeping it would
	// leave the value from a failed run sitting on a job that later succeeded
	// with a different one, and nothing on the row would say which it was.
	{"a failed attempt keeps no result", func(t *testing.T, s store.Store, clock *Clock) {
		made := create(t, s, store.NewJob{Type: "count"})
		held := lease(t, s, store.LeaseRequest{})[0]

		got, err := s.Report(ctx(), store.Report{
			JobID:   made.ID,
			LeaseID: held.LeaseID,
			Outcome: jobs.OutcomeFailed,
			Error:   "half of it worked",
			Result:  []byte(`{"rows":20}`),
		})
		if err != nil {
			t.Fatalf("Report: %v", err)
		}
		if len(got.Result) != 0 {
			t.Errorf("a failed attempt kept the result %s", got.Result)
		}

		// And the retry that succeeds writes the real one.
		clock.Advance(time.Hour)
		again := lease(t, s, store.LeaseRequest{})[0]
		done, err := s.Report(ctx(), store.Report{
			JobID:   made.ID,
			LeaseID: again.LeaseID,
			Outcome: jobs.OutcomeDone,
			Result:  []byte(`{"rows":41}`),
		})
		if err != nil {
			t.Fatalf("Report: %v", err)
		}
		samePayload(t, done.Result, []byte(`{"rows":41}`))
	}},

	{"a job that produced nothing carries no result", func(t *testing.T, s store.Store, clock *Clock) {
		made := create(t, s, store.NewJob{Type: "work"})
		held := lease(t, s, store.LeaseRequest{})[0]

		got, err := s.Report(ctx(), store.Report{JobID: made.ID, LeaseID: held.LeaseID, Outcome: jobs.OutcomeDone})
		if err != nil {
			t.Fatalf("Report: %v", err)
		}
		if len(got.Result) != 0 {
			t.Errorf("a job with no result carries %s", got.Result)
		}
	}},

	{"a result that is not JSON is refused", func(t *testing.T, s store.Store, clock *Clock) {
		made := create(t, s, store.NewJob{Type: "work"})
		held := lease(t, s, store.LeaseRequest{})[0]

		_, err := s.Report(ctx(), store.Report{
			JobID:   made.ID,
			LeaseID: held.LeaseID,
			Outcome: jobs.OutcomeDone,
			Result:  []byte(`{"rows":`),
		})
		if err == nil {
			t.Fatal("a result that is not JSON was stored")
		}

		// And the job is untouched rather than half reported.
		stored, _ := s.Get(ctx(), made.ID)
		if stored.Status != jobs.Leased {
			t.Errorf("status = %q, want the job left alone", stored.Status)
		}
	}},

	{"a refused job is buried on its first attempt", func(t *testing.T, s store.Store, clock *Clock) {
		retries := 5
		made := create(t, s, store.NewJob{Type: "work", MaxRetries: &retries})
		held := lease(t, s, store.LeaseRequest{})[0]

		got, err := s.Report(ctx(), store.Report{
			JobID:   made.ID,
			LeaseID: held.LeaseID,
			Outcome: jobs.OutcomeRefused,
			Error:   "the payload names no account",
		})
		if err != nil {
			t.Fatalf("Report: %v", err)
		}

		// Five retries were asked for, so a plain failure here would send the
		// job back to the queue. That is what makes this rule mean something.
		if got.Status != jobs.Dead {
			t.Errorf("status = %q, want the job buried with five retries unused", got.Status)
		}
		if got.Attempts != 1 {
			t.Errorf("attempts = %d, want 1", got.Attempts)
		}
		if got.LastError != "the payload names no account" {
			t.Errorf("last error = %q", got.LastError)
		}

		// No wait. A job nothing will run again has no next run, and a run_at
		// in the future on a dead job reads as a job that is waiting.
		requireTime(t, "run at", got.RunAt, clock.Now())

		// And the lease is let go, like any other ending.
		if got.LeaseID != "" || got.LeasedBy != "" || got.LeaseExpiresAt != nil {
			t.Errorf("a buried job still carries a lease: %+v", got)
		}
	}},

	{"a refused job is never offered again", func(t *testing.T, s store.Store, clock *Clock) {
		retries := 5
		made := create(t, s, store.NewJob{Type: "work", MaxRetries: &retries})
		held := lease(t, s, store.LeaseRequest{})[0]

		if _, err := s.Report(ctx(), store.Report{
			JobID: made.ID, LeaseID: held.LeaseID, Outcome: jobs.OutcomeRefused, Error: "no",
		}); err != nil {
			t.Fatalf("Report: %v", err)
		}

		// A status is a claim about what happens next. This asks the queue
		// itself rather than reading the column, because a job that says dead
		// and is still handed out is the failure this rule exists to catch.
		clock.Advance(24 * time.Hour)
		again, err := s.Lease(ctx(), store.LeaseRequest{
			Queue: store.DefaultQueue, WorkerID: "worker-2", Limit: 10, TTL: time.Minute,
		})
		if err != nil {
			t.Fatalf("Lease: %v", err)
		}
		for _, job := range again {
			if job.ID == made.ID {
				t.Fatal("a refused job was handed out again a day later")
			}
		}
	}},

	{"a refused job can be revived like any other dead job", func(t *testing.T, s store.Store, clock *Clock) {
		made := create(t, s, store.NewJob{Type: "work"})
		held := lease(t, s, store.LeaseRequest{})[0]

		if _, err := s.Report(ctx(), store.Report{
			JobID: made.ID, LeaseID: held.LeaseID, Outcome: jobs.OutcomeRefused, Error: "no",
		}); err != nil {
			t.Fatalf("Report: %v", err)
		}

		// The refusal says the job cannot be finished as it stands. Somebody
		// who has fixed what was wrong is the one who decides that has
		// changed, and revive is how they say so. A refusal that could not be
		// revived would be a second kind of dead, and there is one kind.
		back, err := s.Revive(ctx(), made.ID, "")
		if err != nil {
			t.Fatalf("Revive: %v", err)
		}
		if back.Status != jobs.Pending {
			t.Errorf("status = %q, want the job back in the queue", back.Status)
		}
		if back.Attempts != 0 {
			t.Errorf("attempts = %d, want a full set again", back.Attempts)
		}
	}},

	// Both stores refuse a job the column cannot hold, and they refuse it the
	// same way.
	//
	// This rule exists because they did not. Priority and max retries are a Go
	// int, and both columns are INTEGER, so a number between the two sizes was
	// accepted by the memory store and refused by PostgreSQL. The API answered
	// 201 against one store and 500 against the other, for the same
	// submission, while all sixty five rules here passed.
	//
	// A store that agrees only with itself is the thing this suite exists to
	// prevent, so the rule is written against the interface and not against
	// either implementation.
	{"a number the column cannot hold is refused by every store", func(t *testing.T, s store.Store, clock *Clock) {
		tooBig := math.MaxInt32 + 1
		tooSmall := math.MinInt32 - 1

		refused := map[string]store.NewJob{
			"a priority past the column":    {Type: "work", Priority: tooBig},
			"a priority under the column":   {Type: "work", Priority: tooSmall},
			"a max retries past the column": {Type: "work", MaxRetries: &tooBig},
		}
		for name, n := range refused {
			_, _, err := s.Create(ctx(), n)
			if err == nil {
				t.Errorf("%s was stored", name)
				continue
			}
			// The sentinel says whose mistake it is, which is how the layer
			// above tells a client mistake from a server fault. This rule
			// used to test the message for a "store: " prefix, which every
			// sentinel in that package carries as well.
			if !errors.Is(err, store.ErrInvalid) {
				t.Errorf("%s gave %q, which does not answer to ErrInvalid", name, err)
			}
		}

		// And the edges are stored, so the bound is not one out.
		for name, n := range map[string]store.NewJob{
			"the highest priority the column holds": {Type: "work", Priority: math.MaxInt32},
			"the lowest priority the column holds":  {Type: "work", Priority: math.MinInt32},
		} {
			got := create(t, s, n)
			if got.Priority != n.Priority {
				t.Errorf("%s came back as %d", name, got.Priority)
			}
		}
	}},

	// Every refusal a store makes about the content of a job answers to one
	// sentinel.
	//
	// The rule above covers three numbers. This one covers the rest, because
	// a refusal that does not carry the sentinel is answered 500 by the layer
	// above, and 500 tells the caller to try again with something the store
	// will never accept.
	//
	// The empty type is the one that matters most: it is the first check in
	// the validator, so it is the one a reader is most likely to reword.
	{"every refusal about the content of a job carries one sentinel", func(t *testing.T, s store.Store, clock *Clock) {
		below := -1
		tooMany := math.MaxInt32
		refused := map[string]store.NewJob{
			"a job with no type":              {},
			"a type longer than the column":   {Type: strings.Repeat("t", 256)},
			"a queue longer than the column":  {Type: "work", Queue: strings.Repeat("q", 256)},
			"a delay that is in the past":     {Type: "work", Delay: -time.Second},
			"a payload that is not JSON":      {Type: "work", Payload: []byte("{oh no")},
			"a key longer than the column":    {Type: "work", IdempotencyKey: strings.Repeat("k", 256)},
			"retries below zero":              {Type: "work", MaxRetries: &below},
			"waiting for a job with no name":  {Type: "work", After: []string{""}},
			"waiting for more jobs than most": {Type: "work", After: make([]string, store.MostAfter+1)},
			"retries past the column":         {Type: "work", MaxRetries: &tooMany, Priority: math.MaxInt32 + 1},
		}
		for name, n := range refused {
			_, _, err := s.Create(ctx(), n)
			if err == nil {
				t.Errorf("%s was stored", name)
				continue
			}
			if !errors.Is(err, store.ErrInvalid) {
				t.Errorf("%s gave %q, which does not answer to ErrInvalid", name, err)
			}
			// And it is not one of the sentinels that means something else.
			// Those all carried the "store: " prefix this rule replaces, so a
			// check written against the prefix could not tell them apart.
			if errors.Is(err, store.ErrNotFound) || errors.Is(err, store.ErrWrongState) {
				t.Errorf("%s answers to a sentinel that means something else", name)
			}
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

	{"the list gives the newest first", func(t *testing.T, s store.Store, clock *Clock) {
		var made []string
		for i := 0; i < 4; i++ {
			made = append(made, create(t, s, store.NewJob{Type: fmt.Sprintf("job-%d", i)}).ID)
			clock.Advance(time.Second)
		}

		got, err := s.List(ctx(), store.Filter{Limit: 3})
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if len(got) != 3 {
			t.Fatalf("a limit of 3 gave %d jobs", len(got))
		}

		want := []string{made[3], made[2], made[1]}
		if fmt.Sprint(ids(got)) != fmt.Sprint(want) {
			t.Errorf("order = %v, want the newest first", ids(got))
		}
	}},

	{"the list narrows by queue, status and type", func(t *testing.T, s store.Store, clock *Clock) {
		wanted := create(t, s, store.NewJob{Type: "email", Queue: "mail"})
		create(t, s, store.NewJob{Type: "email", Queue: "other"})
		create(t, s, store.NewJob{Type: "report", Queue: "mail"})

		byQueue, err := s.List(ctx(), store.Filter{Queue: "mail", Limit: 10})
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if len(byQueue) != 2 {
			t.Errorf("the mail queue gave %d jobs, want 2", len(byQueue))
		}

		byType, err := s.List(ctx(), store.Filter{Type: "email", Limit: 10})
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if len(byType) != 2 {
			t.Errorf("the email type gave %d jobs, want 2", len(byType))
		}

		// Together, and not one or the other.
		both, err := s.List(ctx(), store.Filter{Queue: "mail", Type: "email", Limit: 10})
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if len(both) != 1 || both[0].ID != wanted.ID {
			t.Errorf("the two filters together gave %v, want just the one job in both", ids(both))
		}

		byStatus, err := s.List(ctx(), store.Filter{Status: jobs.Pending, Limit: 10})
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if len(byStatus) != 3 {
			t.Errorf("pending gave %d jobs, want 3", len(byStatus))
		}
		if dead, _ := s.List(ctx(), store.Filter{Status: jobs.Dead, Limit: 10}); len(dead) != 0 {
			t.Errorf("dead gave %d jobs, want none", len(dead))
		}
	}},

	// Paging has to show every job exactly once.
	//
	// An offset cannot promise that. It re-reads and skips the rows before
	// the page, so a job submitted while somebody is reading shifts every
	// later page by one, which shows a row twice and hides another entirely.
	// The cursor names the last job seen, so a job arriving after it changes
	// nothing about the pages behind it.
	{"paging shows every job once", func(t *testing.T, s store.Store, clock *Clock) {
		const total = 10
		var made []string
		for i := 0; i < total; i++ {
			made = append(made, create(t, s, store.NewJob{Type: fmt.Sprintf("job-%d", i)}).ID)
			clock.Advance(time.Second)
		}

		seen := map[string]int{}
		var order []string
		cursor := ""

		for page := 0; page < total; page++ {
			got, err := s.List(ctx(), store.Filter{Limit: 3, Before: cursor})
			if err != nil {
				t.Fatalf("page %d: %v", page, err)
			}
			if len(got) == 0 {
				break
			}
			for _, job := range got {
				seen[job.ID]++
				order = append(order, job.ID)
			}
			cursor = got[len(got)-1].ID

			// A job submitted while the reader is paging must not disturb
			// the pages already behind them.
			create(t, s, store.NewJob{Type: "arrived-later"})
			clock.Advance(time.Second)
		}

		for _, id := range made {
			if seen[id] != 1 {
				t.Errorf("%s was seen %d times, want once", id, seen[id])
			}
		}

		// And the order across the pages is still newest first.
		for i := 0; i < total; i++ {
			if order[i] != made[total-1-i] {
				t.Errorf("position %d is %s, want %s", i, order[i], made[total-1-i])
				break
			}
		}
	}},

	{"the list gives the job that runs soonest first when it is asked to", func(t *testing.T, s store.Store, clock *Clock) {
		// Submitted in one order and due in another, so a list that came back
		// in submission order would pass a test that only counted rows.
		late := create(t, s, store.NewJob{Type: "late", Delay: time.Hour})
		soon := create(t, s, store.NewJob{Type: "soon", Delay: time.Minute})
		middle := create(t, s, store.NewJob{Type: "middle", Delay: 30 * time.Minute})

		got, err := s.List(ctx(), store.Filter{Limit: 10, Order: store.Soonest})
		if err != nil {
			t.Fatalf("List: %v", err)
		}

		want := []string{soon.ID, middle.ID, late.ID}
		if diff := ids(got); !slices.Equal(diff, want) {
			t.Errorf("order = %v, want soonest first %v", diff, want)
		}

		// And the default is still the newest first, which is the reverse of
		// the order they were submitted in and of the order above.
		back, err := s.List(ctx(), store.Filter{Limit: 10})
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if diff := ids(back); !slices.Equal(diff, []string{middle.ID, soon.ID, late.ID}) {
			t.Errorf("the default order changed: %v", diff)
		}
	}},

	{"the list leaves out a job that runs after the moment given", func(t *testing.T, s store.Store, clock *Clock) {
		ready := create(t, s, store.NewJob{Type: "ready"})
		waiting := create(t, s, store.NewJob{Type: "waiting", Delay: time.Hour})

		now := clock.Now()
		got, err := s.List(ctx(), store.Filter{Limit: 10, DueBy: now})
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if diff := ids(got); !slices.Equal(diff, []string{ready.ID}) {
			t.Errorf("due by now gave %v, want only the job that is ready", diff)
		}

		// At the moment itself and not one second before it. An inequality
		// written the wrong way round passes every test that jumps an hour.
		got, err = s.List(ctx(), store.Filter{Limit: 10, DueBy: now.Add(time.Hour - time.Second)})
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if len(got) != 1 {
			t.Errorf("one second early gave %v, want only the ready job", ids(got))
		}

		got, err = s.List(ctx(), store.Filter{Limit: 10, DueBy: now.Add(time.Hour), Order: store.Soonest})
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if diff := ids(got); !slices.Equal(diff, []string{ready.ID, waiting.ID}) {
			t.Errorf("at the moment itself gave %v, want both", diff)
		}

		// The store reads no clock of its own. Moving time forward without
		// saying so changes nothing, which is what lets a caller ask what the
		// queue looked like at a stated moment.
		clock.Advance(2 * time.Hour)
		got, err = s.List(ctx(), store.Filter{Limit: 10, DueBy: now})
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if len(got) != 1 {
			t.Errorf("the answer moved with the clock: %v", ids(got))
		}
	}},

	{"the list narrows to the jobs one worker holds", func(t *testing.T, s store.Store, clock *Clock) {
		create(t, s, store.NewJob{Type: "a"})
		create(t, s, store.NewJob{Type: "b"})
		create(t, s, store.NewJob{Type: "c"})

		mine := lease(t, s, store.LeaseRequest{WorkerID: "worker-7", Limit: 2})
		if len(mine) != 2 {
			t.Fatalf("leased %d jobs, want 2", len(mine))
		}
		lease(t, s, store.LeaseRequest{WorkerID: "worker-8", Limit: 1})

		got, err := s.List(ctx(), store.Filter{Limit: 10, Worker: "worker-7"})
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if len(got) != 2 {
			t.Fatalf("worker-7 holds %v, want the two it leased", ids(got))
		}
		for _, job := range got {
			if job.LeasedBy != "worker-7" {
				t.Errorf("%s is held by %q", job.ID, job.LeasedBy)
			}
		}

		// A finished job belongs to nobody, whoever ran it. The lease is let
		// go when the job ends, so asking by worker answers about work in
		// flight and never about work that is over.
		if _, err := s.Report(ctx(), store.Report{
			JobID: mine[0].ID, LeaseID: mine[0].LeaseID, Outcome: jobs.OutcomeDone,
		}); err != nil {
			t.Fatalf("Report: %v", err)
		}

		got, err = s.List(ctx(), store.Filter{Limit: 10, Worker: "worker-7"})
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if diff := ids(got); !slices.Equal(diff, []string{mine[1].ID}) {
			t.Errorf("after one finished, worker-7 holds %v, want only the other", diff)
		}
	}},

	// This is the rule the compound cursor exists for, and it is the one that
	// passes for the wrong reason if it is written carelessly.
	//
	// Jobs created one after another each get their own run_at, and a cursor
	// comparing run_at alone pages through those correctly. It only breaks
	// when several jobs share a moment, which is what a burst of submissions
	// and what every reclaim sweep produces. So the clock does not move here.
	{"paging in the soonest order shows every job once when they share a moment", func(t *testing.T, s store.Store, clock *Clock) {
		const total = 9
		made := map[string]bool{}
		for i := 0; i < total; i++ {
			// No clock.Advance. Every one of these has the same run_at, so
			// run_at alone cannot place a cursor among them.
			made[create(t, s, store.NewJob{Type: fmt.Sprintf("job-%d", i)}).ID] = true
		}

		seen := map[string]int{}
		cursor := ""
		for page := 0; page < total+2; page++ {
			got, err := s.List(ctx(), store.Filter{Limit: 3, Before: cursor, Order: store.Soonest})
			if err != nil {
				t.Fatalf("page %d: %v", page, err)
			}
			if len(got) == 0 {
				break
			}
			for _, job := range got {
				seen[job.ID]++
			}
			cursor = got[len(got)-1].ID
		}

		for id := range made {
			if seen[id] != 1 {
				t.Errorf("a job sharing its moment with eight others was seen %d times, want once", seen[id])
			}
		}
		if len(seen) != total {
			t.Errorf("%d jobs came back across the pages, want %d", len(seen), total)
		}
	}},

	// A cursor naming a job that is gone leaves the page start undefined.
	// Reading it as the start of the list would send the reader back to page
	// one without saying so.
	{"a cursor naming no job is refused", func(t *testing.T, s store.Store, clock *Clock) {
		create(t, s, store.NewJob{Type: "work"})

		_, err := s.List(ctx(), store.Filter{Limit: 5, Before: "8de1a3d0-0000-0000-0000-000000000000"})
		if !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("an unknown cursor gave %v, want ErrNotFound", err)
		}
	}},

	{"a status the code does not know is refused", func(t *testing.T, s store.Store, clock *Clock) {
		_, err := s.List(ctx(), store.Filter{Limit: 5, Status: jobs.Status("processing")})
		if err == nil {
			t.Fatal("a filter on a status that does not exist was accepted")
		}
		if errors.Is(err, store.ErrNotFound) {
			t.Errorf("a bad status was reported as a missing job: %v", err)
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
