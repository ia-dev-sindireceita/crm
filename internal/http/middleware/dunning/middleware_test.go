package dunning_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	billingdunning "github.com/pericles-luz/crm/internal/billing/dunning"
	dunningmw "github.com/pericles-luz/crm/internal/http/middleware/dunning"
	"github.com/pericles-luz/crm/internal/tenancy"
)

type fakeLookup struct {
	state billingdunning.State
	err   error
	calls int
}

func (f *fakeLookup) CurrentForTenant(_ context.Context, _ uuid.UUID) (*billingdunning.DunningState, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	if f.state == "" {
		return nil, billingdunning.ErrNotFound
	}
	now := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	return billingdunning.HydrateDunningState(uuid.New(), uuid.New(), uuid.New(),
		f.state, now, uuid.Nil, nil, ""), nil
}

func okHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
}

func withTenant(r *http.Request) *http.Request {
	t := &tenancy.Tenant{ID: uuid.New(), Host: "tenant.local"}
	return r.WithContext(tenancy.WithContext(r.Context(), t))
}

func newDiscardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func newMW(t *testing.T, cfg dunningmw.Config) *dunningmw.Middleware {
	t.Helper()
	if cfg.Logger == nil {
		cfg.Logger = newDiscardLogger()
	}
	m, err := dunningmw.New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return m
}

func TestMiddleware_NewRequiresLookup(t *testing.T) {
	if _, err := dunningmw.New(dunningmw.Config{}); err == nil {
		t.Fatal("New returned nil error without Lookup")
	}
}

func TestMiddleware_PassesThroughWithoutTenant(t *testing.T) {
	mw := newMW(t, dunningmw.Config{Lookup: &fakeLookup{state: billingdunning.StateSuspendedFull}})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/anywhere", nil)
	mw.Wrap(okHandler()).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 (no tenant, pass-through)", rec.Code)
	}
}

func TestMiddleware_AllowsCurrentAndWarn(t *testing.T) {
	for _, state := range []billingdunning.State{billingdunning.StateCurrent, billingdunning.StateWarn} {
		t.Run(string(state), func(t *testing.T) {
			mw := newMW(t, dunningmw.Config{
				Lookup:         &fakeLookup{state: state},
				OutboundRoutes: dunningmw.PrefixOutboundClassifier([]string{"/api/send"}),
			})
			rec := httptest.NewRecorder()
			req := withTenant(httptest.NewRequest("POST", "/api/send/msg", nil))
			mw.Wrap(okHandler()).ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Errorf("state %s blocked: %d", state, rec.Code)
			}
		})
	}
}

func TestMiddleware_AllowsTenantWithoutDunningRow(t *testing.T) {
	mw := newMW(t, dunningmw.Config{Lookup: &fakeLookup{err: billingdunning.ErrNotFound}})
	rec := httptest.NewRecorder()
	req := withTenant(httptest.NewRequest("POST", "/api/send", nil))
	mw.Wrap(okHandler()).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 (no dunning row)", rec.Code)
	}
}

func TestMiddleware_OutboundSuspended_BlocksFlaggedRoutes(t *testing.T) {
	mw := newMW(t, dunningmw.Config{
		Lookup:         &fakeLookup{state: billingdunning.StateSuspendedOutbound},
		OutboundRoutes: dunningmw.PrefixOutboundClassifier([]string{"/api/send", "/automations"}),
	})
	rec := httptest.NewRecorder()
	req := withTenant(httptest.NewRequest("POST", "/api/send/msg", nil))
	mw.Wrap(okHandler()).ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", rec.Code)
	}
	if got := rec.Body.String(); !strings.Contains(got, "envio de mensagens") {
		t.Errorf("body = %q, want PT-BR explanation", got)
	}
}

func TestMiddleware_OutboundSuspended_AllowsReadsAndInbound(t *testing.T) {
	mw := newMW(t, dunningmw.Config{
		Lookup:         &fakeLookup{state: billingdunning.StateSuspendedOutbound},
		OutboundRoutes: dunningmw.PrefixOutboundClassifier([]string{"/api/send"}),
	})
	cases := []struct {
		method, path string
	}{
		{"GET", "/api/send/msg"},       // read on outbound prefix — still allowed (only blocks classifier match)
		{"GET", "/api/inbox"},          // unrelated read
		{"POST", "/api/inbox/webhook"}, // inbound POST not in outbound list
	}
	for _, tc := range cases {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := withTenant(httptest.NewRequest(tc.method, tc.path, nil))
			mw.Wrap(okHandler()).ServeHTTP(rec, req)
			// classifier matches /api/send regardless of method; but
			// only the "outbound classifier" returns true for it.
			// For GET on the outbound path, the classifier still
			// matches and the gate triggers — that's the strict rule.
			// We assert behaviour split: GET inbox passes; POST
			// webhook passes; GET /api/send/msg is GATED because the
			// classifier matched.
			if tc.path == "/api/send/msg" {
				if rec.Code != http.StatusForbidden {
					t.Errorf("status = %d, want 403 (outbound prefix gates regardless of method)", rec.Code)
				}
				return
			}
			if rec.Code != http.StatusOK {
				t.Errorf("status = %d, want 200", rec.Code)
			}
		})
	}
}

func TestMiddleware_FullSuspended_BlocksWritesAllowsReads(t *testing.T) {
	mw := newMW(t, dunningmw.Config{
		Lookup: &fakeLookup{state: billingdunning.StateSuspendedFull},
	})
	cases := []struct {
		method   string
		expected int
	}{
		{"GET", http.StatusOK},
		{"HEAD", http.StatusOK},
		{"OPTIONS", http.StatusOK},
		{"POST", http.StatusForbidden},
		{"PUT", http.StatusForbidden},
		{"PATCH", http.StatusForbidden},
		{"DELETE", http.StatusForbidden},
	}
	for _, tc := range cases {
		t.Run(tc.method, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := withTenant(httptest.NewRequest(tc.method, "/api/x", nil))
			mw.Wrap(okHandler()).ServeHTTP(rec, req)
			if rec.Code != tc.expected {
				t.Errorf("status = %d, want %d", rec.Code, tc.expected)
			}
			if tc.expected == http.StatusForbidden {
				if !strings.Contains(rec.Body.String(), "30 dias") {
					t.Errorf("body missing PT-BR explanation: %q", rec.Body.String())
				}
			}
		})
	}
}

func TestMiddleware_Cancelled_BlocksWritesAllowsReads(t *testing.T) {
	mw := newMW(t, dunningmw.Config{Lookup: &fakeLookup{state: billingdunning.StateCancelled}})
	rec := httptest.NewRecorder()
	req := withTenant(httptest.NewRequest("GET", "/api/x", nil))
	mw.Wrap(okHandler()).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("GET on cancelled: status = %d, want 200", rec.Code)
	}
	rec = httptest.NewRecorder()
	req = withTenant(httptest.NewRequest("POST", "/api/x", nil))
	mw.Wrap(okHandler()).ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("POST on cancelled: status = %d, want 403", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "cancelada") {
		t.Errorf("body = %q, want cancelada explanation", rec.Body.String())
	}
}

func TestMiddleware_LookupError_FailClosedByDefault(t *testing.T) {
	mw := newMW(t, dunningmw.Config{
		Lookup: &fakeLookup{err: errors.New("db down")},
	})
	rec := httptest.NewRecorder()
	req := withTenant(httptest.NewRequest("POST", "/api/x", nil))
	mw.Wrap(okHandler()).ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", rec.Code)
	}
}

func TestMiddleware_LookupError_FailOpen(t *testing.T) {
	mw := newMW(t, dunningmw.Config{
		Lookup:   &fakeLookup{err: errors.New("db down")},
		FailOpen: true,
	})
	rec := httptest.NewRecorder()
	req := withTenant(httptest.NewRequest("POST", "/api/x", nil))
	mw.Wrap(okHandler()).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 (fail-open)", rec.Code)
	}
}

func TestMiddleware_HTMXFragmentOnDeny(t *testing.T) {
	mw := newMW(t, dunningmw.Config{Lookup: &fakeLookup{state: billingdunning.StateSuspendedFull}})
	rec := httptest.NewRecorder()
	req := withTenant(httptest.NewRequest("POST", "/api/x", nil))
	req.Header.Set("HX-Request", "true")
	mw.Wrap(okHandler()).ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("content-type = %q, want text/html", ct)
	}
	if !strings.Contains(rec.Body.String(), `class="dunning-blocked"`) {
		t.Errorf("body missing HTMX wrapper: %q", rec.Body.String())
	}
}

func TestPrefixOutboundClassifier(t *testing.T) {
	cls := dunningmw.PrefixOutboundClassifier([]string{"/api/send", "automations"})
	for _, path := range []string{"/api/send/x", "/api/send", "/automations/run"} {
		r := httptest.NewRequest("POST", path, nil)
		if !cls(r) {
			t.Errorf("path %q not classified as outbound", path)
		}
	}
	for _, path := range []string{"/api/inbox", "/health"} {
		r := httptest.NewRequest("POST", path, nil)
		if cls(r) {
			t.Errorf("path %q misclassified as outbound", path)
		}
	}
	if dunningmw.PrefixOutboundClassifier(nil)(httptest.NewRequest("POST", "/x", nil)) {
		t.Error("nil prefixes should classify nothing as outbound")
	}
	if dunningmw.PrefixOutboundClassifier([]string{"", " "})(httptest.NewRequest("POST", "/x", nil)) {
		t.Error("empty prefixes should classify nothing as outbound")
	}
}
