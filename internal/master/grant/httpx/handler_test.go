package httpx_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/pericles-luz/crm/internal/master/grant"
	"github.com/pericles-luz/crm/internal/master/grant/httpx"
	"github.com/pericles-luz/crm/internal/master/grant/memory"
)

func doJSON(t *testing.T, srv *httptest.Server, method, path, master, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), method, srv.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatalf("req: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if master != "" {
		req.Header.Set("X-Test-Master", master)
	}
	req.Header.Set("X-Forwarded-For", "203.0.113.7, 10.0.0.1")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	return resp
}

// We rebuild the server inside each test using a header-driven resolver,
// so the helper above returns the parameters but tests construct their
// own setup. The header-driven setup keeps tests simple.

func newServerHeader(t *testing.T, approval bool) (*httptest.Server, *memory.Repo, *memory.AuditLogger, *memory.AlertNotifier) {
	t.Helper()
	repo := memory.NewRepo()
	audit := memory.NewAuditLogger()
	alerts := memory.NewAlertNotifier()
	clock := memory.NewFixedClock(time.Date(2026, 5, 2, 12, 0, 0, 0, time.UTC))
	ids := memory.NewSequenceIDs("g")
	svc := grant.NewService(grant.NewPolicy(grant.DefaultCaps(), approval), repo, audit, alerts, clock, ids)

	resolver := httpx.PrincipalResolverFunc(func(r *http.Request) (grant.Principal, error) {
		m := r.Header.Get("X-Test-Master")
		if m == "" {
			return grant.Principal{}, errors.New("no master")
		}
		return grant.Principal{MasterID: m}, nil
	})

	mux := http.NewServeMux()
	httpx.NewHandler(svc, resolver).Register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, repo, audit, alerts
}

func TestHandler_CreateSuccess(t *testing.T) {
	t.Parallel()
	srv, _, _, _ := newServerHeader(t, false)
	body, _ := json.Marshal(map[string]any{
		"tenant_id":       "t1",
		"subscription_id": "s1",
		"amount":          5_000_000,
		"reason":          "loyalty",
	})
	resp := doJSON(t, srv, "POST", "/master/grants", "m1", string(body))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: %d", resp.StatusCode)
	}
}

func TestHandler_CreateForbiddenAboveCap(t *testing.T) {
	t.Parallel()
	srv, repo, audit, _ := newServerHeader(t, false)
	body, _ := json.Marshal(map[string]any{
		"tenant_id":       "t1",
		"subscription_id": "s1",
		"amount":          11_000_000,
		"reason":          "abuse-test",
	})
	resp := doJSON(t, srv, "POST", "/master/grants", "m1", string(body))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status: %d", resp.StatusCode)
	}
	var er struct{ Error string }
	_ = json.NewDecoder(resp.Body).Decode(&er)
	if er.Error != "requires approval" {
		t.Errorf("body: %q", er.Error)
	}
	if len(repo.Snapshot()) != 0 {
		t.Errorf("denied request must not persist")
	}
	if got := audit.CountKind(grant.AuditDeniedCap); got != 1 {
		t.Errorf("denied audit: want 1, got %d", got)
	}
}

func TestHandler_CreateUnauthenticated(t *testing.T) {
	t.Parallel()
	srv, _, _, _ := newServerHeader(t, false)
	body := `{"tenant_id":"t1","subscription_id":"s1","amount":1,"reason":"x"}`
	resp := doJSON(t, srv, "POST", "/master/grants", "", body)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status: %d", resp.StatusCode)
	}
}

func TestHandler_CreateBadJSON(t *testing.T) {
	t.Parallel()
	srv, _, _, _ := newServerHeader(t, false)
	resp := doJSON(t, srv, "POST", "/master/grants", "m1", `not json`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status: %d", resp.StatusCode)
	}
}

func TestHandler_CreateValidationError(t *testing.T) {
	t.Parallel()
	srv, _, _, _ := newServerHeader(t, false)
	resp := doJSON(t, srv, "POST", "/master/grants", "m1", `{"tenant_id":"","subscription_id":"s","amount":1,"reason":"r"}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status: %d", resp.StatusCode)
	}
}

func TestHandler_RatifyApproveAndDeny(t *testing.T) {
	t.Parallel()
	srv, repo, _, _ := newServerHeader(t, true)
	body, _ := json.Marshal(map[string]any{
		"tenant_id":       "t1",
		"subscription_id": "s1",
		"amount":          11_000_000,
		"reason":          "above cap",
	})
	resp := doJSON(t, srv, "POST", "/master/grants", "m1", string(body))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected pending->403, got %d", resp.StatusCode)
	}
	rows := repo.Snapshot()
	if len(rows) != 1 {
		t.Fatalf("rows: %d", len(rows))
	}

	// Approve by another master.
	resp2 := doJSON(t, srv, "POST", "/master/grants/"+rows[0].ID+"/ratify", "m2", `{"approve":true,"note":"ok"}`)
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Errorf("ratify approve status: %d", resp2.StatusCode)
	}

	// Self-approval forbidden.
	body2, _ := json.Marshal(map[string]any{"tenant_id": "t1", "subscription_id": "s2", "amount": 11_000_000, "reason": "test"})
	resp3 := doJSON(t, srv, "POST", "/master/grants", "m3", string(body2))
	resp3.Body.Close()
	rows = repo.Snapshot()
	pendingID := rows[len(rows)-1].ID
	resp4 := doJSON(t, srv, "POST", "/master/grants/"+pendingID+"/ratify", "m3", `{"approve":true}`)
	defer resp4.Body.Close()
	if resp4.StatusCode != http.StatusForbidden {
		t.Errorf("self-approval status: %d", resp4.StatusCode)
	}

	// Not-found
	resp5 := doJSON(t, srv, "POST", "/master/grants/missing/ratify", "m9", `{"approve":true}`)
	defer resp5.Body.Close()
	if resp5.StatusCode != http.StatusNotFound {
		t.Errorf("not found: %d", resp5.StatusCode)
	}
}

func TestHandler_RatifyDisabledFlow(t *testing.T) {
	t.Parallel()
	srv, _, _, _ := newServerHeader(t, false)
	resp := doJSON(t, srv, "POST", "/master/grants/anything/ratify", "m1", `{"approve":true}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("status: %d", resp.StatusCode)
	}
}

func TestHandler_RatifyBadJSON(t *testing.T) {
	t.Parallel()
	srv, _, _, _ := newServerHeader(t, true)
	resp := doJSON(t, srv, "POST", "/master/grants/some-id/ratify", "m2", `nope`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status: %d", resp.StatusCode)
	}
}

func TestHandler_RatifyUnauthenticated(t *testing.T) {
	t.Parallel()
	srv, _, _, _ := newServerHeader(t, true)
	resp := doJSON(t, srv, "POST", "/master/grants/some-id/ratify", "", `{"approve":true}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status: %d", resp.StatusCode)
	}
}

func TestHandler_RatifyNotPending(t *testing.T) {
	t.Parallel()
	srv, repo, _, _ := newServerHeader(t, true)
	body := `{"tenant_id":"t1","subscription_id":"s1","amount":5000000,"reason":"r"}`
	resp := doJSON(t, srv, "POST", "/master/grants", "m1", body)
	resp.Body.Close()
	if got := len(repo.Snapshot()); got != 1 {
		t.Fatalf("rows: %d", got)
	}
	id := repo.Snapshot()[0].ID
	resp2 := doJSON(t, srv, "POST", "/master/grants/"+id+"/ratify", "m2", `{"approve":true}`)
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusConflict {
		t.Errorf("status: %d", resp2.StatusCode)
	}
}

func TestClientIPFallback_NoXFF(t *testing.T) {
	t.Parallel()
	// Hit the handler directly to exercise clientIP fallback path.
	srv, _, audit, _ := newServerHeader(t, false)
	body := `{"tenant_id":"t1","subscription_id":"s1","amount":5000000,"reason":"r"}`
	req, _ := http.NewRequest("POST", srv.URL+"/master/grants", bytes.NewBufferString(body))
	req.Header.Set("X-Test-Master", "m1")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: %d", resp.StatusCode)
	}
	for _, e := range audit.Entries() {
		if e.IPAddress == "" && e.Kind == grant.AuditGranted {
			t.Errorf("expected client IP populated by handler")
		}
	}
}
