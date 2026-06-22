package main

// SIN-65158 — inbox compose-button contrast guard. In the dark theme the
// shared --color-primary (#6970dd) gives white-on-fill only 4.23:1, below
// WCAG AA 4.5:1 for the 13px "Enviar" label, and the hover --color-accent
// (#7c84e8) is lighter still. inbox.css pins the dark compose Send button
// to AA-clean brand indigo (base #5b63d3 = 5.0:1, hover #4a51c0 = 6.5:1).
// This test fails if that scoped override is dropped before the global
// dark-primary re-bind lands in the Pitho AA tokens track (SIN-65116).

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestInboxStylesheet_DarkComposeSubmitMeetsAA(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServer(http.Dir("../../web/static"))))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/static/css/inbox.css", nil)
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 — web/static/css/inbox.css must exist", rec.Code)
	}
	body := rec.Body.String()

	// The dark override must target the compose Send button and pin an
	// AA-clean fill. We assert both the scoped selectors and the two
	// known-passing swatches so a value drift back toward the failing
	// #6970dd / #7c84e8 pair is caught.
	for _, needle := range []string{
		`[data-theme="dark"] .conversation__compose-submit {`,
		`[data-theme="dark"] .conversation__compose-submit:hover {`,
		"#5b63d3", // base: 5.0:1 on white
		"#4a51c0", // hover: 6.5:1 on white
	} {
		if !strings.Contains(body, needle) {
			t.Errorf("inbox.css missing dark compose-submit AA fix fragment %q", needle)
		}
	}
}
