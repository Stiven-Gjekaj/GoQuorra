package storetest

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/Stiven-Gjekaj/GoQuorra/internal/jobs"
	"github.com/Stiven-Gjekaj/GoQuorra/internal/store"
)

// Start is the time every test in the suite begins at. A fixed date, so a
// failure message names the same numbers on every machine and in every year.
var Start = time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

// Policy is the retry rule the suite runs under. The waits are stated here
// rather than taken from the store default, so that changing the default does
// not silently change what these tests assert.
var Policy = jobs.Policy{MaxRetries: 2, Base: 10 * time.Second, Max: time.Hour}

// Factory makes an empty store for one test.
//
// The suite calls this once per test and expects a store holding no jobs. An
// implementation backed by a database empties its table here.
type Factory func(t *testing.T, opts store.Options) store.Store

// Run drives every rule the store interface promises.
func Run(t *testing.T, newStore Factory) {
	t.Helper()

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			clock := NewClock(Start)
			opts := store.Options{
				Policy: Policy,
				Now:    clock.Now,
				// A fixed jitter, so the suite states the wait it expects
				// instead of accepting a range. Zero gives half the plain
				// wait, which jobs.Backoff documents.
				Jitter: func() float64 { return 0 },
			}
			c.run(t, newStore(t, opts), clock)
		})
	}
}

type testCase struct {
	name string
	run  func(t *testing.T, s store.Store, clock *Clock)
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func ctx() context.Context { return context.Background() }

// create stores a job and fails the test if it cannot.
func create(t *testing.T, s store.Store, n store.NewJob) *store.Job {
	t.Helper()
	job, err := s.Create(ctx(), n)
	if err != nil {
		t.Fatalf("Create(%+v): %v", n, err)
	}
	return job
}

// lease asks for work and fails the test if it cannot.
func lease(t *testing.T, s store.Store, req store.LeaseRequest) []*store.Job {
	t.Helper()
	if req.Limit == 0 {
		req.Limit = 10
	}
	if req.TTL == 0 {
		req.TTL = 30 * time.Second
	}
	if req.WorkerID == "" {
		req.WorkerID = "worker-1"
	}
	if req.Queue == "" {
		req.Queue = store.DefaultQueue
	}
	got, err := s.Lease(ctx(), req)
	if err != nil {
		t.Fatalf("Lease(%+v): %v", req, err)
	}
	return got
}

// sameTime compares two times with the tolerance a database forces.
//
// PostgreSQL keeps a timestamp to the microsecond and Go keeps one to the
// nanosecond, so a round trip through the database loses the last three
// digits. Comparing with Equal makes the PostgreSQL suite fail on arithmetic
// that is correct, which teaches the next person to delete the assertion.
func sameTime(a, b time.Time) bool {
	d := a.Sub(b)
	if d < 0 {
		d = -d
	}
	return d < time.Millisecond
}

func requireTime(t *testing.T, name string, got, want time.Time) {
	t.Helper()
	if !sameTime(got, want) {
		t.Errorf("%s = %s, want %s", name, got.UTC(), want.UTC())
	}
}

// samePayload compares two payloads by what they mean.
//
// JSONB does not keep the bytes it was given. It reorders the keys, drops the
// spaces, and normalises the numbers. A test that compares the bytes passes
// against the memory store and fails against PostgreSQL, for a reason that
// has nothing to do with either being wrong.
func samePayload(t *testing.T, got, want json.RawMessage) {
	t.Helper()
	var a, b any
	if err := json.Unmarshal(got, &a); err != nil {
		t.Fatalf("the stored payload is not JSON: %s", got)
	}
	if err := json.Unmarshal(want, &b); err != nil {
		t.Fatalf("the wanted payload is not JSON: %s", want)
	}
	if !reflect.DeepEqual(a, b) {
		t.Errorf("payload = %s, want %s", got, want)
	}
}

func ids(list []*store.Job) []string {
	out := make([]string, len(list))
	for i, j := range list {
		out[i] = j.ID
	}
	return out
}

// failUntilBuried leases a job and fails it until the store buries it, and
// reports how many times it ran.
//
// Two cases need this, and writing the loop twice is how they end up counting
// different things.
func failUntilBuried(t *testing.T, s store.Store, clock *Clock, id string) int {
	t.Helper()

	runs := 0
	for runs < 20 {
		handed, err := s.Lease(ctx(), store.LeaseRequest{
			Queue: store.DefaultQueue, WorkerID: "worker-1", Limit: 1, TTL: time.Minute,
		})
		if err != nil {
			t.Fatalf("Lease: %v", err)
		}
		if len(handed) == 0 {
			t.Fatalf("the job stopped being offered after %d runs, and it is not buried", runs)
		}
		runs++

		got, err := s.Report(ctx(), store.Report{
			JobID: id, LeaseID: handed[0].LeaseID, Outcome: jobs.OutcomeFailed, Error: "no",
		})
		if err != nil {
			t.Fatalf("Report: %v", err)
		}
		if got.Status == jobs.Dead {
			return runs
		}
		clock.Advance(time.Hour)
	}

	t.Fatalf("the job never reached the dead letter queue")
	return 0
}
