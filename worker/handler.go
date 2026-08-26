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
	"errors"
	"fmt"
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

	// LeaseExpiresAt is when the server would take the job back, as things
	// stood when the job was handed over.
	//
	// It is not a deadline for the handler, and the context does not end at
	// it. While a handler runs, the worker asks the server to push the lease
	// out, so this moment moves and the value here goes stale almost at once.
	// A handler that stopped at it would stop work the heartbeat was
	// successfully keeping alive.
	//
	// What ends the context is losing the lease: the server saying the job is
	// no longer this worker's. That arrives as a cancellation whose cause is
	// ErrLeaseLost, and a handler that respects its context needs nothing
	// else.
	//
	//	if errors.Is(context.Cause(ctx), worker.ErrLeaseLost) {
	//		// Somebody else has the job. Stop, and do not report.
	//	}
	//
	// This field is here for a handler that wants to reason about how much
	// room it had, or to log it. Use the context for control.
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

// ErrPermanent marks a failure that no later attempt will fix.
//
// A handler returns an error wrapping this, and the server buries the job at
// once whatever its attempt count. The text of the error is still kept on the
// job, so whoever finds it in the dead letter queue reads why.
//
// Use it when the job itself is the problem and not the world around it: a
// payload that names no account, a field that will not parse, an upstream
// that answers 404 for an identifier that was never real. Do not use it for a
// timeout, a refused connection or a rate limit, which are the failures
// retrying exists for.
//
//	if errors.Is(err, sql.ErrNoRows) {
//		return worker.Permanent(fmt.Errorf("no account %q: %w", id, err))
//	}
//
// This is not a way to skip a job quietly. A refused job is dead, it is
// counted, and it can be revived once whatever was wrong is fixed.
var ErrPermanent = errors.New("worker: this job will never succeed")

// Permanent wraps an error so that it buries the job on this attempt.
//
// It exists because doing this by hand is easy to get wrong in a direction
// that says nothing: an error built with %v instead of %w does not wrap, so
// the job is retried as though the handler had never spoken, and no test and
// no log line reports the difference.
//
// Permanent(nil) returns nil, because a handler that refuses nothing has
// finished the job.
func Permanent(err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%w: %w", ErrPermanent, err)
}

// Handler does the work of one job.
//
// Returning nil means the job is done. Returning an error sends the job back
// to the queue, or to the dead letter queue when it has no attempts left, and
// the text of the error is kept on the job.
//
// Returning an error that wraps ErrPermanent sends it to the dead letter
// queue at once, whatever the attempt count.
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
