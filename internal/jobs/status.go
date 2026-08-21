package jobs

import "fmt"

// Status is the state a job is in.
//
// There are four, and there used to be six. The two that went are recorded
// here because the documentation described both and no code ever wrote
// either.
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
)

// All lists every status, in the order a job meets them.
//
// The dashboard and the queue statistics both walk this, so a status added
// here appears in both without a second edit.
func All() []Status {
	return []Status{Pending, Leased, Succeeded, Dead}
}

// Valid reports whether s is a status this package knows.
//
// The store calls this on the way out of the database. A row holding a status
// written by an older version of this program is a fault worth naming at the
// point it is read, rather than one that travels on and is compared against
// nothing.
func (s Status) Valid() bool {
	switch s {
	case Pending, Leased, Succeeded, Dead:
		return true
	default:
		return false
	}
}

// Terminal reports whether a job in this state ever moves again.
func (s Status) Terminal() bool {
	return s == Succeeded || s == Dead
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
