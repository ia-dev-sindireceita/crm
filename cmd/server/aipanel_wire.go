package main

// SIN-65364 — mount the LGPD consent accept/cancel endpoints.
//
// The inbox AI-assist gate (SIN-62928 / SIN-65363) renders the consent
// modal via aipanel.RenderConsentModal whenever a tenant runs with
// ai_policy.consent_required=true. That modal POSTs to
// POST /aipanel/consent/accept and /cancel — but internal/web/aipanel
// was never mounted in cmd/server, so confirming the modal hit the
// custom-domain catch-all and 404'd, leaving the operator stuck. This
// wire closes that gap.
//
// Fail-soft, same envelope as the other web/* surfaces: a nil handler
// leaves the /aipanel/* routes unmounted when DATABASE_URL is unset or
// the pgxpool / consent store cannot be built. The route is mounted in
// the chi authed+tenanted group (router.go) behind RequireAuth +
// RequireAction(ActionTenantInboxRead) — the accept handler resolves
// the tenant from the tenancy context (never from the body) and the
// actor from the session, so authorization never trusts a forgeable
// form field (OWASP A01 / BOPLA).
//
// Hexagonal lens: assembleAIPanelHandler is the pure seam that takes
// the already-built consent recorder + the session user-id resolver,
// so the wire is unit-testable without booting Postgres; the
// pool-backed builder is the only code that touches the DB adapter.

import (
	"context"
	"errors"
	"fmt"
	"log"
	"log/slog"
	"net/http"

	"github.com/google/uuid"

	pgpool "github.com/pericles-luz/crm/internal/adapter/db/postgres"
	aipolicypg "github.com/pericles-luz/crm/internal/adapter/db/postgres/aipolicy"
	"github.com/pericles-luz/crm/internal/aipolicy"
	"github.com/pericles-luz/crm/internal/obs"
	"github.com/pericles-luz/crm/internal/web/aipanel"
)

// buildAIPanelHandler returns the consent accept/cancel mux + cleanup.
// A nil handler means "skip the mount"; the router treats that as
// "don't expose the routes", the safe default when the DB is
// unreachable. metrics may be nil (the aipanel handler is nil-safe on
// the obs surface), but the production caller passes the shared
// boot-time registry so ai_consent_total lands on /metrics.
func buildAIPanelHandler(ctx context.Context, getenv func(string) string, metrics *obs.Metrics) (http.Handler, func()) {
	noop := func() {}
	if dsn := getenv(pgpool.EnvDSN); dsn == "" {
		log.Printf("crm: web/aipanel disabled — DATABASE_URL unset")
		return nil, noop
	}
	pool, err := pgpool.NewFromEnv(ctx, getenv)
	if err != nil {
		log.Printf("crm: web/aipanel disabled — pg connect: %v", err)
		return nil, noop
	}
	consentStore, err := aipolicypg.NewConsentStore(pool)
	if err != nil {
		pool.Close()
		log.Printf("crm: web/aipanel disabled — consent store: %v", err)
		return nil, noop
	}
	consentSvc, err := aipolicy.NewConsentService(consentStore)
	if err != nil {
		pool.Close()
		log.Printf("crm: web/aipanel disabled — consent service: %v", err)
		return nil, noop
	}
	handler, err := assembleAIPanelHandler(consentSvc, userIDFromSessionContext, metrics, slog.Default())
	if err != nil {
		pool.Close()
		log.Printf("crm: web/aipanel disabled — assemble: %v", err)
		return nil, noop
	}
	log.Printf("crm: web/aipanel consent routes mounted (accept+cancel)")
	return handler, func() { pool.Close() }
}

// assembleAIPanelHandler is the pure assembly seam. Tests call it
// directly with a fake recorder so the wire is exercised without a DB.
// userID resolves the actor from the session (uuid.Nil → consent row
// records actor_user_id NULL, which the handler tolerates).
func assembleAIPanelHandler(
	consent aipanel.ConsentRecorder,
	userID aipanel.UserIDFn,
	metrics *obs.Metrics,
	logger *slog.Logger,
) (http.Handler, error) {
	if consent == nil {
		return nil, errors.New("aipanel_wire: consent recorder is nil")
	}
	if userID == nil {
		return nil, errors.New("aipanel_wire: userID resolver is nil")
	}
	if logger == nil {
		logger = slog.Default()
	}
	h, err := aipanel.New(aipanel.Deps{
		Consent: consent,
		UserID:  userID,
		Metrics: metrics,
		Logger:  logger,
	})
	if err != nil {
		return nil, fmt.Errorf("aipanel_wire: build handler: %w", err)
	}
	mux := http.NewServeMux()
	h.Routes(mux)
	return mux, nil
}

// compile-time assertion: *aipolicy.ConsentService satisfies the
// handler's ConsentRecorder port (RecordConsent structural match).
var _ aipanel.ConsentRecorder = (*aipolicy.ConsentService)(nil)

// compile-time assertion: the session user-id resolver matches the
// handler's UserIDFn port.
var _ aipanel.UserIDFn = func(*http.Request) uuid.UUID { return uuid.Nil }
