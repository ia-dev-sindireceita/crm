package main

// SIN-62855 wiring — HTMX identity-split UI (SIN-62799 / Fase 2 F2-13).
//
// buildWebContactsHandler assembles the read+write use cases that back
// the contact-detail page (GET /contacts/{contactID}) and the split
// endpoint (POST /contacts/identity/split). The adapter is the pgx
// IdentityStore from internal/adapter/db/postgres/contacts.
//
// The returned http.Handler is the stdlib *http.ServeMux registered via
// web/contacts.Handler.Routes. cmd/server hands it to httpapi.NewRouter
// via Deps.WebContacts; the chi authed group wraps it with TenantScope
// + Auth + CSRF + RequireAuth before dispatch.
//
// Returns (nil, no-op) when DATABASE_URL is unset so cmd/server keeps
// booting cleanly in health-only / smoke modes (same fail-soft pattern
// as buildIAMHandler / buildInternalHandlerWith).
//
// Inbox HTMX wiring is intentionally deferred: SendOutbound depends on
// inbox.WalletDebitor + inbox.OutboundChannel adapters that do not yet
// exist in production (no central dispatcher). Wiring the inbox shell
// without those would either need stub adapters in production code or
// loosen the existing handler's nil-rejection — both worse than waiting
// for the dispatcher to land. Tracked as a follow-up issue.

import (
	"context"
	"errors"
	"fmt"
	"log"
	"log/slog"
	"net/http"

	pgpool "github.com/pericles-luz/crm/internal/adapter/db/postgres"
	pgcontacts "github.com/pericles-luz/crm/internal/adapter/db/postgres/contacts"
	"github.com/pericles-luz/crm/internal/adapter/httpapi/middleware"
	contactsusecase "github.com/pericles-luz/crm/internal/contacts/usecase"
	webcontacts "github.com/pericles-luz/crm/internal/web/contacts"
)

// buildWebContactsHandler returns the HTMX contacts mux + a cleanup
// closure that releases the pgxpool. A nil handler signals "skip
// mounting on the public listener" so callers can defer the cleanup
// unconditionally.
func buildWebContactsHandler(ctx context.Context, getenv func(string) string) (http.Handler, func()) {
	noop := func() {}
	dsn := getenv(pgpool.EnvDSN)
	if dsn == "" {
		log.Printf("crm: web/contacts handler disabled (DATABASE_URL unset)")
		return nil, noop
	}
	pool, err := pgpool.NewFromEnv(ctx, getenv)
	if err != nil {
		log.Printf("crm: web/contacts handler disabled — pg connect: %v", err)
		return nil, noop
	}
	store, err := pgcontacts.NewIdentityStore(pool)
	if err != nil {
		pool.Close()
		log.Printf("crm: web/contacts handler disabled — identity store: %v", err)
		return nil, noop
	}
	handler, err := assembleWebContactsHandler(store)
	if err != nil {
		pool.Close()
		log.Printf("crm: web/contacts handler disabled — assemble: %v", err)
		return nil, noop
	}
	log.Printf("crm: web/contacts HTMX routes mounted on public listener")
	return handler, func() { pool.Close() }
}

// assembleWebContactsHandler constructs the use cases + handler from an
// already-built IdentitySplitRepository. Split out so tests can swap a
// fake repo without touching pgx.
//
// The downstream constructors (NewLoadIdentityForContact,
// NewSplitIdentityLink, webcontacts.New) fail only when a required
// dependency is nil. The nil-repo guard above makes those branches
// unreachable in practice, so we treat any error from them as a
// programmer bug and panic — preserving honest error reporting at the
// boundary the caller actually exercises.
func assembleWebContactsHandler(repo contactsusecase.IdentitySplitRepository) (http.Handler, error) {
	if repo == nil {
		return nil, errors.New("htmx_wire: identity split repository is nil")
	}
	loadUC, err := contactsusecase.NewLoadIdentityForContact(repo)
	if err != nil {
		panic(fmt.Errorf("htmx_wire: NewLoadIdentityForContact (unreachable): %w", err))
	}
	splitUC, err := contactsusecase.NewSplitIdentityLink(repo)
	if err != nil {
		panic(fmt.Errorf("htmx_wire: NewSplitIdentityLink (unreachable): %w", err))
	}
	h, err := webcontacts.New(webcontacts.Deps{
		LoadIdentity: loadUC,
		SplitLink:    splitUC,
		CSRFToken:    csrfTokenFromSessionContext,
		Logger:       slog.Default(),
	})
	if err != nil {
		panic(fmt.Errorf("htmx_wire: webcontacts.New (unreachable): %w", err))
	}
	mux := http.NewServeMux()
	h.Routes(mux)
	return mux, nil
}

// csrfTokenFromSessionContext reads the validated session installed by
// middleware.Auth and returns its CSRFToken. The empty string surfaces
// to web/contacts as a 500 (the handler treats an empty token as a
// programmer error) — consistent with the chi router's
// csrfSessionTokenFromContext closure.
func csrfTokenFromSessionContext(r *http.Request) string {
	sess, ok := middleware.SessionFromContext(r.Context())
	if !ok {
		return ""
	}
	return sess.CSRFToken
}
