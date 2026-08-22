// Package rpc serves the worker protocol.
//
// The package is named rpc and not grpc. The old one was called grpc, so
// every file that used both it and google.golang.org/grpc had to rename one
// of them at the import, and the server did.
package rpc

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/Stiven-Gjekaj/GoQuorra/internal/jobs"
	"github.com/Stiven-Gjekaj/GoQuorra/internal/metrics"
	"github.com/Stiven-Gjekaj/GoQuorra/internal/quorrapb"
	"github.com/Stiven-Gjekaj/GoQuorra/internal/store"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Limits bound what a worker may ask for.
//
// A worker is not a trusted caller in the way the name suggests. It is a
// process somebody deployed, possibly an old one, possibly one with a typo in
// its environment. Asking for a million jobs, or for a lease measured in
// weeks, has to be answered with a number rather than obeyed.
type Limits struct {
	MaxJobsPerLease int
	MinLeaseTTL     time.Duration
	MaxLeaseTTL     time.Duration
	DefaultLeaseTTL time.Duration

	// MaxResultBytes bounds what a worker may hand back. A queue is not a
	// place to put a large value: a row that holds one is read by every
	// listing that touches it, and the dashboard shows a hundred at a time.
	MaxResultBytes int
}

// DefaultLimits are used when the caller states none.
func DefaultLimits() Limits {
	return Limits{
		MaxJobsPerLease: 100,
		MinLeaseTTL:     time.Second,
		MaxLeaseTTL:     time.Hour,
		DefaultLeaseTTL: 30 * time.Second,
		MaxResultBytes:  64 << 10,
	}
}

// Service answers the worker protocol.
type Service struct {
	quorrapb.UnimplementedQueueServiceServer

	store   store.Store
	metrics *metrics.Metrics
	log     *slog.Logger
	limits  Limits
	now     func() time.Time
}

// New builds the service.
func New(s store.Store, m *metrics.Metrics, log *slog.Logger, limits Limits, now func() time.Time) *Service {
	if now == nil {
		now = time.Now
	}
	return &Service{store: s, metrics: m, log: log, limits: limits, now: now}
}

// Lease hands ready jobs to a worker.
func (s *Service) Lease(ctx context.Context, req *quorrapb.LeaseRequest) (*quorrapb.LeaseResponse, error) {
	if req.GetWorkerId() == "" {
		return nil, status.Error(codes.InvalidArgument, "worker_id is required, because the server records it as the holder of the lease")
	}

	queue := req.GetQueue()
	if queue == "" {
		queue = store.DefaultQueue
	}

	limit := int(req.GetMaxJobs())
	if limit <= 0 {
		limit = 1
	}
	if limit > s.limits.MaxJobsPerLease {
		limit = s.limits.MaxJobsPerLease
	}

	ttl := s.limits.DefaultLeaseTTL
	if req.GetLeaseTtl() != nil {
		ttl = req.GetLeaseTtl().AsDuration()
	}
	if ttl < s.limits.MinLeaseTTL {
		ttl = s.limits.MinLeaseTTL
	}
	if ttl > s.limits.MaxLeaseTTL {
		ttl = s.limits.MaxLeaseTTL
	}

	leased, err := s.store.Lease(ctx, store.LeaseRequest{
		Queue:    queue,
		WorkerID: req.GetWorkerId(),
		Limit:    limit,
		TTL:      ttl,
	})
	if err != nil {
		s.log.Error("cannot lease jobs", "worker", req.GetWorkerId(), "queue", queue, "error", err)
		return nil, status.Error(codes.Internal, "cannot lease jobs")
	}

	s.metrics.JobsLeased(len(leased))
	if len(leased) > 0 {
		s.log.Debug("leased jobs", "worker", req.GetWorkerId(), "queue", queue, "count", len(leased))
	}

	out := make([]*quorrapb.Job, len(leased))
	for i, job := range leased {
		out[i] = toProto(job)
	}
	return &quorrapb.LeaseResponse{Jobs: out}, nil
}

// Report records what happened to a job.
func (s *Service) Report(ctx context.Context, req *quorrapb.ReportRequest) (*quorrapb.ReportResponse, error) {
	if req.GetJobId() == "" {
		return nil, status.Error(codes.InvalidArgument, "job_id is required")
	}
	if req.GetLeaseId() == "" {
		return nil, status.Error(codes.InvalidArgument, "lease_id is required, and an empty one matches no job")
	}

	var outcome jobs.Outcome
	switch req.GetOutcome() {
	case quorrapb.Outcome_OUTCOME_SUCCEEDED:
		outcome = jobs.OutcomeDone
	case quorrapb.Outcome_OUTCOME_FAILED:
		outcome = jobs.OutcomeFailed
	default:
		// An unset outcome is refused rather than read as a success. Zero is
		// what an older client sends, and retiring a job nobody finished is
		// the most expensive way to be wrong here.
		return nil, status.Errorf(codes.InvalidArgument,
			"outcome is %s, and it must be OUTCOME_SUCCEEDED or OUTCOME_FAILED", req.GetOutcome())
	}

	if len(req.GetResult()) > s.limits.MaxResultBytes {
		// Refused rather than trimmed. Half a JSON document is not a smaller
		// result, it is a broken one, and storing it would put a value on the
		// job that nothing can read.
		return nil, status.Errorf(codes.InvalidArgument,
			"the result is %d bytes and the limit is %d. Store a large value where it belongs and report a reference to it.",
			len(req.GetResult()), s.limits.MaxResultBytes)
	}

	job, err := s.store.Report(ctx, store.Report{
		JobID:   req.GetJobId(),
		LeaseID: req.GetLeaseId(),
		Outcome: outcome,
		Error:   req.GetError(),
		Result:  req.GetResult(),
	})

	switch {
	case errors.Is(err, store.ErrNotFound):
		return nil, status.Errorf(codes.NotFound, "no job carries the identifier %s", req.GetJobId())
	case errors.Is(err, store.ErrLeaseNotValid):
		// FailedPrecondition and not PermissionDenied. The worker held this
		// job honestly and lost it, almost always because it took longer than
		// its lease. Telling it so is what lets it stop and log the fact
		// rather than retry the report.
		return nil, status.Errorf(codes.FailedPrecondition,
			"the lease on job %s is no longer valid, so the job has been given to another worker", req.GetJobId())
	case err != nil:
		// A result that is not JSON is the worker's mistake, and the store
		// says so. Answering Internal to it sends the reader to the server.
		if strings.Contains(err.Error(), "not JSON") {
			return nil, status.Error(codes.InvalidArgument, err.Error())
		}
		s.log.Error("cannot record a report", "job", req.GetJobId(), "worker", req.GetWorkerId(), "error", err)
		return nil, status.Error(codes.Internal, "cannot record the report")
	}

	s.metrics.JobFinished(job, s.now())
	s.log.Debug("recorded a report",
		"job", job.ID, "worker", req.GetWorkerId(), "outcome", outcome, "status", job.Status)

	return &quorrapb.ReportResponse{
		Status:   job.Status.String(),
		Attempts: int32(job.Attempts),
		RunAt:    timestamppb.New(job.RunAt),
	}, nil
}

// Heartbeat pushes the expiry of a lease further out.
func (s *Service) Heartbeat(ctx context.Context, req *quorrapb.HeartbeatRequest) (*quorrapb.HeartbeatResponse, error) {
	if req.GetJobId() == "" || req.GetLeaseId() == "" {
		return nil, status.Error(codes.InvalidArgument, "job_id and lease_id are both required")
	}

	by := s.limits.DefaultLeaseTTL
	if req.GetExtendBy() != nil {
		by = req.GetExtendBy().AsDuration()
	}
	// Bounded by the same limits a lease is. A worker asking for a week
	// through this call would otherwise get what it was refused at lease
	// time, which makes the cap at lease time decoration.
	if by < s.limits.MinLeaseTTL {
		by = s.limits.MinLeaseTTL
	}
	if by > s.limits.MaxLeaseTTL {
		by = s.limits.MaxLeaseTTL
	}

	job, err := s.store.ExtendLease(ctx, req.GetJobId(), req.GetLeaseId(), by)

	switch {
	case errors.Is(err, store.ErrNotFound):
		return nil, status.Errorf(codes.NotFound, "no job carries the identifier %s", req.GetJobId())
	case errors.Is(err, store.ErrLeaseNotValid):
		// The same code the report path uses for the same condition, so a
		// worker has one thing to check. It means stop: the job has been
		// cancelled, or taken back and given to somebody else.
		return nil, status.Errorf(codes.FailedPrecondition,
			"the lease on job %s is no longer valid, so the job is no longer yours", req.GetJobId())
	case err != nil:
		s.log.Error("cannot extend a lease", "job", req.GetJobId(), "worker", req.GetWorkerId(), "error", err)
		return nil, status.Error(codes.Internal, "cannot extend the lease")
	}

	answer := &quorrapb.HeartbeatResponse{}
	if job.LeaseExpiresAt != nil {
		answer.LeaseExpiresAt = timestamppb.New(*job.LeaseExpiresAt)
	}
	return answer, nil
}

// toProto turns a stored job into one on the wire.
func toProto(job *store.Job) *quorrapb.Job {
	out := &quorrapb.Job{
		Id:         job.ID,
		Type:       job.Type,
		Payload:    job.Payload,
		Queue:      job.Queue,
		Priority:   int32(job.Priority),
		Attempts:   int32(job.Attempts),
		MaxRetries: int32(job.MaxRetries),
		LeaseId:    job.LeaseID,
		RunAt:      timestamppb.New(job.RunAt),
		CreatedAt:  timestamppb.New(job.CreatedAt),
	}
	if job.LeaseExpiresAt != nil {
		out.LeaseExpiresAt = timestamppb.New(*job.LeaseExpiresAt)
	}
	return out
}
