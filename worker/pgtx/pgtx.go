// Package pgtx runs a handler inside the transaction its report commits in.
//
// It is for one case: a job whose side effect is a write to the same
// PostgreSQL database the queue is in. The handler is handed a transaction,
// writes what it came to write, and the outcome of the job is recorded in
// that same transaction. The two commit together or neither does.
//
// # What this is not
//
// It is not exactly once delivery, and it must not be described as though it
// were. GoQuorra delivers at least once, and no protocol removes the window
// between a side effect and the acknowledgement of it. What this does is make
// that window empty for one case, by making the side effect and the
// acknowledgement one write.
//
// Everything outside that case is unchanged. A handler that calls another
// service, writes to another database, or sends an email has the same window
// it always had, and has to be written to survive running twice.
//
// # Why it is its own package
//
// A handler here takes a pgx.Tx, so this package imports pgx. The worker
// package does not, and a consumer that only submits and runs jobs should not
// start compiling a database driver to do it.
//
// # What can still go wrong
//
//   - The commit fails. Neither the side effect nor the report happened, the
//     lease runs out, and another worker takes the job. Correct.
//   - The worker dies before the commit. The same.
//   - The worker dies after the commit. Both happened, and the job is
//     recorded as finished. Correct.
//   - The lease ran out while the handler worked, and another worker already
//     has the job. The report is refused inside the transaction, so the
//     handler's writes roll back with it. That is the case worth knowing
//     about: the work is undone rather than committed against a job somebody
//     else now owns.
package pgtx

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/Stiven-Gjekaj/GoQuorra/internal/jobs"
	"github.com/Stiven-Gjekaj/GoQuorra/internal/store"
	"github.com/Stiven-Gjekaj/GoQuorra/internal/store/postgres"
	"github.com/Stiven-Gjekaj/GoQuorra/worker"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Func is a handler that writes to the same database as the queue.
//
// Everything it writes through tx commits with the report, or not at all. It
// must not commit or roll back that transaction: this package does, because
// the report is the other half of it.
type Func func(ctx context.Context, tx pgx.Tx, job worker.Job) error

// ResultFunc is a Func that produces something worth keeping on the job.
type ResultFunc func(ctx context.Context, tx pgx.Tx, job worker.Job) (any, error)

// Runner turns a Func into something worker.HandleResult accepts.
type Runner struct {
	pool  *pgxpool.Pool
	store *postgres.Store
}

// New builds a runner over the pool the queue itself uses.
//
// The same database, and it has to be: the whole point is one transaction
// covering the handler's writes and the report. A pool pointed at a different
// database would compile, run, and give exactly the at least once behaviour
// this package exists to improve on, with no sign that it had.
func New(pool *pgxpool.Pool) (*Runner, error) {
	if pool == nil {
		return nil, errors.New("pgtx: a pool is required, and it has to be the one the queue is in")
	}
	return &Runner{pool: pool, store: postgres.NewWithPool(pool, store.Options{})}, nil
}

// Handle wraps a Func for worker.Handle.
func (r *Runner) Handle(fn Func) worker.ResultFunc {
	return r.HandleResult(func(ctx context.Context, tx pgx.Tx, job worker.Job) (any, error) {
		return nil, fn(ctx, tx, job)
	})
}

// HandleResult wraps a ResultFunc for worker.HandleResult.
func (r *Runner) HandleResult(fn ResultFunc) worker.ResultFunc {
	return func(ctx context.Context, job worker.Job) (any, error) {
		tx, err := r.pool.Begin(ctx)
		if err != nil {
			return nil, fmt.Errorf("pgtx: cannot begin: %w", err)
		}
		// Rolled back unless the commit below happened. A rollback of a
		// committed transaction is a no-op, so this is safe on every path.
		defer func() { _ = tx.Rollback(ctx) }()

		result, err := fn(ctx, tx, job)
		if err != nil {
			// The handler's writes go with it. The worker reports the failure
			// over gRPC as it does for any other handler, so the job is
			// retried or buried by the same rules.
			return nil, err
		}

		encoded, err := encode(result)
		if err != nil {
			return nil, err
		}

		if _, err := r.store.ReportIn(ctx, tx, store.Report{
			JobID:   job.ID,
			LeaseID: job.LeaseID(),
			Outcome: jobs.OutcomeDone,
			Result:  encoded,
		}); err != nil {
			// A lease that ran out while the handler worked lands here, and
			// the handler's writes roll back with it. That is the case worth
			// knowing about, and it is better than committing work against a
			// job somebody else now owns.
			return nil, fmt.Errorf("pgtx: cannot record the outcome: %w", err)
		}

		if err := tx.Commit(ctx); err != nil {
			return nil, fmt.Errorf("pgtx: cannot commit: %w", err)
		}

		// The job is written. The worker must not report it again: the row no
		// longer holds this lease, so a second report would be refused and
		// the refusal would read as a fault.
		return nil, worker.ErrAlreadyReported
	}
}

// encode turns what a handler produced into the JSON the job keeps.
func encode(result any) ([]byte, error) {
	if result == nil {
		return nil, nil
	}
	if raw, ok := result.(json.RawMessage); ok {
		return raw, nil
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		return nil, fmt.Errorf("pgtx: what the handler produced is not JSON: %w", err)
	}
	return encoded, nil
}
