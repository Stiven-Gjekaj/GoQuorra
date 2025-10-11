package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// JobStatus represents the current state of a job
type JobStatus string

const (
	StatusPending    JobStatus = "pending"
	StatusLeased     JobStatus = "leased"
	StatusProcessing JobStatus = "processing"
	StatusSucceeded  JobStatus = "succeeded"
	StatusFailed     JobStatus = "failed"
	StatusDead       JobStatus = "dead"
)

// Job represents a job in the queue
type Job struct {
	ID          string                 `json:"id"`
	Type        string                 `json:"type"`
	Payload     map[string]interface{} `json:"payload"`
	Queue       string                 `json:"queue"`
	Priority    int                    `json:"priority"`
	Status      JobStatus              `json:"status"`
	Attempts    int                    `json:"attempts"`
	MaxRetries  int                    `json:"max_retries"`
	LastError   string                 `json:"last_error,omitempty"`
	LeaseID     string                 `json:"lease_id,omitempty"`
	LeasedAt    *time.Time             `json:"leased_at,omitempty"`
	LeasedBy    string                 `json:"leased_by,omitempty"`
	RunAt       time.Time              `json:"run_at"`
	CreatedAt   time.Time              `json:"created_at"`
	UpdatedAt   time.Time              `json:"updated_at"`
}

// CreateJobRequest represents a request to create a new job
type CreateJobRequest struct {
	Type         string                 `json:"type"`
	Payload      map[string]interface{} `json:"payload"`
	Queue        string                 `json:"queue"`
	Priority     int                    `json:"priority"`
	DelaySeconds int                    `json:"delay_seconds"`
	MaxRetries   int                    `json:"max_retries"`
}

// QueueStats holds statistics for a queue
type QueueStats struct {
	Queue   string `json:"queue"`
	Status  string `json:"status"`
	Count   int    `json:"count"`
}

// Store defines the interface for job persistence
type Store interface {
	CreateJob(ctx context.Context, req *CreateJobRequest) (*Job, error)
	GetJob(ctx context.Context, id string) (*Job, error)
	UpdateJobStatus(ctx context.Context, id string, status JobStatus, lastError string) error
	LeaseJobs(ctx context.Context, queue string, workerID string, maxJobs int, leaseTTL time.Duration) ([]*Job, error)
	AckJob(ctx context.Context, jobID, leaseID string, success bool, errorMsg string) error
	GetPendingDelayedJobs(ctx context.Context, limit int) ([]*Job, error)
	MoveToReady(ctx context.Context, jobID string) error
	GetQueueStats(ctx context.Context) ([]QueueStats, error)
	GetRecentJobs(ctx context.Context, limit int) ([]*Job, error)
}

// PostgresStore implements Store using PostgreSQL
type PostgresStore struct {
	db *sql.DB
}

// NewPostgresStore creates a new PostgresStore
func NewPostgresStore(db *sql.DB) *PostgresStore {
	return &PostgresStore{db: db}
}

// CreateJob creates a new job in the database
func (s *PostgresStore) CreateJob(ctx context.Context, req *CreateJobRequest) (*Job, error) {
	id := uuid.New().String()
	now := time.Now()
	runAt := now
	if req.DelaySeconds > 0 {
		runAt = now.Add(time.Duration(req.DelaySeconds) * time.Second)
	}

	if req.Queue == "" {
		req.Queue = "default"
	}
	if req.MaxRetries == 0 {
		req.MaxRetries = 3
	}

	payloadJSON, err := json.Marshal(req.Payload)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal payload: %w", err)
	}

	query := `
		INSERT INTO jobs (id, type, payload, queue, priority, status, max_retries, run_at, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		RETURNING id, type, payload, queue, priority, status, attempts, max_retries, run_at, created_at, updated_at
	`

	var job Job
	var payloadStr string

	err = s.db.QueryRowContext(ctx, query,
		id, req.Type, payloadJSON, req.Queue, req.Priority, StatusPending, req.MaxRetries, runAt, now, now,
	).Scan(&job.ID, &job.Type, &payloadStr, &job.Queue, &job.Priority, &job.Status,
		&job.Attempts, &job.MaxRetries, &job.RunAt, &job.CreatedAt, &job.UpdatedAt)

	if err != nil {
		return nil, fmt.Errorf("failed to create job: %w", err)
	}

	if err := json.Unmarshal([]byte(payloadStr), &job.Payload); err != nil {
		return nil, fmt.Errorf("failed to unmarshal payload: %w", err)
	}

	return &job, nil
}

// GetJob retrieves a job by ID
func (s *PostgresStore) GetJob(ctx context.Context, id string) (*Job, error) {
	query := `
		SELECT id, type, payload, queue, priority, status, attempts, max_retries,
		       last_error, lease_id, leased_at, leased_by, run_at, created_at, updated_at
		FROM jobs
		WHERE id = $1
	`

	var job Job
	var payloadStr string
	var lastError, leaseID, leasedBy sql.NullString
	var leasedAt sql.NullTime

	err := s.db.QueryRowContext(ctx, query, id).Scan(
		&job.ID, &job.Type, &payloadStr, &job.Queue, &job.Priority, &job.Status,
		&job.Attempts, &job.MaxRetries, &lastError, &leaseID, &leasedAt, &leasedBy,
		&job.RunAt, &job.CreatedAt, &job.UpdatedAt,
	)

	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("job not found")
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get job: %w", err)
	}

	if err := json.Unmarshal([]byte(payloadStr), &job.Payload); err != nil {
		return nil, fmt.Errorf("failed to unmarshal payload: %w", err)
	}

	if lastError.Valid {
		job.LastError = lastError.String
	}
	if leaseID.Valid {
		job.LeaseID = leaseID.String
	}
	if leasedBy.Valid {
		job.LeasedBy = leasedBy.String
	}
	if leasedAt.Valid {
		job.LeasedAt = &leasedAt.Time
	}

	return &job, nil
}

// UpdateJobStatus updates the status of a job
func (s *PostgresStore) UpdateJobStatus(ctx context.Context, id string, status JobStatus, lastError string) error {
	query := `
		UPDATE jobs
		SET status = $1, last_error = $2, updated_at = NOW()
		WHERE id = $3
	`
	_, err := s.db.ExecContext(ctx, query, status, lastError, id)
	return err
}

// LeaseJobs atomically leases available jobs for a worker
func (s *PostgresStore) LeaseJobs(ctx context.Context, queue string, workerID string, maxJobs int, leaseTTL time.Duration) ([]*Job, error) {
	leaseID := uuid.New().String()
	now := time.Now()
	leaseUntil := now.Add(leaseTTL)

	// Use SELECT FOR UPDATE SKIP LOCKED for atomic job leasing
	query := `
		UPDATE jobs
		SET status = $1,
		    lease_id = $2,
		    leased_at = $3,
		    leased_by = $4,
		    updated_at = $3
		WHERE id IN (
			SELECT id FROM jobs
			WHERE queue = $5
			  AND status = $6
			  AND run_at <= $7
			ORDER BY priority DESC, run_at ASC
			LIMIT $8
			FOR UPDATE SKIP LOCKED
		)
		RETURNING id, type, payload, queue, priority, status, attempts, max_retries,
		          lease_id, leased_at, leased_by, run_at, created_at, updated_at
	`

	rows, err := s.db.QueryContext(ctx, query,
		StatusLeased, leaseID, now, workerID, queue, StatusPending, now, maxJobs,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to lease jobs: %w", err)
	}
	defer rows.Close()

	var jobs []*Job
	for rows.Next() {
		var job Job
		var payloadStr string
		var leaseID, leasedBy sql.NullString
		var leasedAt sql.NullTime

		err := rows.Scan(
			&job.ID, &job.Type, &payloadStr, &job.Queue, &job.Priority, &job.Status,
			&job.Attempts, &job.MaxRetries, &leaseID, &leasedAt, &leasedBy,
			&job.RunAt, &job.CreatedAt, &job.UpdatedAt,
		)
		if err != nil {
			return nil, fmt.Errorf("failed to scan job: %w", err)
		}

		if err := json.Unmarshal([]byte(payloadStr), &job.Payload); err != nil {
			return nil, fmt.Errorf("failed to unmarshal payload: %w", err)
		}

		if leaseID.Valid {
			job.LeaseID = leaseID.String
		}
		if leasedBy.Valid {
			job.LeasedBy = leasedBy.String
		}
		if leasedAt.Valid {
			job.LeasedAt = &leasedAt.Time
		}

		jobs = append(jobs, &job)
	}

	return jobs, rows.Err()
}

// AckJob acknowledges job completion (success or failure)
func (s *PostgresStore) AckJob(ctx context.Context, jobID, leaseID string, success bool, errorMsg string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer tx.Rollback()

	// Verify lease
	var currentLeaseID sql.NullString
	var attempts, maxRetries int
	err = tx.QueryRowContext(ctx, "SELECT lease_id, attempts, max_retries FROM jobs WHERE id = $1 FOR UPDATE", jobID).
		Scan(&currentLeaseID, &attempts, &maxRetries)
	if err != nil {
		return fmt.Errorf("failed to get job: %w", err)
	}

	if !currentLeaseID.Valid || currentLeaseID.String != leaseID {
		return fmt.Errorf("invalid lease ID")
	}

	if success {
		// Mark as succeeded
		_, err = tx.ExecContext(ctx, `
			UPDATE jobs
			SET status = $1, lease_id = NULL, leased_at = NULL, leased_by = NULL, updated_at = NOW()
			WHERE id = $2
		`, StatusSucceeded, jobID)
	} else {
		// Increment attempts and decide retry or DLQ
		attempts++
		var newStatus JobStatus
		var runAt time.Time

		if attempts >= maxRetries {
			newStatus = StatusDead
			runAt = time.Now()
		} else {
			newStatus = StatusPending
			// Exponential backoff: 2^attempts seconds
			backoffSeconds := 1 << uint(attempts)
			if backoffSeconds > 3600 {
				backoffSeconds = 3600 // Cap at 1 hour
			}
			runAt = time.Now().Add(time.Duration(backoffSeconds) * time.Second)
		}

		_, err = tx.ExecContext(ctx, `
			UPDATE jobs
			SET status = $1, attempts = $2, last_error = $3, run_at = $4,
			    lease_id = NULL, leased_at = NULL, leased_by = NULL, updated_at = NOW()
			WHERE id = $5
		`, newStatus, attempts, errorMsg, runAt, jobID)
	}

	if err != nil {
		return fmt.Errorf("failed to update job: %w", err)
	}

	return tx.Commit()
}

// GetPendingDelayedJobs retrieves jobs that are scheduled but not yet ready
func (s *PostgresStore) GetPendingDelayedJobs(ctx context.Context, limit int) ([]*Job, error) {
	query := `
		SELECT id, type, payload, queue, priority, status, attempts, max_retries, run_at, created_at, updated_at
		FROM jobs
		WHERE status = $1 AND run_at <= $2
		ORDER BY run_at ASC
		LIMIT $3
	`

	rows, err := s.db.QueryContext(ctx, query, StatusPending, time.Now(), limit)
	if err != nil {
		return nil, fmt.Errorf("failed to query delayed jobs: %w", err)
	}
	defer rows.Close()

	var jobs []*Job
	for rows.Next() {
		var job Job
		var payloadStr string

		err := rows.Scan(&job.ID, &job.Type, &payloadStr, &job.Queue, &job.Priority,
			&job.Status, &job.Attempts, &job.MaxRetries, &job.RunAt, &job.CreatedAt, &job.UpdatedAt)
		if err != nil {
			return nil, fmt.Errorf("failed to scan job: %w", err)
		}

		if err := json.Unmarshal([]byte(payloadStr), &job.Payload); err != nil {
			return nil, fmt.Errorf("failed to unmarshal payload: %w", err)
		}

		jobs = append(jobs, &job)
	}

	return jobs, rows.Err()
}

// MoveToReady marks a delayed job as ready to be processed
func (s *PostgresStore) MoveToReady(ctx context.Context, jobID string) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE jobs
		SET status = $1, updated_at = NOW()
		WHERE id = $2 AND status = $3
	`, StatusPending, jobID, StatusPending)
	return err
}

// GetQueueStats returns statistics for all queues
func (s *PostgresStore) GetQueueStats(ctx context.Context) ([]QueueStats, error) {
	query := `SELECT queue, status, count FROM queue_stats ORDER BY queue, status`

	rows, err := s.db.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("failed to query queue stats: %w", err)
	}
	defer rows.Close()

	var stats []QueueStats
	for rows.Next() {
		var stat QueueStats
		if err := rows.Scan(&stat.Queue, &stat.Status, &stat.Count); err != nil {
			return nil, fmt.Errorf("failed to scan stat: %w", err)
		}
		stats = append(stats, stat)
	}

	return stats, rows.Err()
}

// GetRecentJobs returns the most recently created jobs
func (s *PostgresStore) GetRecentJobs(ctx context.Context, limit int) ([]*Job, error) {
	query := `
		SELECT id, type, payload, queue, priority, status, attempts, max_retries,
		       last_error, run_at, created_at, updated_at
		FROM jobs
		ORDER BY created_at DESC
		LIMIT $1
	`

	rows, err := s.db.QueryContext(ctx, query, limit)
	if err != nil {
		return nil, fmt.Errorf("failed to query recent jobs: %w", err)
	}
	defer rows.Close()

	var jobs []*Job
	for rows.Next() {
		var job Job
		var payloadStr string
		var lastError sql.NullString

		err := rows.Scan(&job.ID, &job.Type, &payloadStr, &job.Queue, &job.Priority,
			&job.Status, &job.Attempts, &job.MaxRetries, &lastError,
			&job.RunAt, &job.CreatedAt, &job.UpdatedAt)
		if err != nil {
			return nil, fmt.Errorf("failed to scan job: %w", err)
		}

		if err := json.Unmarshal([]byte(payloadStr), &job.Payload); err != nil {
			return nil, fmt.Errorf("failed to unmarshal payload: %w", err)
		}

		if lastError.Valid {
			job.LastError = lastError.String
		}

		jobs = append(jobs, &job)
	}

	return jobs, rows.Err()
}
