FROM golang:1.25 AS builder

WORKDIR /app

# Copy go.mod and go.sum files
COPY go.mod go.sum ./

# Download dependencies
RUN go mod download

# Copy source code
COPY . .

# Build the application
RUN CGO_ENABLED=0 GOOS=linux go build -o monitor cmd/monitor/main.go

# Use a minimal alpine image for the final container
FROM alpine:latest

WORKDIR /app

# Install dependencies required for DNS resolution
RUN apk --no-cache add ca-certificates tzdata

# Copy the binary from the builder stage
COPY --from=builder /app/monitor /app/monitor

# Create config directory — targets are mounted at runtime via docker-compose volume,
# not baked into the image. Each probe appliance has its own target files.
RUN mkdir -p /app/config

# Expose Prometheus metrics port
EXPOSE 2112

# Run the monitor
CMD ["/app/monitor"]
