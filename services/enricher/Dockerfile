# syntax=docker/dockerfile:1

############################
#  enricher – multi-stage Dockerfile
############################

# ---------- Build Stage ----------
FROM golang:1.24-alpine AS builder

# Set working directory inside the container
WORKDIR /workspace
RUN echo ">>> CONTEXT CONTENTS <<<" && ls -R .

# 1. Copy Go module manifests and download dependencies (caching layers)
COPY services/enricher/go.mod services/enricher/go.sum ./
RUN go mod download

# 2. Copy only the enricher service sources
COPY services/enricher ./services/enricher

# 3. Build the static enricher binary
WORKDIR /workspace/services/enricher
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -v -o /enricher ./cmd

# ---------- Runtime Stage ----------
FROM alpine:3.19

# Create a non-root user to run the service
RUN adduser -D -u 10001 enricher

WORKDIR /app

# Copy the compiled binary from the builder stage
COPY --from=builder /enricher /app/enricher

# Declare mount points for data and config
VOLUME ["/data", "/config"]

# Switch to unprivileged user
USER enricher

# Launch the enricher
ENTRYPOINT ["/app/enricher"]

