// Package config reads the environment.
//
// Two rules shape it.
//
// A bad value is an error, not a default. The version before the rebuild
// parsed an integer by walking the characters and keeping the digits, so
// QUORRA_WORKER_MAX_JOBS=-5 meant five, =1o meant ten, and =five meant
// whatever the default was. Every one of those started a server that ran
// under settings nobody chose and nothing reported.
//
// Every bad value is reported, and not only the first. An operator who fixes
// one typo, restarts, and meets the next one is being made to find them one
// deployment at a time.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/Stiven-Gjekaj/GoQuorra/internal/jobs"
)

// Getenv reads one variable. The process environment is os.Getenv, and a test
// passes a map, so no test has to change the environment it runs in.
type Getenv func(string) string

// FromMap makes a Getenv from a map.
func FromMap(values map[string]string) Getenv {
	return func(key string) string { return values[key] }
}

// Server is what quorra-server needs.
type Server struct {
	HTTPAddr string
	GRPCAddr string
	LogLevel slog.Level

	// Backend is "postgres" or "memory".
	Backend     string
	DatabaseURL string

	// APIKey guards the REST API. There is no default. The old one shipped
	// the string "dev-api-key-change-in-production" in the example file, the
	// compose stack and the README, so every deployment that followed the
	// quick start was reachable by anybody who had read it.
	APIKey string

	Policy jobs.Policy

	// ReclaimEvery is how often the server looks for leases that ran out.
	ReclaimEvery time.Duration
	ReclaimBatch int

	// StatsEvery is how often the queue length gauge is refreshed.
	StatsEvery time.Duration

	ShutdownGrace time.Duration
	MaxBodyBytes  int64
}

// Worker is what quorra-worker needs.
type Worker struct {
	ID         string
	ServerAddr string
	Queues     []string
	MaxJobs    int
	LeaseTTL   time.Duration
	PollEvery  time.Duration
	LogLevel   slog.Level

	// ShutdownGrace is how long a stopping worker waits for the jobs it is
	// running. The old worker passed context.Background() to every job and
	// then closed the connection, so work in flight was abandoned and its
	// lease had to run out before anybody else could take it.
	ShutdownGrace time.Duration
}

// LoadServer reads the server settings.
func LoadServer(getenv Getenv) (*Server, error) {
	l := &loader{getenv: getenv}

	cfg := &Server{
		HTTPAddr: l.text("QUORRA_HTTP_ADDR", ":8080"),
		GRPCAddr: l.text("QUORRA_GRPC_ADDR", ":50051"),
		LogLevel: l.level("QUORRA_LOG_LEVEL", slog.LevelInfo),

		Backend:     l.choice("QUORRA_STORE", "postgres", "postgres", "memory"),
		DatabaseURL: l.text("DATABASE_URL", ""),
		APIKey:      l.required("QUORRA_API_KEY"),

		Policy: jobs.Policy{
			MaxRetries: l.number("QUORRA_MAX_RETRIES", 3),
			Base:       l.duration("QUORRA_RETRY_BASE", 2*time.Second),
			Max:        l.duration("QUORRA_RETRY_MAX", time.Hour),
		},

		ReclaimEvery: l.duration("QUORRA_RECLAIM_EVERY", 10*time.Second),
		ReclaimBatch: l.number("QUORRA_RECLAIM_BATCH", 100),
		StatsEvery:   l.duration("QUORRA_STATS_EVERY", 15*time.Second),

		ShutdownGrace: l.duration("QUORRA_SHUTDOWN_GRACE", 15*time.Second),
		MaxBodyBytes:  int64(l.number("QUORRA_MAX_BODY_BYTES", 1<<20)),
	}

	if err := l.err(); err != nil {
		return nil, err
	}
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

func (c *Server) validate() error {
	var problems []error

	if err := c.Policy.Validate(); err != nil {
		problems = append(problems, err)
	}
	if c.Backend == "postgres" && c.DatabaseURL == "" {
		problems = append(problems, errors.New("config: QUORRA_STORE is postgres, so DATABASE_URL must be set"))
	}
	if c.ReclaimEvery <= 0 {
		problems = append(problems, fmt.Errorf("config: QUORRA_RECLAIM_EVERY is %s, and a ticker cannot run on it", c.ReclaimEvery))
	}
	if c.StatsEvery <= 0 {
		problems = append(problems, fmt.Errorf("config: QUORRA_STATS_EVERY is %s, and a ticker cannot run on it", c.StatsEvery))
	}
	if c.ReclaimBatch <= 0 {
		problems = append(problems, fmt.Errorf("config: QUORRA_RECLAIM_BATCH is %d, so the reclaimer would take nothing back", c.ReclaimBatch))
	}
	if c.MaxBodyBytes <= 0 {
		problems = append(problems, fmt.Errorf("config: QUORRA_MAX_BODY_BYTES is %d, so every request would be refused", c.MaxBodyBytes))
	}

	return errors.Join(problems...)
}

// UsesMemory reports whether the server keeps its jobs in memory. The server
// says so at startup, because nothing in that mode survives a restart.
func (c *Server) UsesMemory() bool { return c.Backend == "memory" }

// LoadWorker reads the worker settings.
func LoadWorker(getenv Getenv) (*Worker, error) {
	l := &loader{getenv: getenv}

	cfg := &Worker{
		ID:            l.text("QUORRA_WORKER_ID", "worker-1"),
		ServerAddr:    l.text("QUORRA_GRPC_ADDR", "localhost:50051"),
		Queues:        l.list("QUORRA_WORKER_QUEUES", []string{"default"}),
		MaxJobs:       l.number("QUORRA_WORKER_MAX_JOBS", 5),
		LeaseTTL:      l.duration("QUORRA_WORKER_LEASE_TTL", 30*time.Second),
		PollEvery:     l.duration("QUORRA_WORKER_POLL_EVERY", time.Second),
		LogLevel:      l.level("QUORRA_LOG_LEVEL", slog.LevelInfo),
		ShutdownGrace: l.duration("QUORRA_SHUTDOWN_GRACE", 30*time.Second),
	}

	if err := l.err(); err != nil {
		return nil, err
	}

	var problems []error
	if len(cfg.Queues) == 0 {
		problems = append(problems, errors.New("config: QUORRA_WORKER_QUEUES names no queue, so the worker would ask for nothing"))
	}
	if cfg.MaxJobs <= 0 {
		problems = append(problems, fmt.Errorf("config: QUORRA_WORKER_MAX_JOBS is %d, so the worker would ask for no jobs", cfg.MaxJobs))
	}
	if cfg.LeaseTTL <= 0 {
		problems = append(problems, fmt.Errorf("config: QUORRA_WORKER_LEASE_TTL is %s, so every lease would expire as it was granted", cfg.LeaseTTL))
	}
	if cfg.PollEvery <= 0 {
		problems = append(problems, fmt.Errorf("config: QUORRA_WORKER_POLL_EVERY is %s, and a ticker cannot run on it", cfg.PollEvery))
	}

	// A worker that takes longer to finish a job than its lease allows will
	// have that job taken back and given to somebody else while it is still
	// working on it. Naming it here is cheaper than finding it in a
	// duplicated side effect.
	if cfg.ShutdownGrace > cfg.LeaseTTL {
		problems = append(problems, fmt.Errorf(
			"config: QUORRA_SHUTDOWN_GRACE is %s and QUORRA_WORKER_LEASE_TTL is %s, so a job still running at shutdown loses its lease and is given to another worker",
			cfg.ShutdownGrace, cfg.LeaseTTL))
	}

	if err := errors.Join(problems...); err != nil {
		return nil, err
	}
	return cfg, nil
}

// ---------------------------------------------------------------------------
// The reader
// ---------------------------------------------------------------------------

type loader struct {
	getenv   Getenv
	problems []error
}

func (l *loader) err() error { return errors.Join(l.problems...) }

func (l *loader) raw(key string) (string, bool) {
	value := strings.TrimSpace(l.getenv(key))
	return value, value != ""
}

func (l *loader) text(key, fallback string) string {
	if value, set := l.raw(key); set {
		return value
	}
	return fallback
}

func (l *loader) required(key string) string {
	value, set := l.raw(key)
	if !set {
		l.problems = append(l.problems, fmt.Errorf("config: %s must be set", key))
	}
	return value
}

func (l *loader) number(key string, fallback int) int {
	value, set := l.raw(key)
	if !set {
		return fallback
	}
	n, err := strconv.Atoi(value)
	if err != nil {
		l.problems = append(l.problems, fmt.Errorf("config: %s is %q, which is not a whole number", key, value))
		return fallback
	}
	return n
}

func (l *loader) duration(key string, fallback time.Duration) time.Duration {
	value, set := l.raw(key)
	if !set {
		return fallback
	}
	d, err := time.ParseDuration(value)
	if err != nil {
		l.problems = append(l.problems, fmt.Errorf("config: %s is %q, which is not a length of time such as 30s or 5m", key, value))
		return fallback
	}
	return d
}

func (l *loader) list(key string, fallback []string) []string {
	value, set := l.raw(key)
	if !set {
		return fallback
	}

	var out []string
	for _, part := range strings.Split(value, ",") {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

func (l *loader) choice(key, fallback string, allowed ...string) string {
	value, set := l.raw(key)
	if !set {
		return fallback
	}
	for _, ok := range allowed {
		if value == ok {
			return value
		}
	}
	l.problems = append(l.problems, fmt.Errorf("config: %s is %q, and it must be one of %s", key, value, strings.Join(allowed, ", ")))
	return fallback
}

func (l *loader) level(key string, fallback slog.Level) slog.Level {
	value, set := l.raw(key)
	if !set {
		return fallback
	}

	var level slog.Level
	if err := level.UnmarshalText([]byte(value)); err != nil {
		l.problems = append(l.problems, fmt.Errorf("config: %s is %q, and it must be one of debug, info, warn, error", key, value))
		return fallback
	}
	return level
}
