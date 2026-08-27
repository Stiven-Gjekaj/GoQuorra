package memory

import (
	"context"
	"time"
)

// Watch reports queues that may have work.
//
// The memory store learns this from its own writes: it is the only writer,
// because there is nothing else holding the same jobs. That is the whole
// difference from the PostgreSQL one, which has to hear about writes made by
// another server.
func (s *Store) Watch(ctx context.Context) (<-chan string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// Buffered, and dropped rather than blocked on when it is full.
	//
	// A hint is worth less than the write that produced it. A store that
	// waited for a slow watcher would make every submission as slow as the
	// slowest thing listening, and the watcher that missed a hint still
	// polls.
	out := make(chan string, 64)

	s.mu.Lock()
	s.watchers = append(s.watchers, out)
	s.mu.Unlock()

	go func() {
		<-ctx.Done()

		s.mu.Lock()
		defer s.mu.Unlock()
		for i, one := range s.watchers {
			if one == out {
				s.watchers = append(s.watchers[:i], s.watchers[i+1:]...)
				break
			}
		}
		close(out)
	}()

	return out, nil
}

// hint says that a queue may have work.
//
// The caller holds the lock. It is called where a job becomes ready now, and
// not where one becomes ready later: a job with a delay, or one waiting out a
// backoff, is deliberately not urgent, and the poll is what finds it.
func (s *Store) hint(queue string) {
	for _, out := range s.watchers {
		select {
		case out <- queue:
		default:
			// Full. The watcher is behind, and a hint it never sees costs it
			// one poll interval.
		}
	}
}

// readyNow reports whether a job is one a worker could take this instant.
func readyNow(status, queue string, runAt, now time.Time) bool {
	return status == "pending" && queue != "" && !runAt.After(now)
}
