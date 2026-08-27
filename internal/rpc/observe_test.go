package rpc_test

import (
	"context"
	"io"
	"log/slog"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/Stiven-Gjekaj/GoQuorra/internal/auth"
	"github.com/Stiven-Gjekaj/GoQuorra/internal/metrics"
	"github.com/Stiven-Gjekaj/GoQuorra/internal/quorrapb"
	"github.com/Stiven-Gjekaj/GoQuorra/internal/rpc"
	"github.com/Stiven-Gjekaj/GoQuorra/internal/store"
	"github.com/Stiven-Gjekaj/GoQuorra/internal/store/memory"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
)

// call is one thing the observer reported.
type call struct {
	method string
	code   string
	took   time.Duration
}

// spy is a Recorder that remembers.
type spy struct {
	mu      sync.Mutex
	calls   []call
	streams []call
}

func (s *spy) GRPCRequest(method, code string, took time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, call{method: method, code: code, took: took})
}

func (s *spy) GRPCStream(method, code string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.streams = append(s.streams, call{method: method, code: code})
}

func (s *spy) seen() ([]call, []call) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]call(nil), s.calls...), append([]call(nil), s.streams...)
}

// observed builds a server with the observer in front of the guard.
func observed(t *testing.T) (quorrapb.QueueServiceClient, *spy) {
	t.Helper()

	backing := memory.New(store.Options{})
	t.Cleanup(func() { _ = backing.Close() })

	fleet, err := auth.NewKey("fleet", auth.Worker, fleetSecret)
	if err != nil {
		t.Fatalf("auth.NewKey: %v", err)
	}
	keys, err := auth.NewSet(fleet)
	if err != nil {
		t.Fatalf("auth.NewSet: %v", err)
	}

	watcher := &spy{}
	guard := rpc.NewGuard(keys)
	observe := rpc.NewObserver(watcher)

	listener := bufconn.Listen(1 << 20)
	server := grpc.NewServer(
		grpc.ChainUnaryInterceptor(observe.Unary(), guard.Unary()),
		grpc.ChainStreamInterceptor(observe.Stream(), guard.Stream()),
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

	return quorrapb.NewQueueServiceClient(conn), watcher
}

// A call on the worker protocol is timed, under the name of the method.
//
// Every job in this queue is leased and reported over gRPC, and the whole of
// that path was untimed. The HTTP histogram beside it has been published
// since the first release.
func TestACallOnTheWorkerProtocolIsTimed(t *testing.T) {
	client, watcher := observed(t)

	if err := leaseOnce(withSecret(fleetSecret), client); err != nil {
		t.Fatalf("Lease: %v", err)
	}

	calls, _ := watcher.seen()
	if len(calls) != 1 {
		t.Fatalf("the observer reported %d calls, want one: %+v", len(calls), calls)
	}
	if calls[0].method != "Lease" {
		t.Errorf("the method is %q, want the name without the service in front of it", calls[0].method)
	}
	if calls[0].code != "OK" {
		t.Errorf("a call that worked is recorded as %q", calls[0].code)
	}
	if calls[0].took <= 0 {
		t.Errorf("the call took %s, which is not a time", calls[0].took)
	}
}

// A call the guard refused is timed as well.
//
// The observer is outside the guard for this reason. A refusal that took a
// second is something an operator wants to see, and an interceptor that only
// ran after the guard would never report one.
func TestACallWithNoKeyIsTimedToo(t *testing.T) {
	client, watcher := observed(t)

	if err := leaseOnce(context.Background(), client); err == nil {
		t.Fatal("a call with no key was answered")
	}

	calls, _ := watcher.seen()
	if len(calls) != 1 {
		t.Fatalf("the observer reported %d calls, want the refused one: %+v", len(calls), calls)
	}
	if calls[0].code != "Unauthenticated" {
		t.Errorf("the refusal is recorded as %q", calls[0].code)
	}
}

// A stream is counted and not timed.
//
// A watch lives as long as the worker does, so the time it was open says how
// long the worker ran rather than how fast this server is. Hours in the same
// histogram as a lease leaves the quantiles meaning nothing.
func TestAStreamIsCountedAndNotTimed(t *testing.T) {
	client, watcher := observed(t)

	// A watch with no queues is refused, which ends the stream at once and
	// with a code worth reading.
	stream, err := client.Watch(withSecret(fleetSecret), &quorrapb.WatchRequest{WorkerId: "w1"})
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	if _, err := stream.Recv(); err == nil {
		t.Fatal("a watch with no queues was allowed")
	}

	calls, streams := watcher.seen()
	if len(calls) != 0 {
		t.Errorf("a stream was put in the histogram for calls that answer once: %+v", calls)
	}
	if len(streams) != 1 {
		t.Fatalf("the observer counted %d streams, want one: %+v", len(streams), streams)
	}
	if streams[0].method != "Watch" {
		t.Errorf("the stream is recorded under %q", streams[0].method)
	}
	if streams[0].code != "InvalidArgument" {
		t.Errorf("the stream ended as %q, want the refusal", streams[0].code)
	}
}
