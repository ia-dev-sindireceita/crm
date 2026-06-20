package main

// SIN-65364 — aipanel wire tests. The handler covers its own behaviour
// exhaustively in internal/web/aipanel; these tests pin the composition
// root: assembleAIPanelHandler returns a non-nil mux that routes the
// consent accept/cancel POSTs, and buildAIPanelHandler returns
// (nil, no-op) when DATABASE_URL is unset (fail-soft mount skip).

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/aipolicy"
	"github.com/pericles-luz/crm/internal/tenancy"
)

// fakeConsentRecorder is the minimal ConsentRecorder the wire test
// injects so assembleAIPanelHandler is exercised without a DB.
type fakeConsentRecorder struct {
	called bool
	scope  aipolicy.ConsentScope
}

func (f *fakeConsentRecorder) RecordConsent(
	_ context.Context,
	scope aipolicy.ConsentScope,
	_ *uuid.UUID,
	_, _, _ string,
) error {
	f.called = true
	f.scope = scope
	return nil
}

func TestAssembleAIPanelHandler_NilDepsError(t *testing.T) {
	t.Parallel()
	if _, err := assembleAIPanelHandler(nil, userIDFromSessionContext, nil, nil); err == nil {
		t.Fatal("expected error when consent recorder is nil")
	}
	if _, err := assembleAIPanelHandler(&fakeConsentRecorder{}, nil, nil, nil); err == nil {
		t.Fatal("expected error when userID resolver is nil")
	}
}

func TestAssembleAIPanelHandler_RoutesCancel(t *testing.T) {
	t.Parallel()
	rec := &fakeConsentRecorder{}
	h, err := assembleAIPanelHandler(rec, func(*http.Request) uuid.UUID { return uuid.Nil }, nil, nil)
	if err != nil {
		t.Fatalf("assembleAIPanelHandler: %v", err)
	}
	if h == nil {
		t.Fatal("handler is nil")
	}

	// Cancel needs only a tenancy context + scope_kind; it never calls
	// the recorder, so it is the cheapest end-to-end route assertion
	// that the mux wired the pattern (a 404 here would prove the mount
	// gap this issue fixes).
	r := httptest.NewRequest(http.MethodPost, "/aipanel/consent/cancel",
		strings.NewReader("scope_kind=channel"))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r = r.WithContext(tenancy.WithContext(r.Context(), &tenancy.Tenant{ID: uuid.New()}))

	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("cancel: status %d, want 200 (route not mounted?)", w.Code)
	}
	if !strings.Contains(w.Body.String(), `id="ai-consent-modal"`) {
		t.Errorf("cancel body missing modal placeholder; got %q", w.Body.String())
	}
	if rec.called {
		t.Error("cancel must not call RecordConsent")
	}
}

func TestBuildAIPanelHandler_FailSoftWithoutDSN(t *testing.T) {
	t.Parallel()
	// getenv returns empty for every key → DATABASE_URL unset → the wire
	// must return (nil, non-nil no-op cleanup) and leave /aipanel/*
	// unmounted, mirroring the other web/* surfaces.
	h, cleanup := buildAIPanelHandler(context.Background(), func(string) string { return "" }, nil)
	if h != nil {
		t.Errorf("handler = %v, want nil when DATABASE_URL unset", h)
	}
	if cleanup == nil {
		t.Fatal("cleanup must be a non-nil no-op even on the disabled path")
	}
	cleanup() // must not panic
}
