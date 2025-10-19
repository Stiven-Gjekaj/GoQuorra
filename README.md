# GoQuorra

> **A lightweight, distributed job queue for Go — built for reliability, observability, and scale.**

[![Go Version](https://img.shields.io/badge/Go-1.21+-00ADD8?style=flat&logo=go)](https://golang.org)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](https://opensource.org/licenses/MIT)
[![Docker](https://img.shields.io/badge/Docker-ready-2496ED?style=flat&logo=docker)](https://www.docker.com/)
[![CI](https://img.shields.io/badge/CI-passing-brightgreen)](https://github.com/features/actions)

---

## Table of Contents

1. [🌀 Overview](#-overview)
2. [⚙️ Features](#️-features)
3. [🧠 Architecture](#-architecture)
4. [🧩 Job Lifecycle](#-job-lifecycle)
5. [🚀 Quickstart](#-quickstart)
6. [💻 API Reference](#-api-reference)
7. [🧑‍💻 Example Worker Code](#-example-worker-code)
8. [📈 Metrics & Monitoring](#-metrics--monitoring)
9. [🛠️ Development Setup](#️-development-setup)
10. [📦 Project Structure](#-project-structure)
11. [🧭 Roadmap](#-roadmap)
12. [🤝 Contributing](#-contributing)
13. [📜 License](LICENSE)

---

## 🌀 Overview

**GoQuorra** is a production-ready distributed job queue system designed for backend engineers who need reliable background job processing without operational complexity. Built in [Go](https://golang.org), it provides REST and [gRPC](https://grpc.io) APIs for job submission, worker communication via streaming protocols, and full observability through [Prometheus](https://prometheus.io) metrics.

Unlike heavyweight message brokers, GoQuorra focuses on **simplicity and correctness**. It uses [PostgreSQL](https://postgresql.org) for ACID-compliant job persistence and optional [Redis](https://redis.io) for high-throughput scenarios, with graceful fallback when Redis is unavailable. Every job is tracked through a well-defined state machine, with automatic retries, exponential backoff, and dead-letter queues for failed jobs.

Whether you're building async APIs, processing webhooks, sending emails, or running batch jobs, GoQuorra gives you the primitives to scale horizontally while maintaining exactly-once processing guarantees. It's designed to be deployed alongside your existing stack — no specialized infrastructure required.

This is an Alpha Version (v0.1.0) that demonstrates core distributed systems concepts: atomic job leasing with `SELECT FOR UPDATE SKIP LOCKED`, lease-based concurrency control, idempotent state transitions, and proper observability. Perfect for portfolio projects or production workloads that don't require Kafka-level complexity.

---

## ⚙️ Features

- **Dual API Support**: REST API for job submission, gRPC for efficient worker communication
- **Multiple Storage Backends**: PostgreSQL (primary) with optional Redis for pub/sub notifications
- **Atomic Job Leasing**: Uses `SELECT FOR UPDATE SKIP LOCKED` to prevent double-processing
- **Smart Retry Logic**: Exponential backoff (2^n seconds, capped at 1 hour) with configurable max retries
- **Dead-Letter Queue**: Failed jobs move to DLQ after exhausting retries
- **Priority & Delayed Jobs**: Schedule jobs for future execution with priority ordering
- **Prometheus Metrics**: Built-in observability with `/metrics` endpoint
- **Web Dashboard**: Real-time monitoring UI with queue statistics and job history
- **CLI Tool**: `quorractl` for job management from the command line
- **Horizontal Scaling**: Run multiple workers and server replicas without coordination overhead
- **Graceful Shutdown**: Proper SIGTERM handling for zero-downtime deploys
- **Battle-Tested Patterns**: Connection pooling, structured logging, health checks, transaction safety
- **Minimal Dependencies**: Standard library + chi router, no bloated frameworks
- **Docker Ready**: Full docker-compose stack for development and testing

---

## 🧠 Architecture

GoQuorra follows a **centralized queue, distributed workers** model. The server manages job state in PostgreSQL, while workers lease jobs via gRPC and process them independently.

### System Diagram

```mermaid
graph LR
    Client[REST Client] -->|POST /v1/jobs| API[GoQuorra Server<br/>:8080]
    API -->|Store Job| DB[(PostgreSQL<br/>Jobs Table)]
    API -.->|Notify| Redis[(Redis<br/>Optional)]

    DB -->|SELECT FOR UPDATE<br/>SKIP LOCKED| GRPC[gRPC Server<br/>:50051]
    GRPC -->|Stream Jobs| W1[Worker 1]
    GRPC -->|Stream Jobs| W2[Worker 2]
    GRPC -->|Stream Jobs| WN[Worker N]

    W1 -->|AckJob/NackJob| GRPC
    W2 -->|AckJob/NackJob| GRPC
    WN -->|AckJob/NackJob| GRPC

    API -->|Expose| Metrics[/metrics<br/>Prometheus]
    API -->|Serve| Dashboard[Web Dashboard<br/>localhost:8080]
```

### How It Works

1. **Job Submission**: Clients send jobs via REST API (`POST /v1/jobs`). Jobs are stored in PostgreSQL with `status=pending`.

2. **Atomic Leasing**: Workers call `LeaseJobs` gRPC method. The server uses PostgreSQL's `SELECT FOR UPDATE SKIP LOCKED` to atomically lease available jobs without blocking other workers. Each job gets a unique `lease_id` and TTL.

3. **Processing**: Workers receive jobs as a gRPC stream, process the payload, and call `AckJob` (success) or `NackJob` (failure).

4. **Retry & DLQ**: Failed jobs return to `pending` with exponential backoff. After `max_retries`, they move to `status=dead` (dead-letter queue).

5. **Scheduler**: A background goroutine moves delayed jobs (future `run_at`) to the ready state when their time arrives.

### Concurrency & Correctness

- **Atomic Leasing**: The `FOR UPDATE SKIP LOCKED` clause ensures only one worker can lease a job, even under high concurrency.
- **Lease Verification**: `AckJob`/`NackJob` validate the `lease_id` to prevent stale acknowledgments.
- **Transaction Safety**: All state transitions use database transactions to maintain consistency.
- **Idempotency**: Retry-safe operations ensure jobs aren't processed multiple times.

### Fault Tolerance

- **Worker Crashes**: Jobs remain leased until TTL expires. Future versions will include lease expiration cleanup.
- **Server Restarts**: Job state persists in PostgreSQL; no data loss.
- **Database Failures**: Server returns errors; clients can retry job submission.
- **Redis Failures**: System gracefully falls back to Postgres-only mode.

---

## 🧩 Job Lifecycle

Jobs transition through a well-defined state machine:

```mermaid
stateDiagram-v2
    [*] --> pending: Job Created
    pending --> leased: Worker Leases
    leased --> processing: Worker Starts
    processing --> succeeded: AckJob
    processing --> pending: NackJob (retry)
    pending --> dead: Max Retries Exceeded
    succeeded --> [*]
    dead --> [*]
```

### States Explained

| State        | Description                                                  |
| ------------ | ------------------------------------------------------------ |
| `pending`    | Job is ready to be processed (or scheduled for future)       |
| `leased`     | Job is assigned to a worker (has `lease_id` and `leased_at`) |
| `processing` | Worker is actively processing the job                        |
| `succeeded`  | Job completed successfully                                   |
| `failed`     | Job failed but will retry (transient state)                  |
| `dead`       | Job exceeded `max_retries`, moved to DLQ                     |

### Sequence Diagram

```mermaid
sequenceDiagram
    participant C as Client
    participant S as GoQuorra Server
    participant DB as PostgreSQL
    participant W as Worker
    participant M as Metrics

    C->>S: POST /v1/jobs
    S->>DB: INSERT INTO jobs (status=pending)
    DB-->>S: Job ID
    S-->>C: {id, status, run_at}

    W->>S: LeaseJobs (gRPC)
    S->>DB: SELECT FOR UPDATE SKIP LOCKED
    DB-->>S: Job row
    S->>DB: UPDATE status=leased, lease_id=...
    S-->>W: Stream Job

    W->>W: Process job payload

    alt Success
        W->>S: AckJob (lease_id)
        S->>DB: UPDATE status=succeeded
        S->>M: Increment jobs_processed
    else Failure
        W->>S: NackJob (lease_id, error)
        S->>DB: UPDATE status=pending, attempts++
        S->>M: Increment jobs_failed
        Note over S,DB: Retry with exponential backoff
    end
```

### Retry Logic

Failed jobs are retried with exponential backoff:

```
Backoff = min(2^attempts seconds, 3600 seconds)
```

**Example:**

- Attempt 1 fails → retry in 2s
- Attempt 2 fails → retry in 4s
- Attempt 3 fails → retry in 8s
- Attempt 10 fails → retry in 1024s (capped at 1 hour)

After `max_retries`, the job moves to `status=dead` and appears in the dead-letter queue.

---

## 🚀 Quickstart

Get GoQuorra running in under 5 minutes.

### Prerequisites

- [Docker](https://docs.docker.com/get-docker/) & [Docker Compose](https://docs.docker.com/compose/install/)
- curl (or [Postman](https://www.postman.com/), [HTTPie](https://httpie.io/))

### Step 1: Start the System

```bash
# Clone the repository
git clone https://github.com/yourusername/goquorra.git
cd goquorra

# Start PostgreSQL, Redis, Server, and Workers
docker-compose -f deployments/docker-compose.yml up --build
```

**What's running:**

- PostgreSQL on `localhost:5432`
- Redis on `localhost:6379`
- GoQuorra Server on `localhost:8080` (HTTP) and `localhost:50051` (gRPC)
- Two workers processing jobs from `default`, `email`, and `processing` queues

### Step 2: Create a Job

```bash
curl -X POST http://localhost:8080/v1/jobs \
  -H "Content-Type: application/json" \
  -H "X-API-Key: dev-api-key-change-in-production" \
  -d '{
    "type": "email_send",
    "payload": {
      "to": "user@example.com",
      "subject": "Welcome to GoQuorra!",
      "body": "Your job queue is ready."
    },
    "queue": "default",
    "priority": 10,
    "max_retries": 3
  }'
```

**Response:**

```json
{
  "id": "550e8400-e29b-41d4-a716-446655440000",
  "status": "pending",
  "run_at": "2025-10-11T12:00:00Z"
}
```

### Step 3: Check Job Status

```bash
# Replace {id} with your job ID
curl http://localhost:8080/v1/jobs/550e8400-e29b-41d4-a716-446655440000 \
  -H "X-API-Key: dev-api-key-change-in-production"
```

**Response:**

```json
{
  "id": "550e8400-e29b-41d4-a716-446655440000",
  "type": "email_send",
  "status": "succeeded",
  "attempts": 1,
  "created_at": "2025-10-11T12:00:00Z",
  "updated_at": "2025-10-11T12:00:03Z"
}
```

### Step 4: View the Dashboard

Open http://localhost:8080 in your browser. You'll see:

- Queue statistics (pending, succeeded, failed counts)
- Recent jobs table
- Auto-refresh every 5 seconds

### Step 5: Check Metrics

```bash
curl http://localhost:8080/metrics | grep quorra_jobs
```

**Output:**

```
quorra_jobs_created_total 1
quorra_jobs_processed_total 1
quorra_jobs_leased_total 1
```

### Delayed Job Example

Schedule a job to run in 60 seconds:

```bash
curl -X POST http://localhost:8080/v1/jobs \
  -H "Content-Type: application/json" \
  -H "X-API-Key: dev-api-key-change-in-production" \
  -d '{
    "type": "reminder",
    "payload": {"message": "Time to review your dashboard!"},
    "delay_seconds": 60,
    "max_retries": 5
  }'
```

The job will remain `pending` until 60 seconds elapse, then a worker will process it.

---

## 💻 API Reference

### REST API

All REST endpoints require authentication via the `X-API-Key` header.

#### `POST /v1/jobs`

Create a new job.

**Request:**

```json
{
  "type": "string (required)",
  "payload": "object (required)",
  "queue": "string (default: 'default')",
  "priority": "integer (default: 0)",
  "delay_seconds": "integer (default: 0)",
  "max_retries": "integer (default: 3)"
}
```

**Response:**

```json
{
  "id": "uuid",
  "status": "pending",
  "run_at": "ISO8601 timestamp"
}
```

**Example:**

```bash
curl -X POST http://localhost:8080/v1/jobs \
  -H "Content-Type: application/json" \
  -H "X-API-Key: your-api-key" \
  -d '{
    "type": "webhook_call",
    "payload": {"url": "https://api.example.com/notify"},
    "queue": "webhooks",
    "priority": 5,
    "max_retries": 3
  }'
```

#### `GET /v1/jobs/{id}`

Retrieve job details.

**Response:**

```json
{
  "id": "uuid",
  "type": "string",
  "payload": "object",
  "queue": "string",
  "priority": "integer",
  "status": "pending|leased|succeeded|failed|dead",
  "attempts": "integer",
  "max_retries": "integer",
  "last_error": "string (optional)",
  "created_at": "ISO8601 timestamp",
  "updated_at": "ISO8601 timestamp"
}
```

#### `GET /v1/queues`

List queue statistics.

**Response:**

```json
{
  "queues": [
    { "queue": "default", "status": "pending", "count": 12 },
    { "queue": "default", "status": "succeeded", "count": 145 },
    { "queue": "email", "status": "pending", "count": 3 }
  ]
}
```

#### `GET /metrics`

Prometheus metrics endpoint (no authentication required).

### gRPC API

Workers communicate via [Protocol Buffers](https://protobuf.dev/). See [`proto/quorra.proto`](proto/quorra.proto) for definitions.

#### `LeaseJobs`

Stream jobs from the server.

**Request:**

```protobuf
message LeaseRequest {
  string worker_id = 1;
  string queue = 2;
  int32 max_jobs = 3;
  int32 lease_ttl_seconds = 4;
}
```

**Response:** Server-streaming `Job` messages.

#### `AckJob`

Acknowledge successful job completion.

**Request:**

```protobuf
message JobAck {
  string job_id = 1;
  string worker_id = 2;
  string lease_id = 3;
  bool success = 4;
}
```

#### `NackJob`

Signal job failure (triggers retry or DLQ).

**Request:**

```protobuf
message JobAck {
  string job_id = 1;
  string worker_id = 2;
  string lease_id = 3;
  bool success = 4;
  string error_message = 5;
}
```

---

## 🧑‍💻 Example Worker Code

Workers connect to the gRPC server, lease jobs, process them, and acknowledge completion.

### Minimal Worker

```go
package main

import (
    "context"
    "encoding/json"
    "fmt"
    "log"
    "os"
    "time"

    "github.com/goquorra/goquorra/internal/worker"
)

func main() {
    cfg := &worker.Config{
        ID:         "email-worker-1",
        ServerAddr: "localhost:50051",
        Queues:     []string{"email", "notifications"},
        MaxJobs:    10,
        LeaseTTL:   60 * time.Second,
    }

    logger := log.New(os.Stdout, "[worker] ", log.LstdFlags)
    w := worker.New(cfg, logger)

    ctx := context.Background()
    if err := w.Start(ctx); err != nil {
        log.Fatal(err)
    }
}
```

### Custom Job Processor

To handle specific job types, modify `internal/worker/worker.go`:

```go
func (w *Worker) executeJob(jobType string, payload map[string]interface{}) bool {
    switch jobType {
    case "email_send":
        return w.sendEmail(payload)
    case "image_resize":
        return w.resizeImage(payload)
    case "webhook_call":
        return w.callWebhook(payload)
    default:
        w.logger.Printf("Unknown job type: %s", jobType)
        return false
    }
}

func (w *Worker) sendEmail(payload map[string]interface{}) bool {
    to := payload["to"].(string)
    subject := payload["subject"].(string)
    body := payload["body"].(string)

    // Implement your email sending logic here
    // Return true on success, false to retry
    fmt.Printf("Sending email to %s: %s\n", to, subject)
    return true
}
```

### Worker Configuration

| Variable                  | Default           | Description                   |
| ------------------------- | ----------------- | ----------------------------- |
| `QUORRA_WORKER_ID`        | `worker-1`        | Unique worker identifier      |
| `QUORRA_WORKER_QUEUES`    | `default`         | Comma-separated queue names   |
| `QUORRA_WORKER_MAX_JOBS`  | `5`               | Max jobs to lease per request |
| `QUORRA_WORKER_LEASE_TTL` | `30s`             | Lease duration                |
| `QUORRA_GRPC_ADDR`        | `localhost:50051` | Server gRPC address           |

---

## 📈 Metrics & Monitoring

GoQuorra exposes [Prometheus](https://prometheus.io)-compatible metrics on `/metrics`.

### Available Metrics

| Metric                                  | Type    | Description                         |
| --------------------------------------- | ------- | ----------------------------------- |
| `quorra_jobs_created_total`             | Counter | Total jobs created                  |
| `quorra_jobs_processed_total`           | Counter | Total jobs successfully processed   |
| `quorra_jobs_failed_total`              | Counter | Total jobs that failed (will retry) |
| `quorra_jobs_dead_total`                | Counter | Total jobs moved to DLQ             |
| `quorra_jobs_leased_total`              | Counter | Total job lease operations          |
| `quorra_job_queue_length{queue,status}` | Gauge   | Current queue length by status      |

### Scraping Metrics

**Manual check:**

```bash
curl http://localhost:8080/metrics
```

**Prometheus configuration:**

```yaml
scrape_configs:
  - job_name: "goquorra"
    static_configs:
      - targets: ["localhost:8080"]
    metrics_path: "/metrics"
```

### Grafana Dashboard

Create panels for:

- **Job Throughput**: `rate(quorra_jobs_processed_total[5m])`
- **Queue Depth**: `quorra_job_queue_length{status="pending"}`
- **Failure Rate**: `rate(quorra_jobs_failed_total[5m]) / rate(quorra_jobs_created_total[5m])`
- **DLQ Growth**: `quorra_jobs_dead_total`

### Health Check

```bash
curl http://localhost:8080/healthz
# Returns 200 OK if server is healthy
```

---

## 🛠️ Development Setup

### Prerequisites

- [Go](https://golang.org/doc/install) 1.21 or later
- [PostgreSQL](https://www.postgresql.org/download/) 15+
- [Redis](https://redis.io/download) 7+ (optional)
- [Docker](https://docs.docker.com/get-docker/) & [Docker Compose](https://docs.docker.com/compose/install/)
- [Make](https://www.gnu.org/software/make/) (optional, for convenience)

### Clone & Install

```bash
git clone https://github.com/yourusername/goquorra.git
cd goquorra

# Download dependencies
go mod download
```

### Environment Configuration

Copy the example environment file:

```bash
cp .env.example .env
```

Edit `.env`:

```bash
# Server
QUORRA_HTTP_ADDR=:8080
QUORRA_GRPC_ADDR=:50051
QUORRA_LOG_LEVEL=info

# Database
DATABASE_URL=postgres://quorra:quorra@localhost:5432/quorra?sslmode=disable

# Redis (optional)
REDIS_URL=redis://localhost:6379/0

# Authentication
QUORRA_API_KEY=your-secret-api-key-here
```

### Initialize Database

```bash
# Start PostgreSQL via Docker
docker-compose -f deployments/docker-compose.yml up -d postgres

# Run schema migration
psql postgres://quorra:quorra@localhost:5432/quorra -f scripts/init_db.sql
```

Or use the Makefile:

```bash
make db-init
```

### Build Binaries

```bash
make build
```

This creates:

- `bin/quorra-server` - Main server
- `bin/quorra-worker` - Worker executable
- `bin/quorractl` - CLI tool

### Run Locally

**Terminal 1: Start server**

```bash
export DATABASE_URL="postgres://quorra:quorra@localhost:5432/quorra?sslmode=disable"
export REDIS_URL="redis://localhost:6379/0"
export QUORRA_API_KEY="dev-api-key"

./bin/quorra-server
```

**Terminal 2: Start worker**

```bash
export QUORRA_WORKER_ID="dev-worker"
export QUORRA_WORKER_QUEUES="default,email"
export QUORRA_GRPC_ADDR="localhost:50051"

./bin/quorra-worker
```

### Run Tests

```bash
# Unit tests
make test

# With coverage
go test -v -race -coverprofile=coverage.out ./...
go tool cover -html=coverage.out

# Integration tests (requires running services)
docker-compose -f deployments/docker-compose.yml up -d
go test -v -tags=integration ./tests/...
```

### Linting

```bash
make lint

# Or manually
gofmt -s -w .
go vet ./...

# With golangci-lint (recommended)
golangci-lint run --timeout 5m
```

### Makefile Commands

| Command             | Description                  |
| ------------------- | ---------------------------- |
| `make build`        | Build all binaries           |
| `make test`         | Run unit tests with coverage |
| `make dev`          | Start docker-compose stack   |
| `make dev-down`     | Stop docker-compose          |
| `make docker-build` | Build Docker image           |
| `make lint`         | Run linters                  |
| `make clean`        | Remove build artifacts       |

---

## 📦 Project Structure

```
goquorra/
├── cmd/
│   ├── quorra-server/          # Main server entry point
│   ├── quorra-worker/          # Worker executable
│   └── quorractl/              # CLI tool
├── internal/
│   ├── api/                    # REST API handlers (chi router)
│   ├── grpc/                   # gRPC service + protobuf code
│   ├── store/                  # PostgreSQL persistence layer
│   ├── queue/                  # Queue manager + scheduler
│   ├── worker/                 # Worker client library
│   ├── metrics/                # Prometheus metrics collectors
│   └── config/                 # Configuration management
├── proto/
│   └── quorra.proto            # Protocol Buffers definitions
├── tests/
│   ├── store_test.go           # Unit tests for store layer
│   ├── queue_test.go           # Queue manager tests
│   └── integration_test.go     # End-to-end tests
├── scripts/
│   ├── init_db.sql             # Database schema
│   └── generate-proto.sh       # Protobuf code generation
├── deployments/
│   ├── docker-compose.yml      # Local development stack
│   └── k8s/                    # Kubernetes manifests (basic)
├── .github/
│   └── workflows/
│       └── ci.yml              # GitHub Actions CI/CD
├── Dockerfile                  # Multi-stage build
├── Makefile                    # Development tasks
├── go.mod                      # Go dependencies
├── go.sum                      # Dependency checksums
├── LICENSE                     # MIT License
└── README.md                   # This file
```

### Key Directories

- **`cmd/`**: Main applications (server, worker, CLI)
- **`internal/`**: Private packages (not importable by external projects)
  - `api/`: HTTP handlers, middleware, dashboard
  - `grpc/`: gRPC service implementation + generated protobuf code
  - `store/`: Database abstraction (PostgreSQL queries)
  - `queue/`: Core queue logic, job scheduling, retry handling
  - `worker/`: Client library for worker implementations
  - `metrics/`: Prometheus metric definitions
- **`proto/`**: Protobuf schema for worker protocol
- **`tests/`**: Test suites (unit and integration)
- **`scripts/`**: Database migrations, code generation
- **`deployments/`**: Deployment configurations (Docker, Kubernetes)

---

## 🧭 Roadmap

### v0.1.0 (Current)

✅ REST API for job management
✅ gRPC worker protocol
✅ PostgreSQL + Redis persistence
✅ Retry logic with exponential backoff
✅ Dead-letter queue
✅ Prometheus metrics
✅ Web dashboard
✅ CLI tool
✅ Docker Compose deployment
✅ Unit and integration tests

### v0.2.0 (Next Release)

- [ ] JWT authentication (replace API key)
- [ ] Lease expiration cleanup job
- [ ] Job cancellation API
- [ ] Worker health checks and auto-scaling
- [ ] Advanced dashboard with filtering and search
- [ ] Helm chart for Kubernetes

### v0.3.0 (Future)

- [ ] Job dependencies (DAG execution)
- [ ] Parent-child job relationships
- [ ] Webhook notifications for job events
- [ ] OpenTelemetry distributed tracing
- [ ] Admin UI (React-based)
- [ ] Multi-tenancy support

### v0.4.0 (Future)

- [ ] Message broker backend (NATS, Kafka)
- [ ] Leader election for high availability
- [ ] Job result persistence and retrieval
- [ ] Scheduled/cron jobs
- [ ] Job priority inheritance
- [ ] Rate limiting per queue

### v1.0.0 (Production Ready)

- [ ] Multi-region support
- [ ] Comprehensive security hardening
- [ ] Performance benchmarks and tuning
- [ ] Migration guides from Sidekiq, Celery, Bull
- [ ] Enterprise features (audit logs, RBAC)

---

## 🤝 Contributing

Contributions are welcome! Whether it's bug reports, feature requests, or pull requests, your input helps make GoQuorra better.

### How to Contribute

1. **Fork the repository** on GitHub
2. **Clone your fork**: `git clone https://github.com/yourusername/goquorra.git`
3. **Create a feature branch**: `git checkout -b feature/amazing-feature`
4. **Make your changes** and add tests
5. **Run tests**: `make test`
6. **Lint your code**: `make lint`
7. **Commit your changes**: `git commit -m 'Add amazing feature'`
8. **Push to your fork**: `git push origin feature/amazing-feature`
9. **Open a Pull Request** on the main repository

### Contribution Guidelines

- Write clear commit messages following [Conventional Commits](https://www.conventionalcommits.org/)
- Add tests for new features
- Update documentation for API changes
- Ensure all tests pass before submitting PR
- Follow Go idioms and [Effective Go](https://go.dev/doc/effective_go) guidelines
- Keep PRs focused (one feature/fix per PR)

### Reporting Bugs

Open an [issue](https://github.com/yourusername/goquorra/issues) with:

- Go version
- Database version (PostgreSQL/Redis)
- Steps to reproduce
- Expected vs. actual behavior
- Relevant logs

### Feature Requests

Have an idea? Open an [issue](https://github.com/yourusername/goquorra/issues) and describe:

- Use case
- Proposed API/behavior
- Why it's important

---

## 📜 License

GoQuorra is licensed under the [MIT License](LICENSE).

```
MIT License

Copyright (c) 2025 GoQuorra Contributors

Permission is hereby granted, free of charge, to any person obtaining a copy
of this software and associated documentation files (the "Software"), to deal
in the Software without restriction, including without limitation the rights
to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
copies of the Software, and to permit persons to whom the Software is
furnished to do so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in all
copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
SOFTWARE.
```

---

**Built with ❤️ and Go**

For questions or support, open an [issue](https://github.com/yourusername/goquorra/issues) or start a [discussion](https://github.com/yourusername/goquorra/discussions).
