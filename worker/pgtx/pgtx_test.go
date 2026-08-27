package pgtx_test

import (
	"context"
	"encoding/json"
	"log/slog"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Stiven-Gjekaj/GoQuorra/internal/auth"
	"github.com/Stiven-Gjekaj/GoQuorra/internal/jobs"
	"github.com/Stiven-Gjekaj/GoQuorra/internal/metrics"
	"github.com/Stiven-Gjekaj/GoQuorra/internal/pgtest"
	"github.com/Stiven-Gjekaj/GoQuorra/internal/quorrapb"
	"github.com/Stiven-Gjekaj/GoQuorra/internal/rpc"
	"github.com/Stiven-Gjekaj/GoQuorra/internal/store"
	"github.com/Stiven-Gjekaj/GoQuorra/internal/store/postgres"
	"github.com/Stiven-Gjekaj/GoQuorra/worker"
	"github.com/Stiven-Gjekaj/GoQuorra/worker/pgtx"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
)

const workerKey = "a-worker-key-that-is-long-enough"

// record is a log a test reads back.
//
// The worker writes a line when a report is refused. The whole point of
// worker.ErrAlreadyReported is that no such line appears, so the log is the
// one place that rule is visible.
type record struct {
	mu      sync.Mutex
	written strings.Builder
}

func (r *record) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.written.Write(p)
}

func (r *record) text() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.written.String()
}

// rig is one test's database, server and worker.
type rig struct {
	pool   *pgxpool.Pool
	store  *postgres.Store
	worker *worker.Worker
	queue  string
	log    *record
}

// Everything here runs against a real database and a real worker.
//
// The whole of this package is one transaction covering a handler's writes
// and the report of the job. There is nothing to test without a database, and
// nothing to test without the worker either: the part that matters is that
// the worker keeps its hands off a job the handler recorded.
func harness(t *testing.T) rig {
	t.Helper()

	ctx := context.Background()
	pool := pgtest.Pool(t)

	// The table a handler writes to, which stands for whatever a real one
	// would write. Made before the reset below, so that it is emptied along
	// with everything else.
	if _, err := pool.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS pgtx_side_effect (
			job_id UUID PRIMARY KEY,
			note   TEXT NOT NULL
		)`); err != nil {
		t.Fatalf("cannot make the side effect table: %v", err)
	}
	pgtest.Reset(t, pool)

	backing := postgres.NewWithPool(pool, store.Options{
		Policy: jobs.Policy{MaxRetries: 2, Base: time.Millisecond, Max: time.Millisecond},
	})

	// A queue this test alone reads. The database is shared with anything
	// else pointed at it, and a job on the default queue is fair game for
	// every worker that is running. One of these tests failed for exactly
	// that reason: another worker leased the job, spent an attempt on it,
	// and the job was buried one run earlier than the test expected.
	queue := strings.ToLower(t.Name())

	written := &record{}
	quiet := slog.New(slog.NewTextHandler(written, nil))
	key, err := auth.NewKey("fleet", auth.Worker, workerKey)
	if err != nil {
		t.Fatalf("auth.NewKey: %v", err)
	}
	keys, err := auth.NewSet(key)
	if err != nil {
		t.Fatalf("auth.NewSet: %v", err)
	}

	guard := rpc.NewGuard(keys)
	listener := bufconn.Listen(1 << 20)
	server := grpc.NewServer(
		grpc.UnaryInterceptor(guard.Unary()),
		grpc.StreamInterceptor(guard.Stream()),
	)
	quorrapb.RegisterQueueServiceServer(server, rpc.New(
		backing, metrics.New(), quiet, rpc.DefaultLimits(), time.Now,
	))
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(server.Stop)

	w, err := worker.New(worker.Config{
		ID:            "pgtx-worker",
		ServerAddr:    "passthrough:///bufnet",
		Queues:        []string{queue},
		MaxJobs:       1,
		LeaseTTL:      30 * time.Second,
		PollEvery:     20 * time.Millisecond,
		ShutdownGrace: 5 * time.Second,
		APIKey:        workerKey,
		Logger:        quiet,
		DialOptions: []grpc.DialOption{
			grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
				return listener.DialContext(ctx)
			}),
			grpc.WithTransportCredentials(insecure.NewCredentials()),
		},
	})
	if err != nil {
		t.Fatalf("worker.New: %v", err)
	}
	t.Cleanup(func() { _ = w.Close() })

	return rig{pool: pool, store: backing, worker: w, queue: queue, log: written}
}

// runUntil starts the worker and stops it when done is closed or time runs
// out.
func runUntil(t *testing.T, w *worker.Worker, done <-chan struct{}) {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	var running sync.WaitGroup
	running.Add(1)
	go func() {
		defer running.Done()
		_ = w.Run(ctx)
	}()

	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Error("the job never finished")
	}
	cancel()
	running.Wait()
}

// The whole point: the side effect and the report commit together.
func TestTheSideEffectAndTheReportCommitTogether(t *testing.T) {
	r := harness(t)
	ctx := context.Background()

	runner, err := pgtx.New(r.pool)
	if err != nil {
		t.Fatalf("pgtx.New: %v", err)
	}

	done := make(chan struct{})
	var once sync.Once
	r.worker.HandleResult("charge", runner.Handle(func(ctx context.Context, tx pgx.Tx, job worker.Job) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO pgtx_side_effect (job_id, note) VALUES ($1, $2)`, job.ID, "charged")
		once.Do(func() { close(done) })
		return err
	}))

	made, _, err := r.store.Create(ctx, store.NewJob{Type: "charge", Queue: r.queue})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	runUntil(t, r.worker, done)

	// The job is finished, and the worker did not report it a second time.
	stored, err := r.store.Get(ctx, made.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if stored.Status != jobs.Succeeded {
		t.Fatalf("the job is %q, want succeeded", stored.Status)
	}

	// And the side effect is there, in the same database.
	var note string
	if err := r.pool.QueryRow(ctx,
		`SELECT note FROM pgtx_side_effect WHERE job_id = $1`, made.ID).Scan(&note); err != nil {
		t.Fatalf("the side effect is not there: %v", err)
	}
	if note != "charged" {
		t.Errorf("the side effect says %q", note)
	}
}

// The worker does not report a job the handler already recorded.
//
// Without worker.ErrAlreadyReported the worker sends a report of its own once
// the handler returns. The row no longer carries that lease, so the server
// refuses it and the worker writes a line saying the result was discarded.
// Nothing else changes: the job is finished either way. The log is the only
// place a second report is visible, so the log is what this reads.
func TestTheWorkerDoesNotReportAgain(t *testing.T) {
	r := harness(t)
	ctx := context.Background()

	runner, _ := pgtx.New(r.pool)

	done := make(chan struct{})
	var once sync.Once
	r.worker.HandleResult("charge", runner.Handle(
		func(ctx context.Context, tx pgx.Tx, job worker.Job) error {
			once.Do(func() { close(done) })
			return nil
		}))

	if _, _, err := r.store.Create(ctx, store.NewJob{Type: "charge", Queue: r.queue}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// runUntil waits for the worker to stop, and the worker waits for the
	// job. Whatever the report path was going to write is written by the
	// time this returns.
	runUntil(t, r.worker, done)

	if written := r.log.text(); strings.Contains(written, "the lease was taken back") {
		t.Errorf("the worker reported a job the handler had already recorded:\n%s", written)
	}
}

// A handler that fails leaves nothing behind, and the job is retried by the
// same rules as any other.
func TestAHandlerThatFailsLeavesNothingBehind(t *testing.T) {
	r := harness(t)
	ctx := context.Background()

	runner, _ := pgtx.New(r.pool)

	runs := make(chan struct{}, 8)
	r.worker.HandleResult("charge", runner.Handle(func(ctx context.Context, tx pgx.Tx, job worker.Job) error {
		if _, err := tx.Exec(ctx,
			`INSERT INTO pgtx_side_effect (job_id, note) VALUES ($1, $2)
				ON CONFLICT (job_id) DO NOTHING`, job.ID, "charged"); err != nil {
			return err
		}
		runs <- struct{}{}
		return errAlwaysFails
	}))

	made, _, err := r.store.Create(ctx, store.NewJob{Type: "charge", Queue: r.queue})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Three runs: the first and two retries, because the policy allows two.
	done := make(chan struct{})
	go func() {
		for i := 0; i < 3; i++ {
			select {
			case <-runs:
			case <-time.After(10 * time.Second):
				return
			}
		}
		close(done)
	}()
	runUntil(t, r.worker, done)

	stored, _ := r.store.Get(ctx, made.ID)
	if stored.Status != jobs.Dead {
		t.Errorf("the job is %q after failing every attempt, want dead", stored.Status)
	}

	// Nothing was left behind by any of the three runs.
	var count int
	if err := r.pool.QueryRow(ctx,
		`SELECT count(*) FROM pgtx_side_effect WHERE job_id = $1`, made.ID).Scan(&count); err != nil {
		t.Fatalf("cannot count the side effect: %v", err)
	}
	if count != 0 {
		t.Errorf("a failed handler left %d rows behind", count)
	}
}

// What a handler produces is kept on the job.
func TestWhatTheHandlerProducesIsKept(t *testing.T) {
	r := harness(t)
	ctx := context.Background()

	runner, _ := pgtx.New(r.pool)

	done := make(chan struct{})
	var once sync.Once
	r.worker.HandleResult("charge", runner.HandleResult(
		func(ctx context.Context, tx pgx.Tx, job worker.Job) (any, error) {
			once.Do(func() { close(done) })
			return map[string]int{"charged": 1250}, nil
		}))

	made, _, err := r.store.Create(ctx, store.NewJob{Type: "charge", Queue: r.queue})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	runUntil(t, r.worker, done)

	// The worker reports asynchronously after the handler returns, so wait
	// for the row rather than reading it once.
	deadline := time.Now().Add(10 * time.Second)
	for {
		stored, _ := r.store.Get(ctx, made.ID)
		if stored != nil && stored.Status == jobs.Succeeded {
			// The column is jsonb, so PostgreSQL returns its own
			// formatting and not the bytes the handler produced. The
			// value is decoded, because comparing the text would test
			// how the database prints JSON.
			var kept map[string]int
			if err := json.Unmarshal(stored.Result, &kept); err != nil {
				t.Fatalf("the job kept %s, which is not JSON: %v", stored.Result, err)
			}
			if kept["charged"] != 1250 {
				t.Errorf("the job kept %s", stored.Result)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the job never succeeded")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// A runner with no pool is refused where it is built.
//
// A pool pointed at another database would compile, run, and give exactly the
// at least once behaviour this package exists to improve on, with no sign
// that it had. Nothing here can check that, so the one thing that can be
// checked is checked.
func TestARunnerNeedsAPool(t *testing.T) {
	if _, err := pgtx.New(nil); err == nil {
		t.Error("a runner was built with no pool")
	}
}

var errAlwaysFails = errString("the card was declined")

type errString string

func (e errString) Error() string { return string(e) }
