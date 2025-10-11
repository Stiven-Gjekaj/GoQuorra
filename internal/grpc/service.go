package grpc

import (
	"context"
	"encoding/json"
	"log"
	"time"

	"github.com/goquorra/goquorra/internal/metrics"
	"github.com/goquorra/goquorra/internal/queue"
	"github.com/goquorra/goquorra/internal/store"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// WorkerServiceServer implements the gRPC WorkerService
type WorkerServiceServer struct {
	UnimplementedWorkerServiceServer
	queueManager *queue.Manager
	metrics      *metrics.Collector
	logger       *log.Logger
}

// NewWorkerService creates a new WorkerService
func NewWorkerService(queueManager *queue.Manager, metrics *metrics.Collector, logger *log.Logger) *WorkerServiceServer {
	return &WorkerServiceServer{
		queueManager: queueManager,
		metrics:      metrics,
		logger:       logger,
	}
}

// LeaseJobs streams jobs to workers
func (s *WorkerServiceServer) LeaseJobs(req *LeaseRequest, stream WorkerService_LeaseJobsServer) error {
	ctx := stream.Context()
	workerID := req.WorkerId
	queue := req.Queue
	maxJobs := int(req.MaxJobs)
	leaseTTL := time.Duration(req.LeaseTtlSeconds) * time.Second

	if queue == "" {
		queue = "default"
	}
	if maxJobs <= 0 {
		maxJobs = 1
	}
	if leaseTTL <= 0 {
		leaseTTL = 30 * time.Second
	}

	s.logger.Printf("Worker %s requesting lease from queue %s (max_jobs=%d, ttl=%v)", workerID, queue, maxJobs, leaseTTL)

	// Lease jobs from the queue
	jobs, err := s.queueManager.LeaseJobs(ctx, queue, workerID, maxJobs, leaseTTL)
	if err != nil {
		s.logger.Printf("Failed to lease jobs: %v", err)
		return err
	}

	s.metrics.RecordJobLeased(len(jobs))

	// Stream jobs to worker
	for _, job := range jobs {
		protoJob := s.convertToProtoJob(job)
		if err := stream.Send(protoJob); err != nil {
			s.logger.Printf("Failed to send job to worker: %v", err)
			return err
		}
		s.logger.Printf("Sent job %s to worker %s", job.ID, workerID)
	}

	return nil
}

// AckJob acknowledges successful job completion
func (s *WorkerServiceServer) AckJob(ctx context.Context, ack *JobAck) (*JobAckResponse, error) {
	s.logger.Printf("Worker %s acknowledging job %s (success=%v)", ack.WorkerId, ack.JobId, ack.Success)

	err := s.queueManager.AckJob(ctx, ack.JobId, ack.LeaseId, true, "")
	if err != nil {
		s.logger.Printf("Failed to ack job: %v", err)
		return &JobAckResponse{
			Acknowledged: false,
			Message:      err.Error(),
		}, err
	}

	s.metrics.RecordJobProcessed()

	return &JobAckResponse{
		Acknowledged: true,
		Message:      "Job completed successfully",
	}, nil
}

// NackJob handles job failure
func (s *WorkerServiceServer) NackJob(ctx context.Context, ack *JobAck) (*JobAckResponse, error) {
	s.logger.Printf("Worker %s nacking job %s: %s", ack.WorkerId, ack.JobId, ack.ErrorMessage)

	err := s.queueManager.AckJob(ctx, ack.JobId, ack.LeaseId, false, ack.ErrorMessage)
	if err != nil {
		s.logger.Printf("Failed to nack job: %v", err)
		return &JobAckResponse{
			Acknowledged: false,
			Message:      err.Error(),
		}, err
	}

	s.metrics.RecordJobFailed()

	return &JobAckResponse{
		Acknowledged: true,
		Message:      "Job failure recorded",
	}, nil
}

// convertToProtoJob converts a store.Job to a protobuf Job
func (s *WorkerServiceServer) convertToProtoJob(job *store.Job) *Job {
	// Marshal payload to JSON bytes
	payloadBytes := []byte("{}")
	if job.Payload != nil {
		if data, err := json.Marshal(job.Payload); err == nil {
			payloadBytes = data
		}
	}

	protoJob := &Job{
		Id:         job.ID,
		Type:       job.Type,
		Payload:    payloadBytes,
		Priority:   int32(job.Priority),
		Attempts:   int32(job.Attempts),
		MaxRetries: int32(job.MaxRetries),
		RunAt:      timestamppb.New(job.RunAt),
		CreatedAt:  timestamppb.New(job.CreatedAt),
		Queue:      job.Queue,
		LeaseId:    job.LeaseID,
	}

	if job.LeasedAt != nil {
		protoJob.LeasedAt = timestamppb.New(*job.LeasedAt)
	}

	return protoJob
}
