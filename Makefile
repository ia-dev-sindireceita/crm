# CRM — Fase 0 Makefile (SIN-62208).
# Targets: up, down, logs, test, lint, notenant, migrate-up, migrate-down, seed-stg, smoke-alert, verify-vendor.

SHELL := /bin/bash
COMPOSE_DIR := deploy/compose
COMPOSE := docker compose --project-directory $(COMPOSE_DIR) -f $(COMPOSE_DIR)/compose.yml
GO ?= go
NOTENANT_BIN := $(CURDIR)/bin/notenant

# golang-migrate CLI shipped as a one-shot container; see migrations/*.sql
# (SIN-62209). Versioned so CI and devs share the same binary.
MIGRATE_IMAGE := migrate/migrate:v4.17.1
MIGRATIONS_DIR := $(CURDIR)/migrations
COMPOSE_NETWORK := crm_crm

.DEFAULT_GOAL := help

.PHONY: help up down logs test lint lint-aicache migrate-up migrate-down seed-stg smoke-alert verify-vendor

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

lint: notenant ## Run go vet + the notenant analyzer over internal/ (SIN-62232 / ADR 0071)
	$(GO) vet ./...
	$(GO) vet -vettool=$(NOTENANT_BIN) ./internal/...

notenant: ## Build the notenant analyzer binary into bin/ (SIN-62232 / ADR 0071)
	$(GO) build -o $(NOTENANT_BIN) ./tools/lint/notenant/cmd/notenant

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

smoke-alert: ## Trip rls_misses_total in stg and verify Slack #alerts (SIN-62218)
	@APP_URL="$${APP_URL:-http://localhost:8080}" \
	ALERTMANAGER_URL="$${ALERTMANAGER_URL:-http://localhost:9093}" \
	SMOKE_ALERT_WEBHOOK_PROBE_URL="$${SMOKE_ALERT_WEBHOOK_PROBE_URL:-http://localhost:9094}" \
	TIMEOUT="$${TIMEOUT:-60}" \
	$(CURDIR)/scripts/smoke-alert.sh

verify-vendor: ## Verify SRI sha-384 hashes for web/static/vendor/** (SIN-62284)
	./scripts/verify-vendor-checksums.sh
