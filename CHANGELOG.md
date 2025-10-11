# Changelog

All notable changes to GoQuorra will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.0.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [0.1.0] - 2025-10-11

### Added
- Initial MVP release of GoQuorra distributed job queue system
- REST API for job management
  - `POST /v1/jobs` - Create jobs
  - `GET /v1/jobs/{id}` - Get job status
  - `GET /v1/queues` - List queue statistics
  - `GET /v1/recent` - Get recent jobs
- gRPC worker protocol
  - `LeaseJobs` - Stream jobs to workers
  - `AckJob` - Acknowledge successful completion
  - `NackJob` - Signal failure for retry
- PostgreSQL persistence layer
  - Job states: pending, leased, processing, succeeded, failed, dead
  - Atomic job leasing with `SELECT FOR UPDATE SKIP LOCKED`
  - Queue statistics view
- Redis Streams support (optional)
  - Graceful fallback to Postgres-only mode
- Job scheduling
  - Delayed jobs with `delay_seconds` parameter
  - Priority-based ordering
  - Background scheduler for delayed job processing
- Retry and error handling
  - Configurable `max_retries` per job
  - Exponential backoff (2^attempts seconds, capped at 1 hour)
  - Dead-letter queue (DLQ) for jobs exceeding retries
- Prometheus metrics
  - `quorra_jobs_created_total`
  - `quorra_jobs_processed_total`
  - `quorra_jobs_failed_total`
  - `quorra_jobs_dead_total`
  - `quorra_jobs_leased_total`
  - `quorra_job_queue_length{queue,status}`
- Web dashboard
  - Real-time queue statistics
  - Recent jobs table
  - Auto-refresh every 5 seconds
- CLI tool (`quorractl`)
  - `create` - Create jobs
  - `get` - Get job details
  - `queues` - View queue statistics
  - `stats` - Alias for queues
- API key authentication for REST endpoints
- Docker support
  - Multi-stage Dockerfile
  - Docker Compose with Postgres, Redis, Server, and Workers
- Makefile with common development tasks
  - `make dev` - Start development environment
  - `make test` - Run tests
  - `make build` - Build binaries
  - `make docker-build` - Build Docker image
- Comprehensive test suite
  - Unit tests for store layer
  - Integration tests for end-to-end workflows
  - Concurrent worker tests
- GitHub Actions CI/CD pipeline
  - Test, lint, build, and integration test jobs
  - PostgreSQL and Redis services
  - Code coverage reporting
- Documentation
  - Comprehensive README with examples
  - API documentation with curl examples
  - Architecture diagram
  - Configuration reference
  - Deployment guide

### Technical Details
- Go 1.21+ required
- Uses chi router for HTTP
- gRPC with Protocol Buffers
- Structured logging
- Graceful shutdown handling
- Environment-based configuration

### Known Limitations
- API key auth only (JWT support planned)
- No OpenTelemetry traces yet
- No job cancellation
- No job dependencies
- Single-region only

[0.1.0]: https://github.com/goquorra/goquorra/releases/tag/v0.1.0
