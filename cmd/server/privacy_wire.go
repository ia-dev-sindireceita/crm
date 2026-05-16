package main

// SIN-62354 wiring — HTMX privacy / DPA page (Fase 3, decisão #8 /
// SIN-62203). SIN-62918 follow-up swaps the boot-time static model
// resolver for the SIN-62351 aipolicy cascade.
//
// buildWebPrivacyHandler assembles the read-only privacy disclosure
// page. The active-AI-model lookup is now backed by the SIN-62351
// aipolicy cascade resolver (tenant scope only): a tenant with an
// ai_policy row sees its configured model, and a tenant without one
// falls through to DefaultPolicy().Model == "openrouter/auto", which
// matches the migration 0098 default that the privacy template
// renders. When DATABASE_URL is unset or the pgx pool / aipolicy
// adapter cannot be built, the wire keeps serving with the static
// fallback so the LGPD-required disclosure page never disappears —
// privacy disclosure is release-blocking.

import (
	"context"
	"errors"
	"fmt"
	"log"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"

	pgpool "github.com/pericles-luz/crm/internal/adapter/db/postgres"
	pgaipolicy "github.com/pericles-luz/crm/internal/adapter/db/postgres/aipolicy"
	"github.com/pericles-luz/crm/internal/aipolicy"
	webprivacy "github.com/pericles-luz/crm/internal/web/privacy"
)

// buildWebPrivacyHandler returns the privacy mux. Cleanup closes any
// pgxpool owned by the wire; callers can defer it unconditionally.
// The handler is non-nil by contract — LGPD disclosure is release-
// blocking, so a failed model-resolver build degrades to the static
// fallback instead of unmounting the page.
func buildWebPrivacyHandler(ctx context.Context, getenv func(string) string) (http.Handler, func()) {
	resolver, cleanup := buildPrivacyModelResolver(ctx, getenv)
	handler, err := assembleWebPrivacyHandler(resolver, time.Now, slog.Default())
	if err != nil {
		// The only way New returns an error here is the
		// DPAMentionsOpenRouter invariant failing — i.e. someone
		// shipped a broken DPA. Refuse to serve the privacy page
		// in that case so CI / smoke catches it loudly.
		cleanup()
		log.Printf("crm: web/privacy handler disabled — %v", err)
		return nil, func() {}
	}
	log.Printf("crm: web/privacy HTMX routes mounted on public listener")
	return handler, cleanup
}

// buildPrivacyModelResolver picks the ModelResolver the privacy page
// will use plus the cleanup closure for any resources it owns. The
// happy path is DATABASE_URL set → pgx pool open → aipolicy.Store →
// aipolicy.Resolver wrapped in aipolicyModelResolver. Any failure in
// that chain falls back to staticModelResolver{model: FallbackModel}
// so the page keeps rendering with the documented default.
//
// "Reutilizar pool pgx existente" (issue scope) means following the
// per-wire pool pattern that funnel_wire / contacts_wire already use:
// each composition-root wire owns its own pool and closes it from its
// cleanup. There is no shared process-wide pool to plug into today.
func buildPrivacyModelResolver(ctx context.Context, getenv func(string) string) (webprivacy.ModelResolver, func()) {
	noop := func() {}
	static := staticModelResolver{model: webprivacy.FallbackModel}

	if dsn := getenv(pgpool.EnvDSN); dsn == "" {
		log.Printf("crm: web/privacy active-model using static fallback (DATABASE_URL unset)")
		return static, noop
	}
	pool, err := pgpool.NewFromEnv(ctx, getenv)
	if err != nil {
		log.Printf("crm: web/privacy active-model using static fallback — pg connect: %v", err)
		return static, noop
	}
	store, err := pgaipolicy.New(pool)
	if err != nil {
		pool.Close()
		log.Printf("crm: web/privacy active-model using static fallback — aipolicy store: %v", err)
		return static, noop
	}
	resolver, err := aipolicy.NewResolver(store)
	if err != nil {
		pool.Close()
		log.Printf("crm: web/privacy active-model using static fallback — aipolicy resolver: %v", err)
		return static, noop
	}
	log.Printf("crm: web/privacy active-model resolved via aipolicy cascade (tenant scope)")
	return aipolicyModelResolver{resolver: resolver}, func() { pool.Close() }
}

// assembleWebPrivacyHandler is the pure assembly seam. Tests call it
// directly with stub deps so the wire is exercised without booting
// the whole server.
func assembleWebPrivacyHandler(
	resolver webprivacy.ModelResolver,
	now webprivacy.Now,
	logger *slog.Logger,
) (http.Handler, error) {
	if resolver == nil {
		return nil, errors.New("privacy_wire: resolver is nil")
	}
	if now == nil {
		return nil, errors.New("privacy_wire: now is nil")
	}
	if logger == nil {
		logger = slog.Default()
	}
	h, err := webprivacy.New(webprivacy.Deps{
		Model:  resolver,
		Now:    now,
		Logger: logger,
	})
	if err != nil {
		return nil, fmt.Errorf("privacy_wire: build handler: %w", err)
	}
	mux := http.NewServeMux()
	h.Routes(mux)
	return mux, nil
}

// staticModelResolver returns a single hard-coded model string. Used
// when DATABASE_URL is unset or the aipolicy adapter chain cannot be
// built — the value matches migration 0098's ai_policy.model default
// so the page renders the same string a fresh tenant would see if
// they queried the DB directly.
type staticModelResolver struct {
	model string
}

func (s staticModelResolver) ActiveModel(_ context.Context, _ uuid.UUID) (string, error) {
	return s.model, nil
}

// policyResolver is the seam buildPrivacyModelResolver depends on so
// the wire stays testable without pgx. The concrete *aipolicy.Resolver
// satisfies it; tests substitute an in-process stub.
type policyResolver interface {
	Resolve(ctx context.Context, in aipolicy.ResolveInput) (aipolicy.Policy, aipolicy.ResolveSource, error)
}

// aipolicyModelResolver adapts the SIN-62351 cascade resolver into
// the webprivacy.ModelResolver port. The privacy page is tenant-
// scoped: it intentionally never narrows by channel or team, so the
// cascade falls through to SourceTenant (configured row) or
// SourceDefault (DefaultPolicy().Model == "openrouter/auto").
//
// On resolver error the adapter bubbles it up; the privacy handler's
// fail-soft path then logs and renders FallbackModel, preserving the
// AC #3 invariant ("resolver error continua não-fatal") covered by
// internal/web/privacy/handlers_test.go.
type aipolicyModelResolver struct {
	resolver policyResolver
}

func (a aipolicyModelResolver) ActiveModel(ctx context.Context, tenantID uuid.UUID) (string, error) {
	policy, _, err := a.resolver.Resolve(ctx, aipolicy.ResolveInput{TenantID: tenantID})
	if err != nil {
		return "", fmt.Errorf("aipolicy resolve: %w", err)
	}
	return policy.Model, nil
}
