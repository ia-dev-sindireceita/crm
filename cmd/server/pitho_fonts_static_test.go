package main

// SIN-65087 / Pitho A2 — regression guard for the self-hosted webfonts.
//
// tokens.css wires Inter + JetBrains Mono via @font-face pointing at
// /static/fonts/*.woff2 (NO Google CDN, so they load under the strict CSP
// `font-src 'self'` directive and work offline). If any woff2 goes missing
// on disk the @font-face 404s silently and the UI falls back to system
// fonts; if the tokens.css wiring is dropped the self-hosted files never
// load. This pins both halves so a future refactor fails here, not in
// staging. Parallel to TestDesignSystemStylesheets_ServedAsCSS.

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPithoFonts_ServedAndWired(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServer(http.Dir("../../web/static"))))

	// 1. tokens.css must wire the self-hosted families + @font-face srcs,
	//    and must NOT reach out to a font CDN.
	t.Run("tokens.css wires self-hosted fonts", func(t *testing.T) {
		t.Parallel()
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/static/css/tokens.css", nil)
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
		body := rec.Body.String()
		needles := []string{
			"@font-face",
			`font-family: "Inter"`,
			`font-family: "JetBrains Mono"`,
			"/static/fonts/inter-latin-wght-normal.woff2",
			"/static/fonts/jetbrains-mono-latin-wght-normal.woff2",
			"font-display: swap",
		}
		for _, n := range needles {
			if !strings.Contains(body, n) {
				t.Errorf("tokens.css missing %q — self-hosted font wiring", n)
			}
		}
		// CSP / offline guard: never load fonts from a remote CDN.
		for _, banned := range []string{"fonts.googleapis.com", "fonts.gstatic.com", "@import"} {
			if strings.Contains(body, banned) {
				t.Errorf("tokens.css must not reference %q (self-host only, CSP font-src 'self')", banned)
			}
		}
	})

	// 2. Every referenced woff2 must exist on disk and serve as a font.
	woff2 := []string{
		"/static/fonts/inter-latin-wght-normal.woff2",
		"/static/fonts/inter-latin-ext-wght-normal.woff2",
		"/static/fonts/jetbrains-mono-latin-wght-normal.woff2",
		"/static/fonts/jetbrains-mono-latin-ext-wght-normal.woff2",
	}
	for _, path := range woff2 {
		path := path
		t.Run(path, func(t *testing.T) {
			t.Parallel()
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, path, nil)
			mux.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 — %s missing on disk", rec.Code, path)
			}
			// woff2 magic number "wOF2" (0x774F4632) — proves it's a real
			// font binary, not an HTML error page served as 200.
			if got := rec.Body.String(); !strings.HasPrefix(got, "wOF2") {
				t.Errorf("%s is not a woff2 binary (missing wOF2 magic)", path)
			}
		})
	}
}
