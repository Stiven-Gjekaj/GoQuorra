package worker

import (
	"context"
	"encoding/json"
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
	"google.golang.org/grpc/metadata"
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

	// HeartbeatEvery is how often a running job asks for its lease to be
	// pushed out. Zero means a third of LeaseTTL.
	//
	// A third, so that two heartbeats can be lost to a slow network or a
	// restarting server before the lease actually runs out. A half leaves no
	// room for the first one to be late, and a tenth asks ten times as often
	// for the same protection.
	HeartbeatEvery time.Duration

	// ShutdownGrace is how long to wait for running jobs when stopping.
	ShutdownGrace time.Duration

	Logger *slog.Logger

	// APIKey is the key this worker presents on every call. It has to hold
	// the worker scope, which is separate from the one an operator's key
	// holds: an operator must not be able to lease the queue empty, and a
	// worker must not be able to cancel anything.
	//
	// The server refuses every call without one. The gRPC port used to have
	// no authentication at all, so a process that could reach it could lease
	// from any queue.
	APIKey string

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
	if c.HeartbeatEvery <= 0 {
		c.HeartbeatEvery = c.LeaseTTL / 3
	}
	if c.HeartbeatEvery <= 0 {
		c.HeartbeatEvery = time.Second
	}
	if c.ShutdownGrace <= 0 {
		c.ShutdownGrace = 30 * time.Second
	}
	if c.Logger == nil {
		c.Logger = slog.New(slog.NewTextHandler(os.Stdout, nil))
	}
}

// sendKey puts the key on every call that answers once.
func sendKey(key string) grpc.UnaryClientInterceptor {
	return func(
		ctx context.Context,
		method string,
		req, reply any,
		conn *grpc.ClientConn,
		invoke grpc.UnaryInvoker,
		options ...grpc.CallOption,
	) error {
		return invoke(withKey(ctx, key), method, req, reply, conn, options...)
	}
}

// sendKeyOnStream puts the key on every call that holds a stream open.
func sendKeyOnStream(key string) grpc.StreamClientInterceptor {
	return func(
		ctx context.Context,
		desc *grpc.StreamDesc,
		conn *grpc.ClientConn,
		method string,
		streamer grpc.Streamer,
		options ...grpc.CallOption,
	) (grpc.ClientStream, error) {
		return streamer(withKey(ctx, key), desc, conn, method, options...)
	}
}

// withKey adds the key to the metadata of one call.
//
// AppendToOutgoingContext and not NewOutgoingContext, so that a caller who
// set metadata of their own keeps it.
func withKey(ctx context.Context, key string) context.Context {
	return metadata.AppendToOutgoingContext(ctx, "x-api-key", key)
}

// ErrAlreadyReported says that a handler recorded the outcome itself.
//
// A handler that returns it has succeeded, and the worker sends no report.
// Returning it after a failure would be wrong: a failure that was not
// recorded is one the worker still has to report, and a handler whose
// transaction rolled back recorded nothing.
//
// It exists for one caller, worker/pgtx, whose handlers write their result
// and their report in one transaction. That is the only way a side effect in
// the same database as the queue can happen effectively once, and it needs
// the worker to keep its hands off the job afterwards. Nothing else should
// return this: a handler that says it reported and did not leaves a job
// leased until its lease runs out.
var ErrAlreadyReported = errors.New("worker: the handler recorded the outcome itself")

// Worker takes jobs from a server and runs them.
type Worker struct {
	cfg    Config
	log    *slog.Logger
	conn   *grpc.ClientConn
	client quorrapb.QueueServiceClient

	// hints carries a note that a queue may have work, one channel per
	// queue. Built once at start up and never written after, so it needs no
	// lock: the watch goroutine sends and the poll goroutines receive.
	hints map[string]chan struct{}

	mu       sync.RWMutex
	handlers map[string]ResultFunc

	// running counts the jobs in flight, so that a stopping worker can wait
	// for them. The old worker passed context.Background() to every job and
	// then closed the connection, so work in flight was abandoned and its
	// lease had to run out before anybody could take it.
	running sync.WaitGroup
}

// New builds a worker and connects it.
func New(cfg Config) (*Worker, error) {
	cfg.fill()

	if cfg.APIKey == "" {
		return nil, errors.New("worker: an API key is required, and it needs the worker scope")
	}

	options := cfg.DialOptions
	if len(options) == 0 {
		options = []grpc.DialOption{grpc.WithTransportCredentials(insecure.NewCredentials())}
	}

	// The key goes on every call through an interceptor rather than being
	// added at each call site. There are four of them, and the one somebody
	// forgets is the one that fails at three in the morning.
	options = append(options,
		grpc.WithUnaryInterceptor(sendKey(cfg.APIKey)),
		grpc.WithStreamInterceptor(sendKeyOnStream(cfg.APIKey)),
	)

	conn, err := grpc.NewClient(cfg.ServerAddr, options...)
	if err != nil {
		return nil, fmt.Errorf("worker: cannot reach %s: %w", cfg.ServerAddr, err)
	}

	// One buffered slot for each queue. A hint says the queue may have work,
	// and a second before the first is read says nothing new.
	hints := make(map[string]chan struct{}, len(cfg.Queues))
	for _, queue := range cfg.Queues {
		hints[queue] = make(chan struct{}, 1)
	}

	return &Worker{
		cfg:      cfg,
		log:      cfg.Logger.With("worker", cfg.ID),
		conn:     conn,
		client:   quorrapb.NewQueueServiceClient(conn),
		handlers: make(map[string]ResultFunc),
		hints:    hints,
	}, nil
}

// Register attaches a handler to a job type.
func (w *Worker) Register(jobType string, handler Handler) {
	w.HandleResult(jobType, func(ctx context.Context, job Job) (any, error) {
		return nil, handler.Handle(ctx, job)
	})
}

// Handle attaches a function to a job type.
func (w *Worker) Handle(jobType string, fn HandlerFunc) { w.Register(jobType, fn) }

// HandleResult attaches a function that produces something worth keeping.
//
// What it returns is stored on the job and served back by the API. Everything
// else about it is the same as Handle.
func (w *Worker) HandleResult(jobType string, fn ResultFunc) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.handlers[jobType] = fn
}

// HeartbeatEvery is how often a running job asks for its lease to be pushed
// out, after the defaults have been filled in.
func (w *Worker) HeartbeatEvery() time.Duration { return w.cfg.HeartbeatEvery }

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

	// One stream for every queue this worker serves, beside the polls rather
	// than instead of them. The polls are what make this correct.
	pollers.Add(1)
	go func() {
		defer pollers.Done()
		w.watch(ctx)
	}()

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

		// Whichever comes first: a hint that the queue has work, or the
		// poll. The poll is what makes this correct, and the hint is what
		// makes it quick.
		//
		// A hint that is lost costs one poll interval. That is the whole
		// reason the hint is allowed to be lost, and why nothing here treats
		// a missing one as a fault.
		select {
		case <-ctx.Done():
			return
		case <-w.hintFor(queue):
			log.Debug("told that this queue may have work")
		case <-time.After(w.cfg.PollEvery):
		}
	}
}

// hintFor gives the channel that carries hints for one queue.
//
// A channel per queue, filled by one watch stream. The alternative was a
// stream per queue, which is one connection per queue per worker and buys
// nothing: the server sends the queue name, so one stream can serve them all.
//
// A queue the worker does not serve gives a nil channel, and a receive on nil
// blocks for ever, which is right: the poll beside it is what wakes the loop.
func (w *Worker) hintFor(queue string) chan struct{} {
	return w.hints[queue]
}

// watch holds the stream that carries hints, and opens it again when it ends.
//
// It is a hint and never a promise, so every failure here is at debug and
// nothing stops. A worker whose watch never connects polls exactly as it did
// before this existed.
func (w *Worker) watch(ctx context.Context) {
	wait := time.Second

	for {
		if ctx.Err() != nil {
			return
		}

		err := w.watchOnce(ctx)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			w.log.Debug("the watch for work ended, and the worker is polling", "error", err)
		}

		// A backoff, up to the poll interval. There is no point retrying a
		// watch more often than the poll it exists to shorten.
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		if wait < w.cfg.PollEvery {
			wait *= 2
		}
	}
}

// watchOnce holds one stream open until it ends.
func (w *Worker) watchOnce(ctx context.Context) error {
	stream, err := w.client.Watch(ctx, &quorrapb.WatchRequest{
		WorkerId: w.cfg.ID,
		Queues:   w.cfg.Queues,
	})
	if err != nil {
		return err
	}

	for {
		event, err := stream.Recv()
		if err != nil {
			return err
		}

		// Dropped rather than blocked on when the channel is full. The queue
		// already has a hint waiting, and a second one says nothing new.
		to := w.hintFor(event.GetQueue())
		if to == nil {
			// A queue this worker does not serve. The server filters, so
			// this only happens to a worker whose configuration changed
			// under a stream it had already opened.
			continue
		}
		select {
		case to <- struct{}{}:
		default:
			// Already one waiting. A second says nothing new.
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

// ErrLeaseLost is the reason a handler's context carries when the job stopped
// being this worker's.
//
// A handler that wants to know why it is being stopped reads it:
//
//	if errors.Is(context.Cause(ctx), worker.ErrLeaseLost) {
//		// The job was cancelled, or taken back. Stop, and do not write
//		// anything: somebody else is running this now.
//	}
var ErrLeaseLost = errors.New("worker: the job is no longer ours")

// run does one job and reports what happened.
func (w *Worker) run(ctx context.Context, job Job) {
	log := w.log.With("job", job.ID, "type", job.Type, "attempt", job.Attempts)

	// The handler runs under a context this worker controls rather than
	// under the one that polls, because the report at the end has to be sent
	// even when the worker is stopping.
	handlerCtx, stop := context.WithCancelCause(context.WithoutCancel(ctx))
	defer stop(nil)

	// While the handler runs, the lease is pushed out. Without this a job
	// slower than its lease is taken back and given to somebody else while
	// this handler is still working on it.
	keeping := make(chan struct{})
	go func() {
		defer close(keeping)
		w.keepLease(handlerCtx, job, stop, log)
	}()

	started := time.Now()
	result, err := w.call(handlerCtx, job)
	took := time.Since(started)

	// Stop asking for the lease before reporting on it, and wait for the
	// asking to finish. A heartbeat racing the report is a call that can only
	// lose, and it fills the log with a refusal for a job that is already
	// done.
	stop(nil)
	<-keeping

	if lost := context.Cause(handlerCtx); errors.Is(lost, ErrLeaseLost) {
		// Reporting would be refused anyway, and the refusal reads as a
		// fault. The job belongs to somebody else now.
		log.Warn("the job stopped being ours while it ran, so the result is discarded",
			"took", took, "handler_error", err)
		return
	}

	if err != nil {
		// A handler that recorded the outcome itself has succeeded, and the
		// job is already written. Reporting would be a second write of a row
		// whose lease this worker no longer holds, and the refusal would
		// read as a fault.
		if errors.Is(err, ErrAlreadyReported) {
			log.Info("job done, and the handler recorded it", "took", took)
			return
		}

		// A handler that wrapped ErrPermanent has read the job and says no
		// attempt will finish it. The server buries it at once rather than
		// spending the remaining attempts, and every wait between them, to
		// reach the answer the handler already gave.
		if errors.Is(err, ErrPermanent) {
			log.Warn("job refused, so it will not be tried again", "error", err, "took", took)
			w.report(ctx, job, quorrapb.Outcome_OUTCOME_REFUSED, err.Error(), nil)
			return
		}

		log.Warn("job failed", "error", err, "took", took)
		w.report(ctx, job, quorrapb.Outcome_OUTCOME_FAILED, err.Error(), nil)
		return
	}

	log.Info("job done", "took", took)
	w.report(ctx, job, quorrapb.Outcome_OUTCOME_SUCCEEDED, "", result)
}

// keepLease asks the server to push the lease out until the job finishes.
//
// It stops the handler when the answer says the job is no longer this
// worker's, which is how a cancellation reaches a handler that is already
// running. Nothing reaches into it directly: the next heartbeat simply fails.
func (w *Worker) keepLease(ctx context.Context, job Job, stop context.CancelCauseFunc, log *slog.Logger) {
	ticker := time.NewTicker(w.cfg.HeartbeatEvery)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		// A short timeout of its own. A heartbeat that hangs for longer than
		// the lease is worse than one that fails, because the handler goes on
		// working while the job is given to somebody else.
		beat, cancel := context.WithTimeout(context.WithoutCancel(ctx), w.cfg.HeartbeatEvery)
		_, err := w.client.Heartbeat(beat, &quorrapb.HeartbeatRequest{
			JobId:    job.ID,
			WorkerId: w.cfg.ID,
			LeaseId:  job.leaseID,
			ExtendBy: durationpb.New(w.cfg.LeaseTTL),
		})
		cancel()

		switch {
		case err == nil:
			continue

		case status.Code(err) == codes.FailedPrecondition, status.Code(err) == codes.NotFound:
			// The job was cancelled, or its lease ran out and somebody else
			// has it. Either way this handler must stop.
			log.Warn("the job is no longer ours, so the handler is being stopped", "reason", err)
			stop(ErrLeaseLost)
			return

		case ctx.Err() != nil:
			// The handler finished while the call was out.
			return

		default:
			// A network fault, or a server restarting. Say so and try again
			// on the next tick: there are two more before the lease runs out.
			log.Warn("cannot extend the lease", "error", err)
		}
	}
}

// call runs the handler and turns a panic into a failure.
//
// A handler is somebody else's code. Letting a panic through takes the whole
// worker down, which loses every other job in flight as well, and each of
// those then has to wait out its lease before anybody else can run it. One
// bad payload should cost one job.
func (w *Worker) call(ctx context.Context, job Job) (result []byte, err error) {
	w.mu.RLock()
	handler, known := w.handlers[job.Type]
	w.mu.RUnlock()

	if !known {
		// Named plainly, because the usual cause is a deployment where the
		// producer knows a job type and the workers do not yet.
		return nil, fmt.Errorf("no handler is registered for the job type %q", job.Type)
	}

	defer func() {
		if panicked := recover(); panicked != nil {
			err = fmt.Errorf("the handler panicked: %v", panicked)
		}
	}()

	value, err := handler(ctx, job)
	if err != nil || value == nil {
		return nil, err
	}

	// A value that cannot be marshalled fails the job rather than being
	// dropped. A handler that returns something unserialisable has a defect,
	// and reporting success with no result would hide it.
	encoded, marshalErr := json.Marshal(value)
	if marshalErr != nil {
		return nil, fmt.Errorf("the handler returned a result that is not JSON: %w", marshalErr)
	}
	return encoded, nil
}

// report tells the server what happened.
//
// It uses a context of its own. The one that ran the job may already have
// ended, which is exactly the moment a report matters most: a worker stopping
// cleanly should hand its results back rather than leave every job it was
// running to be repeated by somebody else after the lease expires.
func (w *Worker) report(ctx context.Context, job Job, outcome quorrapb.Outcome, message string, result []byte) {
	reportCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()

	_, err := w.client.Report(reportCtx, &quorrapb.ReportRequest{
		JobId:    job.ID,
		WorkerId: w.cfg.ID,
		LeaseId:  job.leaseID,
		Outcome:  outcome,
		Error:    message,
		Result:   result,
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
