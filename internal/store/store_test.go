package store

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Stiven-Gjekaj/GoQuorra/internal/jobs"
)

func intPtr(n int) *int { return &n }

func TestValidateRefusesAJobThatCannotBeStored(t *testing.T) {
	valid := NewJob{Type: "email", Payload: json.RawMessage(`{"to":"a@b.c"}`)}
	if err := valid.Validate(); err != nil {
		t.Fatalf("a good job was refused: %v", err)
	}

	// An absent payload is allowed, because prepare turns it into {}.
	if err := (NewJob{Type: "email"}).Validate(); err != nil {
		t.Errorf("a job with no payload was refused: %v", err)
	}

	bad := map[string]NewJob{
		"no type":             {Payload: json.RawMessage(`{}`)},
		"type too long":       {Type: strings.Repeat("t", 256)},
		"queue too long":      {Type: "email", Queue: strings.Repeat("q", 256)},
		"negative delay":      {Type: "email", Delay: -time.Second},
		"negative retries":    {Type: "email", MaxRetries: intPtr(-1)},
		"payload is not json": {Type: "email", Payload: json.RawMessage(`{"to":`)},
	}
	for name, n := range bad {
		if err := n.Validate(); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}

// The defaults are applied once, in one place.
//
// They used to be applied in the HTTP handler and again in the store, and the
// two disagreed. They were also written back into the caller's own struct,
// which changed a value that the caller still held and could still read.
func TestPrepareFillsTheDefaultsAndLeavesTheRequestAlone(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	opts := Options{Policy: jobs.Policy{MaxRetries: 7, Base: time.Second, Max: time.Minute}}.WithDefaults()

	n := NewJob{Type: "email"}
	got := opts.Prepare(n, "id-1", now)

	if got.Queue != DefaultQueue {
		t.Errorf("queue = %q, want %q", got.Queue, DefaultQueue)
	}
	if got.MaxRetries != 7 {
		t.Errorf("max retries = %d, want 7 from the store policy", got.MaxRetries)
	}
	if string(got.Payload) != "{}" {
		t.Errorf("payload = %s, want an empty object", got.Payload)
	}
	if got.Status != jobs.Pending {
		t.Errorf("status = %q", got.Status)
	}
	if !got.RunAt.Equal(now) {
		t.Errorf("run at = %s, want %s", got.RunAt, now)
	}

	// The request the caller still holds must be untouched.
	if n.Queue != "" || n.MaxRetries != nil || len(n.Payload) != 0 {
		t.Errorf("prepare wrote back into the caller's request: %+v", n)
	}
}

func TestPrepareTakesTheRetryCountFromTheJobWhenItNamesOne(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	opts := Options{Policy: jobs.Policy{MaxRetries: 7, Base: time.Second, Max: time.Minute}}.WithDefaults()

	// Including zero, which is a real answer meaning "do not retry" and not
	// an absent one. The old code could not tell those apart, because it used
	// the integer zero for both, so asking for no retries silently gave three.
	got := opts.Prepare(NewJob{Type: "email", MaxRetries: intPtr(0)}, "id-1", now)
	if got.MaxRetries != 0 {
		t.Errorf("max retries = %d, want 0, which the caller asked for", got.MaxRetries)
	}
}

func TestPrepareHoldsADelayedJobBack(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	opts := Options{}.WithDefaults()

	got := opts.Prepare(NewJob{Type: "email", Delay: 90 * time.Second}, "id-1", now)
	if want := now.Add(90 * time.Second); !got.RunAt.Equal(want) {
		t.Errorf("run at = %s, want %s", got.RunAt, want)
	}
}

func TestWithDefaultsLeavesAStatedPolicyAlone(t *testing.T) {
	stated := jobs.Policy{MaxRetries: 0, Base: 5 * time.Second, Max: time.Hour}
	got := Options{Policy: stated}.WithDefaults()
	if got.Policy != stated {
		t.Errorf("policy = %+v, want %+v", got.Policy, stated)
	}
	if got.Now == nil || got.Jitter == nil {
		t.Error("withDefaults left a nil function, which panics on the first call")
	}
}

func TestPolicyForTakesTheWaitsFromTheStoreAndTheCountFromTheJob(t *testing.T) {
	opts := Options{Policy: jobs.Policy{MaxRetries: 3, Base: 5 * time.Second, Max: time.Hour}}.WithDefaults()
	got := opts.PolicyFor(9)

	if got.MaxRetries != 9 {
		t.Errorf("max retries = %d, want the job's 9", got.MaxRetries)
	}
	if got.Base != 5*time.Second || got.Max != time.Hour {
		t.Errorf("waits = %s and %s, want the store's", got.Base, got.Max)
	}
}

func TestTheSentinelErrorsAreDistinct(t *testing.T) {
	if errors.Is(ErrNotFound, ErrLeaseNotValid) {
		t.Error("a missing job and a stale lease compare equal, so a caller cannot tell them apart")
	}
}

// A filter that names no order asks for the newest first.
//
// Every caller that exists was written before there was an order to name, so
// the zero value has to keep meaning what those callers already get. This is
// what makes naming the order a rename rather than a change.
func TestAFilterWithNoOrderAsksForTheNewestFirst(t *testing.T) {
	var f Filter
	if f.Order != Newest {
		t.Errorf("the zero order is %s, want %s", f.Order, Newest)
	}
	if err := f.Validate(); err != nil {
		t.Errorf("an empty filter was refused: %v", err)
	}
}

// An order the package does not know is refused rather than quietly read as
// the default.
//
// A caller who sends a number from a newer build is asking for something this
// store cannot do, and answering with the newest first would give them a page
// that looks right and is in the wrong order.
func TestAnOrderTheStoreDoesNotKnowIsRefused(t *testing.T) {
	err := Filter{Order: Order(99)}.Validate()
	if err == nil {
		t.Fatal("an unknown order was accepted")
	}
	if !strings.Contains(err.Error(), "Order(99)") {
		t.Errorf("the error does not name the order: %v", err)
	}
}

func TestDefaultJitterStaysInRange(t *testing.T) {
	for i := 0; i < 1000; i++ {
		if j := defaultJitter(); j < 0 || j >= 1 {
			t.Fatalf("defaultJitter returned %v", j)
		}
	}
}
