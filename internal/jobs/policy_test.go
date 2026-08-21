package jobs

import (
	"testing"
	"time"
)

var epoch = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// A job with MaxRetries 3 runs four times.
//
// This is the test the rebuild exists for. The old code compared the attempt
// count against max_retries and moved the job to the dead letter queue on the
// third run, so a caller who asked for three retries received two. Counting
// the runs of a whole life is the only way to see that, because every single
// step of it looked right.
func TestARetryLimitCountsRetriesAndNotAttempts(t *testing.T) {
	for _, maxRetries := range []int{0, 1, 3, 5} {
		p := Policy{MaxRetries: maxRetries, Base: time.Second, Max: time.Minute}

		runs, attempts := 0, 0
		status := Pending
		for status == Pending {
			// This is what the store does: it raises the count as it hands
			// the job out, and then asks what to do with the answer.
			attempts++
			runs++
			status = p.Decide(attempts, OutcomeFailed, epoch, 0).Status

			if runs > 100 {
				t.Fatalf("MaxRetries %d: the job never stopped", maxRetries)
			}
		}

		if status != Dead {
			t.Errorf("MaxRetries %d: the job ended at %q", maxRetries, status)
		}
		if want := maxRetries + 1; runs != want {
			t.Errorf("MaxRetries %d: the job ran %d times, want %d", maxRetries, runs, want)
		}
	}
}

// A lease that runs out ages a job exactly as a reported failure does.
//
// These are two different code paths in the store: one is an acknowledgement
// from a worker, the other is the reclaimer finding an expiry. Before the
// rebuild there was no reclaimer at all, so a crashed worker's job stayed
// leased forever and aged not at all. Sending both through one function is
// the fix, and this is the test that says they have not drifted apart again.
func TestAnExpiredLeaseAgesAJobLikeAFailure(t *testing.T) {
	p := Policy{MaxRetries: 4, Base: 2 * time.Second, Max: time.Hour}

	for attempts := 1; attempts <= 6; attempts++ {
		failed := p.Decide(attempts, OutcomeFailed, epoch, 0.5)
		expired := p.Decide(attempts, OutcomeExpired, epoch, 0.5)
		if failed != expired {
			t.Errorf("attempt %d: failure gives %+v and expiry gives %+v", attempts, failed, expired)
		}
	}
}

func TestSuccessEndsTheJobWhateverTheAttemptCount(t *testing.T) {
	p := Policy{MaxRetries: 1, Base: time.Second, Max: time.Minute}

	// Including an attempt count past the retry limit. A worker that finishes
	// its last permitted run has succeeded, and must not be buried for it.
	for _, attempts := range []int{1, 2, 9} {
		got := p.Decide(attempts, OutcomeDone, epoch, 0)
		if got.Status != Succeeded {
			t.Errorf("attempt %d: status %q, want %q", attempts, got.Status, Succeeded)
		}
		if !got.RunAt.Equal(epoch) {
			t.Errorf("attempt %d: a finished job was given a future time %s", attempts, got.RunAt)
		}
	}
}

func TestBackoffDoublesAndThenStops(t *testing.T) {
	p := Policy{MaxRetries: 20, Base: time.Second, Max: 8 * time.Second}

	// With no jitter the answer is half the plain wait, which is the value
	// this table states. Stating the halves rather than the doubles keeps the
	// arithmetic of the jitter visible instead of hidden in a helper.
	want := []time.Duration{
		500 * time.Millisecond, // 1s
		time.Second,            // 2s
		2 * time.Second,        // 4s
		4 * time.Second,        // 8s
		4 * time.Second,        // capped
		4 * time.Second,        // capped
	}
	for i, expected := range want {
		attempts := i + 1
		if got := p.Backoff(attempts, 0); got != expected {
			t.Errorf("Backoff(%d) = %s, want %s", attempts, got, expected)
		}
	}
}

// The wait must stay inside its half at every jitter, and must never exceed
// the cap.
func TestBackoffStaysInsideItsBounds(t *testing.T) {
	p := Policy{MaxRetries: 20, Base: time.Second, Max: time.Minute}

	for attempts := 1; attempts <= 12; attempts++ {
		plain := p.Backoff(attempts, 1)
		half := p.Backoff(attempts, 0)

		if half*2 != plain {
			t.Errorf("attempt %d: jitter 1 gives %s and jitter 0 gives %s, which are not a wait and its half", attempts, plain, half)
		}
		if plain > p.Max {
			t.Errorf("attempt %d: %s is above the cap of %s", attempts, plain, p.Max)
		}

		for _, jitter := range []float64{0, 0.25, 0.5, 0.75, 1} {
			got := p.Backoff(attempts, jitter)
			if got < half || got > plain {
				t.Errorf("attempt %d jitter %v: %s is outside [%s, %s]", attempts, jitter, got, half, plain)
			}
		}
	}
}

// A jitter outside the range is clamped rather than allowed to produce a
// negative wait or a wait past the cap. A caller passing a bad number is a
// defect, and a job scheduled in the past that runs immediately forever is a
// worse way to learn about it than a wait that is merely wrong.
func TestBackoffClampsAJitterOutsideTheRange(t *testing.T) {
	p := Policy{MaxRetries: 5, Base: time.Second, Max: time.Minute}

	if got := p.Backoff(3, -5); got != p.Backoff(3, 0) {
		t.Errorf("a negative jitter gave %s", got)
	}
	if got := p.Backoff(3, 99); got != p.Backoff(3, 1) {
		t.Errorf("a jitter above one gave %s", got)
	}
	if got := p.Backoff(3, -5); got <= 0 {
		t.Errorf("a negative jitter gave a wait of %s, which schedules the job in the past", got)
	}
}

// The doubling must not overflow.
//
// Shifting by the attempt count is the obvious way to write this, and it
// turns negative at 63. A negative wait sets RunAt in the past, so the job
// runs immediately, fails, and comes straight back. The queue then spins on
// one poisoned job for as long as the process lives.
func TestBackoffSurvivesAnAbsurdAttemptCount(t *testing.T) {
	p := Policy{MaxRetries: 1 << 30, Base: time.Second, Max: time.Hour}

	for _, attempts := range []int{62, 63, 64, 1000, 1 << 20} {
		got := p.Backoff(attempts, 1)
		if got <= 0 {
			t.Errorf("Backoff(%d) = %s", attempts, got)
		}
		if got > p.Max {
			t.Errorf("Backoff(%d) = %s, which is above the cap of %s", attempts, got, p.Max)
		}
	}
}

func TestDecideSchedulesTheRetryInTheFuture(t *testing.T) {
	p := Policy{MaxRetries: 3, Base: 2 * time.Second, Max: time.Hour}

	got := p.Decide(1, OutcomeFailed, epoch, 0)
	if got.Status != Pending {
		t.Fatalf("status %q, want %q", got.Status, Pending)
	}
	if !got.RunAt.After(epoch) {
		t.Errorf("the retry is scheduled at %s, which is not after %s", got.RunAt, epoch)
	}
	if want := epoch.Add(time.Second); !got.RunAt.Equal(want) {
		t.Errorf("RunAt = %s, want %s", got.RunAt, want)
	}
}

func TestValidate(t *testing.T) {
	if err := DefaultPolicy().Validate(); err != nil {
		t.Fatalf("the default policy is invalid: %v", err)
	}

	bad := []Policy{
		{MaxRetries: -1, Base: time.Second, Max: time.Minute},
		{MaxRetries: 3, Base: 0, Max: time.Minute},
		{MaxRetries: 3, Base: -time.Second, Max: time.Minute},
		{MaxRetries: 3, Base: time.Minute, Max: time.Second},
	}
	for _, p := range bad {
		if err := p.Validate(); err == nil {
			t.Errorf("%+v was accepted", p)
		}
	}
}

func TestOutcomeString(t *testing.T) {
	cases := map[Outcome]string{
		OutcomeDone:    "done",
		OutcomeFailed:  "failed",
		OutcomeExpired: "expired",
		Outcome(99):    "Outcome(99)",
	}
	for outcome, want := range cases {
		if got := outcome.String(); got != want {
			t.Errorf("Outcome(%d).String() = %q, want %q", int(outcome), got, want)
		}
	}
}
