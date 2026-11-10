# Build stages
FROM golang:1.26.4-alpine AS builder

WORKDIR /app

# Copy go mod files
COPY go.mod go.sum ./
RUN go mod download

# Copy source code
COPY . .

# Build the application
RUN CGO_ENABLED=0 GOOS=linux go build -a -installsuffix cgo -o main .

# Final stage
FROM alpine:latest

# ffmpeg (includes ffprobe) is required by the media pipeline
RUN apk --no-cache add ca-certificates ffmpeg

WORKDIR /root/

# Copy the binary from builder
COPY --from=builder /app/main .

# Soft heap ceiling: the GC works harder before the container's hard
# memory limit (set on docker run) is reached. Override per host if needed.
ENV GOMEMLIMIT=700MiB

EXPOSE 3000

# Marks the container unhealthy when the API or its database stops answering
HEALTHCHECK --interval=30s --timeout=5s --start-period=60s --retries=3 \
  CMD wget -qO- "http://127.0.0.1:${PORT:-3000}/api/v1/health" >/dev/null || exit 1

# Run the binary
CMD ["./main"]