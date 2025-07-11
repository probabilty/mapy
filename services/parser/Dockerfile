# syntax=docker/dockerfile:1

############################
#  parser – multi-stage Dockerfile
############################

# ---------- Build Stage ----------
FROM golang:1.24-alpine AS builder

# Set working directory inside the container
WORKDIR /workspace
RUN echo ">>> CONTEXT CONTENTS <<<" && ls -R .

# 1. Copy Go module manifests and download dependencies (caching layers)
COPY services/parser/go.mod services/parser/go.sum ./
RUN go mod download

# 2. Copy only the parser service sources
COPY services/parser ./services/parser

# 3. Build the static parser binary
WORKDIR /workspace/services/parser
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -v -o /parser ./cmd

# ---------- Runtime Stage ----------
FROM alpine:3.19

# Create a non-root user to run the service
RUN adduser -D -u 10001 parser

WORKDIR /app

# Copy the compiled binary from the builder stage
COPY --from=builder /parser /app/parser

# Declare mount points for data and config
VOLUME ["/data", "/config"]

# Switch to unprivileged user
USER parser

# Launch the parser
ENTRYPOINT ["/app/parser"]

