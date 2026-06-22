package main

// SIN-65123 (Pitho · Tranche-D) — regression guard for the billing
// invoices stylesheet.
//
// internal/web/billing/invoices/templates.go references
// /static/css/billing-invoices.css (alongside tokens.css + components.css).
// The file did NOT exist on disk, so the <link> 404'd silently and both the
// invoice list and detail pages rendered with user-agent defaults — the
// "tela sem formatação" failure mode the Pitho sweep exists to prevent.
// Spinning up the same FileServer setup main.go mounts in production proves
// the asset exists and is served as text/css through the static handler.
//
// Parallel to TestDashboardStylesheet_ServedAsCSS (dashboard_css_static_test.go).

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestBillingInvoicesStylesheet_ServedAsCSS(t *testing.T) {
	t.Parallel()
	// cmd/server lives two levels below the repo root, so web/static is
	// at ../../web/static when go test runs from the package directory.
	mux := http.NewServeMux()
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServer(http.Dir("../../web/static"))))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/static/css/billing-invoices.css", nil)
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 — web/static/css/billing-invoices.css must exist", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); !strings.Contains(got, "text/css") {
		t.Errorf("Content-Type = %q, want it to contain %q", got, "text/css")
	}
	body := rec.Body.String()
	if len(body) == 0 {
		t.Fatal("served body is empty — billing-invoices.css must have rules")
	}
	// Spot-check the load-bearing selectors and the tokens-only contract so
	// a future refactor that drops the Pitho port — or hard-codes a raw
	// colour — regresses here instead of in staging. Each needle gates a
	// distinct concern: page container, list table, status pill, dunning
	// banner, tabular numerals on the R$ column (AC #3), and token usage.
	for _, needle := range []string{
		".invoices-shell",
		".invoices-table",
		".invoice-status--paid",
		".dunning-banner",
		"tabular-nums",
		"var(--surface-1",
		"var(--text-muted",
	} {
		if !strings.Contains(body, needle) {
			t.Errorf("billing-invoices.css missing required selector or token reference %q", needle)
		}
	}
}
