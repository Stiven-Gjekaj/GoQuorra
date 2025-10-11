# Build stage
FROM golang:1.21-alpine AS builder

WORKDIR /app

# Install build dependencies
RUN apk add --no-cache git

# Copy go mod files
COPY go.mod go.sum ./
RUN go mod download

# Copy source code
COPY . .

# Build server
RUN CGO_ENABLED=0 GOOS=linux go build -o /bin/quorra-server ./cmd/quorra-server

# Build worker
RUN CGO_ENABLED=0 GOOS=linux go build -o /bin/quorra-worker ./cmd/quorra-worker

# Build CLI
RUN CGO_ENABLED=0 GOOS=linux go build -o /bin/quorractl ./cmd/quorractl

# Runtime stage
FROM alpine:latest

RUN apk --no-cache add ca-certificates

WORKDIR /app

# Copy binaries from builder
COPY --from=builder /bin/quorra-server /bin/quorra-server
COPY --from=builder /bin/quorra-worker /bin/quorra-worker
COPY --from=builder /bin/quorractl /bin/quorractl

# Expose ports
EXPOSE 8080 50051

# Default command
CMD ["/bin/quorra-server"]
