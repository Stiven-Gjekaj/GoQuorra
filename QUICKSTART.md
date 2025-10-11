# GoQuorra Quick Start

Get GoQuorra running in 5 minutes and verify it works end-to-end.

## Prerequisites

- Docker & Docker Compose installed
- Terminal/Command Prompt
- curl or similar HTTP client

## Step 1: Start the System

```bash
# Navigate to the GoQuorra directory
cd GoQuorra

# Start all services (Postgres, Redis, Server, Workers)
docker-compose -f deployments/docker-compose.yml up --build
```

Wait for the services to start. You should see logs indicating:
- `Connected to PostgreSQL`
- `Connected to Redis`
- `Starting HTTP server on :8080`
- `Starting gRPC server on :50051`
- Workers connecting and polling for jobs

## Step 2: Verify Services

**Check services are running:**
```bash
docker-compose -f deployments/docker-compose.yml ps
```

You should see:
- `postgres` (healthy)
- `redis` (healthy)
- `quorra-server` (running)
- `quorra-worker-1` (running)
- `quorra-worker-2` (running)

**Check server is responding:**
```bash
curl http://localhost:8080/healthz
```

Should return `200 OK`.

## Step 3: Create a Test Job

```bash
curl -X POST http://localhost:8080/v1/jobs \
  -H "Content-Type: application/json" \
  -H "X-API-Key: dev-api-key-change-in-production" \
  -d '{
    "type": "test_job",
    "payload": {
      "message": "Hello GoQuorra!",
      "timestamp": "2025-10-11T12:00:00Z"
    },
    "queue": "default",
    "priority": 10,
    "max_retries": 3
  }'
```

**Expected Output:**
```json
{
  "id": "a1b2c3d4-e5f6-7890-abcd-ef1234567890",
  "status": "pending",
  "run_at": "2025-10-11T12:00:00Z"
}
```

**Save the job ID** from the response!

## Step 4: Check Job Status

Wait a few seconds for the worker to process the job, then:

```bash
# Replace JOB_ID with your actual job ID
curl http://localhost:8080/v1/jobs/JOB_ID \
  -H "X-API-Key: dev-api-key-change-in-production"
```

**Expected Output:**
```json
{
  "id": "a1b2c3d4-e5f6-7890-abcd-ef1234567890",
  "type": "test_job",
  "payload": {
    "message": "Hello GoQuorra!",
    "timestamp": "2025-10-11T12:00:00Z"
  },
  "queue": "default",
  "priority": 10,
  "status": "succeeded",
  "attempts": 1,
  "max_retries": 3,
  "created_at": "2025-10-11T12:00:00Z",
  "updated_at": "2025-10-11T12:00:05Z"
}
```

Status should be `succeeded` (or `leased` if still processing).

## Step 5: View Queue Statistics

```bash
curl http://localhost:8080/v1/queues \
  -H "X-API-Key: dev-api-key-change-in-production"
```

**Expected Output:**
```json
{
  "queues": [
    {"queue": "default", "status": "pending", "count": 0},
    {"queue": "default", "status": "succeeded", "count": 1}
  ]
}
```

## Step 6: Check the Dashboard

Open your browser to:
```
http://localhost:8080
```

You should see:
- Queue statistics showing 1 succeeded job
- Your test job in the "Recent Jobs" table

## Step 7: View Metrics

```bash
curl http://localhost:8080/metrics
```

Look for these metrics:
```
quorra_jobs_created_total 1
quorra_jobs_processed_total 1
quorra_jobs_leased_total 1
```

## Step 8: Test Multiple Jobs

Create multiple jobs to see concurrent processing:

```bash
for i in {1..10}; do
  curl -X POST http://localhost:8080/v1/jobs \
    -H "Content-Type: application/json" \
    -H "X-API-Key: dev-api-key-change-in-production" \
    -d "{
      \"type\": \"batch_job\",
      \"payload\": {\"index\": $i},
      \"queue\": \"default\",
      \"priority\": $i,
      \"max_retries\": 3
    }"
done
```

Check the dashboard or logs to see workers processing jobs.

## Step 9: Test Delayed Jobs

Create a job that runs in 10 seconds:

```bash
curl -X POST http://localhost:8080/v1/jobs \
  -H "Content-Type: application/json" \
  -H "X-API-Key: dev-api-key-change-in-production" \
  -d '{
    "type": "delayed_job",
    "payload": {"message": "This will run in 10 seconds"},
    "queue": "default",
    "delay_seconds": 10,
    "max_retries": 3
  }'
```

Save the job ID and check its status immediately:
```bash
curl http://localhost:8080/v1/jobs/JOB_ID \
  -H "X-API-Key: dev-api-key-change-in-production"
```

Status should be `pending`. Wait 10+ seconds and check again - it should be `succeeded`.

## Step 10: View Worker Logs

```bash
docker-compose -f deployments/docker-compose.yml logs -f quorra-worker-1
```

You should see logs like:
```
[worker] Worker worker-1 connected to quorra-server:50051
[worker] Leased job a1b2c3d4... (type=test_job) from queue default
[worker] Processing job a1b2c3d4... (type=test_job, attempt=1/3)
[worker] Job type=test_job, payload=map[...], took=523ms
[worker] Job a1b2c3d4... completed successfully
```

## Troubleshooting

### Job stays in "pending" status

- Check workers are running: `docker-compose ps`
- Check worker logs: `docker-compose logs quorra-worker-1`
- Workers may be processing other queues - check queue configuration

### "Connection refused" errors

- Ensure services are running: `docker-compose ps`
- Check server logs: `docker-compose logs quorra-server`
- Verify ports are not in use: `netstat -an | grep 8080`

### Database errors

- Check Postgres is healthy: `docker-compose ps postgres`
- Check server logs for DB connection errors
- Restart services: `docker-compose down && docker-compose up`

## What's Next?

Now that GoQuorra is working:

1. **Read the full documentation**: [README.md](README.md)
2. **Explore the API**: [API Documentation](README.md#api-documentation)
3. **Write custom workers**: [USAGE.md#writing-custom-workers](USAGE.md#writing-custom-workers)
4. **Try the CLI tool**: `docker-compose exec quorra-server /bin/quorractl --help`
5. **Monitor with Prometheus**: Set up Grafana dashboards

## Stopping the System

```bash
# Stop services (preserves data)
docker-compose -f deployments/docker-compose.yml down

# Stop and remove all data
docker-compose -f deployments/docker-compose.yml down -v
```

---

**Congratulations!** You've successfully run GoQuorra and processed your first jobs. 🎉
