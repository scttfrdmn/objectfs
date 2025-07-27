# Multi-stage build for ObjectFS
# Build stage
FROM golang:1.21-alpine AS builder

# Install build dependencies
RUN apk add --no-cache \
    git \
    make \
    gcc \
    musl-dev \
    fuse-dev

# Set working directory
WORKDIR /src

# Copy go modules files
COPY go.mod go.sum ./

# Download dependencies
RUN go mod download && go mod verify

# Copy source code
COPY . .

# Build the binary
ARG VERSION=dev
ARG COMMIT=unknown
ARG BUILD_TIME=unknown

RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build \
    -ldflags="-s -w -X main.Version=${VERSION} -X main.Commit=${COMMIT} -X main.BuildTime=${BUILD_TIME}" \
    -tags="release,netgo" \
    -o /bin/objectfs \
    ./cmd/objectfs

# Runtime stage
FROM alpine:3.22

# Install runtime dependencies
RUN apk add --no-cache \
    fuse \
    ca-certificates \
    tzdata

# Create non-root user
RUN addgroup -g 1000 objectfs && \
    adduser -D -u 1000 -G objectfs objectfs

# Create mount directories
RUN mkdir -p /mnt/objectfs && \
    chown objectfs:objectfs /mnt/objectfs

# Copy binary from builder stage
COPY --from=builder /bin/objectfs /usr/local/bin/objectfs

# Create configuration directory
RUN mkdir -p /etc/objectfs && \
    chown objectfs:objectfs /etc/objectfs

# Copy default configuration
COPY examples/config.yaml /etc/objectfs/config.yaml.example

# Set up FUSE
RUN echo "user_allow_other" >> /etc/fuse.conf

# Switch to non-root user for security
USER objectfs

# Set up health check
HEALTHCHECK --interval=30s --timeout=10s --start-period=5s --retries=3 \
    CMD wget --quiet --tries=1 --spider http://localhost:8081/health || exit 1

# Expose ports
EXPOSE 8080 8081

# Set environment variables
ENV OBJECTFS_LOG_LEVEL=INFO
ENV OBJECTFS_METRICS_PORT=8080
ENV OBJECTFS_HEALTH_PORT=8081

# Default command
ENTRYPOINT ["/usr/local/bin/objectfs"]
CMD ["--help"]

# Labels for metadata
LABEL maintainer="ObjectFS Team <maintainers@objectfs.io>"
LABEL org.opencontainers.image.title="ObjectFS"
LABEL org.opencontainers.image.description="Enterprise-Grade High-Performance POSIX Filesystem for Object Storage"
LABEL org.opencontainers.image.vendor="ObjectFS"
LABEL org.opencontainers.image.licenses="Apache-2.0"
LABEL org.opencontainers.image.source="https://github.com/objectfs/objectfs"
LABEL org.opencontainers.image.documentation="https://objectfs.io/docs"