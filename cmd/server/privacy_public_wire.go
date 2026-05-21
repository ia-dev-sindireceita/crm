package main

// SIN-63191 / Fase 6 PR4 — public LGPD-disclosure page wire.
//
// buildPublicPrivacyHandler returns the unauthenticated GET /privacy
// handler. The settings reader is the same postgres TenantResolver
// that backs the tenanted middleware; this wire just adapts it onto
// the SettingsReader port.
//
// Returns nil when DATABASE_URL is unset so cmd/server boots without
// the public privacy page (consistent with the rest of the wire
// layer).

import (
	"context"
	"log"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"

	postgresadapter "github.com/pericles-luz/crm/internal/adapter/db/postgres"
	"github.com/pericles-luz/crm/internal/tenancy"
	webprivacy "github.com/pericles-luz/crm/internal/web/public/privacy"
)

func buildPublicPrivacyHandler(ctx context.Context, getenv func(string) string) (http.Handler, func()) {
	noop := func() {}
	dsn := getenv(postgresadapter.EnvDSN)
	if dsn == "" {
		log.Printf("crm: web/public/privacy disabled (DATABASE_URL unset)")
		return nil, noop
	}
	pool, err := postgresadapter.NewFromEnv(ctx, getenv)
	if err != nil {
		log.Printf("crm: web/public/privacy disabled — pg connect: %v", err)
		return nil, noop
	}
	resolver, err := postgresadapter.NewTenantResolver(pool)
	if err != nil {
		pool.Close()
		log.Printf("crm: web/public/privacy disabled — tenant resolver: %v", err)
		return nil, noop
	}
	h, err := webprivacy.New(webprivacy.Deps{
		Settings: privacyReader{inner: resolver},
		Now:      func() time.Time { return time.Now().UTC() },
		Logger:   slog.Default(),
	})
	if err != nil {
		pool.Close()
		log.Printf("crm: web/public/privacy disabled — handler: %v", err)
		return nil, noop
	}
	log.Printf("crm: web/public/privacy GET /privacy mounted")
	return h, func() { pool.Close() }
}

// privacyReader adapts the postgres TenantResolver onto the
// SettingsReader port. Kept in the wire layer so the public/privacy
// package does not need to import the postgres adapter.
type privacyReader struct {
	inner *postgresadapter.TenantResolver
}

func (p privacyReader) LoadPrivacySettings(ctx context.Context, tenantID uuid.UUID) (tenancy.PrivacySettings, error) {
	return p.inner.LoadPrivacySettings(ctx, tenantID)
}
