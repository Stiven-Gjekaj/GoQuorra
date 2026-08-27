// Command quorra-worker runs a worker with a few example handlers.
//
// It is a demonstration, and it says so on every line of its output. A real
// worker is your own program that imports
// github.com/Stiven-Gjekaj/GoQuorra/worker and registers handlers that do
// your work. This one exists so that the compose stack has something to watch
// and so that the example in the worker package has somewhere to point.
//
// The version before the rebuild put its handler inside the library, where it
// slept for a random time and failed one job in ten. That meant the library
// could not be used for anything real, and that the failures anybody saw
// while trying it out were invented rather than their own.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/Stiven-Gjekaj/GoQuorra/internal/config"
	"github.com/Stiven-Gjekaj/GoQuorra/worker"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	// Before anything else, because an argument here means somebody asked for
	// a different binary and got this one.
	if err := config.CheckNoArguments(os.Args); err != nil {
		return err
	}

	cfg, err := config.LoadWorker(os.Getenv)
	if err != nil {
		return err
	}

	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: cfg.LogLevel}))

	w, err := worker.New(worker.Config{
		ID:            cfg.ID,
		ServerAddr:    cfg.ServerAddr,
		Queues:        cfg.Queues,
		MaxJobs:       cfg.MaxJobs,
		LeaseTTL:      cfg.LeaseTTL,
		PollEvery:     cfg.PollEvery,
		ShutdownGrace: cfg.ShutdownGrace,
		APIKey:        cfg.APIKey,
		Logger:        log,
	})
	if err != nil {
		return err
	}
	defer func() { _ = w.Close() }()

	register(w, log)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	return w.Run(ctx)
}

// register attaches the example handlers.
//
// Each one does exactly what its name says, so that somebody trying the
// system can tell the difference between the queue working and the queue
// pretending to work.
func register(w *worker.Worker, log *slog.Logger) {
	// echo writes the payload to the log and finishes.
	w.Handle("echo", func(_ context.Context, job worker.Job) error {
		log.Info("echo", "job", job.ID, "payload", string(job.Payload))
		return nil
	})

	// sleep waits for the number of milliseconds in the payload. It respects
	// the context, so it stops when the lease ends or the worker is stopping,
	// which is what a real handler must also do.
	w.Handle("sleep", func(ctx context.Context, job worker.Job) error {
		var request struct {
			Milliseconds int `json:"ms"`
		}
		if err := job.Decode(&request); err != nil {
			return fmt.Errorf("the payload is not {\"ms\": number}: %w", err)
		}
		if request.Milliseconds <= 0 {
			request.Milliseconds = 100
		}

		timer := time.NewTimer(time.Duration(request.Milliseconds) * time.Millisecond)
		defer timer.Stop()

		select {
		case <-timer.C:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	})

	// fail always fails. It is here so that the retry schedule and the dead
	// letter queue can be watched happening, which is the part of a queue
	// that is hardest to believe without seeing.
	w.Handle("fail", func(_ context.Context, job worker.Job) error {
		if job.LastAttempt() {
			log.Warn("this failure buries the job", "job", job.ID, "attempts", job.Attempts)
		}
		return errors.New("this handler always fails, on purpose")
	})
}
