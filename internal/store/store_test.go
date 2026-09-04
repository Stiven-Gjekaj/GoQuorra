package store

import (
	"encoding/json"
	"errors"
	"math"
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

	// The edges themselves fit. A bound written one out is a bound that
	// refuses a job the column would have held.
	for name, edge := range map[string]NewJob{
		"the highest priority the column holds": {Type: "e", Priority: math.MaxInt32},
		"the lowest priority the column holds":  {Type: "e", Priority: math.MinInt32},
		"the most retries the column holds":     {Type: "e", MaxRetries: intPtr(math.MaxInt32)},
	} {
		if err := edge.Validate(); err != nil {
			t.Errorf("%s was refused: %v", name, err)
		}
	}

	bad := map[string]NewJob{
		"no type":             {Payload: json.RawMessage(`{}`)},
		"type too long":       {Type: strings.Repeat("t", 256)},
		"queue too long":      {Type: "email", Queue: strings.Repeat("q", 256)},
		"negative delay":      {Type: "email", Delay: -time.Second},
		"negative retries":    {Type: "email", MaxRetries: intPtr(-1)},
		"payload is not json": {Type: "email", Payload: json.RawMessage(`{"to":`)},

		// The column is INTEGER and the field is a Go int, so a number
		// between the two sizes used to pass here and be refused by
		// PostgreSQL, which answered 500 for the client's mistake.
		"priority past the column":    {Type: "email", Priority: math.MaxInt32 + 1},
		"priority under the column":   {Type: "email", Priority: math.MinInt32 - 1},
		"max retries past the column": {Type: "email", MaxRetries: intPtr(math.MaxInt32 + 1)},
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

// A result that is not JSON is reported through a sentinel, not through the
// wording of a message.
//
// The layer above answers 400 to this and 500 to every other failure from
// this package, and it used to decide by searching the text for "not JSON".
// Rewording this sentence would have quietly moved every one of these to a
// 500 and pointed the reader at the server for a mistake the worker made.
// Nothing would have failed.
func TestABadResultIsReportedThroughASentinel(t *testing.T) {
	err := Report{Result: []byte(`{"rows":`)}.Validate()
	if !errors.Is(err, ErrNotJSON) {
		t.Fatalf("Validate gave %v, want ErrNotJSON", err)
	}
	if errors.Is(err, ErrNotFound) || errors.Is(err, ErrWrongState) {
		t.Error("the sentinel is not distinct from the others")
	}

	// A result that is JSON passes, so the check is not simply always true.
	if err := (Report{Result: []byte(`{"rows":20}`)}).Validate(); err != nil {
		t.Errorf("valid JSON was refused: %v", err)
	}
}

// An error the invalid helper builds answers to ErrInvalid and to nothing
// else.
//
// This is the property the layer above rests on. It answers 400 to this and
// 500 to everything else from this package, and every other sentinel here
// means something it must not confuse with a caller's mistake: a job that is
// not there is a 404, and a job in the wrong state is a 409.
func TestAnInvalidRequestIsReportedThroughItsOwnSentinel(t *testing.T) {
	err := invalid("the priority is %d, and the column holds fewer", 3000000000)

	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("invalid built %v, which does not answer to ErrInvalid", err)
	}
	for name, other := range map[string]error{
		"ErrNotFound":      ErrNotFound,
		"ErrLeaseNotValid": ErrLeaseNotValid,
		"ErrWrongState":    ErrWrongState,
		"ErrNotJSON":       ErrNotJSON,
	} {
		if errors.Is(err, other) {
			t.Errorf("a caller's mistake compares equal to %s", name)
		}
	}

	// An error from somewhere else does not answer to it. Without this the
	// test passes against an Is that says yes to everything, which is the
	// shape of the check this sentinel replaces.
	if errors.Is(errors.New("connection refused"), ErrInvalid) {
		t.Error("an error from underneath compares equal to a caller's mistake")
	}
}

// The message a caller reads carries no package name.
//
// Every message in this package used to begin with "store: ", and the HTTP
// layer handed it to the client unchanged, so a person submitting a job read
// the name of a Go package they cannot see.
func TestTheMessageOfAnInvalidRequestNamesNoPackage(t *testing.T) {
	if got := invalid("a job needs a type").Error(); got != "a job needs a type" {
		t.Errorf("the message is %q", got)
	}
}

// What a filter refuses is the caller's mistake as well.
//
// A listing is the route a person uses most, so a filter refused as the
// server's fault sends somebody reading logs for a query they mistyped.
func TestWhatAFilterRefusesIsTheCallersMistake(t *testing.T) {
	for name, f := range map[string]Filter{
		"a status that is not a status": {Status: "banana"},
		"a limit below one":             {Limit: -1},
		"an order the store does not know": {
			Limit: 10,
			Order: Order(99),
		},
	} {
		err := f.Validate()
		if err == nil {
			t.Errorf("%s was accepted", name)
			continue
		}
		if !errors.Is(err, ErrInvalid) {
			t.Errorf("%s gave %q, which does not answer to ErrInvalid", name, err)
		}
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
