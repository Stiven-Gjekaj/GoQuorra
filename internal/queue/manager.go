package queue

import (
	"context"
	"log"
	"time"

	"github.com/goquorra/goquorra/internal/store"
	"github.com/redis/go-redis/v9"
)

// Manager handles job queue operations and scheduling
type Manager struct {
	store       store.Store
	redisClient *redis.Client
	logger      *log.Logger
}

// NewManager creates a new queue manager
func NewManager(store store.Store, redisClient *redis.Client, logger *log.Logger) *Manager {
	return &Manager{
		store:       store,
		redisClient: redisClient,
		logger:      logger,
	}
}

// EnqueueJob creates a new job
func (m *Manager) EnqueueJob(ctx context.Context, req *store.CreateJobRequest) (*store.Job, error) {
	job, err := m.store.CreateJob(ctx, req)
	if err != nil {
		return nil, err
	}

	m.logger.Printf("Enqueued job %s (type=%s, queue=%s, priority=%d)", job.ID, job.Type, job.Queue, job.Priority)

	// If Redis is available, publish notification
	if m.redisClient != nil {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			m.redisClient.Publish(ctx, "quorra:jobs:"+job.Queue, job.ID)
		}()
	}

	return job, nil
}

// GetJob retrieves a job by ID
func (m *Manager) GetJob(ctx context.Context, id string) (*store.Job, error) {
	return m.store.GetJob(ctx, id)
}

// LeaseJobs leases jobs for a worker
func (m *Manager) LeaseJobs(ctx context.Context, queue string, workerID string, maxJobs int, leaseTTL time.Duration) ([]*store.Job, error) {
	jobs, err := m.store.LeaseJobs(ctx, queue, workerID, maxJobs, leaseTTL)
	if err != nil {
		return nil, err
	}

	if len(jobs) > 0 {
		m.logger.Printf("Leased %d jobs to worker %s from queue %s", len(jobs), workerID, queue)
	}

	return jobs, nil
}

// AckJob acknowledges job completion
func (m *Manager) AckJob(ctx context.Context, jobID, leaseID string, success bool, errorMsg string) error {
	err := m.store.AckJob(ctx, jobID, leaseID, success, errorMsg)
	if err != nil {
		return err
	}

	if success {
		m.logger.Printf("Job %s completed successfully", jobID)
	} else {
		m.logger.Printf("Job %s failed: %s", jobID, errorMsg)
	}

	return nil
}

// GetQueueStats returns statistics for all queues
func (m *Manager) GetQueueStats(ctx context.Context) ([]store.QueueStats, error) {
	return m.store.GetQueueStats(ctx)
}

// GetRecentJobs returns recent jobs
func (m *Manager) GetRecentJobs(ctx context.Context, limit int) ([]*store.Job, error) {
	return m.store.GetRecentJobs(ctx, limit)
}

// StartScheduler runs a background scheduler that moves delayed jobs to ready state
func (m *Manager) StartScheduler(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	m.logger.Println("Scheduler started")

	for {
		select {
		case <-ctx.Done():
			m.logger.Println("Scheduler stopped")
			return
		case <-ticker.C:
			m.processDelayedJobs(ctx)
		}
	}
}

func (m *Manager) processDelayedJobs(ctx context.Context) {
	jobs, err := m.store.GetPendingDelayedJobs(ctx, 100)
	if err != nil {
		m.logger.Printf("Error fetching delayed jobs: %v", err)
		return
	}

	if len(jobs) > 0 {
		m.logger.Printf("Processing %d delayed jobs", len(jobs))
		for _, job := range jobs {
			// Jobs are already pending and past their run_at time, so they're ready to be leased
			// No action needed here - the query already filters for ready jobs
		}
	}
}
