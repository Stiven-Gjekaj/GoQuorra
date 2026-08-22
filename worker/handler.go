// Package worker runs jobs from a GoQuorra server.
//
// It is the one package here that other projects import, so it is the one
// that has to stay stable. Nothing in it mentions gRPC or protobuf: a handler
// is given a job with a payload of bytes and returns an error, and the
// transport underneath can be replaced without touching a line of anybody
// else's code.
//
// The version before the rebuild put this in internal/worker, which no other
// module can import at all, while the README told the reader to add their job
// types by editing that file. Following the README meant forking the project.
//
//	w, err := worker.New(worker.Config{
//		ServerAddr: "localhost:50051",
//		Queues:     []string{"email"},
//	})
//	if err != nil {
//		return err
//	}
//	w.Handle("email_send", func(ctx context.Context, job worker.Job) error {
//		var mail struct{ To, Subject string }
//		if err := json.Unmarshal(job.Payload, &mail); err != nil {
//			return err
//		}
//		return send(ctx, mail.To, mail.Subject)
//	})
//	return w.Run(ctx)
package worker

import (
	"context"
	"encoding/json"
	"time"
)

// Job is one piece of work.
type Job struct {
	ID   string
	Type string

	// Payload is the JSON the client submitted. Use Decode to read it.
	Payload []byte

	Queue    string
	Priority int

	// Attempts counts this run and the ones before it, so the first run of a
	// job reports 1. A handler that has to behave differently on a retry
	// reads this.
	Attempts   int
	MaxRetries int

	// LeaseExpiresAt is when the server takes the job back. The context given
	// to a handler ends at this moment, so a handler that respects its
	// context does not need to check this itself.
	LeaseExpiresAt time.Time

	RunAt     time.Time
	CreatedAt time.Time

	// leaseID is not exported. A handler has no use for it, and a handler
	// that could read it could report on its own job behind the worker's
	// back.
	leaseID string
}

// Decode reads the payload into a value.
func (j Job) Decode(into any) error {
	return json.Unmarshal(j.Payload, into)
}

// LastAttempt reports whether a failure now sends the job to the dead letter
// queue. A handler that writes somewhere on the way out uses it.
func (j Job) LastAttempt() bool { return j.Attempts > j.MaxRetries }

// Handler does the work of one job.
//
// Returning nil means the job is done. Returning an error sends the job back
// to the queue, or to the dead letter queue when it has no attempts left, and
// the text of the error is kept on the job.
//
// A handler must expect to be run more than once for the same job. GoQuorra
// delivers at least once, not exactly once: a worker that finishes the work
// and then loses power before it reports leaves a job that another worker
// picks up. There is no way to avoid this that does not simply move the same
// window somewhere else.
type Handler interface {
	Handle(ctx context.Context, job Job) error
}

// HandlerFunc lets a plain function be a Handler.
type HandlerFunc func(ctx context.Context, job Job) error

// Handle calls the function.
func (f HandlerFunc) Handle(ctx context.Context, job Job) error { return f(ctx, job) }

// ResultFunc is a handler that produces something worth keeping.
//
// Whatever it returns is marshalled to JSON and stored on the job, where the
// API serves it back. Returning nil keeps nothing.
//
// Keep it small. A queue row is read by every listing that touches it, and
// the server refuses a result past its limit rather than trimming one: half a
// JSON document is not a smaller result, it is a broken one. Put a large
// value where it belongs and return a reference to it.
type ResultFunc func(ctx context.Context, job Job) (any, error)
