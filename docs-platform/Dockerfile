# Multi-stage build for ObjectFS Documentation Platform

# Stage 1: Build documentation
FROM node:18-alpine AS docs-builder

WORKDIR /app

# Copy package files
COPY package*.json ./

# Install dependencies
RUN npm ci --only=production

# Copy documentation source
COPY .vitepress/ .vitepress/
COPY guide/ guide/
COPY api/ api/
COPY tutorials/ tutorials/
COPY playground/ playground/
COPY sdks/ sdks/
COPY index.md ./
COPY public/ public/

# Build documentation
RUN npm run build:docs

# Stage 2: Production image
FROM node:18-alpine AS production

# Install security updates
RUN apk update && apk upgrade && apk add --no-cache \
    dumb-init \
    tini \
    curl

# Create app user
RUN addgroup -g 1001 -S objectfs && \
    adduser -S objectfs -u 1001

# Set working directory
WORKDIR /app

# Copy package files and install production dependencies
COPY package*.json ./
RUN npm ci --only=production && npm cache clean --force

# Copy application code
COPY src/ src/
COPY --from=docs-builder /app/.vitepress/dist ./public

# Create necessary directories
RUN mkdir -p /app/logs /app/cache && \
    chown -R objectfs:objectfs /app

# Switch to non-root user
USER objectfs

# Expose port
EXPOSE 3001

# Health check
HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 \
    CMD curl -f http://localhost:3001/api/health || exit 1

# Use tini as PID 1
ENTRYPOINT ["/sbin/tini", "--"]

# Start application
CMD ["node", "src/api-server.js"]

# Build args
ARG BUILD_DATE
ARG VERSION
ARG VCS_REF

# Labels
LABEL org.label-schema.build-date=$BUILD_DATE \
      org.label-schema.name="objectfs-docs" \
      org.label-schema.description="ObjectFS Documentation Platform" \
      org.label-schema.version=$VERSION \
      org.label-schema.vcs-ref=$VCS_REF \
      org.label-schema.vcs-url="https://github.com/objectfs/objectfs" \
      org.label-schema.schema-version="1.0" \
      maintainer="ObjectFS Team <team@objectfs.io>"
