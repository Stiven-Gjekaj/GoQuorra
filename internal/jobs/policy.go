package jobs

import (
	"encoding/json"
	"fmt"
	"time"
)

// Outcome is what happened to a job that a worker held.
type Outcome int

const (
	// OutcomeDone means the worker finished the job.
	OutcomeDone Outcome = iota

	// OutcomeFailed means the worker reported that the job did not finish.
	OutcomeFailed

	// OutcomeExpired means the lease ran out and nobody reported anything.
	// The worker crashed, lost the network, or is still running and is too
	// slow. The queue cannot tell these apart, so it treats all three the
	// same way as a reported failure.
	OutcomeExpired

	// OutcomeRefused means the worker read the job and will never finish it.
	// A payload that names no account does not name one on the third
	// attempt, and an upstream that has no record of an identifier will not
	// grow one while the job waits.
	//
	// This is different from OutcomeFailed in one way only, and it is the
	// way that matters: the attempt count does not decide what happens next.
	// Only the worker can tell these apart, because only the worker sees the
	// error.
	OutcomeRefused
)

func (o Outcome) String() string {
	switch o {
	case OutcomeDone:
		return "done"
	case OutcomeFailed:
		return "failed"
	case OutcomeExpired:
		return "expired"
	case OutcomeRefused:
		return "refused"
	default:
		return fmt.Sprintf("Outcome(%d)", int(o))
	}
}

// Outcomes lists every outcome, in the order they are declared.
func Outcomes() []Outcome {
	return []Outcome{OutcomeDone, OutcomeFailed, OutcomeExpired, OutcomeRefused}
}

// ParseOutcome reads an outcome from its name.
//
// The pair to String, so that an outcome survives being written down and read
// back. The type is an int, and writing the number instead would tie every
// stored row and every answer to the order the constants happen to be
// declared in: adding one in the middle would silently change what every
// older row means.
func ParseOutcome(text string) (Outcome, error) {
	for _, o := range Outcomes() {
		if o.String() == text {
			return o, nil
		}
	}
	return 0, fmt.Errorf("jobs: %q is not an outcome", text)
}

// MarshalJSON writes the name and not the number.
//
// Without this an outcome reaches a client as 0, 1, 2 or 3, and the client
// has to carry a copy of the order these are declared in to read it.
func (o Outcome) MarshalJSON() ([]byte, error) {
	if _, err := ParseOutcome(o.String()); err != nil {
		return nil, fmt.Errorf("jobs: cannot write %s as JSON: %w", o, err)
	}
	return []byte(`"` + o.String() + `"`), nil
}

// UnmarshalJSON reads the name.
func (o *Outcome) UnmarshalJSON(data []byte) error {
	var text string
	if err := json.Unmarshal(data, &text); err != nil {
		return fmt.Errorf("jobs: an outcome is a name in JSON: %w", err)
	}
	parsed, err := ParseOutcome(text)
	if err != nil {
		return err
	}
	*o = parsed
	return nil
}

// Policy holds the retry rules.
type Policy struct {
	// MaxRetries is how many times a job runs again after the first attempt.
	//
	// The name says retries, so it counts retries. A job with MaxRetries 3
	// runs four times: once, and then three more. The version before the
	// rebuild compared attempts against this number and gave three runs in
	// total, so a caller asking for three retries got two.
	MaxRetries int

	// Base is the wait before the first retry, before jitter.
	Base time.Duration

	// Max caps the wait. A job that fails all day retries every Max, and not
	// once a fortnight.
	Max time.Duration
}

// DefaultPolicy is what a job gets when the caller names no rules.
func DefaultPolicy() Policy {
	return Policy{MaxRetries: 3, Base: 2 * time.Second, Max: time.Hour}
}

// Validate refuses a policy that cannot work.
func (p Policy) Validate() error {
	if p.MaxRetries < 0 {
		return fmt.Errorf("jobs: MaxRetries is %d, and it cannot be negative", p.MaxRetries)
	}
	if p.Base <= 0 {
		return fmt.Errorf("jobs: Base is %s, and it must be above zero", p.Base)
	}
	if p.Max < p.Base {
		return fmt.Errorf("jobs: Max is %s, which is below Base at %s", p.Max, p.Base)
	}
	return nil
}

// Decision is what the store writes after a job stops running.
type Decision struct {
	Status   Status
	Attempts int
	RunAt    time.Time
}

// Decide says what happens to a job that has stopped running.
//
// attempts counts every time the job has been handed to a worker, including
// the one that just ended. The store raises it when it leases the job, and
// not when the worker reports back. That ordering is the reason a crashed
// worker costs the job an attempt: nobody reports anything in that case, so a
// count kept at report time would never move and the job would retry forever.
//
// jitter comes from the caller and must sit between 0 and 1. Passing it in
// rather than drawing it here keeps this function pure, so a test states the
// wait it expects instead of accepting a range.
func (p Policy) Decide(attempts int, outcome Outcome, now time.Time, jitter float64) Decision {
	if outcome == OutcomeDone {
		return Decision{Status: Succeeded, Attempts: attempts, RunAt: now}
	}

	// A refusal ends the job whatever the attempt count says. The worker has
	// read the job and says no attempt will finish it, and running it again
	// spends a worker to reach the same answer.
	//
	// The job still ends in Dead and not in a status of its own. Dead means
	// the queue will not run this job again, which is exactly what a refusal
	// means, and the reason is on the row. This project has removed two
	// statuses that described how a job arrived somewhere rather than where
	// it was, and it is not adding a third.
	if outcome == OutcomeRefused || attempts > p.MaxRetries {
		return Decision{Status: Dead, Attempts: attempts, RunAt: now}
	}

	return Decision{
		Status:   Pending,
		Attempts: attempts,
		RunAt:    now.Add(p.Backoff(attempts, jitter)),
	}
}

// Backoff is how long a job waits before attempt number attempts+1.
//
// The wait doubles from Base and stops at Max. Half of it is fixed and half
// is jitter, so the answer sits between half the plain wait and all of it.
//
// The jitter is not decoration. Without it, a database that goes away for a
// minute sends every job that failed in that minute back at the same instant,
// and the retry storm knocks the database over a second time. Spreading the
// retries costs nothing and removes that.
func (p Policy) Backoff(attempts int, jitter float64) time.Duration {
	if attempts < 1 {
		attempts = 1
	}
	if jitter < 0 {
		jitter = 0
	}
	if jitter > 1 {
		jitter = 1
	}

	// Doubling in a loop rather than shifting by attempts. A shift overflows
	// int64 at 63, and a job that has failed 63 times is a job this code has
	// to keep answering for. The loop stops at Max and cannot overflow.
	wait := p.Base
	for i := 1; i < attempts; i++ {
		if wait >= p.Max {
			break
		}
		wait *= 2
	}
	if wait > p.Max {
		wait = p.Max
	}

	half := wait / 2
	return half + time.Duration(float64(half)*jitter)
}
