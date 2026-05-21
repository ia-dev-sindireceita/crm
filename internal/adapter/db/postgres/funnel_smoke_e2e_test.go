package postgres_test

// SIN-63220 — GET /funnel authenticated smoke E2E. Drives the real
// httpapi.NewRouter chain against a Postgres-backed iam.Service +
// pgfunnel.Store so a regression of the funnel handler's wireup or its
// dependency on the CSRF session round-trip is caught at the smoke level
// instead of 500'ing for every operator in staging.
//
// The F4 pen-test (SIN-63190 → SIN-63220) saw GET /funnel return 500
// for every authenticated user against acme.crm.crm.someu.com.br. Root
// cause was the same as SIN-63222: the sessions table had no csrf_token
// column, so SessionStore.Get re-hydrated iam.Session with
// CSRFToken="" and the funnel.board() handler tripped its
// `if token == ""` guard with 500. With migration 0111 in place, the
// same flow renders the board shell.
//
// This test fills the F4 smoke AC: a real-DB end-to-end that logs in
// via POST /login, then hits GET /funnel with the cookies and asserts
// 200 plus a stable template marker.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/adapter/db/postgres"
	pgfunnel "github.com/pericles-luz/crm/internal/adapter/db/postgres/funnel"
	pginbox "github.com/pericles-luz/crm/internal/adapter/db/postgres/inbox"
	"github.com/pericles-luz/crm/internal/adapter/httpapi"
	"github.com/pericles-luz/crm/internal/adapter/httpapi/middleware"
	"github.com/pericles-luz/crm/internal/adapter/httpapi/sessioncookie"
	"github.com/pericles-luz/crm/internal/funnel"
	"github.com/pericles-luz/crm/internal/iam"
	"github.com/pericles-luz/crm/internal/tenancy"
	webfunnel "github.com/pericles-luz/crm/internal/web/funnel"
)

// funnelSlogPublisher is a no-op funnel.EventPublisher. The smoke
// renders the read-side board and never calls MoveConversation, so
// Publish is never invoked; the type only exists so funnel.NewService's
// nil-check passes.
type funnelSlogPublisher struct{}

func (funnelSlogPublisher) Publish(context.Context, string, any) error { return nil }

// inboxAssignmentHistoryAdapter mirrors the cmd/server adapter (private
// to package main): it narrows pginbox.Store down to ListHistory and
// remaps *inbox.Assignment → webfunnel.AssignmentEntry so the funnel
// handler can be wired without importing the inbox domain.
type inboxAssignmentHistoryAdapter struct {
	port *pginbox.Store
}

func (a inboxAssignmentHistoryAdapter) ListHistory(ctx context.Context, tenantID, conversationID uuid.UUID) ([]webfunnel.AssignmentEntry, error) {
	rows, err := a.port.ListHistory(ctx, tenantID, conversationID)
	if err != nil {
		return nil, err
	}
	out := make([]webfunnel.AssignmentEntry, 0, len(rows))
	for _, r := range rows {
		if r == nil {
			continue
		}
		out = append(out, webfunnel.AssignmentEntry{
			AssignedAt: r.AssignedAt,
			UserID:     r.UserID,
			Reason:     string(r.Reason),
		})
	}
	return out, nil
}

// transitionsHistoryAdapter narrows funnel.TransitionRepository down to
// ListForConversation so the funnel handler's FunnelHistory port can be
// satisfied by the same pgfunnel.Store value the BoardReader port uses.
type transitionsHistoryAdapter struct {
	port funnel.TransitionRepository
}

func (a transitionsHistoryAdapter) ListForConversation(ctx context.Context, tenantID, conversationID uuid.UUID) ([]*funnel.Transition, error) {
	return a.port.ListForConversation(ctx, tenantID, conversationID)
}

// csrfTokenFromContext + userIDFromContext mirror the closures wired in
// cmd/server/{htmx_wire,funnel_wire}.go. Inlined here because those
// helpers live in package main; copying the three-line contract beats
// pulling cmd/server out of the binary just to share two closures.
func csrfTokenFromContext(r *http.Request) string {
	sess, ok := middleware.SessionFromContext(r.Context())
	if !ok {
		return ""
	}
	return sess.CSRFToken
}

func userIDFromContext(r *http.Request) uuid.UUID {
	sess, ok := middleware.SessionFromContext(r.Context())
	if !ok {
		return uuid.Nil
	}
	return sess.UserID
}

// TestRouter_FunnelBoard_E2E_PostgresLoginThenBoard is the SIN-63220
// smoke. Without migration 0111 + the matching SessionStore patch (the
// SIN-63222 fix), GET /funnel returns 500 with body "Internal Server
// Error" because the handler's `if token == ""` guard fires. With the
// fix in place the same flow renders the five-column funnel-board
// shell.
func TestRouter_FunnelBoard_E2E_PostgresLoginThenBoard(t *testing.T) {
	db := freshDBWithIAM(t)
	const host = "acme.crm.local"
	// Layer the funnel-side migrations on top of freshDBWithIAM's
	// 0004-0006 + 0077 + 0111 chain. 0088 → 0092 → 0093 is the same
	// order the production migrator runs.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	applyChain(t, ctx, db,
		"0088_inbox_contacts.up.sql",
		"0092_identity_link_assignment_history.up.sql",
		"0093_funnel_stage_transition.up.sql",
	)
	tenantID, _, plaintext := seedTenant(t, db, host, "alice@funnel.test")

	// Real IAM stack — same Postgres-backed Login flow SIN-63222's
	// csrf_e2e_test.go exercises, so the session round-trip MUST put a
	// non-empty CSRFToken into the request context.
	svc := &iam.Service{
		Tenants:  fixedTenantResolver{host: host, tenantID: tenantID},
		Users:    postgres.NewUserCredentialReader(db.RuntimePool()),
		Sessions: postgres.NewSessionStore(db.RuntimePool()),
		TTL:      time.Hour,
	}

	// Real funnel adapter — Board() runs against the seeded
	// funnel_stage rows (the migration 0093 trigger seeded the five
	// defaults when the test inserted the tenant row).
	funnelStore, err := pgfunnel.New(db.RuntimePool())
	if err != nil {
		t.Fatalf("pgfunnel.New: %v", err)
	}
	inboxStore, err := pginbox.New(db.RuntimePool())
	if err != nil {
		t.Fatalf("pginbox.New: %v", err)
	}
	funnelSvc, err := funnel.NewService(funnel.Config{
		Stages:      funnelStore,
		Transitions: funnelStore,
		Publisher:   funnelSlogPublisher{},
	})
	if err != nil {
		t.Fatalf("funnel.NewService: %v", err)
	}
	webFunnelHandler, err := webfunnel.New(webfunnel.Deps{
		Mover:             funnelSvc,
		Board:             funnelStore,
		StageResolver:     funnelStore,
		FunnelHistory:     transitionsHistoryAdapter{port: funnelStore},
		AssignmentHistory: inboxAssignmentHistoryAdapter{port: inboxStore},
		CSRFToken:         csrfTokenFromContext,
		UserID:            userIDFromContext,
	})
	if err != nil {
		t.Fatalf("webfunnel.New: %v", err)
	}
	funnelMux := http.NewServeMux()
	webFunnelHandler.Routes(funnelMux)

	h := httpapi.NewRouter(httpapi.Deps{
		IAM: svc,
		TenantResolver: tenancyResolver{
			host:   host,
			tenant: &tenancy.Tenant{ID: tenantID, Name: "acme", Host: host},
		},
		MasterHost: "master.crm.local",
		WebFunnel:  funnelMux,
	})

	// POST /login — issued with form body + Host header. The handler
	// mints a session row + CSRF token and sets __Host-sess-tenant +
	// __Host-csrf cookies on the response.
	form := url.Values{}
	form.Set("email", "alice@funnel.test")
	form.Set("password", plaintext)
	loginReq := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(form.Encode()))
	loginReq.Host = host
	loginReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	loginRec := httptest.NewRecorder()
	h.ServeHTTP(loginRec, loginReq)
	if loginRec.Code != http.StatusFound {
		t.Fatalf("login status=%d, want 302; body=%s", loginRec.Code, loginRec.Body.String())
	}
	var sessCookie, csrfCookie *http.Cookie
	for _, c := range loginRec.Result().Cookies() {
		switch c.Name {
		case sessioncookie.NameTenant:
			sessCookie = c
		case sessioncookie.NameCSRF:
			csrfCookie = c
		}
	}
	if sessCookie == nil || csrfCookie == nil {
		t.Fatalf("login did not set both session+csrf cookies (sess=%v, csrf=%v)", sessCookie, csrfCookie)
	}
	if csrfCookie.Value == "" {
		t.Fatalf("CSRF cookie value is empty; iam.Login should mint and the handler should mirror it")
	}

	// GET /funnel — the F4 path. Before SIN-63222's fix this is the
	// 500. After the fix the chi authed chain installs a Principal +
	// non-empty CSRFToken in context, the funnel handler renders the
	// board shell, and the test sees 200 + the stable template marker.
	boardReq := httptest.NewRequest(http.MethodGet, "/funnel", nil)
	boardReq.Host = host
	boardReq.AddCookie(sessCookie)
	boardReq.AddCookie(csrfCookie)
	boardRec := httptest.NewRecorder()
	h.ServeHTTP(boardRec, boardReq)

	if boardRec.Code != http.StatusOK {
		t.Fatalf("GET /funnel status=%d, want 200; body=%s", boardRec.Code, boardRec.Body.String())
	}
	body := boardRec.Body.String()
	// Pin a stable subset of the board shell so a careless template
	// refactor lights up this smoke too — same anchors the in-package
	// webfunnel handler test uses.
	for _, want := range []string{
		"funnel-board",
		`data-stage-key="novo"`,
		`data-stage-key="qualificando"`,
		`data-stage-key="proposta"`,
		`data-stage-key="ganho"`,
		`data-stage-key="perdido"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("rendered board missing %q", want)
		}
	}
	if ct := boardRec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Fatalf("Content-Type=%q, want text/html prefix", ct)
	}
}
