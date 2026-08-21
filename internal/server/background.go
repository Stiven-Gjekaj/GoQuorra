package server

import (
	"context"
	"log/slog"
	"time"

	"github.com/Stiven-Gjekaj/GoQuorra/internal/metrics"
	"github.com/Stiven-Gjekaj/GoQuorra/internal/store"
)

// reclaim takes back leases that ran out, until the context ends.
//
// This loop is the whole reason a crashed worker is survivable. Without it a
// job leased by a process that then lost power stays leased for as long as
// the table lives: no worker can take it, and nothing anywhere reports that
// it is stuck. The old server had no such loop, and its README said so in one
// line, under "Fault Tolerance", as though it were a detail.
func reclaim(ctx context.Context, s store.Store, m *metrics.Metrics, log *slog.Logger, every time.Duration, batch int) {
	ticker := time.NewTicker(every)
	defer ticker.Stop()

	log.Info("reclaiming expired leases", "every", every, "batch", batch)

	for {
		select {
		case <-ctx.Done():
			log.Debug("no longer reclaiming leases")
			return
		case <-ticker.C:
		}

		// One batch per tick, on purpose. Draining until empty would let a
		// large backlog hold the loop and its database connection for an
		// unbounded time, and the next tick arrives soon enough.
		moved, err := s.ReclaimExpired(ctx, batch)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			log.Error("cannot reclaim expired leases", "error", err)
			continue
		}

		if moved > 0 {
			m.LeasesReclaimed(moved)
			// A count worth a line at info. Leases expiring means workers are
			// dying or running past their time, and both are worth seeing.
			log.Info("took back expired leases", "count", moved)
		}
	}
}

// refreshStats keeps the queue length gauge current.
//
// The gauge is a snapshot on a timer rather than a value updated as jobs
// move. Counting on every change would need every path that writes a job to
// also write a metric, and the paths that forget are the ones that matter.
// The lag is named in the help text of the metric.
func refreshStats(ctx context.Context, s store.Store, m *metrics.Metrics, log *slog.Logger, every time.Duration) {
	ticker := time.NewTicker(every)
	defer ticker.Stop()

	update := func() {
		stats, err := s.QueueStats(ctx)
		if err != nil {
			if ctx.Err() == nil {
				log.Warn("cannot count the queues for the gauge", "error", err)
			}
			return
		}
		m.SetQueueLengths(stats)
	}

	// Once at startup, so that the page is not empty until the first tick.
	update()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			update()
		}
	}
}
