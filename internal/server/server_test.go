package server_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Stiven-Gjekaj/GoQuorra/internal/auth"
	"github.com/Stiven-Gjekaj/GoQuorra/internal/config"
	"github.com/Stiven-Gjekaj/GoQuorra/internal/jobs"
	"github.com/Stiven-Gjekaj/GoQuorra/internal/quorrapb"
	"github.com/Stiven-Gjekaj/GoQuorra/internal/server"
	"github.com/Stiven-Gjekaj/GoQuorra/internal/store"
	"github.com/Stiven-Gjekaj/GoQuorra/internal/store/memory"
	"github.com/Stiven-Gjekaj/GoQuorra/worker"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/types/known/durationpb"
)

const key = "a-key-that-somebody-chose"

// start runs a real server on ports the operating system chooses.
//
// Port zero, so that two tests running at once cannot collide, and so that
// nothing here depends on 8080 being free on the machine running it.
func start(t *testing.T) (*server.Server, store.Store) {
	t.Helper()

	cfg := &config.Server{
		HTTPAddr:      "127.0.0.1:0",
		GRPCAddr:      "127.0.0.1:0",
		Backend:       "memory",
		Keys:          testKeys(t, key),
		Policy:        jobs.Policy{MaxRetries: 2, Base: time.Millisecond, Max: 10 * time.Millisecond},
		ReclaimEvery:  20 * time.Millisecond,
		ReclaimBatch:  100,
		StatsEvery:    20 * time.Millisecond,
		ShutdownGrace: 5 * time.Second,
		MaxBodyBytes:  1 << 16,
	}

	backing := memory.New(store.Options{Policy: cfg.Policy})
	s := server.New(cfg, backing, slog.New(slog.NewTextHandler(io.Discard, nil)))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()

	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("Run: %v", err)
			}
		case <-time.After(10 * time.Second):
			t.Error("the server did not stop")
		}
	})

	select {
	case <-s.Ready():
	case <-time.After(10 * time.Second):
		t.Fatal("the server did not start")
	}

	return s, backing
}

func submit(t *testing.T, s *server.Server, body string) string {
	t.Helper()

	request, err := http.NewRequest(http.MethodPost,
		"http://"+s.HTTPAddr().String()+"/v1/jobs", strings.NewReader(body))
	if err != nil {
		t.Fatalf("cannot build the request: %v", err)
	}
	request.Header.Set("X-API-Key", key)
	request.Header.Set("Content-Type", "application/json")

	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("cannot submit: %v", err)
	}
	defer func() { _ = response.Body.Close() }()

	if response.StatusCode != http.StatusCreated {
		raw, _ := io.ReadAll(response.Body)
		t.Fatalf("submit gave %s: %s", response.Status, raw)
	}

	var created struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(response.Body).Decode(&created); err != nil {
		t.Fatalf("the answer is not JSON: %v", err)
	}
	return created.ID
}

func waitFor(t *testing.T, why string, check func() bool) {
	t.Helper()

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if check() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", why)
}

// A job submitted over HTTP is run by a worker over gRPC.
//
// This is the whole product in one test, and it is the test that the version
// before the rebuild could not have passed at any point in its history: the
// module did not compile, and the gRPC codec would have refused the first
// call if it had.
func TestAJobTravelsFromHTTPToAWorker(t *testing.T) {
	s, backing := start(t)

	id := submit(t, s, `{"type":"greet","payload":{"name":"world"}}`)

	w, err := worker.New(worker.Config{
		ID:            "test-worker",
		ServerAddr:    s.GRPCAddr().String(),
		Queues:        []string{"default"},
		MaxJobs:       5,
		LeaseTTL:      30 * time.Second,
		PollEvery:     5 * time.Millisecond,
		ShutdownGrace: 5 * time.Second,
		Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("worker.New: %v", err)
	}
	defer func() { _ = w.Close() }()

	seen := make(chan string, 1)
	w.Handle("greet", func(_ context.Context, job worker.Job) error {
		var payload struct {
			Name string `json:"name"`
		}
		if err := job.Decode(&payload); err != nil {
			return err
		}
		select {
		case seen <- payload.Name:
		default:
		}
		return nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = w.Run(ctx) }()

	select {
	case name := <-seen:
		if name != "world" {
			t.Errorf("the handler saw %q", name)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the worker never received the job")
	}

	waitFor(t, "the job to be marked as done", func() bool {
		job, err := backing.Get(context.Background(), id)
		return err == nil && job.Status == jobs.Succeeded
	})
}

// A worker that stops without reporting has its job taken back.
//
// The lease is taken with a plain gRPC client and then abandoned, because
// that is what a process losing power looks like from the server: the job was
// handed out and nothing is ever said about it again. The worker package
// cannot be used to write this test, and the reason is worth recording. Its
// handlers are given a context that ends when the lease does, so a handler
// that respects its context returns at that moment and the worker reports the
// failure itself. That is the right behaviour, and it means the SDK never
// produces the state this test needs.
//
// The old server had no reclaimer at all, so a job in this state stayed
// leased for as long as the table lived: no worker could take it, and nothing
// anywhere reported that it was stuck.
func TestAnAbandonedLeaseIsTakenBack(t *testing.T) {
	s, backing := start(t)

	id := submit(t, s, `{"type":"work","payload":{}}`)

	conn, err := grpc.NewClient(s.GRPCAddr().String(),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("cannot connect: %v", err)
	}
	defer func() { _ = conn.Close() }()

	leased, err := quorrapb.NewQueueServiceClient(conn).Lease(context.Background(), &quorrapb.LeaseRequest{
		WorkerId: "the-worker-that-died",
		Queue:    "default",
		MaxJobs:  1,
		LeaseTtl: durationpb.New(300 * time.Millisecond),
	})
	if err != nil {
		t.Fatalf("Lease: %v", err)
	}
	if len(leased.GetJobs()) != 1 {
		t.Fatalf("leased %d jobs, want 1", len(leased.GetJobs()))
	}

	held, err := backing.Get(context.Background(), id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if held.Status != jobs.Leased {
		t.Fatalf("status = %q, want the job held", held.Status)
	}

	// Nothing is ever reported. The lease runs out and the reclaimer acts.
	waitFor(t, "the lease to be reclaimed", func() bool {
		job, err := backing.Get(context.Background(), id)
		return err == nil && job.Status == jobs.Pending
	})

	job, _ := backing.Get(context.Background(), id)
	if job.Attempts != 1 {
		t.Errorf("attempts = %d, want the one the dead worker used", job.Attempts)
	}
	if !strings.Contains(job.LastError, "the-worker-that-died") {
		t.Errorf("the reason does not name the worker that held it: %q", job.LastError)
	}

	// And the job really is available again, rather than merely marked.
	//
	// Asked for in a loop rather than once. A reclaimed job is treated as a
	// failed one, so it carries a backoff before it may run again, and a
	// single request sent immediately can arrive inside that wait. The first
	// version of this test did exactly that and passed or failed depending on
	// how busy the machine was.
	var after *quorrapb.LeaseResponse
	waitFor(t, "the job to be offered to another worker", func() bool {
		got, err := quorrapb.NewQueueServiceClient(conn).Lease(context.Background(), &quorrapb.LeaseRequest{
			WorkerId: "the-worker-that-took-over",
			Queue:    "default",
			MaxJobs:  1,
			LeaseTtl: durationpb.New(30 * time.Second),
		})
		if err != nil {
			t.Fatalf("Lease: %v", err)
		}
		if len(got.GetJobs()) == 0 {
			return false
		}
		after = got
		return true
	})

	if after.GetJobs()[0].GetLeaseId() == leased.GetJobs()[0].GetLeaseId() {
		t.Error("the new lease carries the same identifier as the abandoned one, so the dead worker could still report on it")
	}
	if after.GetJobs()[0].GetAttempts() != 2 {
		t.Errorf("attempts = %d on the second run, want 2", after.GetJobs()[0].GetAttempts())
	}
}

func TestTheHealthRoutesAnswer(t *testing.T) {
	s, _ := start(t)

	for _, path := range []string{"/healthz", "/readyz", "/metrics"} {
		response, err := http.Get("http://" + s.HTTPAddr().String() + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		_ = response.Body.Close()
		if response.StatusCode != http.StatusOK {
			t.Errorf("GET %s = %s", path, response.Status)
		}
	}
}

// The queue gauge is refreshed by the background loop.
//
// The gauge was declared and never set in the old server, so every panel
// built on it read nothing for ever.
func TestTheQueueGaugeIsRefreshed(t *testing.T) {
	s, _ := start(t)

	submit(t, s, `{"type":"work","payload":{}}`)

	waitFor(t, "the gauge to hold the pending job", func() bool {
		response, err := http.Get("http://" + s.HTTPAddr().String() + "/metrics")
		if err != nil {
			return false
		}
		defer func() { _ = response.Body.Close() }()

		page, err := io.ReadAll(response.Body)
		if err != nil {
			return false
		}
		return strings.Contains(string(page), `quorra_queue_length{queue="default",status="pending"} 1`)
	})
}

// The sweep removes a finished job once it is old enough.
func TestFinishedJobsAreRemovedOnceTheyAreOldEnough(t *testing.T) {
	cfg := &config.Server{
		HTTPAddr: "127.0.0.1:0", GRPCAddr: "127.0.0.1:0",
		Backend: "memory", Keys: testKeys(t, key),
		Policy:        jobs.Policy{MaxRetries: 1, Base: time.Millisecond, Max: time.Millisecond},
		ReclaimEvery:  time.Hour,
		ReclaimBatch:  100,
		StatsEvery:    time.Hour,
		ShutdownGrace: 5 * time.Second,
		MaxBodyBytes:  1 << 16,

		// Anything cancelled for longer than an instant.
		Retention:      map[jobs.Status]time.Duration{jobs.Cancelled: time.Millisecond},
		RetentionEvery: 20 * time.Millisecond,
		RetentionBatch: 100,
	}

	backing := memory.New(store.Options{Policy: cfg.Policy})
	s := server.New(cfg, backing, slog.New(slog.NewTextHandler(io.Discard, nil)))

	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = s.Run(ctx) }()
	t.Cleanup(func() { cancel(); time.Sleep(50 * time.Millisecond) })

	select {
	case <-s.Ready():
	case <-time.After(10 * time.Second):
		t.Fatal("the server did not start")
	}

	id := submit(t, s, `{"type":"work"}`)
	if _, err := backing.Cancel(context.Background(), id); err != nil {
		t.Fatalf("Cancel: %v", err)
	}

	waitFor(t, "the cancelled job to be removed", func() bool {
		_, err := backing.Get(context.Background(), id)
		return errors.Is(err, store.ErrNotFound)
	})

	// A job that was not cancelled is untouched: only the status with a
	// retention set is swept.
	other := submit(t, s, `{"type":"work"}`)
	time.Sleep(100 * time.Millisecond)
	if _, err := backing.Get(context.Background(), other); err != nil {
		t.Errorf("a waiting job was removed: %v", err)
	}
}

// With no retention set, nothing is ever removed. This is the default, and
// the sweep has to be a loop that does not run rather than one that runs and
// finds nothing.
func TestNothingIsRemovedWhenNoRetentionIsSet(t *testing.T) {
	s, backing := start(t)

	id := submit(t, s, `{"type":"work"}`)
	if _, err := backing.Cancel(context.Background(), id); err != nil {
		t.Fatalf("Cancel: %v", err)
	}

	time.Sleep(150 * time.Millisecond)
	if _, err := backing.Get(context.Background(), id); err != nil {
		t.Errorf("a job was removed with no retention set: %v", err)
	}
}

// testKeys builds a one key set for a test harness.
//
// Named "test" and allowed to write, because these harnesses drive every
// route. A test about scopes builds its own set rather than using this.
func testKeys(t *testing.T, secret string) *auth.Set {
	t.Helper()
	key, err := auth.NewKey("test", auth.Write, secret)
	if err != nil {
		t.Fatalf("auth.NewKey: %v", err)
	}
	set, err := auth.NewSet(key)
	if err != nil {
		t.Fatalf("auth.NewSet: %v", err)
	}
	return set
}
