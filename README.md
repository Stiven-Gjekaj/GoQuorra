# GoQuorra

**A reliable, distributed job queue system built in Go for background task processing.**

[![CI](https://github.com/goquorra/goquorra/actions/workflows/ci.yml/badge.svg)](https://github.com/goquorra/goquorra/actions/workflows/ci.yml)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](https://opensource.org/licenses/MIT)
[![Go Version](https://img.shields.io/badge/Go-1.21+-00ADD8?style=flat&logo=go)](https://golang.org)

GoQuorra is a production-ready distributed job queue that accepts jobs via REST and gRPC, persists job metadata in PostgreSQL, dispatches work to workers, and provides retry logic, priority queuing, delayed jobs, dead-letter queue (DLQ), authentication, metrics, and a web dashboard.

---

## Table of Contents

- [Features](#features)
- [Architecture](#architecture)
- [Quick Start](#quick-start)
- [API Documentation](#api-documentation)
- [Configuration](#configuration)
- [Development](#development)
- [Testing](#testing)
- [Deployment](#deployment)
- [Contributing](#contributing)
- [License](#license)

---

## Features

- **REST API** for job management (create, get status, list queues)
- **gRPC Worker Protocol** with streaming job lease
- **Job Persistence** using PostgreSQL
- **Redis Streams** support (optional, graceful fallback to Postgres-only mode)
- **Delayed Jobs** with configurable run_at time
- **Priority Queuing** for job ordering
- **Retry Logic** with exponential backoff and configurable max retries
- **Dead-Letter Queue (DLQ)** for jobs exceeding retry limits
- **Prometheus Metrics** endpoint for monitoring
- **Web Dashboard** for real-time queue monitoring
- **CLI Tool** (`quorractl`) for job management
- **API Key Authentication** for REST endpoints
- **Docker + Docker Compose** for easy local development
- **Comprehensive Tests** (unit and integration)
- **CI/CD** with GitHub Actions

---

## Architecture

```
                    ┌─────────────────┐
                    │   REST Client   │
                    └────────┬────────┘
                             │ POST /v1/jobs
                             ▼
                    ┌─────────────────┐
                    │  GoQuorra       │
                    │  HTTP Server    │◄────── GET /metrics (Prometheus)
                    │  (Port 8080)    │
                    └────────┬────────┘
                             │
                ┌────────────┴────────────┐
                │                         │
                ▼                         ▼
        ┌───────────────┐         ┌──────────────┐
        │  PostgreSQL   │         │    Redis     │
        │  (Jobs Store) │         │  (Optional)  │
        └───────────────┘         └──────────────┘
                │
                │ Lease Jobs (via gRPC)
                │
                ▼
        ┌─────────────────┐
        │  GoQuorra       │
        │  gRPC Server    │
        │  (Port 50051)   │
        └────────┬────────┘
                 │
        ┌────────┴─────────┬──────────┐
        ▼                  ▼          ▼
   ┌─────────┐       ┌─────────┐  ┌─────────┐
   │Worker 1 │       │Worker 2 │  │Worker N │
   └─────────┘       └─────────┘  └─────────┘

   Flow:
   1. Client creates job via REST API
   2. Job stored in PostgreSQL with status=pending
   3. Worker calls LeaseJobs via gRPC
   4. Server atomically leases job (status=leased)
   5. Worker processes job
   6. Worker calls AckJob (success) or NackJob (failure)
   7. Server updates job status (succeeded/failed/dead)
```

---

## Quick Start

### Prerequisites

- Docker & Docker Compose
- Go 1.21+ (for local development)
- Make (optional, for convenience commands)

### Run with Docker Compose

```bash
# Clone the repository
git clone https://github.com/goquorra/goquorra.git
cd goquorra

# Start all services (Postgres, Redis, Server, Workers)
make dev

# Or using docker-compose directly:
docker-compose -f deployments/docker-compose.yml up --build
```

This starts:
- **PostgreSQL** on port 5432
- **Redis** on port 6379
- **GoQuorra Server** on ports 8080 (HTTP) and 50051 (gRPC)
- **Two Workers** processing jobs from queues

### Access the Dashboard

Open your browser to:
```
http://localhost:8080
```

You'll see:
- Real-time queue statistics
- Recent jobs
- Job statuses

---

## API Documentation

### Authentication

All API requests require an API key passed via header:

```bash
X-API-Key: dev-api-key-change-in-production
```

Or as query parameter:
```
?api_key=dev-api-key-change-in-production
```

### Endpoints

#### Create Job

**`POST /v1/jobs`**

Create a new job.

**Request:**
```json
{
  "type": "email_send",
  "payload": {
    "to": "user@example.com",
    "subject": "Welcome!",
    "body": "Thanks for signing up."
  },
  "queue": "default",
  "priority": 10,
  "delay_seconds": 0,
  "max_retries": 5
}
```

**Response:**
```json
{
  "id": "a1b2c3d4-e5f6-7890-abcd-ef1234567890",
  "status": "pending",
  "run_at": "2025-10-11T12:00:00Z"
}
```

**Example:**
```bash
curl -X POST http://localhost:8080/v1/jobs \
  -H "Content-Type: application/json" \
  -H "X-API-Key: dev-api-key-change-in-production" \
  -d '{
    "type": "email_send",
    "payload": {"to": "test@example.com", "subject": "Hello"},
    "queue": "default",
    "priority": 10,
    "max_retries": 3
  }'
```

#### Get Job

**`GET /v1/jobs/{id}`**

Retrieve job details by ID.

**Response:**
```json
{
  "id": "a1b2c3d4-e5f6-7890-abcd-ef1234567890",
  "type": "email_send",
  "payload": {"to": "test@example.com", "subject": "Hello"},
  "queue": "default",
  "priority": 10,
  "status": "succeeded",
  "attempts": 1,
  "max_retries": 3,
  "created_at": "2025-10-11T12:00:00Z",
  "updated_at": "2025-10-11T12:00:05Z"
}
```

**Example:**
```bash
curl http://localhost:8080/v1/jobs/a1b2c3d4-e5f6-7890-abcd-ef1234567890 \
  -H "X-API-Key: dev-api-key-change-in-production"
```

#### List Queues

**`GET /v1/queues`**

Get statistics for all queues.

**Response:**
```json
{
  "queues": [
    {"queue": "default", "status": "pending", "count": 12},
    {"queue": "default", "status": "succeeded", "count": 145},
    {"queue": "email", "status": "pending", "count": 3}
  ]
}
```

**Example:**
```bash
curl http://localhost:8080/v1/queues \
  -H "X-API-Key: dev-api-key-change-in-production"
```

#### Metrics

**`GET /metrics`**

Prometheus metrics endpoint (no auth required).

**Example:**
```bash
curl http://localhost:8080/metrics
```

**Metrics exposed:**
- `quorra_jobs_created_total` - Total jobs created
- `quorra_jobs_processed_total` - Total jobs successfully processed
- `quorra_jobs_failed_total` - Total jobs that failed
- `quorra_jobs_dead_total` - Total jobs in DLQ
- `quorra_jobs_leased_total` - Total job leases
- `quorra_job_queue_length{queue,status}` - Current queue length by status

---

## CLI Tool: quorractl

`quorractl` is a command-line tool for interacting with GoQuorra.

### Installation

```bash
# Build from source
make build

# Or build manually
go build -o bin/quorractl ./cmd/quorractl
```

### Usage

**Create a job:**
```bash
./bin/quorractl create email_send \
  --payload '{"to":"user@example.com","subject":"Test"}' \
  --queue default \
  --priority 10 \
  --retries 3
```

**Get job details:**
```bash
./bin/quorractl get a1b2c3d4-e5f6-7890-abcd-ef1234567890
```

**View queue stats:**
```bash
./bin/quorractl queues
```

---

## Configuration

GoQuorra is configured via environment variables. See [.env.example](.env.example) for all options.

### Server Configuration

| Variable | Default | Description |
|----------|---------|-------------|
| `QUORRA_HTTP_ADDR` | `:8080` | HTTP server address |
| `QUORRA_GRPC_ADDR` | `:50051` | gRPC server address |
| `QUORRA_LOG_LEVEL` | `info` | Log level (debug/info/warn/error) |
| `DATABASE_URL` | (required) | PostgreSQL connection string |
| `REDIS_URL` | (optional) | Redis connection string |
| `QUORRA_API_KEY` | (required) | API key for REST authentication |

### Worker Configuration

| Variable | Default | Description |
|----------|---------|-------------|
| `QUORRA_WORKER_ID` | `worker-1` | Unique worker identifier |
| `QUORRA_WORKER_QUEUES` | `default` | Comma-separated queue names |
| `QUORRA_WORKER_MAX_JOBS` | `5` | Max jobs to lease per request |
| `QUORRA_WORKER_LEASE_TTL` | `30s` | Lease duration |
| `QUORRA_GRPC_ADDR` | `localhost:50051` | Server gRPC address |

---

## Development

### Prerequisites

- Go 1.21+
- PostgreSQL 15+
- Redis 7+ (optional)
- Docker & Docker Compose

### Setup

```bash
# Clone repository
git clone https://github.com/goquorra/goquorra.git
cd goquorra

# Install dependencies
go mod download

# Start Postgres and Redis via Docker
docker-compose -f deployments/docker-compose.yml up -d postgres redis

# Initialize database
make db-init

# Or manually:
export DATABASE_URL="postgres://quorra:quorra@localhost:5432/quorra?sslmode=disable"
psql $DATABASE_URL -f scripts/init_db.sql
```

### Run Locally

**Terminal 1: Start server**
```bash
export DATABASE_URL="postgres://quorra:quorra@localhost:5432/quorra?sslmode=disable"
export REDIS_URL="redis://localhost:6379/0"
export QUORRA_API_KEY="dev-api-key-change-in-production"

make run-server
# Or: go run ./cmd/quorra-server
```

**Terminal 2: Start worker**
```bash
export QUORRA_WORKER_ID="worker-local"
export QUORRA_WORKER_QUEUES="default,email"
export QUORRA_GRPC_ADDR="localhost:50051"

make run-worker
# Or: go run ./cmd/quorra-worker
```

**Terminal 3: Create test job**
```bash
curl -X POST http://localhost:8080/v1/jobs \
  -H "Content-Type: application/json" \
  -H "X-API-Key: dev-api-key-change-in-production" \
  -d '{
    "type": "test_job",
    "payload": {"message": "Hello GoQuorra!"},
    "queue": "default",
    "priority": 5,
    "max_retries": 3
  }'
```

### Project Structure

```
goquorra/
├── cmd/
│   ├── quorra-server/      # Main server binary
│   ├── quorra-worker/      # Worker binary
│   └── quorractl/          # CLI tool
├── internal/
│   ├── api/                # HTTP REST handlers
│   ├── grpc/               # gRPC service implementation
│   ├── store/              # Database abstraction (Postgres)
│   ├── queue/              # Queue manager and scheduler
│   ├── worker/             # Worker client library
│   ├── metrics/            # Prometheus metrics
│   └── config/             # Configuration management
├── proto/                  # Protobuf definitions
├── tests/                  # Unit and integration tests
├── scripts/                # Database migrations and scripts
├── deployments/            # Docker Compose, Kubernetes manifests
├── Dockerfile              # Multi-stage Docker build
├── Makefile                # Common tasks
└── README.md
```

---

## Testing

### Unit Tests

```bash
make test

# Or manually:
go test -v -race -coverprofile=coverage.out ./...
```

### Integration Tests

Integration tests require the full stack (Postgres, Redis, Server, Workers).

```bash
# Start services
make dev

# In another terminal, run integration tests
go test -v -tags=integration ./tests/...
```

### Test Coverage

```bash
go test -coverprofile=coverage.out ./...
go tool cover -html=coverage.out
```

---

## Deployment

### Docker

**Build image:**
```bash
make docker-build
# Or: docker build -t goquorra:latest .
```

**Run container:**
```bash
docker run -p 8080:8080 -p 50051:50051 \
  -e DATABASE_URL="postgres://..." \
  -e REDIS_URL="redis://..." \
  -e QUORRA_API_KEY="your-secret-key" \
  goquorra:latest
```

### Kubernetes

Basic Kubernetes manifests are provided in `deployments/k8s/`:

```bash
kubectl apply -f deployments/k8s/
```

(For production, use Helm charts or Kustomize with proper secrets management)

### Production Considerations

1. **Database**: Use managed PostgreSQL (RDS, Cloud SQL, etc.)
2. **Redis**: Use managed Redis (ElastiCache, Cloud Memorystore)
3. **Secrets**: Store `QUORRA_API_KEY` in Kubernetes Secrets or HashiCorp Vault
4. **Monitoring**: Integrate Prometheus and Grafana for metrics
5. **Logging**: Use structured logging and send to centralized system (ELK, Loki)
6. **Scaling**: Run multiple server and worker replicas
7. **TLS**: Enable TLS for gRPC and HTTPS for REST
8. **Rate Limiting**: Add rate limiting to REST API

---

## How It Works

### Job Lifecycle

1. **Creation**: Client sends `POST /v1/jobs` → Job stored with `status=pending`
2. **Scheduling**: Scheduler moves delayed jobs to ready state when `run_at <= now`
3. **Leasing**: Worker calls `LeaseJobs` → Server atomically leases jobs with `status=leased`
4. **Processing**: Worker processes job payload
5. **Acknowledgment**:
   - Success: Worker calls `AckJob` → `status=succeeded`
   - Failure: Worker calls `NackJob` → retry or `status=dead`
6. **Retry**: Failed jobs return to `pending` with exponential backoff
7. **DLQ**: Jobs exceeding `max_retries` move to `status=dead`

### Concurrency Safety

- **Atomic Leasing**: Uses PostgreSQL `SELECT FOR UPDATE SKIP LOCKED` to prevent double-leasing
- **Lease Verification**: `AckJob`/`NackJob` verify `lease_id` to prevent stale acks
- **Transaction Safety**: Job state transitions use database transactions

### Retry Logic

Failed jobs are retried with exponential backoff:

```
Backoff = 2^attempts seconds (capped at 1 hour)
```

Example:
- Attempt 1 fails → retry in 2s
- Attempt 2 fails → retry in 4s
- Attempt 3 fails → retry in 8s
- After `max_retries` → move to DLQ (`status=dead`)

---

## Worker Implementation

Workers connect via gRPC and process jobs. Example:

```go
import (
    "context"
    "log"
    "os"

    "github.com/goquorra/goquorra/internal/worker"
)

func main() {
    cfg := &worker.Config{
        ID:         "my-worker",
        ServerAddr: "localhost:50051",
        Queues:     []string{"default", "email"},
        MaxJobs:    5,
        LeaseTTL:   30 * time.Second,
    }

    logger := log.New(os.Stdout, "[worker] ", log.LstdFlags)
    w := worker.New(cfg, logger)

    ctx := context.Background()
    if err := w.Start(ctx); err != nil {
        log.Fatal(err)
    }
}
```

Workers can customize job processing by modifying `internal/worker/worker.go::executeJob`.

---

## Roadmap

- [ ] JWT authentication support
- [ ] OpenTelemetry traces
- [ ] Job priority inheritance
- [ ] Job dependencies (DAG execution)
- [ ] Job cancellation
- [ ] Scheduled/cron jobs
- [ ] Webhook notifications
- [ ] Job result persistence
- [ ] Multi-tenancy
- [ ] Admin UI (React-based)
- [ ] Helm chart for Kubernetes

---

## Contributing

Contributions are welcome! Please:

1. Fork the repository
2. Create a feature branch (`git checkout -b feature/amazing-feature`)
3. Commit your changes (`git commit -m 'Add amazing feature'`)
4. Push to the branch (`git push origin feature/amazing-feature`)
5. Open a Pull Request

Ensure:
- Tests pass (`make test`)
- Code is formatted (`go fmt ./...`)
- Linters pass (`make lint`)

---

## License

GoQuorra is licensed under the [MIT License](LICENSE).

---

## Support

- **Issues**: [GitHub Issues](https://github.com/goquorra/goquorra/issues)
- **Discussions**: [GitHub Discussions](https://github.com/goquorra/goquorra/discussions)
- **Documentation**: [Wiki](https://github.com/goquorra/goquorra/wiki)

---

## Acknowledgments

Built with:
- [Go](https://golang.org)
- [PostgreSQL](https://postgresql.org)
- [Redis](https://redis.io)
- [gRPC](https://grpc.io)
- [Prometheus](https://prometheus.io)
- [Chi Router](https://github.com/go-chi/chi)

---

**Made with ❤️ by the GoQuorra team**
