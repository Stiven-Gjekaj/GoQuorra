package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/Stiven-Gjekaj/GoQuorra/internal/quorrapb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"
)

// Config sets a worker up.
type Config struct {
	// ID names this worker in the server logs and on the lease. Two workers
	// sharing an identifier still work correctly, because the lease is what
	// grants a job, but nobody can then tell them apart in an incident.
	ID string

	// ServerAddr is the gRPC address of the server.
	ServerAddr string

	// Queues are the queues to take work from. One goroutine polls each.
	Queues []string

	// MaxJobs is the most jobs to take in one request.
	MaxJobs int

	// LeaseTTL is how long to ask to hold a job. Make it longer than the
	// slowest handler. A job still running when its lease ends is given to
	// another worker, and the work happens twice.
	LeaseTTL time.Duration

	// PollEvery is the wait between requests when the queue is empty. A
	// request that returns jobs is followed immediately by another, so a busy
	// queue is drained at full speed and an idle one is asked once a second.
	PollEvery time.Duration

	// ShutdownGrace is how long to wait for running jobs when stopping.
	ShutdownGrace time.Duration

	Logger *slog.Logger

	// DialOptions are passed to gRPC. Leave empty for an insecure connection,
	// which is the right choice only inside a private network.
	DialOptions []grpc.DialOption
}

func (c *Config) fill() {
	if c.ID == "" {
		c.ID = "worker-1"
	}
	if c.ServerAddr == "" {
		c.ServerAddr = "localhost:50051"
	}
	if len(c.Queues) == 0 {
		c.Queues = []string{"default"}
	}
	if c.MaxJobs <= 0 {
		c.MaxJobs = 5
	}
	if c.LeaseTTL <= 0 {
		c.LeaseTTL = 30 * time.Second
	}
	if c.PollEvery <= 0 {
		c.PollEvery = time.Second
	}
	if c.ShutdownGrace <= 0 {
		c.ShutdownGrace = 30 * time.Second
	}
	if c.Logger == nil {
		c.Logger = slog.New(slog.NewTextHandler(os.Stdout, nil))
	}
}

// Worker takes jobs from a server and runs them.
type Worker struct {
	cfg    Config
	log    *slog.Logger
	conn   *grpc.ClientConn
	client quorrapb.QueueServiceClient

	mu       sync.RWMutex
	handlers map[string]Handler

	// running counts the jobs in flight, so that a stopping worker can wait
	// for them. The old worker passed context.Background() to every job and
	// then closed the connection, so work in flight was abandoned and its
	// lease had to run out before anybody could take it.
	running sync.WaitGroup
}

// New builds a worker and connects it.
func New(cfg Config) (*Worker, error) {
	cfg.fill()

	options := cfg.DialOptions
	if len(options) == 0 {
		options = []grpc.DialOption{grpc.WithTransportCredentials(insecure.NewCredentials())}
	}

	conn, err := grpc.NewClient(cfg.ServerAddr, options...)
	if err != nil {
		return nil, fmt.Errorf("worker: cannot reach %s: %w", cfg.ServerAddr, err)
	}

	return &Worker{
		cfg:      cfg,
		log:      cfg.Logger.With("worker", cfg.ID),
		conn:     conn,
		client:   quorrapb.NewQueueServiceClient(conn),
		handlers: make(map[string]Handler),
	}, nil
}

// Register attaches a handler to a job type.
func (w *Worker) Register(jobType string, handler Handler) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.handlers[jobType] = handler
}

// Handle attaches a function to a job type.
func (w *Worker) Handle(jobType string, fn HandlerFunc) { w.Register(jobType, fn) }

// Close releases the connection.
func (w *Worker) Close() error { return w.conn.Close() }

// Run takes jobs until the context ends.
//
// It returns when every queue poller has stopped and every job in flight has
// finished or the shutdown wait has passed.
func (w *Worker) Run(ctx context.Context) error {
	w.mu.RLock()
	registered := len(w.handlers)
	w.mu.RUnlock()

	// A worker with no handlers fails every job it is given, at the full
	// retry count, until each reaches the dead letter queue. Saying so at
	// startup is cheaper than reading it out of the queue an hour later.
	if registered == 0 {
		return errors.New("worker: no handler is registered, so every job would fail")
	}

	w.log.Info("starting",
		"server", w.cfg.ServerAddr,
		"queues", w.cfg.Queues,
		"handlers", registered,
		"lease", w.cfg.LeaseTTL)

	var pollers sync.WaitGroup
	for _, queue := range w.cfg.Queues {
		pollers.Add(1)
		go func(queue string) {
			defer pollers.Done()
			w.poll(ctx, queue)
		}(queue)
	}
	pollers.Wait()

	return w.drain()
}

// drain waits for the jobs still running.
func (w *Worker) drain() error {
	finished := make(chan struct{})
	go func() {
		w.running.Wait()
		close(finished)
	}()

	select {
	case <-finished:
		w.log.Info("stopped, with every job finished")
		return nil
	case <-time.After(w.cfg.ShutdownGrace):
		// Say so rather than exiting quietly. Each job still running holds a
		// lease, so the server will hand it to somebody else once that lease
		// ends, and the work happens twice. That is worth a line in the log
		// of the process that caused it.
		w.log.Warn("stopped with jobs still running, so the server will give them to another worker",
			"waited", w.cfg.ShutdownGrace)
		return fmt.Errorf("worker: jobs were still running after %s", w.cfg.ShutdownGrace)
	}
}

// poll asks one queue for work until the context ends.
func (w *Worker) poll(ctx context.Context, queue string) {
	log := w.log.With("queue", queue)

	for {
		if ctx.Err() != nil {
			log.Debug("no longer asking for work")
			return
		}

		count, err := w.leaseAndRun(ctx, queue)
		switch {
		case err != nil && ctx.Err() != nil:
			// The context ended while the request was out. That is a stop,
			// not a fault.
			return
		case err != nil:
			log.Error("cannot lease jobs", "error", err)
		}

		// A request that returned work is followed by another at once, so a
		// busy queue drains at full speed. Only an empty one waits.
		if count > 0 && err == nil {
			continue
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(w.cfg.PollEvery):
		}
	}
}

// leaseAndRun takes one batch and starts it.
func (w *Worker) leaseAndRun(ctx context.Context, queue string) (int, error) {
	response, err := w.client.Lease(ctx, &quorrapb.LeaseRequest{
		WorkerId: w.cfg.ID,
		Queue:    queue,
		MaxJobs:  int32(w.cfg.MaxJobs),
		LeaseTtl: durationpb.New(w.cfg.LeaseTTL),
	})
	if err != nil {
		return 0, err
	}

	for _, message := range response.GetJobs() {
		job := fromProto(message)

		w.running.Add(1)
		go func() {
			defer w.running.Done()
			w.run(ctx, job)
		}()
	}

	return len(response.GetJobs()), nil
}

// run does one job and reports what happened.
func (w *Worker) run(ctx context.Context, job Job) {
	log := w.log.With("job", job.ID, "type", job.Type, "attempt", job.Attempts)

	// The handler's time runs out when the lease does. Past that moment the
	// server has given the job to somebody else, so anything this handler
	// goes on to do is work that is being done twice.
	handlerCtx := ctx
	if !job.LeaseExpiresAt.IsZero() {
		var cancel context.CancelFunc
		handlerCtx, cancel = context.WithDeadline(context.WithoutCancel(ctx), job.LeaseExpiresAt)
		defer cancel()
	}

	started := time.Now()
	err := w.call(handlerCtx, job)
	took := time.Since(started)

	if err != nil {
		log.Warn("job failed", "error", err, "took", took)
		w.report(ctx, job, quorrapb.Outcome_OUTCOME_FAILED, err.Error())
		return
	}

	log.Info("job done", "took", took)
	w.report(ctx, job, quorrapb.Outcome_OUTCOME_SUCCEEDED, "")
}

// call runs the handler and turns a panic into a failure.
//
// A handler is somebody else's code. Letting a panic through takes the whole
// worker down, which loses every other job in flight as well, and each of
// those then has to wait out its lease before anybody else can run it. One
// bad payload should cost one job.
func (w *Worker) call(ctx context.Context, job Job) (err error) {
	w.mu.RLock()
	handler, known := w.handlers[job.Type]
	w.mu.RUnlock()

	if !known {
		// Named plainly, because the usual cause is a deployment where the
		// producer knows a job type and the workers do not yet.
		return fmt.Errorf("no handler is registered for the job type %q", job.Type)
	}

	defer func() {
		if panicked := recover(); panicked != nil {
			err = fmt.Errorf("the handler panicked: %v", panicked)
		}
	}()

	return handler.Handle(ctx, job)
}

// report tells the server what happened.
//
// It uses a context of its own. The one that ran the job may already have
// ended, which is exactly the moment a report matters most: a worker stopping
// cleanly should hand its results back rather than leave every job it was
// running to be repeated by somebody else after the lease expires.
func (w *Worker) report(ctx context.Context, job Job, outcome quorrapb.Outcome, message string) {
	reportCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()

	_, err := w.client.Report(reportCtx, &quorrapb.ReportRequest{
		JobId:    job.ID,
		WorkerId: w.cfg.ID,
		LeaseId:  job.leaseID,
		Outcome:  outcome,
		Error:    message,
	})
	if err == nil {
		return
	}

	if status.Code(err) == codes.FailedPrecondition {
		// The lease was taken back while the job ran. Retrying the report
		// cannot help, and the job is already somewhere else.
		w.log.Warn("the lease was taken back while the job ran, so the result is discarded",
			"job", job.ID, "type", job.Type)
		return
	}

	w.log.Error("cannot report the result", "job", job.ID, "error", err)
}

// fromProto turns a job on the wire into the one a handler sees.
func fromProto(message *quorrapb.Job) Job {
	job := Job{
		ID:         message.GetId(),
		Type:       message.GetType(),
		Payload:    message.GetPayload(),
		Queue:      message.GetQueue(),
		Priority:   int(message.GetPriority()),
		Attempts:   int(message.GetAttempts()),
		MaxRetries: int(message.GetMaxRetries()),
		leaseID:    message.GetLeaseId(),
	}
	if message.GetLeaseExpiresAt() != nil {
		job.LeaseExpiresAt = message.GetLeaseExpiresAt().AsTime()
	}
	if message.GetRunAt() != nil {
		job.RunAt = message.GetRunAt().AsTime()
	}
	if message.GetCreatedAt() != nil {
		job.CreatedAt = message.GetCreatedAt().AsTime()
	}
	return job
}
