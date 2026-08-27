package rpc_test

import (
	"context"
	"io"
	"log/slog"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/Stiven-Gjekaj/GoQuorra/internal/auth"
	"github.com/Stiven-Gjekaj/GoQuorra/internal/metrics"
	"github.com/Stiven-Gjekaj/GoQuorra/internal/quorrapb"
	"github.com/Stiven-Gjekaj/GoQuorra/internal/rpc"
	"github.com/Stiven-Gjekaj/GoQuorra/internal/store"
	"github.com/Stiven-Gjekaj/GoQuorra/internal/store/memory"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/types/known/durationpb"
)

const (
	fleetSecret    = "a-worker-secret-long-enough"
	operatorSecret = "an-operator-secret-long-enough"
	readerSecret   = "a-reader-secret-long-enough-ok"
)

// guarded builds a server with the guard in front of it.
func guarded(t *testing.T) quorrapb.QueueServiceClient {
	t.Helper()

	backing := memory.New(store.Options{})
	t.Cleanup(func() { _ = backing.Close() })

	keys := func() *auth.Set {
		fleet, err := auth.NewKey("fleet", auth.Worker, fleetSecret)
		if err != nil {
			t.Fatalf("auth.NewKey: %v", err)
		}
		ops, err := auth.NewKey("ops", auth.Write, operatorSecret)
		if err != nil {
			t.Fatalf("auth.NewKey: %v", err)
		}
		set, err := auth.NewSet(fleet, ops)
		if err != nil {
			t.Fatalf("auth.NewSet: %v", err)
		}
		return set
	}()

	guard := rpc.NewGuard(keys)
	listener := bufconn.Listen(1 << 20)
	server := grpc.NewServer(
		grpc.UnaryInterceptor(guard.Unary()),
		grpc.StreamInterceptor(guard.Stream()),
	)
	quorrapb.RegisterQueueServiceServer(server, rpc.New(
		backing, metrics.New(),
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		rpc.DefaultLimits(), time.Now,
	))
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(server.Stop)

	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return listener.DialContext(ctx)
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("grpc.NewClient: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	return quorrapb.NewQueueServiceClient(conn)
}

// withSecret puts a key on a call.
func withSecret(secret string) context.Context {
	return metadata.AppendToOutgoingContext(context.Background(), "x-api-key", secret)
}

// leaseOnce is the smallest call this protocol has.
func leaseOnce(ctx context.Context, c quorrapb.QueueServiceClient) error {
	_, err := c.Lease(ctx, &quorrapb.LeaseRequest{
		WorkerId: "w1", Queue: "default", MaxJobs: 1,
		LeaseTtl: durationpb.New(time.Minute),
	})
	return err
}

// The port had no authentication at all, so a process that could reach it
// could lease from any queue.
func TestTheWorkerProtocolRefusesACallWithNoKey(t *testing.T) {
	c := guarded(t)

	err := leaseOnce(context.Background(), c)
	if status.Code(err) != codes.Unauthenticated {
		t.Fatalf("a call with no key gave %v, want Unauthenticated", err)
	}

	// A key the server does not know is the same answer. Nothing here knows
	// who the caller is, so there is nothing to deny.
	if err := leaseOnce(withSecret("a-key-nobody-configured-here"), c); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("an unknown key gave %v, want Unauthenticated", err)
	}
}

// A worker key works, and an operator's key does not.
//
// This is the reason the permissions stopped being an ordered number. Leasing
// work off the queue is not more than changing a job: a key an operator keeps
// in a shell profile must not be able to lease the queue empty.
func TestOnlyAWorkerKeyMayLease(t *testing.T) {
	c := guarded(t)

	if err := leaseOnce(withSecret(fleetSecret), c); err != nil {
		t.Fatalf("the worker key was refused: %v", err)
	}

	err := leaseOnce(withSecret(operatorSecret), c)
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("an operator key gave %v, want PermissionDenied", err)
	}

	// PermissionDenied and not Unauthenticated, and the message names the
	// key. The key is real and the server knows whose it is, so answering
	// Unauthenticated would send somebody to check a key that works.
	for _, want := range []string{"ops", "write", "worker"} {
		if !strings.Contains(status.Convert(err).Message(), want) {
			t.Errorf("the refusal does not say %q: %s", want, status.Convert(err).Message())
		}
	}
}

// Watching needs at least one queue, and not more than the bound.
//
// Empty is refused rather than read as "all of them": a worker that watched
// every queue would be woken by work it cannot take, and a caller that meant
// to name one and sent none should hear about it.
func TestWatchingNeedsQueuesAndIsBounded(t *testing.T) {
	c := guarded(t)

	var many []string
	for i := 0; i < rpc.MostWatchedQueues+1; i++ {
		many = append(many, "queue-"+string(rune('a'+i)))
	}

	cases := map[string][]string{
		"no queues at all":  nil,
		"only empty names":  {"", "  "},
		"more than allowed": many,
	}
	for name, queues := range cases {
		// A deadline, because the answer to this is a refusal and the
		// failure is a stream that stays open. Without one, a build that
		// stopped refusing would hang the suite rather than fail it.
		ctx, stop := context.WithTimeout(withSecret(fleetSecret), 3*time.Second)

		stream, err := c.Watch(ctx, &quorrapb.WatchRequest{WorkerId: "w1", Queues: queues})
		if err == nil {
			_, err = stream.Recv()
		}
		if status.Code(err) != codes.InvalidArgument {
			t.Errorf("watching with %s gave %v, want InvalidArgument", name, err)
		}
		stop()
	}
}

// Watching is guarded like every other call on this protocol.
//
// It is the streaming one, and a guard on the unary calls alone is a door
// beside an open window.
func TestWatchingIsGuarded(t *testing.T) {
	c := guarded(t)

	stream, err := c.Watch(context.Background(), &quorrapb.WatchRequest{
		WorkerId: "w1", Queues: []string{"default"},
	})
	if err == nil {
		_, err = stream.Recv()
	}
	if status.Code(err) != codes.Unauthenticated {
		t.Errorf("watching with no key gave %v, want Unauthenticated", err)
	}

	// An operator's key is refused too, for the same reason it cannot lease.
	stream, err = c.Watch(withSecret(operatorSecret), &quorrapb.WatchRequest{
		WorkerId: "w1", Queues: []string{"default"},
	})
	if err == nil {
		_, err = stream.Recv()
	}
	if status.Code(err) != codes.PermissionDenied {
		t.Errorf("watching with an operator key gave %v, want PermissionDenied", err)
	}
}

// Every call is guarded, and not only the first one somebody thought of.
func TestEveryCallOnTheWorkerProtocolIsGuarded(t *testing.T) {
	c := guarded(t)
	ctx := context.Background()

	calls := map[string]func() error{
		"Lease": func() error { return leaseOnce(ctx, c) },
		"Report": func() error {
			_, err := c.Report(ctx, &quorrapb.ReportRequest{
				JobId: "8de1a3d0-0000-0000-0000-000000000000", LeaseId: "x",
			})
			return err
		},
		"Heartbeat": func() error {
			_, err := c.Heartbeat(ctx, &quorrapb.HeartbeatRequest{
				JobId: "8de1a3d0-0000-0000-0000-000000000000", LeaseId: "x",
				ExtendBy: durationpb.New(time.Minute),
			})
			return err
		},
	}
	for name, call := range calls {
		if code := status.Code(call()); code != codes.Unauthenticated {
			t.Errorf("%s with no key gave %s, want Unauthenticated", name, code)
		}
	}
}
