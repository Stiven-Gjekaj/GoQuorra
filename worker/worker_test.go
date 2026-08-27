package worker_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

	made, _, err := backing.Create(t.Context(), store.NewJob{
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

	made, _, err := backing.Create(t.Context(), store.NewJob{Type: "nobody_handles_this"})
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

	bad, _, err := backing.Create(t.Context(), store.NewJob{Type: "explodes"})
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
	later, _, err := backing.Create(t.Context(), store.NewJob{Type: "fine"})
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

	made, _, err := backing.Create(t.Context(), store.NewJob{Type: "always_fails"})
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

// A handler that refuses a job runs once and the job is buried.
//
// This is the same server and the same shape as the failing handler above,
// which runs three times, so the run count is the whole assertion. It also
// covers both ways of writing a refusal, because a handler author will write
// whichever one reads better and both have to work.
func TestARefusingHandlerRunsOnceAndBuriesTheJob(t *testing.T) {
	wrapped := errors.New("the payload names no account")

	// Each form carries the words the handler wrote, and each form writes
	// them differently, so each states its own. A single expected string here
	// would pass for the wrong reason on two of the three.
	forms := map[string]struct {
		refusal error
		says    string
	}{
		"Permanent wraps it":       {worker.Permanent(wrapped), wrapped.Error()},
		"the handler wraps it":     {fmt.Errorf("%w: %w", worker.ErrPermanent, wrapped), wrapped.Error()},
		"wrapped the other way up": {fmt.Errorf("no account: %w", worker.ErrPermanent), "no account"},
	}

	for name, form := range forms {
		refusal, says := form.refusal, form.says
		t.Run(name, func(t *testing.T) {
			backing, dial := serve(t)
			w := newWorker(t, dial)

			var runs atomic.Int64
			w.Handle("refuses", func(context.Context, worker.Job) error {
				runs.Add(1)
				return refusal
			})

			made, _, err := backing.Create(t.Context(), store.NewJob{Type: "refuses"})
			if err != nil {
				t.Fatalf("Create: %v", err)
			}

			stop := run(t, w)
			defer stop()

			waitFor(t, "the job to be buried", func() bool {
				job, err := backing.Get(t.Context(), made.ID)
				return err == nil && job.Status == jobs.Dead
			})

			// MaxRetries is 2 in this server, so a plain failure here would
			// run three times. That is what this number is measured against.
			if got := runs.Load(); got != 1 {
				t.Errorf("the handler ran %d times, want 1", got)
			}

			// The reason still reaches the row, and it carries the handler's
			// own words and not only the sentinel.
			job, _ := backing.Get(t.Context(), made.ID)
			if !strings.Contains(job.LastError, says) {
				t.Errorf("last error = %q, and it does not say %q", job.LastError, says)
			}
		})
	}
}

// A worker keeps retrying a failure that does not say it is permanent.
//
// The pair of tests is the point. Without this one, a bug that made every
// failure permanent would leave the refusal test passing and the failure test
// above is the only thing standing between that bug and production.
func TestAPanicIsNotTreatedAsPermanent(t *testing.T) {
	backing, dial := serve(t)
	w := newWorker(t, dial)

	var runs atomic.Int64
	w.Handle("panics", func(context.Context, worker.Job) error {
		runs.Add(1)
		panic("a nil map entry")
	})

	made, _, err := backing.Create(t.Context(), store.NewJob{Type: "panics"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	stop := run(t, w)
	defer stop()

	waitFor(t, "the job to be buried", func() bool {
		job, err := backing.Get(t.Context(), made.ID)
		return err == nil && job.Status == jobs.Dead
	})

	// A panic on one payload says nothing about the next attempt, so it is
	// retried like any other failure: three runs and not one.
	if got := runs.Load(); got != 3 {
		t.Errorf("the handler ran %d times, want 3, so a panic was read as permanent", got)
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

	made, _, err := backing.Create(t.Context(), store.NewJob{Type: "slow"})
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

// A handler slower than its lease finishes.
//
// This is the case the heartbeat exists for. Before it, the reclaimer took
// the job at the expiry and gave it to somebody else while the first worker
// was still running it, so the work happened twice and nothing said so.
func TestASlowHandlerKeepsItsJob(t *testing.T) {
	backing, dial := serve(t)

	w, err := worker.New(worker.Config{
		ID:         "patient",
		ServerAddr: "passthrough:///bufnet",
		Queues:     []string{"default"},
		MaxJobs:    1,
		// The server refuses a lease under a second, so asking for less than
		// that gets a second anyway. The first version of this test asked for
		// 300ms against a 900ms handler and passed with the heartbeat turned
		// off, because the lease it actually held was longer than the work.
		// The handler below runs for two and a half times the real lease.
		LeaseTTL:       time.Second,
		HeartbeatEvery: 200 * time.Millisecond,
		PollEvery:      5 * time.Millisecond,
		ShutdownGrace:  5 * time.Second,
		Logger:         slog.New(slog.NewTextHandler(io.Discard, nil)),
		DialOptions:    dial,
	})
	if err != nil {
		t.Fatalf("worker.New: %v", err)
	}
	t.Cleanup(func() { _ = w.Close() })

	var runs atomic.Int64
	w.Handle("slow", func(ctx context.Context, job worker.Job) error {
		runs.Add(1)
		select {
		case <-time.After(2500 * time.Millisecond):
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	})

	made, _, err := backing.Create(t.Context(), store.NewJob{Type: "slow"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// A reclaimer, because without one there is nothing for the heartbeat to
	// save the job from. The first version of this test had no sweeper and
	// passed with the heartbeat disabled, which made it a test of nothing.
	reclaiming, stopReclaiming := context.WithCancel(t.Context())
	defer stopReclaiming()
	go func() {
		tick := time.NewTicker(20 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-reclaiming.Done():
				return
			case <-tick.C:
				_, _ = backing.ReclaimExpired(reclaiming, 10)
			}
		}
	}()

	stop := run(t, w)
	defer stop()

	waitFor(t, "the slow job to finish", func() bool {
		job, err := backing.Get(t.Context(), made.ID)
		return err == nil && job.Status == jobs.Succeeded
	})

	// Once. A job taken back mid-flight is handed out again, so a second run
	// is exactly what a lease that was not held looks like.
	if got := runs.Load(); got != 1 {
		t.Errorf("the handler ran %d times, want once. The lease was not held.", got)
	}
}

// Cancelling a job stops the handler that is running it.
//
// Nothing reaches into the handler. Its next heartbeat is refused, the worker
// cancels the context it gave the handler, and a handler that respects its
// context stops there.
func TestCancellingAJobStopsTheHandler(t *testing.T) {
	backing, dial := serve(t)

	w, err := worker.New(worker.Config{
		ID:             "stoppable",
		ServerAddr:     "passthrough:///bufnet",
		Queues:         []string{"default"},
		MaxJobs:        1,
		LeaseTTL:       2 * time.Second,
		HeartbeatEvery: 20 * time.Millisecond,
		PollEvery:      5 * time.Millisecond,
		ShutdownGrace:  5 * time.Second,
		Logger:         slog.New(slog.NewTextHandler(io.Discard, nil)),
		DialOptions:    dial,
	})
	if err != nil {
		t.Fatalf("worker.New: %v", err)
	}
	t.Cleanup(func() { _ = w.Close() })

	started := make(chan struct{})
	stopped := make(chan error, 1)
	var once sync.Once

	w.Handle("long", func(ctx context.Context, job worker.Job) error {
		once.Do(func() { close(started) })
		select {
		case <-time.After(30 * time.Second):
			stopped <- nil
		case <-ctx.Done():
			// The reason is on the context, so a handler can tell being
			// cancelled from the worker shutting down.
			stopped <- context.Cause(ctx)
		}
		return nil
	})

	made, _, err := backing.Create(t.Context(), store.NewJob{Type: "long"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	stop := run(t, w)
	defer stop()

	<-started
	if _, err := backing.Cancel(t.Context(), made.ID, ""); err != nil {
		t.Fatalf("Cancel: %v", err)
	}

	select {
	case cause := <-stopped:
		if !errors.Is(cause, worker.ErrLeaseLost) {
			t.Errorf("the handler stopped because of %v, want ErrLeaseLost", cause)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the handler was never stopped")
	}

	// And the job stays cancelled, rather than the worker reporting over it.
	time.Sleep(100 * time.Millisecond)
	job, _ := backing.Get(t.Context(), made.ID)
	if job.Status != jobs.Cancelled {
		t.Errorf("status = %q, want the job to stay cancelled", job.Status)
	}
}

func TestTheHeartbeatIntervalDefaultsToAThirdOfTheLease(t *testing.T) {
	_, dial := serve(t)

	w, err := worker.New(worker.Config{
		ServerAddr:  "passthrough:///bufnet",
		LeaseTTL:    30 * time.Second,
		Logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		DialOptions: dial,
	})
	if err != nil {
		t.Fatalf("worker.New: %v", err)
	}
	t.Cleanup(func() { _ = w.Close() })

	// A third, so that two heartbeats can be lost to a slow network before
	// the lease actually runs out.
	if got := w.HeartbeatEvery(); got != 10*time.Second {
		t.Errorf("the interval is %s, want a third of the lease", got)
	}
}

func TestAHandlerCanKeepWhatItProduced(t *testing.T) {
	backing, dial := serve(t)
	w := newWorker(t, dial)

	w.HandleResult("count", func(_ context.Context, job worker.Job) (any, error) {
		return map[string]int{"rows": 41}, nil
	})

	made, _, err := backing.Create(t.Context(), store.NewJob{Type: "count"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	stop := run(t, w)
	defer stop()

	waitFor(t, "the job to finish", func() bool {
		job, err := backing.Get(t.Context(), made.ID)
		return err == nil && job.Status == jobs.Succeeded
	})

	job, _ := backing.Get(t.Context(), made.ID)
	if string(job.Result) != `{"rows":41}` {
		t.Errorf("the result is %s", job.Result)
	}
}

// A handler that returns something unserialisable has a defect. Reporting
// success with no result would hide it, so the job fails instead.
func TestAResultThatCannotBeEncodedFailsTheJob(t *testing.T) {
	backing, dial := serve(t)
	w := newWorker(t, dial)

	w.HandleResult("bad", func(_ context.Context, job worker.Job) (any, error) {
		// A channel cannot be marshalled.
		return make(chan int), nil
	})

	made, _, err := backing.Create(t.Context(), store.NewJob{Type: "bad"})
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
	if !strings.Contains(job.LastError, "not JSON") {
		t.Errorf("the reason does not say what went wrong: %q", job.LastError)
	}
}

// A handler registered with Handle keeps nothing, and that path still works.
func TestAHandlerWithNoResultStoresNone(t *testing.T) {
	backing, dial := serve(t)
	w := newWorker(t, dial)

	w.Handle("plain", func(context.Context, worker.Job) error { return nil })

	made, _, err := backing.Create(t.Context(), store.NewJob{Type: "plain"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	stop := run(t, w)
	defer stop()

	waitFor(t, "the job to finish", func() bool {
		job, err := backing.Get(t.Context(), made.ID)
		return err == nil && job.Status == jobs.Succeeded
	})

	job, _ := backing.Get(t.Context(), made.ID)
	if len(job.Result) != 0 {
		t.Errorf("a plain handler stored the result %s", job.Result)
	}
}

// The context a handler is given does not end at LeaseExpiresAt.
//
// The documentation on that field said it did. Nothing set a deadline, and
// setting one would have been wrong: the heartbeat pushes the lease out while
// the handler runs, so a deadline at the value captured when the job was
// handed over would stop work that is being kept alive on purpose.
//
// A handler that ran past its first expiry proves both halves at once. It
// finishes, so no deadline cut it off, and it finishes successfully, so the
// heartbeat did its job.
func TestTheHandlerContextDoesNotEndAtTheFirstLeaseExpiry(t *testing.T) {
	backing, dial := serve(t)

	// A lease short enough that the handler crosses it. The server refuses a
	// lease under a second, so a second is the shortest real one, and the
	// handler below runs past it.
	w, err := worker.New(worker.Config{
		ID:             "patient",
		ServerAddr:     "passthrough:///bufnet",
		Queues:         []string{"default"},
		MaxJobs:        1,
		LeaseTTL:       time.Second,
		HeartbeatEvery: 200 * time.Millisecond,
		PollEvery:      5 * time.Millisecond,
		ShutdownGrace:  5 * time.Second,
		Logger:         slog.New(slog.NewTextHandler(io.Discard, nil)),
		DialOptions:    dial,
	})
	if err != nil {
		t.Fatalf("worker.New: %v", err)
	}
	t.Cleanup(func() { _ = w.Close() })

	var expiry atomic.Value
	var ranPast, deadlineSet atomic.Bool

	w.Handle("slow", func(ctx context.Context, job worker.Job) error {
		expiry.Store(job.LeaseExpiresAt)
		if _, ok := ctx.Deadline(); ok {
			deadlineSet.Store(true)
		}
		// Past the lease the job arrived with. The heartbeat keeps it.
		select {
		case <-time.After(1600 * time.Millisecond):
			ranPast.Store(true)
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	})

	made, _, err := backing.Create(t.Context(), store.NewJob{Type: "slow"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	stop := run(t, w)
	defer stop()

	waitFor(t, "the job to finish", func() bool {
		job, err := backing.Get(t.Context(), made.ID)
		return err == nil && job.Status == jobs.Succeeded
	})

	if deadlineSet.Load() {
		t.Error("the handler context carries a deadline, which the heartbeat would fight")
	}
	if !ranPast.Load() {
		t.Error("the handler did not run past the expiry it was given, so this proves nothing")
	}

	// And the moment it was given really was in the past by the time it
	// finished, so the test is measuring what it says it is.
	given, _ := expiry.Load().(time.Time)
	if time.Now().Before(given) {
		t.Errorf("the handler finished before the expiry it was given (%s), so it never crossed it", given)
	}
}
