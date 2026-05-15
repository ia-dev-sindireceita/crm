# CRM — Fase 0 Makefile (SIN-62208).
# Targets: up, down, logs, test, test-integration, test-integration-cover,
#          lint, lint-aicache, lint-imports, notenant, forbidimport,
#          migrate-up, migrate-down, seed-stg, smoke-alert, verify-vendor.

SHELL := /bin/bash
COMPOSE_DIR := deploy/compose
COMPOSE := docker compose --project-directory $(COMPOSE_DIR) -f $(COMPOSE_DIR)/compose.yml
GO ?= go
NOTENANT_BIN := $(CURDIR)/bin/notenant
FORBIDIMPORT_BIN := $(CURDIR)/bin/forbidimport
FORBIDWEBBOUNDARY_BIN := $(CURDIR)/bin/forbidwebboundary

# golang-migrate CLI shipped as a one-shot container; see migrations/*.sql
# (SIN-62209). Versioned so CI and devs share the same binary.
MIGRATE_IMAGE := migrate/migrate:v4.17.1
MIGRATIONS_DIR := $(CURDIR)/migrations
COMPOSE_NETWORK := crm_crm

# Integration test coverage threshold (SIN-62277 acceptance criterion).
# The webhook stack + its adapters must clear this bar measured across
# unit + integration runs combined.
ITEST_COVER_PKGS := github.com/pericles-luz/crm/internal/webhook,github.com/pericles-luz/crm/internal/adapter/store/postgres,github.com/pericles-luz/crm/internal/adapter/channel/meta
ITEST_COVER_THRESHOLD ?= 85.0

.DEFAULT_GOAL := help

.PHONY: help up down logs test test-integration test-integration-cover \
        lint lint-aicache lint-customdomainnet lint-imports lint-postgres-adapter-tests \
        lint-webboundary notenant forbidimport forbidwebboundary \
        migrate-up migrate-down seed-stg smoke-alert verify-vendor

help: ## Show available targets
	@awk 'BEGIN {FS = ":.*## "} /^[a-zA-Z0-9_-]+:.*## / {printf "  \033[36m%-16s\033[0m %s\n", $$1, $$2}' $(MAKEFILE_LIST)

up: ## Bring up the local stack (Postgres, Redis, NATS, MinIO, Caddy, app)
	@if [ ! -f $(COMPOSE_DIR)/.env ]; then \
		echo "missing $(COMPOSE_DIR)/.env — copy $(COMPOSE_DIR)/.env.example and fill secrets"; \
		exit 1; \
	fi
	$(COMPOSE) up -d --wait

down: ## Stop the stack and remove orphaned containers (volumes preserved)
	$(COMPOSE) down --remove-orphans

logs: ## Tail logs from every service
	$(COMPOSE) logs -f --tail=200

test: ## Run Go test suite with coverage
	$(GO) test ./... -race -count=1 -cover

test-integration: ## Run webhook integration suite (Postgres real, build tag `integration`)
	@if [ -z "$$TEST_POSTGRES_DSN" ]; then \
		echo "test-integration: TEST_POSTGRES_DSN not set; falling back to testcontainers (requires Docker)."; \
	fi
	$(GO) test -tags integration -count=1 -timeout 300s \
		./internal/webhook/integration/...

test-integration-cover: ## Run unit + integration with combined coverage and enforce >=$(ITEST_COVER_THRESHOLD)%
	@if [ -z "$$TEST_POSTGRES_DSN" ]; then \
		echo "test-integration-cover: TEST_POSTGRES_DSN not set; falling back to testcontainers (requires Docker)."; \
	fi
	$(GO) test -count=1 -coverpkg=$(ITEST_COVER_PKGS) \
		-coverprofile=coverage.unit.out \
		./internal/webhook/... ./internal/adapter/...
	$(GO) test -tags integration -count=1 -timeout 300s \
		-coverpkg=$(ITEST_COVER_PKGS) \
		-coverprofile=coverage.itest.out \
		./internal/webhook/integration/...
	@{ \
		head -1 coverage.unit.out; \
		tail -n +2 coverage.unit.out; \
		tail -n +2 coverage.itest.out; \
	} > coverage.combined.out
	@pct=$$($(GO) tool cover -func=coverage.combined.out | awk '/^total:/ {sub("%","",$$3); print $$3}'); \
		echo "combined coverage (webhook+adapters): $${pct}%"; \
		awk -v p="$$pct" -v t="$(ITEST_COVER_THRESHOLD)" \
			'BEGIN { exit !(p+0 >= t+0) }' || { \
			echo "coverage $${pct}% below $(ITEST_COVER_THRESHOLD)% threshold (SIN-62277 quality bar)"; \
			exit 1; \
		}

lint: notenant lint-imports lint-postgres-adapter-tests lint-webboundary ## Run go vet + the notenant + forbidimport + forbidwebboundary analyzers + adapter-test guard (SIN-62232 / ADR 0071, SIN-62216, SIN-62750, SIN-62735)
	$(GO) vet ./...
	$(GO) vet -vettool=$(NOTENANT_BIN) ./internal/...

lint-postgres-adapter-tests: ## Reject *_test.go files under internal/adapter/db/postgres/<subpkg>/ (SIN-62750)
	./scripts/check-postgres-adapter-tests.sh

notenant: ## Build the notenant analyzer binary into bin/ (SIN-62232 / ADR 0071)
	$(GO) build -o $(NOTENANT_BIN) ./tools/lint/notenant/cmd/notenant

lint-customdomainnet: ## Run the SIN-62242 net/http guard analyzer over internal/customdomain/...
	$(GO) build -o ./bin/customdomainnet ./cmd/customdomainnet
	$(GO) vet -vettool=$(CURDIR)/bin/customdomainnet ./internal/customdomain/...

forbidimport: ## Build the forbidimport analyzer binary into bin/ (SIN-62216)
	$(GO) build -o $(FORBIDIMPORT_BIN) ./tools/lint/forbidimport/cmd/forbidimport

lint-imports: forbidimport ## Forbid database/sql + pgx + lib/pq outside the postgres adapter (SIN-62216)
	$(GO) vet -vettool=$(FORBIDIMPORT_BIN) ./internal/...

forbidwebboundary: ## Build the forbidwebboundary analyzer binary into bin/ (SIN-62735)
	$(GO) build -o $(FORBIDWEBBOUNDARY_BIN) ./tools/lint/forbidwebboundary/cmd/forbidwebboundary

lint-webboundary: forbidwebboundary ## Forbid imports of internal/inbox from internal/web/... (SIN-62735)
	$(GO) vet -vettool=$(FORBIDWEBBOUNDARY_BIN) ./internal/web/...

lint-aicache: ## Run the SIN-62236 aicache analyzer over internal/ai/ as a vet tool
	$(GO) build -o ./bin/aicache ./cmd/aicache
	$(GO) vet -vettool=$(CURDIR)/bin/aicache ./internal/ai/...

migrate-up: ## Apply all DB migrations against the compose Postgres (SIN-62209)
	@if [ ! -f $(COMPOSE_DIR)/.env ]; then \
		echo "missing $(COMPOSE_DIR)/.env — copy .env.example and fill secrets"; exit 1; \
	fi
	set -a; . $(COMPOSE_DIR)/.env; set +a; \
	docker run --rm --network $(COMPOSE_NETWORK) \
		-v $(MIGRATIONS_DIR):/migrations:ro \
		$(MIGRATE_IMAGE) -path /migrations \
		-database "postgres://$$POSTGRES_USER:$$POSTGRES_PASSWORD@postgres:5432/$$POSTGRES_DB?sslmode=disable" \
		up

migrate-down: ## Roll back ALL DB migrations (destructive; SIN-62209)
	@if [ ! -f $(COMPOSE_DIR)/.env ]; then \
		echo "missing $(COMPOSE_DIR)/.env"; exit 1; \
	fi
	set -a; . $(COMPOSE_DIR)/.env; set +a; \
	docker run --rm --network $(COMPOSE_NETWORK) \
		-v $(MIGRATIONS_DIR):/migrations:ro \
		$(MIGRATE_IMAGE) -path /migrations \
		-database "postgres://$$POSTGRES_USER:$$POSTGRES_PASSWORD@postgres:5432/$$POSTGRES_DB?sslmode=disable" \
		down -all

seed-stg: ## Apply staging seed fixtures (idempotent; SIN-62209)
	@if [ ! -f $(COMPOSE_DIR)/.env ]; then \
		echo "missing $(COMPOSE_DIR)/.env"; exit 1; \
	fi
	set -a; . $(COMPOSE_DIR)/.env; set +a; \
	$(COMPOSE) exec -T postgres \
		psql -v ON_ERROR_STOP=1 -U "$$POSTGRES_USER" -d "$$POSTGRES_DB" \
		< $(MIGRATIONS_DIR)/seed/stg.sql

smoke-alert: ## Inject a synthetic alert into Slack #alerts (wired in PR10)
	@echo "smoke-alert: stub — PR10 (SIN Fase 0) implements Slack injection"

verify-vendor: ## Verify SRI sha-384 hashes for web/static/vendor/** (SIN-62284)
	./scripts/verify-vendor-checksums.sh
