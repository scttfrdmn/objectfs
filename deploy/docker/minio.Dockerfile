# MinIO and mc for the development cluster in docker-compose.yaml, built from pinned source.
#
# Built, not pulled, because there is nothing to pull. MinIO's community edition is distributed as
# source only: `minio/minio` and `minio/mc` are both gone from Docker Hub (404), and Quay's copies need
# authentication. This compose file pinned `minio/minio:RELEASE.2025-09-07T16-13-09Z`, which therefore
# worked only on a machine that had pulled it before — and CI's s3compat cell found the same thing when
# a digest copied from such a machine was refused (#600, #599).
#
# The MinIO version is the one ci.yml's tagged-suite (s3compat) cell builds and verifies, so the store
# this development cluster coordinates over is the store CI checks conditional writes against. Change
# them together. Base images are pinned by index digest, read from Docker Hub's API, not from a cache.

FROM docker.io/library/golang:1.26.10-bookworm@sha256:fe3da92a5cb2881e4aa75e625cb0b7c50571632fb4cf340cbe53e6ccdcffdd51 AS build

ARG MINIO_VERSION=v0.0.0-20260212201848-7aac2a2c5b7c
ARG MC_VERSION=v0.0.0-20251106162529-77f82e18b540

ENV CGO_ENABLED=0 GOBIN=/out
RUN go install "github.com/minio/minio@${MINIO_VERSION}" \
 && go install "github.com/minio/mc@${MC_VERSION}"

FROM docker.io/library/debian:bookworm-slim@sha256:7c7b2c966bc9ee8cedfeef67e0e279108992c77681fa595db4a9d65c06ccc587

COPY --from=build /out/minio /out/mc /usr/local/bin/

EXPOSE 9000 9001
ENTRYPOINT ["/usr/local/bin/minio"]
CMD ["server", "/data", "--console-address", ":9001"]
