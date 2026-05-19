# syntax=docker/dockerfile:1.7
#
# Hardened multi-stage build for the payroll engine.
#
# Build stage produces a fully static binary (CGO disabled, netgo+osusergo
# build tags), stripped of debug symbols and build IDs for reproducibility.
# The runtime stage is Google's distroless/static — no shell, no package
# manager, no busybox utilities, just glibc and CA certs. Attack surface is
# the binary alone; an attacker who finds RCE has no shell to pivot from.
#
# Image is multi-arch friendly (amd64 + arm64) when buildx is used. OCI
# labels are populated from build-time --build-args so the registry shows
# provenance metadata required by SOC 2 and a typical container scanner.

# ---------- build stage ---------------------------------------------------
FROM --platform=$BUILDPLATFORM golang:1.25-alpine AS builder

WORKDIR /src

# Dependencies first — separate layer so go.mod/go.sum changes don't bust
# the source-code layer below and vice versa.
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download -x

COPY . .

ARG TARGETOS
ARG TARGETARCH

# Static, stripped, reproducible build:
#   CGO_ENABLED=0   pure-Go, no glibc dependency at runtime
#   -trimpath       strips local file paths from binary (reproducible across machines)
#   -ldflags -s -w  drops the symbol table and DWARF debug info (~30% smaller)
#   -ldflags -buildid=  zeroes the Go build ID for byte-identical reproducible builds
#   netgo,osusergo  pure-Go DNS + user/group lookups; no dynamic resolver
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build \
        -trimpath \
        -tags netgo,osusergo \
        -ldflags '-s -w -buildid=' \
        -o /out/payroll-app \
        ./cmd/api

# ---------- runtime stage -------------------------------------------------
# distroless/static-debian12 carries CA certs and tzdata only; nonroot variant
# runs as UID/GID 65532 by default — no USER directive required, no setuid
# binary in the image. The image is debug-impossible (no shell), which is the
# point: exploit kits have nothing to land on.
FROM gcr.io/distroless/static-debian12:nonroot

# Migrations are bundled into the image because the binary applies them at
# startup. In production we still recommend running migrate as a separate
# init job, but bundling keeps single-binary deployments self-sufficient.
COPY --from=builder /src/internal/db/migrations /internal/db/migrations
COPY --from=builder /out/payroll-app /payroll-app

# OCI labels — populated by CI via --build-arg so the registry surfaces
# provenance for compliance and incident response. Defaults are placeholders
# so a developer-driven `docker build .` still produces a labelled image.
ARG VCS_REF=dev
ARG BUILD_DATE=unknown
ARG VERSION=dev
LABEL org.opencontainers.image.title="go-payroll-engine"
LABEL org.opencontainers.image.description="Production payroll disbursement engine wrapping Monnify."
LABEL org.opencontainers.image.source="https://github.com/ObeeJ/gopayrollengine"
LABEL org.opencontainers.image.licenses="MIT"
LABEL org.opencontainers.image.vendor="ObeeJ"
LABEL org.opencontainers.image.revision="${VCS_REF}"
LABEL org.opencontainers.image.created="${BUILD_DATE}"
LABEL org.opencontainers.image.version="${VERSION}"

EXPOSE 8080

# HEALTHCHECK is intentionally omitted: distroless has no shell, so an in-
# image healthcheck would require shipping curl/wget, which expands attack
# surface. Use the orchestrator's HTTP probe against /healthz instead —
# docker-compose, ECS, and Kubernetes all support this natively and the
# probe is visible to the platform's observability layer.

ENTRYPOINT ["/payroll-app"]
