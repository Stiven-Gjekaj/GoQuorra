package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/goquorra/goquorra/internal/config"
	"github.com/goquorra/goquorra/internal/worker"
)

func main() {
	cfg := config.Load()

	logger := log.New(os.Stdout, "[worker] ", log.LstdFlags)
	logger.Printf("Starting GoQuorra worker: %s", cfg.WorkerID)

	queues := strings.Split(cfg.WorkerQueues, ",")
	for i := range queues {
		queues[i] = strings.TrimSpace(queues[i])
	}

	// Parse server address
	serverAddr := cfg.GRPCAddr
	if strings.HasPrefix(serverAddr, ":") {
		serverAddr = "localhost" + serverAddr
	}

	workerCfg := &worker.Config{
		ID:         cfg.WorkerID,
		ServerAddr: serverAddr,
		Queues:     queues,
		MaxJobs:    cfg.WorkerMaxJobs,
		LeaseTTL:   cfg.WorkerLeaseTTL,
	}

	w := worker.New(workerCfg, logger)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Handle signals for graceful shutdown
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)

	go func() {
		<-sigChan
		logger.Println("Received shutdown signal")
		cancel()
	}()

	if err := w.Start(ctx); err != nil {
		logger.Fatalf("Worker error: %v", err)
	}

	logger.Println("Worker stopped")
}
