# Multi-stage build for ObjectFS
# Build stage
FROM golang:1.27-alpine AS builder

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

# Build the binary.
#
# TARGETARCH, not a hardcoded amd64. It is supplied by the builder from the platform being built, and
# the release workflow builds linux/amd64,linux/arm64 from this one file — so with GOARCH pinned, the
# arm64 manifest was published containing an **x86-64 binary**. Verified by ELF header: machine type
# 0x3e in an image whose manifest reports arm64. It runs on a developer machine, because both Docker
# Desktop and podman's VM register qemu binfmt handlers and silently emulate it; on a Graviton
# instance or an arm64 Kubernetes node it is `exec format error` at container start. That is the worst
# shape for this defect — invisible everywhere it is tested, fatal only where it is deployed.
#
# GOOS stays pinned: the runtime stage is Alpine either way.
ARG TARGETARCH
ARG VERSION=dev

RUN CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH:-amd64} \
    go build \
    -ldflags="-s -w" \
    -tags="release,netgo" \
    -o /bin/objectfs \
    ./cmd/objectfs

# Fail the build if the image would report a version other than the one it was asked for.
#
# The -ldflags above no longer inject anything, and that is the fix rather than an omission. They said
# `-X main.Version=${VERSION} -X main.Commit=${COMMIT} -X main.BuildTime=${BUILD_TIME}`, and none of
# those three symbols exists: cmd/objectfs/main.go declares `version` in a **const** block, which the
# linker cannot rewrite. -X against a missing symbol is silently ignored, so every image ever built
# from this file reported whatever the constant happened to say, and appeared to work precisely because
# someone had remembered to edit it. Proven by building with VERSION=NOT-THE-REAL-VERSION, which
# produced an image reporting 0.10.1.
#
# So the constant is the single authority — as CLAUDE.md and release.yml both already treat it — and
# this asserts the two agree instead of pretending to overwrite one with the other. VERSION defaults
# to `dev`, which no release tag matches, so the check applies only when a caller passes a real one.
RUN if [ "$VERSION" != "dev" ]; then \
        reported="$(/bin/objectfs --version)"; \
        case "$reported" in \
            *"${VERSION#v}") ;; \
            *) echo "ERROR: --build-arg VERSION=$VERSION but the binary reports: $reported" >&2; \
               echo "The version is a const in cmd/objectfs/main.go and cannot be injected at link" >&2; \
               echo "time. Update that constant to match the tag." >&2; \
               exit 1 ;; \
        esac; \
    fi

# Runtime stage
FROM alpine:3.24

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
COPY configs/example.yaml /etc/objectfs/config.yaml.example

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
LABEL org.opencontainers.image.source="https://github.com/scttfrdmn/objectfs"
LABEL org.opencontainers.image.documentation="https://objectfs.io/docs"
