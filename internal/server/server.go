// Package server assembles the parts and runs them.
//
// It exists so that cmd/quorra-server holds only the reading of the
// environment and the building of a logger. Everything that could be wrong
// about the order things start and stop in is here, where a test can drive
// it.
package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/Stiven-Gjekaj/GoQuorra/internal/api"
	"github.com/Stiven-Gjekaj/GoQuorra/internal/config"
	"github.com/Stiven-Gjekaj/GoQuorra/internal/metrics"
	"github.com/Stiven-Gjekaj/GoQuorra/internal/quorrapb"
	"github.com/Stiven-Gjekaj/GoQuorra/internal/rpc"
	"github.com/Stiven-Gjekaj/GoQuorra/internal/store"
	"google.golang.org/grpc"
)

// Server holds everything the process runs.
type Server struct {
	cfg     *config.Server
	store   store.Store
	metrics *metrics.Metrics
	log     *slog.Logger

	http *http.Server
	grpc *grpc.Server

	// Addresses are read after Run has started, which is what lets a test
	// listen on port zero and then find out what it got.
	httpAddr net.Addr
	grpcAddr net.Addr
	ready    chan struct{}
	once     sync.Once
}

// New builds the server.
func New(cfg *config.Server, s store.Store, log *slog.Logger) *Server {
	m := metrics.New()

	handler := api.New(api.Options{
		Store:            s,
		Metrics:          m,
		Log:              log,
		Keys:             cfg.Keys,
		MaxBodyBytes:     cfg.MaxBodyBytes,
		DashboardEnabled: true,
	}).Handler()

	// The worker protocol is guarded by the same keys the HTTP API uses, and
	// a worker key is a scope of its own. The port had no authentication at
	// all before this: a process that could reach it could lease from any
	// queue.
	guard := rpc.NewGuard(cfg.Keys)
	grpcServer := grpc.NewServer(
		grpc.UnaryInterceptor(guard.Unary()),
		grpc.StreamInterceptor(guard.Stream()),
	)
	quorrapb.RegisterQueueServiceServer(grpcServer,
		rpc.New(s, m, log, rpc.DefaultLimits(), time.Now))

	return &Server{
		cfg:     cfg,
		store:   s,
		metrics: m,
		log:     log,
		http: &http.Server{
			Addr:    cfg.HTTPAddr,
			Handler: handler,

			// A client that opens a connection and sends headers slowly holds
			// a goroutine for as long as it likes. Without this timeout a few
			// hundred such connections stop the server answering anybody, and
			// nothing in the logs says why.
			ReadHeaderTimeout: 10 * time.Second,
			ReadTimeout:       30 * time.Second,
			WriteTimeout:      60 * time.Second,
			IdleTimeout:       2 * time.Minute,
			MaxHeaderBytes:    1 << 16,
		},
		grpc:  grpcServer,
		ready: make(chan struct{}),
	}
}

// Ready closes once both listeners are open. A test waits on it.
func (s *Server) Ready() <-chan struct{} { return s.ready }

// HTTPAddr is the address the HTTP server listens on.
func (s *Server) HTTPAddr() net.Addr { return s.httpAddr }

// GRPCAddr is the address the gRPC server listens on.
func (s *Server) GRPCAddr() net.Addr { return s.grpcAddr }

// Run serves until the context ends, then stops cleanly.
func (s *Server) Run(ctx context.Context) error {
	httpListener, err := net.Listen("tcp", s.cfg.HTTPAddr)
	if err != nil {
		return fmt.Errorf("server: cannot listen on %s: %w", s.cfg.HTTPAddr, err)
	}
	grpcListener, err := net.Listen("tcp", s.cfg.GRPCAddr)
	if err != nil {
		_ = httpListener.Close()
		return fmt.Errorf("server: cannot listen on %s: %w", s.cfg.GRPCAddr, err)
	}

	s.httpAddr = httpListener.Addr()
	s.grpcAddr = grpcListener.Addr()
	s.once.Do(func() { close(s.ready) })

	if s.cfg.UsesMemory() {
		// Said at warning level, because somebody who reaches for this in
		// production will not read it anywhere else.
		s.log.Warn("the jobs are kept in memory, so everything is lost when this process stops")
	}
	s.log.Info("listening", "http", s.httpAddr.String(), "grpc", s.grpcAddr.String())

	background, stopBackground := context.WithCancel(ctx)
	defer stopBackground()

	if s.cfg.RemovesAnything() {
		// At warning, because a sweep that removes jobs is worth one line in
		// a log somebody reads after an upgrade.
		s.log.Warn("finished jobs will be removed once they are old enough",
			"retention", s.cfg.Retention, "every", s.cfg.RetentionEvery)
	}

	var loops sync.WaitGroup
	loops.Add(3)
	go func() {
		defer loops.Done()
		reclaim(background, s.store, s.metrics, s.log, s.cfg.ReclaimEvery, s.cfg.ReclaimBatch)
	}()
	go func() {
		defer loops.Done()
		refreshStats(background, s.store, s.metrics, s.log, s.cfg.StatsEvery)
	}()
	go func() {
		defer loops.Done()
		sweep(background, s.store, s.metrics, s.log,
			s.cfg.RetentionEvery, s.cfg.RetentionBatch, s.cfg.Retention, s.cfg.WorkerRetention)
	}()

	// A listener that dies is reported through this channel rather than
	// ending the process from inside a goroutine. The old server called
	// Fatalf there, which skipped every deferred close and left the database
	// connections to time out.
	failed := make(chan error, 2)
	go func() {
		if err := s.http.Serve(httpListener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			failed <- fmt.Errorf("server: the HTTP listener stopped: %w", err)
		}
	}()
	go func() {
		if err := s.grpc.Serve(grpcListener); err != nil && !errors.Is(err, grpc.ErrServerStopped) {
			failed <- fmt.Errorf("server: the gRPC listener stopped: %w", err)
		}
	}()

	var runErr error
	select {
	case <-ctx.Done():
		s.log.Info("stopping")
	case runErr = <-failed:
		s.log.Error("stopping after a failure", "error", runErr)
	}

	stopBackground()
	loops.Wait()

	return errors.Join(runErr, s.shutdown())
}

// shutdown stops the listeners in the order that loses least work.
func (s *Server) shutdown() error {
	// gRPC first. It carries the workers, and GracefulStop waits for the
	// calls in flight, which includes a worker reporting a job it has just
	// finished. Stopping HTTP first would only stop new submissions arriving,
	// and those are the cheapest thing to lose.
	stopped := make(chan struct{})
	go func() {
		s.grpc.GracefulStop()
		close(stopped)
	}()

	timer := time.NewTimer(s.cfg.ShutdownGrace)
	defer timer.Stop()

	select {
	case <-stopped:
	case <-timer.C:
		s.log.Warn("a worker call was still running, so it is being cut off",
			"waited", s.cfg.ShutdownGrace)
		s.grpc.Stop()
	}

	ctx, cancel := context.WithTimeout(context.Background(), s.cfg.ShutdownGrace)
	defer cancel()

	if err := s.http.Shutdown(ctx); err != nil {
		return fmt.Errorf("server: the HTTP server did not stop cleanly: %w", err)
	}

	s.log.Info("stopped")
	return nil
}
