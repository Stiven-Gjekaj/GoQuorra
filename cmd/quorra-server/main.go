// Command quorra-server runs the queue.
//
// It reads the environment, opens a store, and starts the HTTP and gRPC
// listeners. Everything about the order things start and stop in lives in
// internal/server, where a test can drive it.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/Stiven-Gjekaj/GoQuorra/internal/config"
	"github.com/Stiven-Gjekaj/GoQuorra/internal/server"
	"github.com/Stiven-Gjekaj/GoQuorra/internal/store"
	"github.com/Stiven-Gjekaj/GoQuorra/internal/store/memory"
	"github.com/Stiven-Gjekaj/GoQuorra/internal/store/postgres"
)

// version is the build this binary came from.
//
// The Dockerfile has passed -X main.version since the first image, and no
// such variable existed, so the flag did nothing and every build said
// nothing about itself. Go accepts -X for a symbol that is not there without
// a word, which is why it went unnoticed.
var version = "dev"

func main() {
	if err := run(); err != nil {
		// One place that ends the process, and it runs after every deferred
		// close. The old server called Fatalf from inside two goroutines,
		// which skipped all of them.
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

	cfg, err := config.LoadServer(os.Getenv)
	if err != nil {
		return err
	}

	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: cfg.LogLevel}))
	slog.SetDefault(log)

	// First line in the log, because an operator reading a log of a queue
	// that is behaving strangely asks which build it is before anything else.
	// Neither this nor the worker takes an argument, on purpose and for a
	// reason written in config.CheckNoArguments, so the log is where it goes.
	log.Info("starting", "version", version)

	// SIGTERM as well as an interrupt, because SIGTERM is what a container
	// runtime sends and an interrupt is what a keyboard sends.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	jobStore, err := open(ctx, cfg, log)
	if err != nil {
		return err
	}
	defer func() {
		if err := jobStore.Close(); err != nil {
			log.Error("cannot close the store", "error", err)
		}
	}()

	return server.New(cfg, jobStore, log).Run(ctx)
}

func open(ctx context.Context, cfg *config.Server, log *slog.Logger) (store.Store, error) {
	// The store reports what it cannot return through a function rather than
	// through a logger, so that this package chooses the logging and that one
	// holds no library.
	opts := store.Options{
		Policy: cfg.Policy,
		Log:    func(message string, err error) { log.Warn(message, "error", err) },
	}

	if cfg.UsesMemory() {
		return memory.New(opts), nil
	}

	s, err := postgres.Open(ctx, cfg.DatabaseURL, opts)
	if err != nil {
		return nil, err
	}
	return s, nil
}
