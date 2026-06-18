package main

// SIN-65123 (Peitho · Tranche-D) — regression guard for the billing
// invoices copy-to-clipboard script.
//
// internal/web/billing/invoices/templates.go references
// /static/js/billing-invoices.js; the file did NOT exist on disk, so the
// <script> 404'd and the PIX copia-e-cola button silently no-op'd at
// runtime without breaking a single Go test. Parallel to
// TestAppShellToggleScript_ServedAsJS (design_system_js_static_test.go).

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestBillingInvoicesScript_ServedAsJS(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServer(http.Dir("../../web/static"))))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/static/js/billing-invoices.js", nil)
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 — /static/js/billing-invoices.js missing on disk", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); !strings.Contains(got, "javascript") {
		t.Errorf("Content-Type = %q, want it to contain %q", got, "javascript")
	}

	body := rec.Body.String()
	if len(body) < 200 {
		t.Fatalf("body too short (%d bytes) — billing-invoices.js stub or empty?", len(body))
	}

	// Pin one needle per behaviour the script must wire so a future
	// refactor that drops the copy button / Clipboard API / event
	// delegation fails this guard rather than silently breaking the PIX
	// copia-e-cola affordance under strict CSP.
	for _, needle := range []string{
		"data-copy-target",
		"clipboard",
		"addEventListener",
		"is-copied",
	} {
		if !strings.Contains(body, needle) {
			t.Errorf("billing-invoices.js missing %q — copy wiring incomplete", needle)
		}
	}
}
