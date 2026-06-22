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

	"github.com/google/uuid"

	pgpool "github.com/pericles-luz/crm/internal/adapter/db/postgres"
	pgcontacts "github.com/pericles-luz/crm/internal/adapter/db/postgres/contacts"
	pginbox "github.com/pericles-luz/crm/internal/adapter/db/postgres/inbox"
	"github.com/pericles-luz/crm/internal/adapter/httpapi/middleware"
	"github.com/pericles-luz/crm/internal/contacts"
	contactsusecase "github.com/pericles-luz/crm/internal/contacts/usecase"
	inboxdomain "github.com/pericles-luz/crm/internal/inbox"
	webcontacts "github.com/pericles-luz/crm/internal/web/contacts"
	"github.com/pericles-luz/crm/internal/web/userlabel"
)

// contactConversationReader is the narrow read port the contact detail
// view uses for recent conversation history. The postgres inbox Store
// satisfies it structurally (ListConversationsByContact). Declared here
// in the composition root so the wire can hand it to
// contactsusecase.NewGetContactDetail, whose own reader interface is
// unexported.
type contactConversationReader interface {
	ListConversationsByContact(ctx context.Context, tenantID, contactID uuid.UUID, limit int) ([]*inboxdomain.Conversation, error)
}

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
	// SIN-64977 — the management surface (list/search/detail/edit) reads
	// the contacts.Repository adapter (Store) and the inbox conversation
	// history reader. Both come off the same pool. A failure to build
	// either degrades to the identity-split-only handler rather than
	// dropping the whole surface.
	contactsRepo, err := pgcontacts.New(pool)
	if err != nil {
		pool.Close()
		log.Printf("crm: web/contacts handler disabled — contacts store: %v", err)
		return nil, noop
	}
	var convReader contactConversationReader
	if inboxStore, ierr := pginbox.New(pool); ierr != nil {
		log.Printf("crm: web/contacts conversation history disabled — inbox store: %v", ierr)
	} else {
		convReader = inboxStore
	}
	// SIN-65578 — resolve the top-bar account label off the users table.
	// A build failure soft-degrades to the "Conta" fallback (nil
	// directory) rather than dropping the surface.
	var userDir userlabel.Directory
	if dir, derr := pginbox.NewUserDirectory(pool); derr != nil {
		log.Printf("crm: web/contacts top-bar label disabled — user directory: %v", derr)
	} else {
		userDir = dir
	}
	handler, err := assembleWebContactsHandlerWith(store, contactsRepo, convReader, userDir)
	if err != nil {
		pool.Close()
		log.Printf("crm: web/contacts handler disabled — assemble: %v", err)
		return nil, noop
	}
	log.Printf("crm: web/contacts HTMX routes mounted on public listener (management surface enabled)")
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
	return assembleWebContactsHandlerWith(repo, nil, nil)
}

// assembleWebContactsHandlerWith constructs the full management handler:
// the identity-split use cases (always) plus the SIN-64977 list/detail/
// edit use cases when contactsRepo is non-nil. convReader is optional —
// nil degrades the detail view's conversation history to empty.
//
// The downstream Must* constructors panic only on a nil dependency; the
// nil-repo guards here make those branches unreachable, so a panic would
// be a genuine programmer bug rather than an operational error.
// userLabels is variadic so the existing 3-arg test call sites keep
// compiling; the production path passes the UserDirectory adapter as the
// optional fourth argument. Only the first value is honoured.
func assembleWebContactsHandlerWith(
	identityRepo contactsusecase.IdentitySplitRepository,
	contactsRepo contacts.Repository,
	convReader contactConversationReader,
	userLabels ...userlabel.Directory,
) (http.Handler, error) {
	if identityRepo == nil {
		return nil, errors.New("htmx_wire: identity split repository is nil")
	}
	loadUC, err := contactsusecase.NewLoadIdentityForContact(identityRepo)
	if err != nil {
		panic(fmt.Errorf("htmx_wire: NewLoadIdentityForContact (unreachable): %w", err))
	}
	splitUC, err := contactsusecase.NewSplitIdentityLink(identityRepo)
	if err != nil {
		panic(fmt.Errorf("htmx_wire: NewSplitIdentityLink (unreachable): %w", err))
	}
	var userDir userlabel.Directory
	if len(userLabels) > 0 {
		userDir = userLabels[0]
	}
	deps := webcontacts.Deps{
		LoadIdentity: loadUC,
		SplitLink:    splitUC,
		CSRFToken:    csrfTokenFromSessionContext,
		UserID:       userIDFromSessionContext,
		UserLabels:   userDir,
		Logger:       slog.Default(),
	}
	if contactsRepo != nil {
		deps.ListContacts = contactsusecase.MustNewListContacts(contactsRepo)
		deps.UpdateContact = contactsusecase.MustNewUpdateContact(contactsRepo)
		deps.GetDetail = contactsusecase.MustNewGetContactDetail(contactsRepo, convReader)
	}
	h, err := webcontacts.New(deps)
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
