package tests

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/goquorra/goquorra/internal/store"
	_ "github.com/lib/pq"
)

func setupTestDB(t *testing.T) *sql.DB {
	// This assumes DATABASE_URL is set in environment
	dbURL := "postgres://quorra:quorra@localhost:5432/quorra?sslmode=disable"
	db, err := sql.Open("postgres", dbURL)
	if err != nil {
		t.Skipf("Skipping test - cannot connect to database: %v", err)
	}

	if err := db.Ping(); err != nil {
		t.Skipf("Skipping test - database not available: %v", err)
	}

	// Clean up existing test data
	db.Exec("DELETE FROM jobs WHERE type LIKE 'test_%'")

	return db
}

func TestCreateJob(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	s := store.NewPostgresStore(db)
	ctx := context.Background()

	req := &store.CreateJobRequest{
		Type:       "test_email",
		Payload:    map[string]interface{}{"to": "test@example.com"},
		Queue:      "default",
		Priority:   10,
		MaxRetries: 3,
	}

	job, err := s.CreateJob(ctx, req)
	if err != nil {
		t.Fatalf("Failed to create job: %v", err)
	}

	if job.ID == "" {
		t.Error("Job ID is empty")
	}

	if job.Status != store.StatusPending {
		t.Errorf("Expected status pending, got %s", job.Status)
	}

	if job.Type != req.Type {
		t.Errorf("Expected type %s, got %s", req.Type, job.Type)
	}

	// Fetch and verify
	fetchedJob, err := s.GetJob(ctx, job.ID)
	if err != nil {
		t.Fatalf("Failed to fetch job: %v", err)
	}

	if fetchedJob.ID != job.ID {
		t.Errorf("Job ID mismatch: expected %s, got %s", job.ID, fetchedJob.ID)
	}
}

func TestDelayedJob(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	s := store.NewPostgresStore(db)
	ctx := context.Background()

	req := &store.CreateJobRequest{
		Type:         "test_delayed",
		Payload:      map[string]interface{}{"data": "test"},
		Queue:        "default",
		DelaySeconds: 60,
		MaxRetries:   3,
	}

	job, err := s.CreateJob(ctx, req)
	if err != nil {
		t.Fatalf("Failed to create job: %v", err)
	}

	// Run time should be in the future
	if !job.RunAt.After(time.Now()) {
		t.Error("Delayed job run_at should be in the future")
	}

	// Should not be returned by lease (not ready yet)
	jobs, err := s.LeaseJobs(ctx, "default", "test-worker", 10, 30*time.Second)
	if err != nil {
		t.Fatalf("Failed to lease jobs: %v", err)
	}

	// Verify our delayed job is not in the leased jobs
	for _, leasedJob := range jobs {
		if leasedJob.ID == job.ID {
			t.Error("Delayed job should not be leased before run_at time")
		}
	}
}

func TestLeaseJobs(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	s := store.NewPostgresStore(db)
	ctx := context.Background()

	// Create multiple jobs
	for i := 0; i < 5; i++ {
		req := &store.CreateJobRequest{
			Type:       "test_lease",
			Payload:    map[string]interface{}{"index": i},
			Queue:      "default",
			Priority:   i,
			MaxRetries: 3,
		}
		_, err := s.CreateJob(ctx, req)
		if err != nil {
			t.Fatalf("Failed to create job: %v", err)
		}
	}

	// Lease jobs
	jobs, err := s.LeaseJobs(ctx, "default", "worker-1", 3, 30*time.Second)
	if err != nil {
		t.Fatalf("Failed to lease jobs: %v", err)
	}

	if len(jobs) == 0 {
		t.Error("Expected to lease at least some jobs")
	}

	// Verify leased status
	for _, job := range jobs {
		if job.Status != store.StatusLeased {
			t.Errorf("Expected leased status, got %s", job.Status)
		}
		if job.LeaseID == "" {
			t.Error("Lease ID should not be empty")
		}
		if job.LeasedBy != "worker-1" {
			t.Errorf("Expected leased_by worker-1, got %s", job.LeasedBy)
		}
	}

	// Try to lease same jobs again - should get different jobs or none
	jobs2, err := s.LeaseJobs(ctx, "default", "worker-2", 3, 30*time.Second)
	if err != nil {
		t.Fatalf("Failed to lease jobs: %v", err)
	}

	// Verify no overlap
	for _, job1 := range jobs {
		for _, job2 := range jobs2 {
			if job1.ID == job2.ID {
				t.Error("Same job leased to two workers")
			}
		}
	}
}

func TestAckJobSuccess(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	s := store.NewPostgresStore(db)
	ctx := context.Background()

	// Create and lease a job
	req := &store.CreateJobRequest{
		Type:       "test_ack",
		Payload:    map[string]interface{}{"test": "data"},
		Queue:      "default",
		MaxRetries: 3,
	}

	job, err := s.CreateJob(ctx, req)
	if err != nil {
		t.Fatalf("Failed to create job: %v", err)
	}

	jobs, err := s.LeaseJobs(ctx, "default", "worker-1", 1, 30*time.Second)
	if err != nil || len(jobs) == 0 {
		t.Fatalf("Failed to lease job: %v", err)
	}

	leasedJob := jobs[0]

	// Ack success
	err = s.AckJob(ctx, leasedJob.ID, leasedJob.LeaseID, true, "")
	if err != nil {
		t.Fatalf("Failed to ack job: %v", err)
	}

	// Verify status
	updatedJob, err := s.GetJob(ctx, job.ID)
	if err != nil {
		t.Fatalf("Failed to get job: %v", err)
	}

	if updatedJob.Status != store.StatusSucceeded {
		t.Errorf("Expected succeeded status, got %s", updatedJob.Status)
	}
}

func TestAckJobFailureWithRetry(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	s := store.NewPostgresStore(db)
	ctx := context.Background()

	// Create and lease a job
	req := &store.CreateJobRequest{
		Type:       "test_retry",
		Payload:    map[string]interface{}{"test": "data"},
		Queue:      "default",
		MaxRetries: 3,
	}

	job, err := s.CreateJob(ctx, req)
	if err != nil {
		t.Fatalf("Failed to create job: %v", err)
	}

	jobs, err := s.LeaseJobs(ctx, "default", "worker-1", 1, 30*time.Second)
	if err != nil || len(jobs) == 0 {
		t.Fatalf("Failed to lease job: %v", err)
	}

	leasedJob := jobs[0]

	// Ack failure
	err = s.AckJob(ctx, leasedJob.ID, leasedJob.LeaseID, false, "simulated error")
	if err != nil {
		t.Fatalf("Failed to nack job: %v", err)
	}

	// Verify status is back to pending for retry
	updatedJob, err := s.GetJob(ctx, job.ID)
	if err != nil {
		t.Fatalf("Failed to get job: %v", err)
	}

	if updatedJob.Status != store.StatusPending {
		t.Errorf("Expected pending status for retry, got %s", updatedJob.Status)
	}

	if updatedJob.Attempts != 1 {
		t.Errorf("Expected attempts=1, got %d", updatedJob.Attempts)
	}

	if updatedJob.LastError != "simulated error" {
		t.Errorf("Expected error message to be stored")
	}
}

func TestJobMovesToDLQ(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	s := store.NewPostgresStore(db)
	ctx := context.Background()

	// Create a job with max_retries=1
	req := &store.CreateJobRequest{
		Type:       "test_dlq",
		Payload:    map[string]interface{}{"test": "data"},
		Queue:      "default",
		MaxRetries: 1,
	}

	job, err := s.CreateJob(ctx, req)
	if err != nil {
		t.Fatalf("Failed to create job: %v", err)
	}

	// Fail it once (reaches max retries)
	jobs, err := s.LeaseJobs(ctx, "default", "worker-1", 1, 30*time.Second)
	if err != nil || len(jobs) == 0 {
		t.Fatalf("Failed to lease job: %v", err)
	}

	err = s.AckJob(ctx, jobs[0].ID, jobs[0].LeaseID, false, "fatal error")
	if err != nil {
		t.Fatalf("Failed to nack job: %v", err)
	}

	// Verify it's in dead status
	updatedJob, err := s.GetJob(ctx, job.ID)
	if err != nil {
		t.Fatalf("Failed to get job: %v", err)
	}

	if updatedJob.Status != store.StatusDead {
		t.Errorf("Expected dead status, got %s", updatedJob.Status)
	}
}
