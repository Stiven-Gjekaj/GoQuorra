# GoQuorra Usage Guide

This guide walks you through using GoQuorra from installation to running your first jobs.

## Table of Contents

1. [Installation](#installation)
2. [Starting the System](#starting-the-system)
3. [Creating Jobs](#creating-jobs)
4. [Monitoring Jobs](#monitoring-jobs)
5. [Using the CLI](#using-the-cli)
6. [Writing Custom Workers](#writing-custom-workers)
7. [Troubleshooting](#troubleshooting)

---

## Installation

### Option 1: Docker Compose (Recommended)

The easiest way to run GoQuorra locally:

```bash
git clone https://github.com/goquorra/goquorra.git
cd goquorra
make dev
```

This starts:
- PostgreSQL (port 5432)
- Redis (port 6379)
- GoQuorra Server (ports 8080, 50051)
- Two workers

### Option 2: From Source

```bash
# Prerequisites: Go 1.21+, PostgreSQL, Redis (optional)

# Clone and build
git clone https://github.com/goquorra/goquorra.git
cd goquorra
make build

# Set up database
export DATABASE_URL="postgres://quorra:quorra@localhost:5432/quorra?sslmode=disable"
make db-init

# Run server
./bin/quorra-server

# In another terminal, run worker
./bin/quorra-worker
```

---

## Starting the System

### With Docker Compose

```bash
# Start all services
docker-compose -f deployments/docker-compose.yml up -d

# View logs
docker-compose -f deployments/docker-compose.yml logs -f

# Stop services
docker-compose -f deployments/docker-compose.yml down
```

### Manual Start

**Terminal 1: Server**
```bash
export DATABASE_URL="postgres://quorra:quorra@localhost:5432/quorra?sslmode=disable"
export REDIS_URL="redis://localhost:6379/0"
export QUORRA_API_KEY="your-secret-key"
./bin/quorra-server
```

**Terminal 2: Worker**
```bash
export QUORRA_WORKER_ID="worker-1"
export QUORRA_WORKER_QUEUES="default,email"
export QUORRA_GRPC_ADDR="localhost:50051"
./bin/quorra-worker
```

---

## Creating Jobs

### Using curl

**Basic job:**
```bash
curl -X POST http://localhost:8080/v1/jobs \
  -H "Content-Type: application/json" \
  -H "X-API-Key: dev-api-key-change-in-production" \
  -d '{
    "type": "email_send",
    "payload": {
      "to": "user@example.com",
      "subject": "Welcome!",
      "body": "Thanks for signing up"
    },
    "queue": "default",
    "priority": 10,
    "max_retries": 3
  }'
```

**Delayed job (runs in 60 seconds):**
```bash
curl -X POST http://localhost:8080/v1/jobs \
  -H "Content-Type: application/json" \
  -H "X-API-Key: dev-api-key-change-in-production" \
  -d '{
    "type": "reminder",
    "payload": {"message": "Time to review!"},
    "delay_seconds": 60,
    "max_retries": 5
  }'
```

**High-priority job:**
```bash
curl -X POST http://localhost:8080/v1/jobs \
  -H "Content-Type: application/json" \
  -H "X-API-Key: dev-api-key-change-in-production" \
  -d '{
    "type": "urgent_notification",
    "payload": {"alert": "System critical"},
    "priority": 100,
    "queue": "urgent"
  }'
```

### Response

```json
{
  "id": "550e8400-e29b-41d4-a716-446655440000",
  "status": "pending",
  "run_at": "2025-10-11T12:00:00Z"
}
```

Save the `id` to check job status later.

---

## Monitoring Jobs

### Get Job Status

```bash
curl http://localhost:8080/v1/jobs/550e8400-e29b-41d4-a716-446655440000 \
  -H "X-API-Key: dev-api-key-change-in-production"
```

**Response:**
```json
{
  "id": "550e8400-e29b-41d4-a716-446655440000",
  "type": "email_send",
  "payload": {"to": "user@example.com", "subject": "Welcome!"},
  "queue": "default",
  "priority": 10,
  "status": "succeeded",
  "attempts": 1,
  "max_retries": 3,
  "created_at": "2025-10-11T12:00:00Z",
  "updated_at": "2025-10-11T12:00:05Z"
}
```

**Job Statuses:**
- `pending` - Waiting to be processed
- `leased` - Assigned to a worker
- `processing` - Currently being processed
- `succeeded` - Completed successfully
- `failed` - Failed but will retry
- `dead` - Exceeded max retries, moved to DLQ

### View Queue Statistics

```bash
curl http://localhost:8080/v1/queues \
  -H "X-API-Key: dev-api-key-change-in-production"
```

**Response:**
```json
{
  "queues": [
    {"queue": "default", "status": "pending", "count": 5},
    {"queue": "default", "status": "succeeded", "count": 123},
    {"queue": "email", "status": "pending", "count": 2}
  ]
}
```

### Web Dashboard

Open http://localhost:8080 in your browser.

The dashboard shows:
- Queue counts by status
- Recent jobs
- Auto-refreshes every 5 seconds

### Prometheus Metrics

```bash
curl http://localhost:8080/metrics
```

Example metrics:
```
quorra_jobs_created_total 150
quorra_jobs_processed_total 142
quorra_jobs_failed_total 5
quorra_jobs_dead_total 2
```

---

## Using the CLI

The `quorractl` tool provides a convenient command-line interface.

### Build CLI

```bash
make build
# or
go build -o bin/quorractl ./cmd/quorractl
```

### Create a Job

```bash
./bin/quorractl create email_send \
  --server http://localhost:8080 \
  --api-key dev-api-key-change-in-production \
  --payload '{"to":"test@example.com","subject":"Test"}' \
  --queue default \
  --priority 10 \
  --delay 0 \
  --retries 3
```

**Output:**
```
Job created successfully!
ID:     550e8400-e29b-41d4-a716-446655440000
Status: pending
Run at: 2025-10-11T12:00:00Z
```

### Get Job Details

```bash
./bin/quorractl get 550e8400-e29b-41d4-a716-446655440000 \
  --server http://localhost:8080 \
  --api-key dev-api-key-change-in-production
```

**Output:**
```json
{
  "id": "550e8400-e29b-41d4-a716-446655440000",
  "type": "email_send",
  "status": "succeeded",
  "attempts": 1,
  ...
}
```

### View Queue Stats

```bash
./bin/quorractl queues \
  --server http://localhost:8080 \
  --api-key dev-api-key-change-in-production
```

**Output:**
```
Queue Statistics:
─────────────────────────────────────────

default:
  pending     : 5
  succeeded   : 123
  failed      : 2

email:
  pending     : 2
  succeeded   : 45
```

---

## Writing Custom Workers

Workers process jobs from queues. You can customize job processing logic.

### Example: Custom Worker

Create a new file `my-worker/main.go`:

```go
package main

import (
    "context"
    "encoding/json"
    "fmt"
    "log"
    "os"

    "github.com/goquorra/goquorra/internal/worker"
)

func main() {
    cfg := &worker.Config{
        ID:         "my-custom-worker",
        ServerAddr: "localhost:50051",
        Queues:     []string{"email", "notifications"},
        MaxJobs:    10,
        LeaseTTL:   60 * time.Second,
    }

    logger := log.New(os.Stdout, "[my-worker] ", log.LstdFlags)
    w := worker.New(cfg, logger)

    // Customize job processing (see internal/worker/worker.go)
    // You can fork and modify executeJob() to implement your logic

    ctx := context.Background()
    if err := w.Start(ctx); err != nil {
        log.Fatal(err)
    }
}
```

### Custom Job Processing

Edit `internal/worker/worker.go` and modify the `executeJob` function:

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

    // Implement email sending logic
    // Return true on success, false on failure
    return true
}
```

### Build and Run

```bash
go build -o my-worker my-worker/main.go
./my-worker
```

---

## Troubleshooting

### Jobs Not Processing

**Check worker is running:**
```bash
docker-compose -f deployments/docker-compose.yml ps
```

**View worker logs:**
```bash
docker-compose -f deployments/docker-compose.yml logs -f quorra-worker-1
```

**Check job status:**
```bash
curl http://localhost:8080/v1/jobs/{JOB_ID} \
  -H "X-API-Key: dev-api-key-change-in-production"
```

### Database Connection Issues

**Check database is running:**
```bash
docker-compose -f deployments/docker-compose.yml ps postgres
```

**Test connection:**
```bash
psql postgres://quorra:quorra@localhost:5432/quorra -c "SELECT 1"
```

**Check schema:**
```bash
psql postgres://quorra:quorra@localhost:5432/quorra -c "\dt"
```

### Jobs Stuck in Leased State

Jobs may get stuck if a worker crashes during processing. The lease TTL expires, but jobs remain leased.

**Manual fix:**
```sql
UPDATE jobs SET status = 'pending', lease_id = NULL, leased_at = NULL
WHERE status = 'leased' AND leased_at < NOW() - INTERVAL '5 minutes';
```

(In production, implement a lease expiration cleanup job)

### High Job Failure Rate

**Check worker logs for errors:**
```bash
docker-compose -f deployments/docker-compose.yml logs quorra-worker-1 | grep ERROR
```

**Query failed jobs:**
```sql
SELECT id, type, last_error, attempts FROM jobs WHERE status = 'failed' LIMIT 10;
```

### Authentication Errors

**Error: "Invalid or missing API key"**

Make sure you're sending the API key:
```bash
-H "X-API-Key: dev-api-key-change-in-production"
```

Or set it in the environment:
```bash
export QUORRA_API_KEY="dev-api-key-change-in-production"
```

### Performance Issues

**Check queue lengths:**
```bash
curl http://localhost:8080/v1/queues \
  -H "X-API-Key: dev-api-key-change-in-production"
```

**Scale workers:**
```bash
# Add more workers in docker-compose.yml
# Or run additional worker instances manually
```

**Monitor metrics:**
```bash
curl http://localhost:8080/metrics | grep quorra_job_queue_length
```

---

## Next Steps

- Read the [Architecture Documentation](README.md#architecture)
- Explore [API Documentation](README.md#api-documentation)
- Check out [Examples](examples/) (if available)
- Join the [Community Discussions](https://github.com/goquorra/goquorra/discussions)

---

**Happy Job Processing with GoQuorra!** 🚀
