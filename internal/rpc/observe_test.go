package rpc_test

import (
	"context"
	"log/slog"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Stiven-Gjekaj/GoQuorra/internal/auth"
	"github.com/Stiven-Gjekaj/GoQuorra/internal/metrics"
	"github.com/Stiven-Gjekaj/GoQuorra/internal/quorrapb"
	"github.com/Stiven-Gjekaj/GoQuorra/internal/reqid"
	"github.com/Stiven-Gjekaj/GoQuorra/internal/rpc"
	"github.com/Stiven-Gjekaj/GoQuorra/internal/store"
	"github.com/Stiven-Gjekaj/GoQuorra/internal/store/memory"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/types/known/durationpb"
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

// written is a log a test reads back.
type written struct {
	mu    sync.Mutex
	lines strings.Builder
}

func (w *written) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.lines.Write(p)
}

func (w *written) text() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.lines.String()
}

// observed builds a server with the observer in front of the guard.
func observed(t *testing.T) (quorrapb.QueueServiceClient, *spy, *written) {
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
	kept := &written{}
	quorrapb.RegisterQueueServiceServer(server, rpc.New(
		backing, metrics.New(),
		slog.New(slog.NewTextHandler(kept, &slog.HandlerOptions{Level: slog.LevelDebug})),
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

	return quorrapb.NewQueueServiceClient(conn), watcher, kept
}

// A call on the worker protocol is timed, under the name of the method.
//
// Every job in this queue is leased and reported over gRPC, and the whole of
// that path was untimed. The HTTP histogram beside it has been published
// since the first release.
func TestACallOnTheWorkerProtocolIsTimed(t *testing.T) {
	client, watcher, _ := observed(t)

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
	client, watcher, _ := observed(t)

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
	client, watcher, _ := observed(t)

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

// A call on the worker protocol gets an identifier and sends it back.
//
// The same story as the HTTP header. A worker with a failure has one string
// to quote, and the server has one string to search its log for.
func TestACallOnTheWorkerProtocolCarriesAnIdentifier(t *testing.T) {
	client, _, _ := observed(t)

	var first, second metadata.MD
	if _, err := client.Lease(withSecret(fleetSecret), &quorrapb.LeaseRequest{
		WorkerId: "w1", Queue: "default", MaxJobs: 1,
		LeaseTtl: durationpb.New(time.Minute),
	}, grpc.Header(&first)); err != nil {
		t.Fatalf("Lease: %v", err)
	}
	if _, err := client.Lease(withSecret(fleetSecret), &quorrapb.LeaseRequest{
		WorkerId: "w1", Queue: "default", MaxJobs: 1,
		LeaseTtl: durationpb.New(time.Minute),
	}, grpc.Header(&second)); err != nil {
		t.Fatalf("Lease: %v", err)
	}

	one := header(first)
	two := header(second)
	if one == "" {
		t.Fatal("a call carries no identifier")
	}
	if one == two {
		t.Errorf("two calls share the identifier %q", one)
	}
}

// What a worker sent is what comes back.
func TestWhatAWorkerSentComesBack(t *testing.T) {
	client, _, _ := observed(t)

	ctx := metadata.AppendToOutgoingContext(withSecret(fleetSecret),
		strings.ToLower(reqid.Header), "trace-worker-7")

	var got metadata.MD
	if _, err := client.Lease(ctx, &quorrapb.LeaseRequest{
		WorkerId: "w1", Queue: "default", MaxJobs: 1,
		LeaseTtl: durationpb.New(time.Minute),
	}, grpc.Header(&got)); err != nil {
		t.Fatalf("Lease: %v", err)
	}

	if header(got) != "trace-worker-7" {
		t.Errorf("the answer carries %q, and the worker sent trace-worker-7", header(got))
	}
}

// header reads the request identifier off what a call answered with.
func header(md metadata.MD) string {
	values := md.Get(strings.ToLower(reqid.Header))
	if len(values) == 0 {
		return ""
	}
	return values[0]
}

// A stream handler sees the identifier of the call that opened the stream.
//
// A handler reads its context from the stream and not from an argument, so an
// interceptor that wants to put anything on it has to wrap the stream. Taking
// that wrapper away breaks nothing that compiles, and the only sign of it is
// a log line that stopped naming its request.
func TestAStreamHandlerSeesTheIdentifier(t *testing.T) {
	client, _, kept := observed(t)

	ctx, stop := context.WithCancel(withSecret(fleetSecret))
	defer stop()
	ctx = metadata.AppendToOutgoingContext(ctx, strings.ToLower(reqid.Header), "trace-watch-9")

	stream, err := client.Watch(ctx, &quorrapb.WatchRequest{
		WorkerId: "w1", Queues: []string{"default"},
	})
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	t.Cleanup(func() { _ = stream.CloseSend() })

	// The line is written when the watch is accepted, and the watch is
	// accepted before anything is sent, so this waits for the line.
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(kept.text(), "a worker is watching") {
		if time.Now().After(deadline) {
			t.Fatalf("the server never wrote the line, only: %s", kept.text())
		}
		time.Sleep(5 * time.Millisecond)
	}

	if !strings.Contains(kept.text(), "trace-watch-9") {
		t.Errorf("the line does not name the request that opened the stream: %s", kept.text())
	}
}
