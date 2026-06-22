package main

// SIN-65088 — regression guard for the Pitho brand assets.
//
// The base layouts (internal/web/shell/layout.html and
// internal/adapter/httpapi/views/layout.html) reference the favicon, the
// web manifest and the brand logo SVGs from /static/brand/ and
// /static/. If any file is missing on disk the link/img tags 404
// silently and the chrome loses its favicon/logo. This spins up the same
// FileServer setup customdomain_wire.go mounts in production and proves
// each asset exists and is served with the right Content-Type (see
// static_mime.go for the registration that makes SVG render in <img>).
//
// Mirrors TestAuthStylesheet_ServedAsCSS — both live in cmd/server
// because that is where the static-route wiring lives.

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestBrandAssets_Served(t *testing.T) {
	t.Parallel()

	mux := http.NewServeMux()
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServer(http.Dir("../../web/static"))))

	cases := []struct {
		path        string
		contentType string
	}{
		{"/static/brand/favicon.svg", "image/svg+xml"},
		{"/static/brand/pitho-icon.svg", "image/svg+xml"},
		{"/static/brand/pitho-mark.svg", "image/svg+xml"},
		{"/static/brand/pitho-logo-light.svg", "image/svg+xml"},
		{"/static/brand/pitho-logo-dark.svg", "image/svg+xml"},
		{"/static/site.webmanifest", "application/manifest+json"},
		{"/static/css/brand.css", "text/css"},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.path, func(t *testing.T) {
			t.Parallel()
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, tc.path, nil)
			mux.ServeHTTP(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 — %s must exist on disk", rec.Code, tc.path)
			}
			if got := rec.Header().Get("Content-Type"); !strings.Contains(got, tc.contentType) {
				t.Errorf("Content-Type = %q, want it to contain %q", got, tc.contentType)
			}
			if rec.Body.Len() == 0 {
				t.Fatalf("%s served an empty body", tc.path)
			}
		})
	}
}

func TestWebManifest_CarriesPithoBrand(t *testing.T) {
	t.Parallel()

	// SIN-65589 — the PWA manifest's name/short_name is user-visible: it is
	// the label the browser shows in the install prompt and on the home-screen
	// shortcut. The Peitho → Pitho rename's bulk sed missed this asset, so it
	// kept showing "Peitho" while the rest of the UI read "Pitho". Guard the
	// body here so the next rename can't regress it silently.
	mux := http.NewServeMux()
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServer(http.Dir("../../web/static"))))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/static/site.webmanifest", nil)
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 for /static/site.webmanifest", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"Pitho"`) {
		t.Errorf("manifest body should carry the Pitho brand name; got:\n%s", body)
	}
	if strings.Contains(body, "Peitho") {
		t.Errorf("manifest body still contains the stale Peitho brand name; got:\n%s", body)
	}
}

func TestBrandLogos_HaveThemeVariants(t *testing.T) {
	t.Parallel()

	mux := http.NewServeMux()
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServer(http.Dir("../../web/static"))))

	// The light/dark logos must differ — a copy-paste that points both
	// theme variants at the same artwork would defeat the [data-theme]
	// switch. Their accent rects use distinct fills (#5B63D3 vs #6970DD).
	get := func(path string) string {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d for %s", rec.Code, path)
		}
		return rec.Body.String()
	}

	light := get("/static/brand/pitho-logo-light.svg")
	dark := get("/static/brand/pitho-logo-dark.svg")
	if light == dark {
		t.Fatal("light and dark logos are identical — theme switch is a no-op")
	}
	if !strings.Contains(light, "Pitho") || !strings.Contains(dark, "Pitho") {
		t.Error("brand logos should carry the Pitho wordmark text")
	}
}
