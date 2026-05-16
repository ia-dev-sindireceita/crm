package instagram_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/pericles-luz/crm/internal/adapter/channels/instagram"
)

func TestHandleChallenge_SubscribeEchoesChallenge(t *testing.T) {
	t.Parallel()
	a, _ := newAdapter(t)
	mux := http.NewServeMux()
	a.Register(mux)
	req := httptest.NewRequest(http.MethodGet,
		"/webhooks/instagram?hub.mode=subscribe&hub.verify_token="+testVerifyToken+"&hub.challenge=42", nil)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", rr.Code)
	}
	if rr.Body.String() != "42" {
		t.Fatalf("body: got %q, want %q", rr.Body.String(), "42")
	}
}

func TestHandleChallenge_BadTokenReturns403(t *testing.T) {
	t.Parallel()
	a, _ := newAdapter(t)
	mux := http.NewServeMux()
	a.Register(mux)
	req := httptest.NewRequest(http.MethodGet,
		"/webhooks/instagram?hub.mode=subscribe&hub.verify_token=wrong&hub.challenge=42", nil)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("status: got %d, want 403", rr.Code)
	}
}

func TestHandleChallenge_BadModeReturns400(t *testing.T) {
	t.Parallel()
	a, _ := newAdapter(t)
	mux := http.NewServeMux()
	a.Register(mux)
	req := httptest.NewRequest(http.MethodGet,
		"/webhooks/instagram?hub.mode=other&hub.verify_token="+testVerifyToken+"&hub.challenge=42", nil)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400", rr.Code)
	}
}

func TestNew_RejectsMissingAppSecret(t *testing.T) {
	t.Parallel()
	cfg := instagram.Config{VerifyToken: "y", RateMaxPerMin: 1, MaxBodyBytes: 1, DeliverTimeout: 1}
	_, err := instagram.New(cfg, newFakeInbox(), newFakeResolver(), newFakeFlag(true), newFakeRateLimiter(0))
	if err == nil {
		t.Fatal("expected error for empty AppSecret")
	}
}

func TestNew_RejectsMissingVerifyToken(t *testing.T) {
	t.Parallel()
	cfg := instagram.Config{AppSecret: "x", RateMaxPerMin: 1, MaxBodyBytes: 1, DeliverTimeout: 1}
	_, err := instagram.New(cfg, newFakeInbox(), newFakeResolver(), newFakeFlag(true), newFakeRateLimiter(0))
	if err == nil {
		t.Fatal("expected error for empty VerifyToken")
	}
}

func TestNew_RejectsNilDeps(t *testing.T) {
	t.Parallel()
	cfg := instagram.Config{AppSecret: "x", VerifyToken: "y", RateMaxPerMin: 1, MaxBodyBytes: 1, DeliverTimeout: 1}
	if _, err := instagram.New(cfg, nil, newFakeResolver(), newFakeFlag(true), newFakeRateLimiter(0)); err == nil {
		t.Fatal("expected error for nil inbox")
	}
	if _, err := instagram.New(cfg, newFakeInbox(), nil, newFakeFlag(true), newFakeRateLimiter(0)); err == nil {
		t.Fatal("expected error for nil resolver")
	}
	if _, err := instagram.New(cfg, newFakeInbox(), newFakeResolver(), nil, newFakeRateLimiter(0)); err == nil {
		t.Fatal("expected error for nil flag")
	}
	if _, err := instagram.New(cfg, newFakeInbox(), newFakeResolver(), newFakeFlag(true), nil); err == nil {
		t.Fatal("expected error for nil rate limiter")
	}
}
