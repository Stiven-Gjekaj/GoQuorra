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
	"github.com/Stiven-Gjekaj/GoQuorra/internal/metrics"
	"github.com/Stiven-Gjekaj/GoQuorra/internal/quorrapb"
	"github.com/Stiven-Gjekaj/GoQuorra/internal/server"
	"github.com/Stiven-Gjekaj/GoQuorra/internal/store"
	"github.com/Stiven-Gjekaj/GoQuorra/internal/store/memory"
	"github.com/Stiven-Gjekaj/GoQuorra/worker"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
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
		APIKey:        key,
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

	// The key goes on by hand here, because this test uses the generated
	// client directly rather than the worker package that adds it.
	worked := metadata.AppendToOutgoingContext(context.Background(), "x-api-key", key)

	leased, err := quorrapb.NewQueueServiceClient(conn).Lease(worked, &quorrapb.LeaseRequest{
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
		got, err := quorrapb.NewQueueServiceClient(conn).Lease(worked, &quorrapb.LeaseRequest{
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
	if _, err := backing.Cancel(context.Background(), id, ""); err != nil {
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

// A schedule produces a job for each window it is due.
func TestAScheduleProducesJobs(t *testing.T) {
	backing := memory.New(store.Options{})
	t.Cleanup(func() { _ = backing.Close() })

	ctx := context.Background()
	made, err := backing.CreateSchedule(ctx, store.NewSchedule{
		Name: "hourly", Cron: "0 * * * *", CatchUp: jobs.CatchUpAll,
		Type: "report", Payload: []byte(`{"kind":"summary"}`),
	})
	if err != nil {
		t.Fatalf("CreateSchedule: %v", err)
	}

	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	start := time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)

	// The first pass produces nothing and marks the schedule at now. A new
	// schedule that caught up from the year one would be the alternative.
	if err := server.ProduceOnce(ctx, backing, metrics.New(), quiet, start); err != nil {
		t.Fatalf("the first pass: %v", err)
	}
	if page, _ := backing.List(ctx, store.Filter{Limit: 100}); len(page) != 0 {
		t.Fatalf("a new schedule produced %d jobs on its first pass", len(page))
	}

	// Three hours later, three windows are due.
	if err := server.ProduceOnce(ctx, backing, metrics.New(), quiet, start.Add(3*time.Hour)); err != nil {
		t.Fatalf("the second pass: %v", err)
	}

	page, err := backing.List(ctx, store.Filter{Limit: 100})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(page) != 3 {
		t.Fatalf("the schedule produced %d jobs, want 3", len(page))
	}
	for _, job := range page {
		if job.ScheduleID != made.ID {
			t.Errorf("a job says it came from %q", job.ScheduleID)
		}
		if job.Type != "report" || string(job.Payload) != `{"kind":"summary"}` {
			t.Errorf("a job is %+v, and the schedule asked for a report", job)
		}
	}
}

// A pass that runs twice over the same window produces one job.
//
// Two servers run this loop, and the idempotency key on each firing is what
// makes that safe. Without it a two server deployment doubles every schedule.
func TestTwoPassesOverOneWindowProduceOneJob(t *testing.T) {
	backing := memory.New(store.Options{})
	t.Cleanup(func() { _ = backing.Close() })

	ctx := context.Background()
	if _, err := backing.CreateSchedule(ctx, store.NewSchedule{
		Name: "hourly", Cron: "0 * * * *", CatchUp: jobs.CatchUpAll, Type: "report",
	}); err != nil {
		t.Fatalf("CreateSchedule: %v", err)
	}

	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	start := time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)
	_ = server.ProduceOnce(ctx, backing, metrics.New(), quiet, start)

	// The same moment, twice. The mark stops the second pass finding the
	// window, and the key stops it even if the mark had not been written.
	at := start.Add(time.Hour)
	for i := 0; i < 2; i++ {
		if err := server.ProduceOnce(ctx, backing, metrics.New(), quiet, at); err != nil {
			t.Fatalf("pass %d: %v", i, err)
		}
	}

	page, _ := backing.List(ctx, store.Filter{Limit: 100})
	if len(page) != 1 {
		t.Errorf("two passes over one window produced %d jobs, want 1", len(page))
	}
}

// A window another server already produced is not produced again.
//
// The mark alone stops one server producing a window twice. It does not stop
// two servers producing the same window at the same moment: both read the
// same mark, and both decide the window is due. The idempotency key on each
// firing is what makes that safe, and this is the case that exercises it.
func TestAWindowAnotherServerProducedIsNotProducedAgain(t *testing.T) {
	backing := memory.New(store.Options{})
	t.Cleanup(func() { _ = backing.Close() })

	ctx := context.Background()
	made, err := backing.CreateSchedule(ctx, store.NewSchedule{
		Name: "hourly", Cron: "0 * * * *", CatchUp: jobs.CatchUpAll, Type: "report",
	})
	if err != nil {
		t.Fatalf("CreateSchedule: %v", err)
	}

	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	start := time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)
	_ = server.ProduceOnce(ctx, backing, metrics.New(), quiet, start)

	// The other server got there first: the job exists and the mark has not
	// moved, which is exactly the state a race leaves behind.
	window := time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC)
	first, created, err := backing.Create(ctx, store.NewJob{
		Type: "report", ScheduleID: made.ID,
		IdempotencyKey: store.FiringKey(made.ID, window),
	})
	if err != nil || !created {
		t.Fatalf("the other server could not submit: %v, created %v", err, created)
	}

	if err := server.ProduceOnce(ctx, backing, metrics.New(), quiet, window); err != nil {
		t.Fatalf("ProduceOnce: %v", err)
	}

	page, _ := backing.List(ctx, store.Filter{Limit: 100})
	if len(page) != 1 {
		t.Fatalf("the window was produced %d times, want 1", len(page))
	}
	if page[0].ID != first.ID {
		t.Errorf("the job is %s, want the one the other server submitted", page[0].ID)
	}
}

// A schedule that is switched off produces nothing.
func TestAScheduleThatIsOffProducesNothing(t *testing.T) {
	backing := memory.New(store.Options{})
	t.Cleanup(func() { _ = backing.Close() })

	ctx := context.Background()
	if _, err := backing.CreateSchedule(ctx, store.NewSchedule{
		Name: "hourly", Cron: "0 * * * *", CatchUp: jobs.CatchUpAll, Type: "report",
	}); err != nil {
		t.Fatalf("CreateSchedule: %v", err)
	}
	if _, err := backing.SetScheduleEnabled(ctx, "hourly", false); err != nil {
		t.Fatalf("SetScheduleEnabled: %v", err)
	}

	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	start := time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)
	_ = server.ProduceOnce(ctx, backing, metrics.New(), quiet, start)
	_ = server.ProduceOnce(ctx, backing, metrics.New(), quiet, start.Add(5*time.Hour))

	if page, _ := backing.List(ctx, store.Filter{Limit: 100}); len(page) != 0 {
		t.Errorf("a schedule that is off produced %d jobs", len(page))
	}
}

// A worker nobody has seen is removed, in the same sweep as the jobs.
//
// A worker identifier is usually the name of a container, so a deployment
// retires every row in that table and writes a new set. Without this the
// table grows once for each worker on each release, for ever, and nothing
// anywhere reports it.
func TestWorkersNobodyHasSeenAreRemoved(t *testing.T) {
	cfg := &config.Server{
		HTTPAddr: "127.0.0.1:0", GRPCAddr: "127.0.0.1:0",
		Backend: "memory", Keys: testKeys(t, key),
		Policy:        jobs.Policy{MaxRetries: 1, Base: time.Millisecond, Max: time.Millisecond},
		ReclaimEvery:  time.Hour,
		ReclaimBatch:  100,
		StatsEvery:    time.Hour,
		ShutdownGrace: 5 * time.Second,
		MaxBodyBytes:  1 << 16,

		// No job retention at all, so this proves the worker sweep runs on
		// its own rather than riding on the job one.
		RetentionEvery:  20 * time.Millisecond,
		RetentionBatch:  100,
		WorkerRetention: time.Millisecond,
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

	// An ask that finds nothing is still an ask, and is what puts the worker
	// in the table.
	if _, err := backing.Lease(context.Background(), store.LeaseRequest{
		Queue: "default", WorkerID: "the-old-pod", Limit: 1, TTL: time.Minute,
	}); err != nil {
		t.Fatalf("Lease: %v", err)
	}

	seen, err := backing.Workers(context.Background())
	if err != nil || len(seen) != 1 {
		t.Fatalf("Workers: %v, %d rows", err, len(seen))
	}

	waitFor(t, "the worker nobody has seen to be removed", func() bool {
		left, err := backing.Workers(context.Background())
		return err == nil && len(left) == 0
	})
}

// With no retention set, nothing is ever removed. This is the default, and
// the sweep has to be a loop that does not run rather than one that runs and
// finds nothing.
func TestNothingIsRemovedWhenNoRetentionIsSet(t *testing.T) {
	s, backing := start(t)

	id := submit(t, s, `{"type":"work"}`)
	if _, err := backing.Cancel(context.Background(), id, ""); err != nil {
		t.Fatalf("Cancel: %v", err)
	}

	// And the worker table is left alone too, which is the same default.
	if _, err := backing.Lease(context.Background(), store.LeaseRequest{
		Queue: "default", WorkerID: "still-here", Limit: 1, TTL: time.Minute,
	}); err != nil {
		t.Fatalf("Lease: %v", err)
	}

	time.Sleep(150 * time.Millisecond)
	if _, err := backing.Get(context.Background(), id); err != nil {
		t.Errorf("a job was removed with no retention set: %v", err)
	}
	if seen, err := backing.Workers(context.Background()); err != nil || len(seen) != 1 {
		t.Errorf("a worker was removed with no retention set: %v, %d rows", err, len(seen))
	}
}

// testKeys builds a one key set for a test harness.
//
// Named "test" and allowed everything, because these harnesses drive the HTTP
// routes and the worker protocol from one process. A test about scopes builds
// its own set rather than using this.
func testKeys(t *testing.T, secret string) *auth.Set {
	t.Helper()
	key, err := auth.NewKey("test", auth.Everything, secret)
	if err != nil {
		t.Fatalf("auth.NewKey: %v", err)
	}
	set, err := auth.NewSet(key)
	if err != nil {
		t.Fatalf("auth.NewSet: %v", err)
	}
	return set
}
