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
	cfg, err := config.LoadServer(os.Getenv)
	if err != nil {
		return err
	}

	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: cfg.LogLevel}))
	slog.SetDefault(log)

	// SIGTERM as well as an interrupt, because SIGTERM is what a container
	// runtime sends and an interrupt is what a keyboard sends.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	jobStore, err := open(ctx, cfg)
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

func open(ctx context.Context, cfg *config.Server) (store.Store, error) {
	opts := store.Options{Policy: cfg.Policy}

	if cfg.UsesMemory() {
		return memory.New(opts), nil
	}

	s, err := postgres.Open(ctx, cfg.DatabaseURL, opts)
	if err != nil {
		return nil, err
	}
	return s, nil
}
