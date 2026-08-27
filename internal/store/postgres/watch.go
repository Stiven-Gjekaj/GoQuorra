package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// readyChannel is the PostgreSQL notification channel this store uses.
//
// One channel for every queue, with the queue name in the payload, rather
// than one channel per queue. A server does not know which queues exist until
// it looks, and a queue is a string a caller chose: LISTEN takes an
// identifier, so a channel named after a caller's string would have to be
// quoted and would still be a name somebody could collide with.
const readyChannel = "quorra_ready"

// Watch reports queues that may have work.
//
// It holds one connection open for the whole call, which is what LISTEN
// needs: the notifications arrive on the connection that issued it, and a
// pooled connection handed back between statements would lose them.
//
// docs/milestones.md named the three things this needs. This is all three.
// The connection is held here. The fallback is that every caller still polls,
// so a notification that is dropped costs latency and nothing else. And the
// case it warned about, a job becoming ready after a backoff, is not covered
// and does not need to be: a job waiting out a backoff is deliberately not
// urgent, and no insert trigger would have seen it either.
func (s *Store) Watch(ctx context.Context) (<-chan string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	taken, err := s.pool.Acquire(ctx)
	if err != nil {
		return nil, fmt.Errorf("postgres: cannot take a connection to listen on: %w", err)
	}

	// Hijacked, so the connection leaves the pool for good.
	//
	// A LISTEN lives on the connection that issued it and outlives the
	// statement. Releasing this one back would put a listening connection
	// into the pool, where the next ordinary query would collect the
	// notifications meant for this watcher and the watcher after it would be
	// handed a queue of stale ones. That was found by a contract rule about
	// closing a watcher, which saw two hints on a channel nothing had
	// notified.
	conn := taken.Hijack()

	if _, err := conn.Exec(ctx, "LISTEN "+readyChannel); err != nil {
		_ = conn.Close(context.Background())
		return nil, fmt.Errorf("postgres: cannot listen: %w", err)
	}

	// Buffered, and dropped rather than blocked on when it is full. A hint is
	// worth less than the work it points at, and a watcher that missed one
	// still polls.
	out := make(chan string, 64)

	go func() {
		defer close(out)
		defer func() {
			// Its own context, because the caller's has already ended by the
			// time this runs and a close on a cancelled one does nothing.
			closing, stop := context.WithTimeout(context.Background(), 5*time.Second)
			defer stop()
			_ = conn.Close(closing)
		}()

		for {
			note, err := conn.WaitForNotification(ctx)
			if err != nil {
				if ctx.Err() == nil {
					// The connection went away. The caller falls back to its
					// poll, which is why this is a hint and not a promise.
					s.opts.Log("the listen connection ended", err)
				}
				return
			}

			select {
			case out <- note.Payload:
			default:
			}
		}
	}()

	return out, nil
}

// hint says that a queue may have work.
//
// It runs on whatever connection the caller has, inside their transaction
// when they have one. A NOTIFY inside a transaction is delivered when that
// transaction commits and not before, which is exactly right: a listener
// told about a job that was then rolled back would find nothing.
//
// A failure here is logged and not returned. The write that produced the
// hint has already happened, and failing a submission because a hint could
// not be sent would trade correctness for latency in the wrong direction.
func hint(ctx context.Context, q execer, log func(string, error), queue string) {
	if queue == "" {
		return
	}
	// pg_notify and not NOTIFY, because the payload is a queue name a caller
	// chose and NOTIFY takes it as a literal that would have to be quoted.
	if _, err := q.Exec(ctx, `SELECT pg_notify($1, $2)`, readyChannel, queue); err != nil {
		log("cannot send a hint that a queue has work", err)
	}
}

// execer is what both the pool and a transaction offer.
type execer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// readyNow reports whether a job is one a worker could take this instant.
func readyNow(status, queue string, runAt, now time.Time) bool {
	return status == "pending" && queue != "" && !runAt.After(now)
}
