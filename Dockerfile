# CRM container images — Fase 0 PR9 (SIN-62215) + SIN-62935 worker targets.
#
# Multi-stage build with one shared `builder` stage and three named final
# runtime stages — one per shipped binary. Each runtime stage copies a
# single statically linked binary into a distroless-static base that has
# no shell and no package manager.
#
# Final stages (build with `--target <name>`):
#
#   crm-server                  → cmd/server (the HTTP API + HTMX UI)
#   crm-mediascan-worker        → cmd/mediascan-worker (NATS consumer)
#   crm-wallet-alerter-worker   → cmd/wallet-alerter-worker (NATS consumer)
#
# `crm-server` is also the DEFAULT final stage (last `FROM` in this file)
# so an unqualified `docker build .` continues to produce the server image
# that .github/workflows/cd-stg.yml has been pushing since SIN-62215.
# That workflow now passes `target: crm-server` explicitly so future stage
# reordering does not silently break staging.
#
# Pin policy (CEO follow-up SIN-62208 / AC #6 of SIN-62215): builder and
# every runtime base are pinned by SHA256 digest. Bumping the digests is
# documented in docs/deploy/staging.md ("Bumping infra image digests").
# The floating tag is kept alongside the digest for human readability;
# Docker resolves strictly by digest.

# syntax=docker/dockerfile:1.7

# --- builder stage ---------------------------------------------------------
FROM golang:1.26.3-alpine@sha256:91eda9776261207ea25fd06b5b7fed8d397dd2c0a283e77f2ab6e91bfa71079d AS builder

WORKDIR /src

# Module cache layer — only changes when go.mod/go.sum change.
COPY go.mod go.sum ./
RUN go mod download

# Source layer.
COPY cmd ./cmd
COPY internal ./internal
COPY adapters ./adapters
# web/static/vendor is imported as a Go package by
# internal/adapter/transport/http/customdomain/templates.go (SIN-62535).
# It embeds CHECKSUMS.txt only; the JS payloads themselves stay out of the
# binary. The matching `!web/static/vendor` exception in .dockerignore keeps
# this path visible in the build context.
COPY web/static/vendor ./web/static/vendor

# CGO_ENABLED=0 + -trimpath + -ldflags="-s -w" yields a small, reproducible,
# statically linked binary. GOFLAGS prevents the toolchain from auto-downloading
# a different Go version at build time (we want the pinned 1.26.3 alpine image,
# matching go.mod's `toolchain go1.26.3` directive — bumped here as a follow-up
# to SIN-62297 c4b2c73 toolchain pin so the in-container compile matches CI).
ENV CGO_ENABLED=0 GOFLAGS=-mod=readonly GOTOOLCHAIN=local
RUN go build -trimpath -ldflags="-s -w" -o /out/server                ./cmd/server         && \
    go build -trimpath -ldflags="-s -w" -o /out/mediascan-worker      ./cmd/mediascan-worker && \
    go build -trimpath -ldflags="-s -w" -o /out/wallet-alerter-worker ./cmd/wallet-alerter-worker

# --- mediascan-worker runtime ---------------------------------------------
# distroless/static-debian12:nonroot — no shell, no apt, UID/GID 65532, only
# CA certs and the binary. The worker only needs outbound TCP to NATS,
# Postgres, clamd, MinIO and Slack — distroless covers all of that via the
# CA bundle baked into the base. See gcr.io/distroless/static for the contract.
FROM gcr.io/distroless/static-debian12:nonroot@sha256:a9329520abc449e3b14d5bc3a6ffae065bdde0f02667fa10880c49b35c109fd1 AS crm-mediascan-worker

COPY --from=builder /out/mediascan-worker /app/mediascan-worker

USER 65532:65532
WORKDIR /app
ENTRYPOINT ["/app/mediascan-worker"]

# --- wallet-alerter-worker runtime ----------------------------------------
# Same distroless contract as mediascan-worker. Outbound to NATS + Slack only.
FROM gcr.io/distroless/static-debian12:nonroot@sha256:a9329520abc449e3b14d5bc3a6ffae065bdde0f02667fa10880c49b35c109fd1 AS crm-wallet-alerter-worker

COPY --from=builder /out/wallet-alerter-worker /app/wallet-alerter-worker

USER 65532:65532
WORKDIR /app
ENTRYPOINT ["/app/wallet-alerter-worker"]

# --- server runtime (DEFAULT) ---------------------------------------------
# Kept as the LAST stage so an unqualified `docker build .` still produces
# the server image that cd-stg.yml has been pushing since SIN-62215.
FROM gcr.io/distroless/static-debian12:nonroot@sha256:a9329520abc449e3b14d5bc3a6ffae065bdde0f02667fa10880c49b35c109fd1 AS crm-server

COPY --from=builder /out/server /app/crm

USER 65532:65532
WORKDIR /app
EXPOSE 8080
ENV HTTP_ADDR=":8080"

ENTRYPOINT ["/app/crm"]
