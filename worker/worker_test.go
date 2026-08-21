package worker_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Stiven-Gjekaj/GoQuorra/internal/jobs"
	"github.com/Stiven-Gjekaj/GoQuorra/internal/metrics"
	"github.com/Stiven-Gjekaj/GoQuorra/internal/quorrapb"
	"github.com/Stiven-Gjekaj/GoQuorra/internal/rpc"
	"github.com/Stiven-Gjekaj/GoQuorra/internal/store"
	"github.com/Stiven-Gjekaj/GoQuorra/internal/store/memory"
	"github.com/Stiven-Gjekaj/GoQuorra/worker"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
)

// The worker is driven against a real server over a real connection.
//
// A fake client would test the loop and skip the protocol, and the protocol
// is where this project's worst defect lived. The server here is the same one
// the binary runs, over a connection in memory, so a test costs milliseconds
// and still covers the codec.
func serve(t *testing.T) (store.Store, []grpc.DialOption) {
	t.Helper()

	backing := memory.New(store.Options{
		Policy: jobs.Policy{MaxRetries: 2, Base: time.Millisecond, Max: time.Millisecond},
	})
	t.Cleanup(func() { _ = backing.Close() })

	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	service := rpc.New(backing, metrics.New(), quiet, rpc.DefaultLimits(), time.Now)

	listener := bufconn.Listen(1 << 20)
	server := grpc.NewServer()
	quorrapb.RegisterQueueServiceServer(server, service)
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(server.Stop)

	return backing, []grpc.DialOption{
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return listener.DialContext(ctx)
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	}
}

func newWorker(t *testing.T, dial []grpc.DialOption) *worker.Worker {
	t.Helper()

	w, err := worker.New(worker.Config{
		ID:            "test-worker",
		ServerAddr:    "passthrough:///bufnet",
		Queues:        []string{"default"},
		MaxJobs:       5,
		LeaseTTL:      30 * time.Second,
		PollEvery:     5 * time.Millisecond,
		ShutdownGrace: 5 * time.Second,
		Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		DialOptions:   dial,
	})
	if err != nil {
		t.Fatalf("worker.New: %v", err)
	}
	t.Cleanup(func() { _ = w.Close() })
	return w
}

// waitFor polls a condition rather than sleeping for a fixed time. A sleep
// long enough to be reliable on a loaded machine makes every run slow, and a
// short one makes the suite flake.
func waitFor(t *testing.T, why string, check func() bool) {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if check() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", why)
}

func TestAHandlerRunsTheJobItIsRegisteredFor(t *testing.T) {
	backing, dial := serve(t)
	w := newWorker(t, dial)

	var got atomic.Value
	w.Handle("email", func(ctx context.Context, job worker.Job) error {
		var mail struct {
			To string `json:"to"`
		}
		if err := job.Decode(&mail); err != nil {
			return err
		}
		got.Store(mail.To)
		return nil
	})

	made, err := backing.Create(t.Context(), store.NewJob{
		Type:    "email",
		Payload: json.RawMessage(`{"to":"a@b.c"}`),
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	stop := run(t, w)
	defer stop()

	waitFor(t, "the job to finish", func() bool {
		job, err := backing.Get(t.Context(), made.ID)
		return err == nil && job.Status == jobs.Succeeded
	})

	if to, _ := got.Load().(string); to != "a@b.c" {
		t.Errorf("the handler saw %q", to)
	}
}

// A job type nobody handles fails, and says why.
//
// The old worker ran a simulator for every type it was given and returned
// success nine times out of ten, so a job type that no code knew about was
// reported as done. The work never happened and nothing anywhere said so.
func TestAnUnknownJobTypeFailsAndSaysSo(t *testing.T) {
	backing, dial := serve(t)
	w := newWorker(t, dial)

	w.Handle("known", func(context.Context, worker.Job) error { return nil })

	made, err := backing.Create(t.Context(), store.NewJob{Type: "nobody_handles_this"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	stop := run(t, w)
	defer stop()

	waitFor(t, "the job to be buried", func() bool {
		job, err := backing.Get(t.Context(), made.ID)
		return err == nil && job.Status == jobs.Dead
	})

	job, _ := backing.Get(t.Context(), made.ID)
	if job.LastError == "" {
		t.Fatal("the job carries no reason")
	}
	if !strings.Contains(job.LastError, "nobody_handles_this") {
		t.Errorf("the reason does not name the job type: %q", job.LastError)
	}
}

// A handler that panics costs one job, and not the worker.
//
// Letting a panic through ends the process, which loses every other job in
// flight as well, and each of those then waits out its lease before anybody
// can run it. One bad payload should cost one job.
func TestAPanicInAHandlerCostsOneJob(t *testing.T) {
	backing, dial := serve(t)
	w := newWorker(t, dial)

	var good atomic.Int64
	w.Handle("explodes", func(context.Context, worker.Job) error {
		panic("the payload was not what I expected")
	})
	w.Handle("fine", func(context.Context, worker.Job) error {
		good.Add(1)
		return nil
	})

	bad, err := backing.Create(t.Context(), store.NewJob{Type: "explodes"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	stop := run(t, w)
	defer stop()

	waitFor(t, "the exploding job to be buried", func() bool {
		job, err := backing.Get(t.Context(), bad.ID)
		return err == nil && job.Status == jobs.Dead
	})

	// The worker is still alive, so a job submitted afterwards still runs.
	later, err := backing.Create(t.Context(), store.NewJob{Type: "fine"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	waitFor(t, "the later job to finish", func() bool {
		job, err := backing.Get(t.Context(), later.ID)
		return err == nil && job.Status == jobs.Succeeded
	})

	job, _ := backing.Get(t.Context(), bad.ID)
	if !strings.Contains(job.LastError, "panicked") {
		t.Errorf("the reason does not say that the handler panicked: %q", job.LastError)
	}
}

func TestAFailingHandlerSendsTheJobBackAndThenBuriesIt(t *testing.T) {
	backing, dial := serve(t)
	w := newWorker(t, dial)

	var runs atomic.Int64
	w.Handle("always_fails", func(context.Context, worker.Job) error {
		runs.Add(1)
		return errors.New("the host refused the connection")
	})

	made, err := backing.Create(t.Context(), store.NewJob{Type: "always_fails"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	stop := run(t, w)
	defer stop()

	waitFor(t, "the job to be buried", func() bool {
		job, err := backing.Get(t.Context(), made.ID)
		return err == nil && job.Status == jobs.Dead
	})

	// MaxRetries is 2 in this server, so the job runs three times.
	if got := runs.Load(); got != 3 {
		t.Errorf("the handler ran %d times, want 3", got)
	}

	job, _ := backing.Get(t.Context(), made.ID)
	if job.LastError != "the host refused the connection" {
		t.Errorf("last error = %q", job.LastError)
	}
}

// A stopping worker finishes the job it is running and reports it.
//
// The old worker gave every job context.Background() and then closed the
// connection, so a job in flight was abandoned with its lease held. Nobody
// could run it until that lease expired, which on the default settings is
// thirty seconds of nothing happening for every job that was in flight.
func TestAStoppingWorkerFinishesTheJobItIsRunning(t *testing.T) {
	backing, dial := serve(t)
	w := newWorker(t, dial)

	started := make(chan struct{})
	var once sync.Once
	w.Handle("slow", func(ctx context.Context, job worker.Job) error {
		once.Do(func() { close(started) })
		time.Sleep(150 * time.Millisecond)
		return nil
	})

	made, err := backing.Create(t.Context(), store.NewJob{Type: "slow"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()

	<-started
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the worker did not stop")
	}

	// Run has returned, so the drain is over and the report has been sent.
	job, err := backing.Get(t.Context(), made.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if job.Status != jobs.Succeeded {
		t.Errorf("status = %q, want the job finished and reported before the worker stopped", job.Status)
	}
}

// A worker with no handlers refuses to start.
//
// It would otherwise fail every job it was given, at the full retry count,
// until each reached the dead letter queue.
func TestAWorkerWithNoHandlersRefusesToStart(t *testing.T) {
	_, dial := serve(t)
	w := newWorker(t, dial)

	if err := w.Run(t.Context()); err == nil {
		t.Fatal("a worker with no handlers started")
	}
}

func TestLastAttemptSaysWhenAFailureIsFinal(t *testing.T) {
	cases := []struct {
		attempts, maxRetries int
		want                 bool
	}{
		{attempts: 1, maxRetries: 2, want: false},
		{attempts: 2, maxRetries: 2, want: false},
		{attempts: 3, maxRetries: 2, want: true},
		{attempts: 1, maxRetries: 0, want: true},
	}
	for _, c := range cases {
		job := worker.Job{Attempts: c.attempts, MaxRetries: c.maxRetries}
		if got := job.LastAttempt(); got != c.want {
			t.Errorf("attempt %d of %d retries: LastAttempt() = %v, want %v",
				c.attempts, c.maxRetries, got, c.want)
		}
	}
}

func run(t *testing.T, w *worker.Worker) func() {
	t.Helper()

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = w.Run(ctx)
	}()

	return func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("the worker did not stop")
		}
	}
}
