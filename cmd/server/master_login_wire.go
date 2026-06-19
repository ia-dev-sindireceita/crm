package main

// SIN-65254 — master-operator login composition.
//
// /m/login was wired (SIN-65223, iam_wire.go) to delegate its credential
// check to iamSvc.Login — the per-request, tenant-scoped iam.Service.Login.
// That path resolves host→tenant and then looks up
// LookupCredentials(tenantID, email). The only seeded master operator
// (master@crm.local, migrations/seed/stg.sql) has tenant_id=NULL /
// is_master=true, so the tenant-scoped lookup NEVER found it and every
// master login returned 401 (verified live in staging @4e2437b). The
// existing handler + wire tests stub MasterLoginFunc to always succeed, so
// the gap shipped green.
//
// buildMasterLogin replaces that with a master-aware login fn that resolves
// the GLOBAL operator by (email, is_master=true, tenant_id IS NULL) over the
// app_master_ops pool — the SAME role the rest of the master /m/* stack
// (sessions, directory, seed repo) must run under via WithMasterOps. The
// previous wire handed buildMasterMFAStack the tenant-scoped IAM runtime
// pool (app_runtime), which cannot satisfy WithMasterOps' audit trigger;
// buildMasterLogin therefore also returns the master-ops pool so the caller
// builds the whole stack on it.

import (
	"context"
	"log"
	"log/slog"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	postgresadapter "github.com/pericles-luz/crm/internal/adapter/db/postgres"
	"github.com/pericles-luz/crm/internal/adapter/httpapi/mastermfa"
	"github.com/pericles-luz/crm/internal/iam"
	"github.com/pericles-luz/crm/internal/iam/ratelimit"
)

// buildMasterLogin assembles the tenant-less master-operator login fn and
// the app_master_ops pool the master /m/* stack runs under.
//
// limiter / policies drive the m_login lockout (Threshold 5, 30m,
// AlertOnLock) — the ADR 0074 §6 master-login brute-force policy. alerter is
// the synchronous Slack notifier (no-ops when SLACK_WEBHOOK_URL is unset).
//
// Returns (nil, nil, noop) on any missing input — MASTER_OPS_DATABASE_URL or
// MASTER_OPS_ACTOR_ID unset/invalid, a pool connect failure, or an adapter
// constructor error. A nil login fn makes buildMasterMFAStack fall back to
// the noop stack, so the router leaves /m/* unmounted exactly as in a
// health-only boot. The returned pool is non-nil only alongside a non-nil
// login fn; the caller threads its Close into the server cleanup chain.
func buildMasterLogin(
	ctx context.Context,
	limiter ratelimit.RateLimiter,
	policies map[string]ratelimit.Policy,
	alerter ratelimit.Alerter,
	logger *slog.Logger,
	getenv func(string) string,
) (mastermfa.MasterLoginFunc, *pgxpool.Pool, func()) {
	noop := func() {}

	masterDSN := strings.TrimSpace(getenv(envMasterOpsDSN))
	if masterDSN == "" {
		log.Printf("crm: master login disabled (%s unset)", envMasterOpsDSN)
		return nil, nil, noop
	}
	actorRaw := strings.TrimSpace(getenv(envMasterOpsActorID))
	if actorRaw == "" {
		log.Printf("crm: master login disabled (%s unset)", envMasterOpsActorID)
		return nil, nil, noop
	}
	actorID, err := uuid.Parse(actorRaw)
	if err != nil || actorID == uuid.Nil {
		log.Printf("crm: master login disabled — invalid %s: %v", envMasterOpsActorID, err)
		return nil, nil, noop
	}

	pool, err := pgxpool.New(ctx, masterDSN)
	if err != nil {
		log.Printf("crm: master login disabled — master pg connect: %v", err)
		return nil, nil, noop
	}

	reader, err := postgresadapter.NewMasterCredentialReader(pool, actorID)
	if err != nil {
		pool.Close()
		log.Printf("crm: master login disabled — credential reader: %v", err)
		return nil, nil, noop
	}
	lockouts, err := postgresadapter.NewMasterLockouts(pool, actorID)
	if err != nil {
		pool.Close()
		log.Printf("crm: master login disabled — master lockouts: %v", err)
		return nil, nil, noop
	}

	svc := &iam.Service{
		MasterUsers: reader,
		Lockouts:    lockouts,
		Limiter:     limiter,
		// m_login is the ADR 0074 §6 master-login policy (AlertOnLock=true).
		// A missing key yields the zero Policy → LockoutEnabled() false →
		// no durable lockout writes, but credential resolution still works.
		LoginPolicy: policies["m_login"],
		Alerter:     alerter,
		Logger:      logger,
	}

	log.Print("crm: master login assembled (app_master_ops pool)")
	return svc.MasterLogin, pool, pool.Close
}
