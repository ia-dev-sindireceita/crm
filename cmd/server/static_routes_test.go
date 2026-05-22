package main

// SIN-63303 — boot-level regression guard for the /static/ FileServer.
//
// Pre-fix, the /static/ FileServer was registered inside
// registerCustomDomainRoutes (customdomain_wire.go), which is only
// constructed when CUSTOM_DOMAIN_UI_ENABLED=1. Staging did not set
// the flag, so every tenant template referencing /static/css/* and
// /static/vendor/htmx/* silently 404'd for weeks (e.g.
// /static/css/auth.css from SIN-63294, /static/css/privacy.css from
// SIN-62916). The unit tests that existed before this fix all mounted
// the FileServer directly in their own setup (auth_css_static_test.go,
// privacy_wire_test.go's TestPrivacyStylesheet_ServedAsCSS), so they
// validated the bytes on disk but never exercised the production
// wire-up that actually serves the route.
//
// This test boots the real runWith path on a free port with NO env
// overrides (DATABASE_URL unset, CUSTOM_DOMAIN_UI_ENABLED unset,
// IAM disabled — fail-soft contract documented in
// customdomain_wire.go) and asserts /static/css/auth.css responds
// 200 text/css. If a future refactor re-buries the FileServer behind
// a feature flag, this test fails red instead of waiting for a
// staging deploy to surface the regression.

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestRunWith_ServesStaticUnconditionally(t *testing.T) {
	// cmd/server's runWith uses http.Dir("web/static"), which is
	// relative to the binary's cwd. The production binary runs from
	// /app where the Dockerfile copies web/static; tests run from
	// cmd/server, so we chdir to the repo root before booting.
	// t.Chdir auto-restores after the test. This test cannot run in
	// parallel because t.Chdir mutates process-global state.
	t.Chdir("../..")

	addr := freePort(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	getenv := func(k string) string {
		if k == "HTTP_ADDR" {
			return addr
		}
		// Intentionally return "" for everything else — the
		// regression we are guarding against is the case where
		// CUSTOM_DOMAIN_UI_ENABLED is NOT set and the FileServer
		// must still serve /static/ for the tenant pages
		// (privacy, funnel, campaigns, layout/auth, etc.).
		return ""
	}

	errCh := make(chan error, 1)
	go func() {
		errCh <- runWith(ctx, addr, getenv, defaultWebhookDial)
	}()

	waitForListening(t, addr)
	defer func() {
		cancel()
		select {
		case err := <-errCh:
			if err != nil {
				t.Errorf("runWith returned error: %v", err)
			}
		case <-time.After(10 * time.Second):
			t.Error("runWith did not return after cancel")
		}
	}()

	res, err := http.Get("http://" + addr + "/static/css/auth.css")
	if err != nil {
		t.Fatalf("GET /static/css/auth.css: %v", err)
	}
	defer res.Body.Close()

	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 — /static/ must be mounted on the public listener even when CUSTOM_DOMAIN_UI_ENABLED is unset", res.StatusCode)
	}
	if got := res.Header.Get("Content-Type"); !strings.Contains(got, "text/css") {
		t.Errorf("Content-Type = %q, want it to contain %q", got, "text/css")
	}
	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if len(body) == 0 {
		t.Fatal("served body is empty — auth.css must have rules")
	}
}

// TestRunWith_StaticServesVendorAssets pins the second leg of the
// SIN-63303 root cause: /static/vendor/* also 404'd in staging,
// proving the failure was route-level (no FileServer wired at all)
// rather than bytes-level (vendor has always been in the image).
// CHECKSUMS.txt is the smallest reliable vendor file and ships at
// the root of web/static/vendor/.
func TestRunWith_StaticServesVendorAssets(t *testing.T) {
	t.Chdir("../..")

	addr := freePort(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	getenv := func(k string) string {
		if k == "HTTP_ADDR" {
			return addr
		}
		return ""
	}

	errCh := make(chan error, 1)
	go func() {
		errCh <- runWith(ctx, addr, getenv, defaultWebhookDial)
	}()

	waitForListening(t, addr)
	defer func() {
		cancel()
		select {
		case err := <-errCh:
			if err != nil {
				t.Errorf("runWith returned error: %v", err)
			}
		case <-time.After(10 * time.Second):
			t.Error("runWith did not return after cancel")
		}
	}()

	res, err := http.Get("http://" + addr + "/static/vendor/CHECKSUMS.txt")
	if err != nil {
		t.Fatalf("GET /static/vendor/CHECKSUMS.txt: %v", err)
	}
	defer res.Body.Close()

	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 — /static/vendor/* must be reachable on the public listener", res.StatusCode)
	}
}
