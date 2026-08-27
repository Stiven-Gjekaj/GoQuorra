package server

import (
	"context"
	"log/slog"
	"time"

	"github.com/Stiven-Gjekaj/GoQuorra/internal/jobs"
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

// sweep removes finished jobs that are older than their retention.
//
// It runs one status at a time, and skips any whose retention is zero. Zero
// is the default for all of them, so a deployment that has not asked for this
// keeps every job it has ever run.
func sweep(
	ctx context.Context,
	s store.Store,
	m *metrics.Metrics,
	log *slog.Logger,
	every time.Duration,
	batch int,
	retention map[jobs.Status]time.Duration,
	workerRetention time.Duration,
) {
	wanted := make(map[jobs.Status]time.Duration, len(retention))
	for status, keep := range retention {
		if keep > 0 {
			wanted[status] = keep
		}
	}
	if len(wanted) == 0 && workerRetention <= 0 {
		log.Debug("no retention is set, so nothing is ever removed")
		return
	}

	ticker := time.NewTicker(every)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		for status, keep := range wanted {
			// One batch per status per tick. Draining until empty would let
			// a first sweep over a year of history hold a database
			// connection for as long as it took, and the next tick is soon.
			removed, err := s.DeleteFinished(ctx, status, time.Now().Add(-keep), batch)
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				log.Error("cannot remove finished jobs", "status", status, "error", err)
				continue
			}
			if removed > 0 {
				m.JobsRemoved(status, removed)
				// At info, because this is the loop that deletes things. An
				// operator asking where a job went should find the answer in
				// the log rather than by reading the source.
				log.Info("removed finished jobs", "status", status, "count", removed, "older_than", keep)
			}
		}

		// The workers a deployment left behind, in the same loop rather than
		// a fourth one. It runs on the same tick, deletes in the same
		// bounded way, and a second ticker for it would be a second thing
		// that can stop.
		if workerRetention > 0 {
			gone, err := s.DeleteStaleWorkers(ctx, time.Now().Add(-workerRetention), batch)
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				log.Error("cannot remove stale workers", "error", err)
				continue
			}
			if gone > 0 {
				// At debug and not info. A deployment retires a whole fleet
				// at once, so this line would be the loudest thing in the log
				// on every release, and unlike a removed job nobody goes
				// looking for where a worker name went.
				log.Debug("removed workers nobody has seen", "count", gone, "older_than", workerRetention)
			}
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
