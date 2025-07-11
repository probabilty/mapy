# syntax=docker/dockerfile:1

############################
#  indexer – multi-stage Dockerfile
############################

# ---------- Build Stage ----------
FROM golang:1.24-alpine AS builder

# Set working directory inside the container
WORKDIR /workspace
RUN echo ">>> CONTEXT CONTENTS <<<" && ls -R .

# 1. Copy Go module manifests and download dependencies (caching layers)
COPY services/indexer/go.mod services/indexer/go.sum ./
RUN go mod download

# 2. Copy only the indexer service sources
COPY services/indexer ./services/indexer

# 3. Build the static indexer binary
WORKDIR /workspace/services/indexer
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -v -o /indexer ./cmd

# ---------- Runtime Stage ----------
FROM alpine:3.19

# Create a non-root user to run the service
RUN adduser -D -u 10001 indexer

WORKDIR /app

# Copy the compiled binary from the builder stage
COPY --from=builder /indexer /app/indexer

# Declare mount points for data and config
VOLUME ["/data", "/config"]

# Switch to unprivileged user
USER indexer

# Launch the indexer
ENTRYPOINT ["/app/indexer"]

