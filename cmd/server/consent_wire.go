package main

// SIN-63191 / Fase 6 PR4 — cookie consent banner wire.
//
// buildConsentHandler returns the unauthenticated GET / POST /consent/*
// handler. Production wires the RecordingRegistry on top of the
// SIN-63185 pgconsent.Store so each authenticated decision lands in
// audit_log_data; anonymous /privacy visitors get a cookie-only
// experience (the registry needs a Subject.ID).
//
// Returns nil when DATABASE_URL is unset so cmd/server boots without
// the banner (consistent with the rest of the wire layer). The banner
// route then 404s and authenticated layouts that try to hx-get the
// banner simply do not render it.

import (
	"context"
	"log"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	postgresadapter "github.com/pericles-luz/crm/internal/adapter/db/postgres"
	pgconsent "github.com/pericles-luz/crm/internal/adapter/db/postgres/consent"
	"github.com/pericles-luz/crm/internal/iam"
	"github.com/pericles-luz/crm/internal/iam/consent"
	webconsent "github.com/pericles-luz/crm/internal/web/consent"
)

// envConsentCookieInsecure flips the Secure attribute off so local
// HTTP dev can observe the cookie in the browser. Production leaves
// it unset.
const envConsentCookieInsecure = "CONSENT_COOKIE_INSECURE"

// auditPool is the SIN-66332 dedicated app_audit pool (nil in dev); when nil
// the consent audit writer falls back to this wire's own runtime pool.
func buildConsentHandler(ctx context.Context, getenv func(string) string, auditPool *pgxpool.Pool) (http.Handler, func()) {
	noop := func() {}
	dsn := getenv(postgresadapter.EnvDSN)
	if dsn == "" {
		log.Printf("crm: web/consent disabled (DATABASE_URL unset)")
		return nil, noop
	}
	pool, err := postgresadapter.NewFromEnv(ctx, getenv)
	if err != nil {
		log.Printf("crm: web/consent disabled — pg connect: %v", err)
		return nil, noop
	}
	store, err := pgconsent.NewStore(pool)
	if err != nil {
		pool.Close()
		log.Printf("crm: web/consent disabled — store: %v", err)
		return nil, noop
	}
	splitLogger, err := postgresadapter.NewSplitAuditLogger(auditExecutorOr(auditPool, pool))
	if err != nil {
		pool.Close()
		log.Printf("crm: web/consent disabled — audit logger: %v", err)
		return nil, noop
	}
	registry, err := consent.NewRecordingRegistry(store, splitLogger, consent.RecordingConfig{
		Now:              func() time.Time { return time.Now().UTC() },
		ActorFromContext: consentActorFromContext,
	})
	if err != nil {
		pool.Close()
		log.Printf("crm: web/consent disabled — recording registry: %v", err)
		return nil, noop
	}
	h, err := webconsent.New(webconsent.Deps{
		Registry:     registry,
		Now:          func() time.Time { return time.Now().UTC() },
		CookieSecure: !consentCookieInsecure(getenv),
		Logger:       slog.Default(),
	})
	if err != nil {
		pool.Close()
		log.Printf("crm: web/consent disabled — handler: %v", err)
		return nil, noop
	}
	mux := http.NewServeMux()
	h.Routes(mux)
	log.Printf("crm: web/consent /consent/{cookies-banner,cookies} mounted")
	return mux, func() { pool.Close() }
}

// consentActorFromContext resolves the iam.Principal sitting on the
// request context (set by middleware.Auth) and returns its UserID.
// Anonymous visitors return (uuid.Nil, false) and the decorator skips
// the audit emission.
func consentActorFromContext(ctx context.Context) (uuid.UUID, bool) {
	p, ok := iam.PrincipalFromContext(ctx)
	if !ok || p.UserID == uuid.Nil {
		return uuid.Nil, false
	}
	return p.UserID, true
}

// consentCookieInsecure returns true when the operator wants the
// banner cookie emitted without the Secure attribute (local HTTP dev).
// Any truthy value triggers the downgrade; production leaves it unset.
func consentCookieInsecure(getenv func(string) string) bool {
	switch strings.ToLower(strings.TrimSpace(getenv(envConsentCookieInsecure))) {
	case "1", "true", "yes":
		return true
	default:
		return false
	}
}
