package webchat_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/pericles-luz/crm/internal/adapter/channels/webchat"
)

// perOriginValidator computes a distinct HMAC per origin so the message
// path can be exercised for "origin drift" (a session created on origin A
// whose token is replayed from origin B). Valid always allows; only the
// signature differentiates origins.
type perOriginValidator struct{}

func (perOriginValidator) Valid(_ context.Context, _ uuid.UUID, _ string) (bool, error) {
	return true, nil
}

func (perOriginValidator) HMAC(_ context.Context, _ uuid.UUID, origin string) (string, error) {
	return "sig:" + origin, nil
}

// D4 (ADR-0021): an explicit X-Webchat-Origin-Signature header that does
// not match the signature bound to the session is rejected with 403.
func TestHandleMessage_OriginSignatureHeaderMismatch_Returns403(t *testing.T) {
	a, _ := newAdapter(t, &fakeFlag{on: true}, webchat.NewInMemoryRateLimiter(0), &fakeOrigins{allowAll: true, sig: "s"}, &fakeInbox{})
	mux := http.NewServeMux()
	a.Register(mux)
	tenantID := uuid.New()
	sessID, csrf := createSession(t, mux, tenantID)

	body := `{"body":"hi","client_msg_id":"` + uuid.New().String() + `"}`
	req := httptest.NewRequest(http.MethodPost, "/widget/v1/message", strings.NewReader(body))
	req.Header.Set(webchat.HeaderSession, sessID)
	req.Header.Set(webchat.HeaderCSRF, csrf)
	req.Header.Set("Origin", "https://example.com")
	req.Header.Set(webchat.HeaderOriginSig, "forged-signature")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Errorf("want 403 for mismatched origin signature, got %d", w.Code)
	}
}

// D4: a matching explicit X-Webchat-Origin-Signature header is accepted
// and the message is delivered (204).
func TestHandleMessage_OriginSignatureHeaderValid_Returns204(t *testing.T) {
	fi := &fakeInbox{}
	a, _ := newAdapter(t, &fakeFlag{on: true}, webchat.NewInMemoryRateLimiter(0), &fakeOrigins{allowAll: true, sig: "s"}, fi)
	mux := http.NewServeMux()
	a.Register(mux)
	tenantID := uuid.New()
	sessID, csrf := createSession(t, mux, tenantID)

	body := `{"body":"hi","client_msg_id":"` + uuid.New().String() + `"}`
	req := httptest.NewRequest(http.MethodPost, "/widget/v1/message", strings.NewReader(body))
	req.Header.Set(webchat.HeaderSession, sessID)
	req.Header.Set(webchat.HeaderCSRF, csrf)
	req.Header.Set("Origin", "https://example.com")
	req.Header.Set(webchat.HeaderOriginSig, "s") // matches fakeOrigins.sig bound at create
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusNoContent {
		t.Fatalf("want 204 for valid origin signature, got %d", w.Code)
	}
	if len(fi.calls) != 1 {
		t.Errorf("want message delivered to inbox, got %d calls", len(fi.calls))
	}
}

// D4: a session/CSRF token replayed from a different origin is rejected
// because the signature recomputed from the request Origin no longer
// matches the one bound at session create — even when no explicit
// X-Webchat-Origin-Signature header is sent.
func TestHandleMessage_OriginDrift_Returns403(t *testing.T) {
	// Build an adapter with the per-origin validator so create binds
	// sig:https://example.com and the replay request recomputes a
	// different signature for its own origin.
	a, err := webchat.New(&fakeInbox{}, webchat.NewInMemorySessionStore(), perOriginValidator{}, &fakeFlag{on: true}, webchat.NewInMemoryRateLimiter(0), webchat.NewBroker(), nil)
	if err != nil {
		t.Fatalf("webchat.New: %v", err)
	}
	mux := http.NewServeMux()
	a.Register(mux)
	tenantID := uuid.New()
	sessID, csrf := createSession(t, mux, tenantID) // Origin https://example.com

	body := `{"body":"hi","client_msg_id":"` + uuid.New().String() + `"}`
	req := httptest.NewRequest(http.MethodPost, "/widget/v1/message", strings.NewReader(body))
	req.Header.Set(webchat.HeaderSession, sessID)
	req.Header.Set(webchat.HeaderCSRF, csrf)
	req.Header.Set("Origin", "https://evil.example") // replay from a different origin
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Errorf("want 403 for origin drift, got %d", w.Code)
	}
}

// D4: a same-origin message (Origin matches the session) with no explicit
// signature header is accepted — the recomputed signature matches the
// bound one. Pins backward compatibility for clients that rely on the
// recompute path rather than echoing the header.
func TestHandleMessage_OriginMatches_NoHeader_Returns204(t *testing.T) {
	fi := &fakeInbox{}
	a, err := webchat.New(fi, webchat.NewInMemorySessionStore(), perOriginValidator{}, &fakeFlag{on: true}, webchat.NewInMemoryRateLimiter(0), webchat.NewBroker(), nil)
	if err != nil {
		t.Fatalf("webchat.New: %v", err)
	}
	mux := http.NewServeMux()
	a.Register(mux)
	tenantID := uuid.New()
	sessID, csrf := createSession(t, mux, tenantID) // Origin https://example.com

	body := `{"body":"hi","client_msg_id":"` + uuid.New().String() + `"}`
	req := httptest.NewRequest(http.MethodPost, "/widget/v1/message", strings.NewReader(body))
	req.Header.Set(webchat.HeaderSession, sessID)
	req.Header.Set(webchat.HeaderCSRF, csrf)
	req.Header.Set("Origin", "https://example.com") // same origin as create
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusNoContent {
		t.Fatalf("want 204 for matching origin, got %d", w.Code)
	}
	if len(fi.calls) != 1 {
		t.Errorf("want message delivered, got %d calls", len(fi.calls))
	}
}
