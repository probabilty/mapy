# syntax=docker/dockerfile:1

############################
#  Downloader – multi-stage Dockerfile
############################

# ---------- Build Stage ----------
FROM golang:1.24-alpine AS builder

# Set working directory inside the container
WORKDIR /workspace
RUN echo ">>> CONTEXT CONTENTS <<<" && ls -R .

# 1. Copy Go module manifests and download dependencies (caching layers)
COPY services/downloader/go.mod services/downloader/go.sum ./
RUN go mod download

# 2. Copy only the downloader service sources
COPY services/downloader ./services/downloader

# 3. Build the static downloader binary
WORKDIR /workspace/services/downloader
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -v -o /downloader ./cmd

# ---------- Runtime Stage ----------
FROM alpine:3.19

# Create a non-root user to run the service
RUN adduser -D -u 10001 downloader

WORKDIR /app

# Copy the compiled binary from the builder stage
COPY --from=builder /downloader /app/downloader

# Declare mount points for data and config
VOLUME ["/data", "/config"]

# Switch to unprivileged user
USER downloader

# Launch the downloader
ENTRYPOINT ["/app/downloader"]

