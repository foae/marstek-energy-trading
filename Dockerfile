# Build stage
FROM golang:1.25-bookworm AS builder

WORKDIR /app

# Copy go mod files
COPY go.mod go.sum ./
RUN go mod download

# Copy source code
COPY . .

# Build the binary
RUN CGO_ENABLED=0 GOOS=linux go build -o energy-trader ./cmd/trader

# Runtime stage
FROM debian:bookworm-slim

# Install CA certificates for HTTPS
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates && rm -rf /var/lib/apt/lists/*

# Create non-root user
RUN useradd -r -u 1000 -s /bin/false trader

WORKDIR /app

# Copy binary from builder
COPY --from=builder /app/energy-trader .

# Create data directory
RUN mkdir -p /app/data && chown -R trader:trader /app

# Switch to non-root user
USER trader

# Expose HTTP port
EXPOSE 8080

# Set default data directory
ENV DATA_DIR=/app/data

# Run the service
CMD ["./energy-trader"]
