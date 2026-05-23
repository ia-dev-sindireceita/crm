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
# web/static is needed at TWO layers:
#  - web/static/vendor is imported as a Go package by
#    internal/adapter/transport/http/customdomain/templates.go (SIN-62535).
#    It embeds CHECKSUMS.txt only; the JS payloads themselves stay out
#    of the binary.
#  - web/static/css/*.css, web/static/js/*.js, web/static/customdomain.css
#    and web/static/customdomain.js are served at runtime by the
#    http.FileServer mounted in cmd/server/customdomain_wire.go
#    (SIN-63299 — staging /login rendered without styles because the
#    asset tree was missing from the crm-server runtime image).
# The matching `!web/static` exception in .dockerignore keeps the full
# tree visible in the build context; copying once here avoids duplicate
# COPY layers and is the source for the runtime-stage COPY below.
COPY web/static ./web/static

# COMMIT_SHA is injected at link time into internal/version.commitSHA so
# /health surfaces the build identifier (SIN-63146). cd-stg.yml passes
# github.event.workflow_run.head_sha here; an unset arg keeps the in-binary
# default ("unknown") so unqualified `docker build .` still produces a
# runnable image.
ARG COMMIT_SHA=unknown

# CGO_ENABLED=0 + -trimpath + -ldflags="-s -w" yields a small, reproducible,
# statically linked binary. GOFLAGS prevents the toolchain from auto-downloading
# a different Go version at build time (we want the pinned 1.26.3 alpine image,
# matching go.mod's `toolchain go1.26.3` directive — bumped here as a follow-up
# to SIN-62297 c4b2c73 toolchain pin so the in-container compile matches CI).
# -X github.com/.../internal/version.commitSHA=${COMMIT_SHA} injects the SHA
# into the server binary only — workers omit the -X so changing the build
# arg does not bust their cached layers for unrelated rebuilds.
ENV CGO_ENABLED=0 GOFLAGS=-mod=readonly GOTOOLCHAIN=local
RUN go build -trimpath \
        -ldflags="-s -w -X github.com/pericles-luz/crm/internal/version.commitSHA=${COMMIT_SHA}" \
        -o /out/server ./cmd/server && \
    go build -trimpath -ldflags="-s -w" -o /out/mediascan-worker      ./cmd/mediascan-worker && \
    go build -trimpath -ldflags="-s -w" -o /out/wallet-alerter-worker ./cmd/wallet-alerter-worker

# --- mediascan-worker runtime ---------------------------------------------
# distroless/static-debian12:nonroot — no shell, no apt, UID/GID 65532, only
# CA certs and the binary. The worker only needs outbound TCP to NATS,
# Postgres, clamd, MinIO and Slack — distroless covers all of that via the
# CA bundle baked into the base. See gcr.io/distroless/static for the contract.
FROM gcr.io/distroless/static-debian12:nonroot@sha256:d093aa3e30dbadd3efe1310db061a14da60299baff8450a17fe0ccc514a16639 AS crm-mediascan-worker

COPY --from=builder /out/mediascan-worker /app/mediascan-worker

USER 65532:65532
WORKDIR /app
ENTRYPOINT ["/app/mediascan-worker"]

# --- wallet-alerter-worker runtime ----------------------------------------
# Same distroless contract as mediascan-worker. Outbound to NATS + Slack only.
FROM gcr.io/distroless/static-debian12:nonroot@sha256:d093aa3e30dbadd3efe1310db061a14da60299baff8450a17fe0ccc514a16639 AS crm-wallet-alerter-worker

COPY --from=builder /out/wallet-alerter-worker /app/wallet-alerter-worker

USER 65532:65532
WORKDIR /app
ENTRYPOINT ["/app/wallet-alerter-worker"]

# --- server runtime (DEFAULT) ---------------------------------------------
# Kept as the LAST stage so an unqualified `docker build .` still produces
# the server image that cd-stg.yml has been pushing since SIN-62215.
FROM gcr.io/distroless/static-debian12:nonroot@sha256:d093aa3e30dbadd3efe1310db061a14da60299baff8450a17fe0ccc514a16639 AS crm-server

COPY --from=builder /out/server /app/crm

# SIN-63299 — runtime static assets. The FileServer in
# cmd/server/customdomain_wire.go resolves http.Dir("web/static") relative
# to WORKDIR (/app), so the bytes need to land at /app/web/static. Without
# this COPY the link tags for /static/css/auth.css, /static/css/privacy.css
# and /static/customdomain.css 404 with text/plain and the rendered pages
# fall back to user-agent defaults. distroless/static has no shell so the
# only way to ship the bytes is a COPY layer; the docker-smoke crm-server
# leg curls these routes at PR time to keep the regression fenced.
COPY --from=builder /src/web/static /app/web/static

# SIN-63332 — ship the schema migrations alongside the binary so the
# cd-stg `migrate-up` step (deploy/scripts/stg-deploy.sh `migrate-up`
# verb) can extract them from the just-deployed image with `docker cp`
# and apply them via a one-shot migrate/migrate sidecar before the
# login smoke runs. Distroless has no shell, so the migrations are
# read-only data here, not an entrypoint — the server binary never
# touches /migrations at runtime.
COPY migrations /migrations

USER 65532:65532
WORKDIR /app
EXPOSE 8080
ENV HTTP_ADDR=":8080"

ENTRYPOINT ["/app/crm"]
