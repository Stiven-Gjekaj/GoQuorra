package jobs

import "fmt"

// Status is the state a job is in.
//
// There are six. Two of an earlier six went, and two arrived since, and both
// halves of that are recorded here: a state that was removed for a reason
// still holds that reason against being added back.
//
// "processing" went because the server cannot observe it. A worker holds the
// job between the lease and the acknowledgement, and the server hears nothing
// in that window. A status the server cannot set is a status that lies.
//
// "failed" went because it was never distinguishable from "pending". A job
// that fails and has attempts left goes back into the queue, which is what
// pending means. Writing a second name for the same state gave a reader two
// answers to one question.
type Status string

const (
	// Pending means the job waits for a worker. It is ready when RunAt has
	// passed, and a delayed job or a job waiting out a backoff sits here too.
	Pending Status = "pending"

	// Leased means a worker holds the job. The lease carries an expiry, and
	// the reclaimer takes the job back when that expiry passes.
	Leased Status = "leased"

	// Succeeded means a worker reported that the job is done.
	Succeeded Status = "succeeded"

	// Dead means the job used every attempt it had. It stays in the table so
	// that somebody can read the last error and decide what to do.
	Dead Status = "dead"

	// Cancelled means a person stopped the job.
	//
	// It is separate from Dead on purpose. Both are endings that are not a
	// success, and the difference between them is the only thing that says
	// whether the queue gave up or somebody decided. An operator counting
	// failures wants one number and not the other.
	Cancelled Status = "cancelled"

	// Blocked means the job waits for another job to succeed.
	//
	// It is separate from Pending because Pending is a claim: it says the
	// queue will hand this job to the next worker that asks, once RunAt has
	// passed. A job waiting for a parent is not that, and calling it pending
	// would make the queue length, the dashboard and every listing count work
	// as ready when it is not.
	//
	// The alternative that was considered and refused was a RunAt far in the
	// future. It needs no new state, and it lies in a worse way: the job then
	// says it runs in the year nine thousand, which is what the soonest order
	// and the ready filter would show.
	//
	// It is not terminal. A parent that succeeds moves the job to Pending,
	// and a parent that will never succeed moves it to Cancelled.
	Blocked Status = "blocked"
)

// All lists every status, in the order a job meets them.
//
// The dashboard and the queue statistics both walk this, so a status added
// here appears in both without a second edit.
func All() []Status {
	return []Status{Blocked, Pending, Leased, Succeeded, Dead, Cancelled}
}

// Valid reports whether s is a status this package knows.
//
// The store calls this on the way out of the database. A row holding a status
// written by an older version of this program is a fault worth naming at the
// point it is read, rather than one that travels on and is compared against
// nothing.
func (s Status) Valid() bool {
	switch s {
	case Blocked, Pending, Leased, Succeeded, Dead, Cancelled:
		return true
	default:
		return false
	}
}

// Terminal reports whether a job in this state ever moves again.
func (s Status) Terminal() bool {
	return s == Succeeded || s == Dead || s == Cancelled
}

// ParseStatus turns text into a Status, and refuses anything else.
func ParseStatus(text string) (Status, error) {
	s := Status(text)
	if !s.Valid() {
		return "", fmt.Errorf("jobs: %q is not a status", text)
	}
	return s, nil
}

func (s Status) String() string { return string(s) }
