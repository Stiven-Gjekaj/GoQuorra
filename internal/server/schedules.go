package server

import (
	"context"
	"log/slog"
	"time"

	"github.com/Stiven-Gjekaj/GoQuorra/internal/metrics"
	"github.com/Stiven-Gjekaj/GoQuorra/internal/store"
)

// produce turns repeat schedules into jobs, until the context ends.
//
// In the server and not in a trigger in the database, and not in a worker.
// A trigger would put the rule in a second place that has to agree with the
// Go one. A worker that produced work would be a worker whose absence is
// silent: the queue would look healthy and empty, and the reason would be
// that nothing was making the work.
//
// Every firing is submitted under an idempotency key built from the schedule
// and the window it belongs to. Two servers running this loop at once then
// produce one job, and a loop that runs twice over the same window produces
// one job. That is the whole concurrency story here, and it is the one the
// queue already has.
func produce(
	ctx context.Context,
	s store.Store,
	m *metrics.Metrics,
	log *slog.Logger,
	every time.Duration,
) {
	tick, ok := ticker(log, "schedules", every)
	if !ok {
		return
	}
	defer tick.Stop()

	log.Info("producing jobs from schedules", "every", every)

	for {
		select {
		case <-ctx.Done():
			log.Debug("no longer producing jobs from schedules")
			return
		case <-tick.C:
		}

		if err := produceOnce(ctx, s, m, log, time.Now()); err != nil {
			if ctx.Err() != nil {
				return
			}
			log.Error("cannot produce jobs from schedules", "error", err)
		}
	}
}

// produceOnce does one pass over the schedules.
//
// The moment is a parameter so that a test states it rather than waiting for
// it, which is the same rule the rest of this project follows.
func produceOnce(
	ctx context.Context,
	s store.Store,
	m *metrics.Metrics,
	log *slog.Logger,
	now time.Time,
) error {
	schedules, err := s.Schedules(ctx, true, store.MostSchedules)
	if err != nil {
		return err
	}

	for _, one := range schedules {
		// A schedule the loop cannot read does not stop the others. A rule
		// or a zone that this build cannot parse is one row's problem, and
		// stopping here would let it hold up every other schedule.
		windows, mark, dropped, err := one.Due(now)
		if err != nil {
			log.Error("cannot read a schedule", "schedule", one.Name, "error", err)
			continue
		}

		if dropped > 0 {
			// At warn, and it names the count. A schedule that dropped
			// windows is a schedule whose jobs did not run, and the number
			// is the only place that says how many.
			log.Warn("a schedule did not run for some of its windows",
				"schedule", one.Name, "dropped", dropped, "policy", one.CatchUp)
		}

		for _, window := range windows {
			job, created, err := s.Create(ctx, store.NewJob{
				Type:           one.Type,
				Payload:        one.Payload,
				Queue:          one.Queue,
				Priority:       one.Priority,
				MaxRetries:     one.MaxRetries,
				ScheduleID:     one.ID,
				IdempotencyKey: store.FiringKey(one.ID, window),
			})
			if err != nil {
				log.Error("cannot submit a job from a schedule",
					"schedule", one.Name, "window", window, "error", err)
				break
			}
			if !created {
				// Another server produced this window. Not a failure, and
				// not worth a line at info on every tick of a two server
				// deployment.
				log.Debug("this window was already produced",
					"schedule", one.Name, "window", window, "job", job.ID)
				continue
			}

			m.JobCreated()
			m.ScheduleFired(one.Name)
			log.Info("job submitted from a schedule",
				"schedule", one.Name, "window", window, "job", job.ID, "type", job.Type)
		}

		// Written only when it moved, so an idle deployment is not one write
		// per schedule per tick.
		//
		// The comparison is against what is stored and not against whether
		// anything was produced. A schedule that has never fired produces
		// nothing on its first pass and still has to record that it starts
		// from now: without this it stays never fired for ever, and every
		// pass reads it as new.

		// The mark moves whatever was produced, including for a policy that
		// produced nothing. Leaving it would catch the same windows up on
		// every tick, for ever.
		if one.LastFiredAt != nil && !mark.After(*one.LastFiredAt) {
			continue
		}
		if err := s.MarkScheduleFired(ctx, one.ID, mark); err != nil {
			log.Error("cannot record what a schedule has produced",
				"schedule", one.Name, "window", mark, "error", err)
		}
	}
	return nil
}

// ProduceOnce does one pass over the schedules, at a stated moment.
//
// Exported for a test, which is the only caller outside this file. The loop
// itself reads the clock; this takes the moment as a parameter so that a test
// states it rather than waiting for it.
func ProduceOnce(
	ctx context.Context,
	s store.Store,
	m *metrics.Metrics,
	log *slog.Logger,
	now time.Time,
) error {
	return produceOnce(ctx, s, m, log, now)
}
