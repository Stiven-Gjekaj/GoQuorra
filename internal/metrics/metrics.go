// Package metrics counts what the server does.
//
// Everything registers on a registry this package owns, and not on the global
// default that promauto uses. Two reasons. A test can build a set of metrics,
// drive it, and read the numbers back, which a global registry makes
// impossible because the second test to build one panics on a duplicate
// registration. And the exposed page holds what this program publishes rather
// than whatever any linked library decided to add to the default.
package metrics

import (
	"net/http"
	"time"

	"github.com/Stiven-Gjekaj/GoQuorra/internal/jobs"
	"github.com/Stiven-Gjekaj/GoQuorra/internal/store"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Metrics holds every measurement the server publishes.
type Metrics struct {
	registry *prometheus.Registry

	created   prometheus.Counter
	leased    prometheus.Counter
	succeeded prometheus.Counter
	retried   prometheus.Counter
	dead      prometheus.Counter
	refused   prometheus.Counter
	reclaimed prometheus.Counter
	cancelled prometheus.Counter
	revived   prometheus.Counter

	removed     *prometheus.CounterVec
	queueLength *prometheus.GaugeVec
	lifetime    *prometheus.HistogramVec
	httpLatency *prometheus.HistogramVec
}

// New builds a set of metrics on a registry of its own.
func New() *Metrics {
	registry := prometheus.NewRegistry()

	m := &Metrics{
		registry: registry,

		created: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "quorra_jobs_created_total",
			Help: "Jobs accepted from a client.",
		}),
		leased: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "quorra_jobs_leased_total",
			Help: "Times a job has been handed to a worker. A job that is retried is counted again.",
		}),
		succeeded: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "quorra_jobs_succeeded_total",
			Help: "Jobs a worker reported as finished.",
		}),
		// Retries and burials are separate counters.
		//
		// The old code raised one "failed" counter on every failure, the last
		// one included, and never raised the dead counter at all. So the
		// dashboard panel for the dead letter queue read zero for ever, and
		// the failure rate double counted the job that had just been buried.
		retried: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "quorra_jobs_retried_total",
			Help: "Failures that sent a job back to the queue. A job buried on its last attempt is not counted here.",
		}),
		dead: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "quorra_jobs_dead_total",
			Help: "Jobs that moved to the dead letter queue, however they got there.",
		}),

		// Refusals are a part of the deaths above and not a number beside
		// them, so the two divide. The question an operator asks is what
		// share of the dead letter queue is the queue giving up against a
		// handler saying no, and those are different problems: the first is
		// usually something outside that is down, and the second is usually
		// something wrong with the work being submitted.
		//
		// A label on the dead counter would answer the same question. Two
		// counters are used because a label makes every existing dashboard
		// panel and alert that reads quorra_jobs_dead_total start summing
		// over a dimension it does not know about.
		refused: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "quorra_jobs_refused_total",
			Help: "Jobs a handler refused, which are a part of quorra_jobs_dead_total.",
		}),
		reclaimed: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "quorra_leases_reclaimed_total",
			Help: "Leases that ran out before a worker reported, and were taken back.",
		}),

		// Cancellations and revivals are counted apart from everything else,
		// because both are a person acting. A rise in either says something
		// about the operators rather than about the work, and mixing them
		// into the job counters would hide that.
		cancelled: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "quorra_jobs_cancelled_total",
			Help: "Jobs stopped by a person.",
		}),
		revived: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "quorra_jobs_revived_total",
			Help: "Jobs put back in the queue by a person.",
		}),

		removed: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "quorra_jobs_removed_total",
			Help: "Finished jobs removed by the retention sweep.",
		}, []string{"status"}),

		queueLength: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "quorra_queue_length",
			Help: "Jobs in each queue, by status. Refreshed on a timer, so it lags by up to QUORRA_STATS_EVERY.",
		}, []string{"queue", "status"}),

		// Lifetime is measured from the moment the job was accepted to the
		// moment it stopped, so it includes the waiting, the retries and the
		// backoff. That is the number somebody submitting a job actually
		// waits, which is why it is the one published.
		lifetime: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "quorra_job_lifetime_seconds",
			Help:    "Time from a job being accepted to it reaching a final state.",
			Buckets: []float64{0.05, 0.1, 0.5, 1, 5, 15, 60, 300, 1800, 7200},
		}, []string{"queue", "status"}),

		httpLatency: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "quorra_http_request_duration_seconds",
			Help:    "Time taken to answer an HTTP request.",
			Buckets: prometheus.DefBuckets,
		}, []string{"route", "method", "code"}),
	}

	registry.MustRegister(
		m.created, m.leased, m.succeeded, m.retried, m.dead, m.refused, m.reclaimed,
		m.cancelled, m.revived, m.removed,
		m.queueLength, m.lifetime, m.httpLatency,
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)

	return m
}

// Handler serves the metrics page.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{})
}

// Registry gives the registry, for a test that wants to read the numbers.
func (m *Metrics) Registry() *prometheus.Registry { return m.registry }

// JobCreated records an accepted job.
func (m *Metrics) JobCreated() { m.created.Inc() }

// JobsLeased records jobs handed to a worker.
func (m *Metrics) JobsLeased(n int) {
	if n > 0 {
		m.leased.Add(float64(n))
	}
}

// LeasesReclaimed records leases taken back after they ran out.
func (m *Metrics) LeasesReclaimed(n int) {
	if n > 0 {
		m.reclaimed.Add(float64(n))
	}
}

// JobFinished records where a job ended up, and what put it there.
//
// It takes the job after the store has written it, so the status it counts is
// the status the table holds. Counting the intention instead is how the old
// code raised its failure counter for a job that had in fact been buried.
//
// The outcome is read only to divide one of those statuses, never to decide
// it. A refusal is counted when the job is dead, and a refusal that somehow
// left the job somewhere else is not counted at all, because the counter
// answers "what share of the dead letter queue is refusals" and an answer
// that could exceed the whole is not an answer.
func (m *Metrics) JobFinished(job *store.Job, outcome jobs.Outcome, now time.Time) {
	if job == nil {
		return
	}

	switch job.Status {
	case jobs.Succeeded:
		m.succeeded.Inc()
	case jobs.Dead:
		m.dead.Inc()
		if outcome == jobs.OutcomeRefused {
			m.refused.Inc()
		}
	case jobs.Pending:
		// Back in the queue, so the job has not finished. This is a retry.
		m.retried.Inc()
		return
	default:
		return
	}

	m.lifetime.WithLabelValues(job.Queue, job.Status.String()).
		Observe(now.Sub(job.CreatedAt).Seconds())
}

// JobCancelled records a job stopped by a person.
func (m *Metrics) JobCancelled() { m.cancelled.Inc() }

// JobRevived records a job put back in the queue by a person.
func (m *Metrics) JobRevived() { m.revived.Inc() }

// JobsRemoved records jobs taken out by the retention sweep.
//
// Labelled by status, because removing succeeded jobs is housekeeping and
// removing dead ones is throwing away the record of a failure. One number for
// both would hide the second inside the first.
func (m *Metrics) JobsRemoved(status jobs.Status, n int) {
	if n > 0 {
		m.removed.WithLabelValues(status.String()).Add(float64(n))
	}
}

// HTTPRequest records one answered request.
func (m *Metrics) HTTPRequest(route, method, code string, took time.Duration) {
	m.httpLatency.WithLabelValues(route, method, code).Observe(took.Seconds())
}

// SetQueueLengths replaces the queue gauge.
//
// Reset first, and then set. Without the reset, a queue that empties keeps
// its last value for ever, because nothing sends a zero for a row the
// database no longer returns. An alert on a queue depth would then stay lit
// after the queue had drained.
func (m *Metrics) SetQueueLengths(stats []store.QueueStat) {
	m.queueLength.Reset()
	for _, stat := range stats {
		m.queueLength.WithLabelValues(stat.Queue, stat.Status.String()).Set(float64(stat.Count))
	}
}
