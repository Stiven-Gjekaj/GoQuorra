# Development tasks.
#
# `make verify` runs what CI runs. Everything else is a step of it, or a way
# to start something locally.

SHELL := /bin/bash
BINARIES := quorra-server quorra-worker quorractl
COMPOSE := docker compose -f deployments/docker-compose.yml

.DEFAULT_GOAL := help

.PHONY: help
help: ## Show this help
	@echo 'make <target>'
	@echo
	@awk 'BEGIN {FS = ":.*?## "} /^[a-zA-Z_-]+:.*?## / {printf "  %-16s %s\n", $$1, $$2}' $(MAKEFILE_LIST)

.PHONY: verify
verify: fmt-check vet build test proto-check links ## Run everything CI runs

# VERSION is written into the binaries, and every one of them says it.
#
# From the git description when there is one, so a build from a tag names the
# tag and a build after it names the tag and the distance. A tree with no tags
# and a tree with no git both fall back to dev, which is what the binaries said
# before this existed.
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

.PHONY: build
build: ## Build the three binaries into bin/
	@for name in $(BINARIES); do \
		echo "building $$name $(VERSION)"; \
		go build -ldflags="-X main.version=$(VERSION)" -o bin/$$name ./cmd/$$name || exit 1; \
	done

.PHONY: test
test: ## Run the tests with the race detector
	go test -race -count=1 ./...

.PHONY: test-postgres
test-postgres: ## Run the tests against a real database
	@test -n "$$QUORRA_TEST_DATABASE_URL" || { \
		echo "Set QUORRA_TEST_DATABASE_URL first."; \
		echo "For the compose stack: postgres://quorra:quorra@localhost:5432/quorra?sslmode=disable"; \
		exit 1; }
	QUORRA_TEST_REQUIRE_POSTGRES=1 go test -race -count=1 ./...

.PHONY: cover
cover: ## Write a coverage report to coverage.html
	go test -count=1 -coverprofile=coverage.out ./...
	go tool cover -html=coverage.out -o coverage.html
	@go tool cover -func=coverage.out | tail -1

.PHONY: fmt
fmt: ## Format the code
	gofmt -s -w .

# A check and not a rewrite. A target that reformats cannot fail, so CI would
# report success and leave the difference in the working tree of whoever ran
# it next.
.PHONY: fmt-check
fmt-check: ## Check the formatting
	@unformatted=$$(gofmt -s -l . | grep -v '^internal/quorrapb/' || true); \
	if [ -n "$$unformatted" ]; then \
		echo "These files are not formatted. Run make fmt:"; \
		echo "$$unformatted"; \
		exit 1; \
	fi

.PHONY: vet
vet: ## Run go vet
	go vet ./...

.PHONY: proto
proto: ## Generate the code for the worker protocol
	bash scripts/generate-proto.sh

# The check that would have caught the hand written files this repository
# used to carry.
.PHONY: proto-check
proto-check: ## Check that the generated code matches the protocol
	@bash scripts/generate-proto.sh > /dev/null
	@if ! git diff --quiet -- internal/quorrapb; then \
		echo "The generated code does not match proto/. Run make proto and commit the result:"; \
		git --no-pager diff --stat -- internal/quorrapb; \
		exit 1; \
	fi
	@echo "The generated code matches the protocol."

.PHONY: links
links: ## Check every relative link in the documentation
	bash scripts/check-links.sh

.PHONY: db-init
db-init: ## Apply every migration to the database in DATABASE_URL
	@test -n "$$DATABASE_URL" || { echo "Set DATABASE_URL first."; exit 1; }
	@for file in migrations/*.sql; do \
		echo "applying $$file"; \
		psql "$$DATABASE_URL" -v ON_ERROR_STOP=1 -f "$$file" || exit 1; \
	done

.PHONY: dev
dev: ## Start the whole stack in containers
	$(COMPOSE) up --build

.PHONY: dev-down
dev-down: ## Stop the stack and remove its data
	$(COMPOSE) down -v

.PHONY: dev-logs
dev-logs: ## Follow the logs of the stack
	$(COMPOSE) logs -f

.PHONY: docker-build
docker-build: ## Build the container image
	docker build -t goquorra:latest .

.PHONY: clean
clean: ## Remove the build output
	rm -rf bin/ coverage.out coverage.html
