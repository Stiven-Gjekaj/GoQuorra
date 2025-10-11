package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Collector holds all Prometheus metrics
type Collector struct {
	JobsCreated   prometheus.Counter
	JobsProcessed prometheus.Counter
	JobsFailed    prometheus.Counter
	JobsDead      prometheus.Counter
	JobsLeased    prometheus.Counter
	QueueLength   *prometheus.GaugeVec
}

// NewCollector creates a new metrics collector
func NewCollector() *Collector {
	return &Collector{
		JobsCreated: promauto.NewCounter(prometheus.CounterOpts{
			Name: "quorra_jobs_created_total",
			Help: "Total number of jobs created",
		}),
		JobsProcessed: promauto.NewCounter(prometheus.CounterOpts{
			Name: "quorra_jobs_processed_total",
			Help: "Total number of jobs processed successfully",
		}),
		JobsFailed: promauto.NewCounter(prometheus.CounterOpts{
			Name: "quorra_jobs_failed_total",
			Help: "Total number of jobs that failed",
		}),
		JobsDead: promauto.NewCounter(prometheus.CounterOpts{
			Name: "quorra_jobs_dead_total",
			Help: "Total number of jobs moved to dead letter queue",
		}),
		JobsLeased: promauto.NewCounter(prometheus.CounterOpts{
			Name: "quorra_jobs_leased_total",
			Help: "Total number of jobs leased to workers",
		}),
		QueueLength: promauto.NewGaugeVec(prometheus.GaugeOpts{
			Name: "quorra_job_queue_length",
			Help: "Current length of job queues by queue and status",
		}, []string{"queue", "status"}),
	}
}

// RecordJobProcessed increments the processed counter
func (c *Collector) RecordJobProcessed() {
	c.JobsProcessed.Inc()
}

// RecordJobFailed increments the failed counter
func (c *Collector) RecordJobFailed() {
	c.JobsFailed.Inc()
}

// RecordJobDead increments the dead counter
func (c *Collector) RecordJobDead() {
	c.JobsDead.Inc()
}

// RecordJobLeased increments the leased counter
func (c *Collector) RecordJobLeased(count int) {
	c.JobsLeased.Add(float64(count))
}

// UpdateQueueLength updates the queue length gauge
func (c *Collector) UpdateQueueLength(queue, status string, length float64) {
	c.QueueLength.WithLabelValues(queue, status).Set(length)
}
