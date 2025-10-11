.PHONY: help build test dev clean docker-build proto lint

help: ## Show this help message
	@echo 'Usage: make [target]'
	@echo ''
	@echo 'Available targets:'
	@awk 'BEGIN {FS = ":.*?## "} /^[a-zA-Z_-]+:.*?## / {printf "  %-15s %s\n", $$1, $$2}' $(MAKEFILE_LIST)

build: ## Build all binaries
	@echo "Building GoQuorra binaries..."
	go build -o bin/quorra-server ./cmd/quorra-server
	go build -o bin/quorra-worker ./cmd/quorra-worker
	go build -o bin/quorractl ./cmd/quorractl
	@echo "Build complete! Binaries in bin/"

test: ## Run tests
	@echo "Running tests..."
	go test -v -race -coverprofile=coverage.out ./...
	@echo "Test coverage:"
	go tool cover -func=coverage.out | tail -1

test-integration: ## Run integration tests with docker-compose
	@echo "Running integration tests..."
	docker-compose -f deployments/docker-compose.yml up -d postgres redis
	sleep 5
	DATABASE_URL="postgres://quorra:quorra@localhost:5432/quorra?sslmode=disable" \
	REDIS_URL="redis://localhost:6379/0" \
	go test -v -tags=integration ./tests/...
	docker-compose -f deployments/docker-compose.yml down

dev: ## Start development environment with docker-compose
	@echo "Starting GoQuorra development environment..."
	docker-compose -f deployments/docker-compose.yml up --build

dev-down: ## Stop development environment
	docker-compose -f deployments/docker-compose.yml down

dev-logs: ## Show logs from development environment
	docker-compose -f deployments/docker-compose.yml logs -f

docker-build: ## Build Docker image
	@echo "Building Docker image..."
	docker build -t goquorra:latest .

proto: ## Generate protobuf code
	@echo "Generating protobuf code..."
	bash scripts/generate-proto.sh

lint: ## Run linters
	@echo "Running linters..."
	@if command -v golangci-lint > /dev/null; then \
		golangci-lint run --timeout 5m; \
	else \
		echo "golangci-lint not installed. Install from: https://golangci-lint.run/usage/install/"; \
		go vet ./...; \
		go fmt ./...; \
	fi

clean: ## Clean build artifacts
	@echo "Cleaning..."
	rm -rf bin/
	rm -f coverage.out
	docker-compose -f deployments/docker-compose.yml down -v

deps: ## Download dependencies
	@echo "Downloading dependencies..."
	go mod download
	go mod tidy

run-server: build ## Run server locally
	@echo "Starting server..."
	./bin/quorra-server

run-worker: build ## Run worker locally
	@echo "Starting worker..."
	./bin/quorra-worker

db-init: ## Initialize database schema
	@echo "Initializing database..."
	psql $(DATABASE_URL) -f scripts/init_db.sql

.DEFAULT_GOAL := help
