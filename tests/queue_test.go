package tests

import (
	"context"
	"log"
	"os"
	"testing"

	"github.com/goquorra/goquorra/internal/queue"
	"github.com/goquorra/goquorra/internal/store"
)

func TestQueueManager(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	s := store.NewPostgresStore(db)
	logger := log.New(os.Stdout, "[test] ", log.LstdFlags)
	qm := queue.NewManager(s, nil, logger)

	ctx := context.Background()

	// Test enqueue
	req := &store.CreateJobRequest{
		Type:       "test_queue_manager",
		Payload:    map[string]interface{}{"test": "data"},
		Queue:      "test",
		Priority:   5,
		MaxRetries: 3,
	}

	job, err := qm.EnqueueJob(ctx, req)
	if err != nil {
		t.Fatalf("Failed to enqueue job: %v", err)
	}

	if job.ID == "" {
		t.Error("Job ID should not be empty")
	}

	// Test get job
	fetchedJob, err := qm.GetJob(ctx, job.ID)
	if err != nil {
		t.Fatalf("Failed to get job: %v", err)
	}

	if fetchedJob.ID != job.ID {
		t.Error("Job ID mismatch")
	}
}
