package rpc_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"testing"
	"time"

	"github.com/Stiven-Gjekaj/GoQuorra/internal/jobs"
	"github.com/Stiven-Gjekaj/GoQuorra/internal/metrics"
	"github.com/Stiven-Gjekaj/GoQuorra/internal/quorrapb"
	"github.com/Stiven-Gjekaj/GoQuorra/internal/rpc"
	"github.com/Stiven-Gjekaj/GoQuorra/internal/store"
	"github.com/Stiven-Gjekaj/GoQuorra/internal/store/memory"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/types/known/durationpb"
)

var start = time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

// Every test here runs over a real gRPC connection.
//
// Calling the service methods directly would test the Go code and skip the
// codec, which is exactly the part that was broken before the rebuild: the
// old messages satisfied every call in Go and were refused on the wire. A
// connection in memory costs almost nothing and covers it.
func dial(t *testing.T) (quorrapb.QueueServiceClient, store.Store, *clock) {
	t.Helper()

	tick := &clock{now: start}
	backing := memory.New(store.Options{
		Policy: jobs.Policy{MaxRetries: 2, Base: 10 * time.Second, Max: time.Hour},
		Now:    tick.Now,
		Jitter: func() float64 { return 0 },
	})

	service := rpc.New(
		backing,
		metrics.New(),
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		rpc.DefaultLimits(),
		tick.Now,
	)

	listener := bufconn.Listen(1 << 20)
	server := grpc.NewServer()
	quorrapb.RegisterQueueServiceServer(server, service)

	go func() { _ = server.Serve(listener) }()
	t.Cleanup(server.Stop)

	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return listener.DialContext(ctx)
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("cannot connect: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	return quorrapb.NewQueueServiceClient(conn), backing, tick
}

type clock struct{ now time.Time }

func (c *clock) Now() time.Time          { return c.now }
func (c *clock) Advance(d time.Duration) { c.now = c.now.Add(d) }

// A job goes out and its result comes back, over the wire.
//
// This is the call that the old generated code could not complete. It failed
// inside the codec with a message about marshalling, and no test existed that
// would have run it.
func TestAJobIsLeasedAndReportedOverTheWire(t *testing.T) {
	client, backing, _ := dial(t)
	ctx := t.Context()

	made, err := backing.Create(ctx, store.NewJob{
		Type:    "email",
		Payload: json.RawMessage(`{"to":"a@b.c"}`),
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	leased, err := client.Lease(ctx, &quorrapb.LeaseRequest{
		WorkerId: "worker-1",
		Queue:    "default",
		MaxJobs:  5,
		LeaseTtl: durationpb.New(45 * time.Second),
	})
	if err != nil {
		t.Fatalf("Lease: %v", err)
	}

	if len(leased.GetJobs()) != 1 {
		t.Fatalf("leased %d jobs, want 1", len(leased.GetJobs()))
	}
	got := leased.GetJobs()[0]

	if got.GetId() != made.ID {
		t.Errorf("id = %q, want %q", got.GetId(), made.ID)
	}
	if string(got.GetPayload()) != `{"to":"a@b.c"}` {
		t.Errorf("payload = %s", got.GetPayload())
	}
	if got.GetLeaseId() == "" {
		t.Error("the job arrived with no lease identifier, so it can never be reported")
	}
	if got.GetAttempts() != 1 {
		t.Errorf("attempts = %d, want 1 on the first run", got.GetAttempts())
	}
	if want := start.Add(45 * time.Second); !got.GetLeaseExpiresAt().AsTime().Equal(want) {
		t.Errorf("lease expiry = %s, want %s", got.GetLeaseExpiresAt().AsTime(), want)
	}

	reported, err := client.Report(ctx, &quorrapb.ReportRequest{
		JobId:    got.GetId(),
		WorkerId: "worker-1",
		LeaseId:  got.GetLeaseId(),
		Outcome:  quorrapb.Outcome_OUTCOME_SUCCEEDED,
	})
	if err != nil {
		t.Fatalf("Report: %v", err)
	}
	if reported.GetStatus() != "succeeded" {
		t.Errorf("status = %q", reported.GetStatus())
	}
}

// A failure is recorded as a failure.
//
// The old service had two methods and a success field that nothing read, so a
// worker reporting a failure through the wrong one was recorded as having
// succeeded. There is one method now, and the outcome decides.
func TestAFailureSendsTheJobBack(t *testing.T) {
	client, backing, _ := dial(t)
	ctx := t.Context()

	create(t, backing, ctx)
	leased := leaseOne(t, client, ctx)

	got, err := client.Report(ctx, &quorrapb.ReportRequest{
		JobId:    leased.GetId(),
		WorkerId: "worker-1",
		LeaseId:  leased.GetLeaseId(),
		Outcome:  quorrapb.Outcome_OUTCOME_FAILED,
		Error:    "the host refused the connection",
	})
	if err != nil {
		t.Fatalf("Report: %v", err)
	}

	if got.GetStatus() != "pending" {
		t.Errorf("status = %q, want the job back in the queue", got.GetStatus())
	}
	if !got.GetRunAt().AsTime().After(start) {
		t.Errorf("run at = %s, which is not after %s, so no backoff was applied", got.GetRunAt().AsTime(), start)
	}
}

// An unset outcome is refused.
//
// Zero is what an older client sends and what a new field left unwritten
// sends. Reading it as a success retires a job nobody finished.
func TestAnUnsetOutcomeIsRefused(t *testing.T) {
	client, backing, _ := dial(t)
	ctx := t.Context()

	create(t, backing, ctx)
	leased := leaseOne(t, client, ctx)

	_, err := client.Report(ctx, &quorrapb.ReportRequest{
		JobId:   leased.GetId(),
		LeaseId: leased.GetLeaseId(),
	})
	if got := status.Code(err); got != codes.InvalidArgument {
		t.Fatalf("code = %s, want InvalidArgument (error: %v)", got, err)
	}
}

func TestAStaleLeaseIsRefusedWithAReasonTheWorkerCanAct(t *testing.T) {
	client, backing, tick := dial(t)
	ctx := t.Context()

	create(t, backing, ctx)
	leased := leaseOne(t, client, ctx)

	tick.Advance(2 * time.Minute)
	if _, err := backing.ReclaimExpired(ctx, 10); err != nil {
		t.Fatalf("ReclaimExpired: %v", err)
	}

	_, err := client.Report(ctx, &quorrapb.ReportRequest{
		JobId:   leased.GetId(),
		LeaseId: leased.GetLeaseId(),
		Outcome: quorrapb.Outcome_OUTCOME_SUCCEEDED,
	})

	// FailedPrecondition and not PermissionDenied. The worker held the job
	// honestly and lost it, and the difference tells it to stop rather than
	// to retry the report.
	if got := status.Code(err); got != codes.FailedPrecondition {
		t.Fatalf("code = %s, want FailedPrecondition (error: %v)", got, err)
	}
}

func TestAnUnknownJobIsNotFound(t *testing.T) {
	client, _, _ := dial(t)

	_, err := client.Report(t.Context(), &quorrapb.ReportRequest{
		JobId:   "6f1c0c64-0000-0000-0000-000000000000",
		LeaseId: "8de1a3d0-0000-0000-0000-000000000000",
		Outcome: quorrapb.Outcome_OUTCOME_SUCCEEDED,
	})
	if got := status.Code(err); got != codes.NotFound {
		t.Fatalf("code = %s, want NotFound (error: %v)", got, err)
	}
}

func TestALeaseWithNoWorkerIsRefused(t *testing.T) {
	client, _, _ := dial(t)

	_, err := client.Lease(t.Context(), &quorrapb.LeaseRequest{Queue: "default", MaxJobs: 1})
	if got := status.Code(err); got != codes.InvalidArgument {
		t.Fatalf("code = %s, want InvalidArgument (error: %v)", got, err)
	}
}

// A worker asking for more than the server allows gets the server's number.
//
// A typo in an environment variable, or an old worker with a different idea
// of what is reasonable, must not be able to take the whole queue in one call
// or to hold a lease for a week.
func TestTheServerBoundsWhatAWorkerAsksFor(t *testing.T) {
	client, backing, _ := dial(t)
	ctx := t.Context()

	for i := 0; i < 120; i++ {
		create(t, backing, ctx)
	}

	leased, err := client.Lease(ctx, &quorrapb.LeaseRequest{
		WorkerId: "greedy",
		Queue:    "default",
		MaxJobs:  1_000_000,
		LeaseTtl: durationpb.New(9000 * time.Hour),
	})
	if err != nil {
		t.Fatalf("Lease: %v", err)
	}

	limits := rpc.DefaultLimits()
	if len(leased.GetJobs()) != limits.MaxJobsPerLease {
		t.Errorf("leased %d jobs, want the server limit of %d", len(leased.GetJobs()), limits.MaxJobsPerLease)
	}

	expires := leased.GetJobs()[0].GetLeaseExpiresAt().AsTime()
	if want := start.Add(limits.MaxLeaseTTL); !expires.Equal(want) {
		t.Errorf("lease expiry = %s, want the server cap at %s", expires, want)
	}
}

// create puts a job in the store directly. The protocol has no way to submit
// one, because submitting is the REST side of the server, and a worker is not
// allowed to make its own work.
func create(t *testing.T, backing store.Store, ctx context.Context) {
	t.Helper()
	if _, err := backing.Create(ctx, store.NewJob{Type: "work"}); err != nil {
		t.Fatalf("Create: %v", err)
	}
}

func leaseOne(t *testing.T, client quorrapb.QueueServiceClient, ctx context.Context) *quorrapb.Job {
	t.Helper()

	got, err := client.Lease(ctx, &quorrapb.LeaseRequest{
		WorkerId: "worker-1",
		Queue:    "default",
		MaxJobs:  1,
		LeaseTtl: durationpb.New(time.Minute),
	})
	if err != nil {
		t.Fatalf("Lease: %v", err)
	}
	if len(got.GetJobs()) != 1 {
		t.Fatalf("leased %d jobs, want 1", len(got.GetJobs()))
	}
	return got.GetJobs()[0]
}
