package main

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/goquorra/goquorra/internal/api"
	"github.com/goquorra/goquorra/internal/config"
	grpcserver "github.com/goquorra/goquorra/internal/grpc"
	"github.com/goquorra/goquorra/internal/metrics"
	"github.com/goquorra/goquorra/internal/queue"
	"github.com/goquorra/goquorra/internal/store"
	_ "github.com/lib/pq"
	"github.com/redis/go-redis/v9"
	"google.golang.org/grpc"
)

func main() {
	cfg := config.Load()

	// Setup structured logging
	logger := log.New(os.Stdout, "[quorra] ", log.LstdFlags|log.Lshortfile)
	logger.Printf("Starting GoQuorra server - HTTP: %s, gRPC: %s", cfg.HTTPAddr, cfg.GRPCAddr)

	// Connect to PostgreSQL
	db, err := sql.Open("postgres", cfg.DatabaseURL)
	if err != nil {
		logger.Fatalf("Failed to connect to database: %v", err)
	}
	defer db.Close()

	if err := db.Ping(); err != nil {
		logger.Fatalf("Failed to ping database: %v", err)
	}
	logger.Println("Connected to PostgreSQL")

	// Initialize store
	jobStore := store.NewPostgresStore(db)

	// Connect to Redis (optional)
	var redisClient *redis.Client
	if cfg.RedisURL != "" {
		opts, err := redis.ParseURL(cfg.RedisURL)
		if err != nil {
			logger.Printf("Warning: failed to parse Redis URL: %v. Running in Postgres-only mode", err)
		} else {
			redisClient = redis.NewClient(opts)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := redisClient.Ping(ctx).Err(); err != nil {
				logger.Printf("Warning: Redis connection failed: %v. Running in Postgres-only mode", err)
				redisClient = nil
			} else {
				logger.Println("Connected to Redis")
			}
		}
	}

	// Initialize queue manager
	queueManager := queue.NewManager(jobStore, redisClient, logger)

	// Start scheduler
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go queueManager.StartScheduler(ctx)

	// Initialize metrics
	metricsCollector := metrics.NewCollector()

	// Setup HTTP server with API
	apiHandler := api.NewHandler(jobStore, queueManager, metricsCollector, cfg.APIKey, logger)
	httpServer := &http.Server{
		Addr:    cfg.HTTPAddr,
		Handler: apiHandler.Router(),
	}

	// Setup gRPC server
	grpcListener, err := net.Listen("tcp", cfg.GRPCAddr)
	if err != nil {
		logger.Fatalf("Failed to listen on gRPC port: %v", err)
	}

	grpcServer := grpc.NewServer()
	workerService := grpcserver.NewWorkerService(queueManager, metricsCollector, logger)
	grpcserver.RegisterWorkerServiceServer(grpcServer, workerService)

	// Start servers
	go func() {
		logger.Printf("Starting HTTP server on %s", cfg.HTTPAddr)
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Fatalf("HTTP server error: %v", err)
		}
	}()

	go func() {
		logger.Printf("Starting gRPC server on %s", cfg.GRPCAddr)
		if err := grpcServer.Serve(grpcListener); err != nil {
			logger.Fatalf("gRPC server error: %v", err)
		}
	}()

	// Graceful shutdown
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)
	<-sigChan

	logger.Println("Shutting down gracefully...")
	cancel()

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()

	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		logger.Printf("HTTP server shutdown error: %v", err)
	}

	grpcServer.GracefulStop()

	if redisClient != nil {
		redisClient.Close()
	}

	logger.Println("Server stopped")
	fmt.Println("GoQuorra shutdown complete")
}
