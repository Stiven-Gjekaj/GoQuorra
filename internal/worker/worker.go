package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math/rand"
	"time"

	pb "github.com/goquorra/goquorra/internal/grpc"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// Worker represents a job worker
type Worker struct {
	id         string
	serverAddr string
	queues     []string
	maxJobs    int
	leaseTTL   time.Duration
	logger     *log.Logger
	client     pb.WorkerServiceClient
	conn       *grpc.ClientConn
}

// Config holds worker configuration
type Config struct {
	ID         string
	ServerAddr string
	Queues     []string
	MaxJobs    int
	LeaseTTL   time.Duration
}

// New creates a new worker
func New(cfg *Config, logger *log.Logger) *Worker {
	if len(cfg.Queues) == 0 {
		cfg.Queues = []string{"default"}
	}
	if cfg.MaxJobs == 0 {
		cfg.MaxJobs = 5
	}
	if cfg.LeaseTTL == 0 {
		cfg.LeaseTTL = 30 * time.Second
	}

	return &Worker{
		id:         cfg.ID,
		serverAddr: cfg.ServerAddr,
		queues:     cfg.Queues,
		maxJobs:    cfg.MaxJobs,
		leaseTTL:   cfg.LeaseTTL,
		logger:     logger,
	}
}

// Start connects to the server and starts processing jobs
func (w *Worker) Start(ctx context.Context) error {
	// Connect to gRPC server
	conn, err := grpc.Dial(w.serverAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return fmt.Errorf("failed to connect to server: %w", err)
	}
	w.conn = conn
	w.client = pb.NewWorkerServiceClient(conn)

	w.logger.Printf("Worker %s connected to %s", w.id, w.serverAddr)

	// Process jobs from each queue
	for _, queue := range w.queues {
		go w.processQueue(ctx, queue)
	}

	// Wait for context cancellation
	<-ctx.Done()
	w.logger.Printf("Worker %s shutting down", w.id)
	return w.conn.Close()
}

// processQueue continuously processes jobs from a specific queue
func (w *Worker) processQueue(ctx context.Context, queue string) {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			w.leaseAndProcessJobs(ctx, queue)
		}
	}
}

// leaseAndProcessJobs leases jobs from the server and processes them
func (w *Worker) leaseAndProcessJobs(ctx context.Context, queue string) {
	req := &pb.LeaseRequest{
		WorkerId:        w.id,
		Queue:           queue,
		MaxJobs:         int32(w.maxJobs),
		LeaseTtlSeconds: int32(w.leaseTTL.Seconds()),
	}

	stream, err := w.client.LeaseJobs(ctx, req)
	if err != nil {
		w.logger.Printf("Failed to lease jobs from queue %s: %v", queue, err)
		return
	}

	jobCount := 0
	for {
		job, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			w.logger.Printf("Error receiving job: %v", err)
			break
		}

		jobCount++
		w.logger.Printf("Leased job %s (type=%s) from queue %s", job.Id, job.Type, queue)

		// Process job in goroutine
		go w.processJob(context.Background(), job)
	}

	if jobCount > 0 {
		w.logger.Printf("Leased %d jobs from queue %s", jobCount, queue)
	}
}

// processJob processes a single job
func (w *Worker) processJob(ctx context.Context, job *pb.Job) {
	w.logger.Printf("Processing job %s (type=%s, attempt=%d/%d)", job.Id, job.Type, job.Attempts+1, job.MaxRetries)

	// Parse payload
	var payload map[string]interface{}
	if err := json.Unmarshal(job.Payload, &payload); err != nil {
		w.logger.Printf("Failed to parse job payload: %v", err)
		w.nackJob(ctx, job, fmt.Sprintf("Invalid payload: %v", err))
		return
	}

	// Simulate work
	success := w.executeJob(job.Type, payload)

	// Ack or nack
	if success {
		w.ackJob(ctx, job)
	} else {
		w.nackJob(ctx, job, "Job processing failed")
	}
}

// executeJob simulates job execution
func (w *Worker) executeJob(jobType string, payload map[string]interface{}) bool {
	// Simulate random processing time
	processingTime := time.Duration(500+rand.Intn(2000)) * time.Millisecond
	time.Sleep(processingTime)

	w.logger.Printf("Job type=%s, payload=%v, took=%v", jobType, payload, processingTime)

	// Simulate 10% failure rate
	return rand.Float64() > 0.1
}

// ackJob acknowledges successful job completion
func (w *Worker) ackJob(ctx context.Context, job *pb.Job) {
	ack := &pb.JobAck{
		JobId:    job.Id,
		WorkerId: w.id,
		LeaseId:  job.LeaseId,
		Success:  true,
	}

	resp, err := w.client.AckJob(ctx, ack)
	if err != nil {
		w.logger.Printf("Failed to ack job %s: %v", job.Id, err)
		return
	}

	if resp.Acknowledged {
		w.logger.Printf("Job %s completed successfully", job.Id)
	}
}

// nackJob signals job failure
func (w *Worker) nackJob(ctx context.Context, job *pb.Job, errorMsg string) {
	ack := &pb.JobAck{
		JobId:        job.Id,
		WorkerId:     w.id,
		LeaseId:      job.LeaseId,
		Success:      false,
		ErrorMessage: errorMsg,
	}

	resp, err := w.client.NackJob(ctx, ack)
	if err != nil {
		w.logger.Printf("Failed to nack job %s: %v", job.Id, err)
		return
	}

	if resp.Acknowledged {
		w.logger.Printf("Job %s failed: %s", job.Id, errorMsg)
	}
}
