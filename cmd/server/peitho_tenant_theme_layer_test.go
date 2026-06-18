package main

// SIN-65115 — regression guard for the tenant-theme cascade.
//
// app-shell injects the per-tenant palette as an inline
// `<style id="tenant-theme">:root{--color-primary:…}</style>` in <head>
// BEFORE the /static/css/tokens.css <link> (internal/web/shell/layout.html
// L7-8). tokens.css also declares `--color-primary` (and the rest of the
// brand tokens) at :root — at EQUAL specificity. By plain source-order
// cascade the later tokens.css default would WIN and shadow the tenant
// palette app-wide for everything reading var(--color-primary).
//
// The fix wraps tokens.css' default token blocks (light :root + dark
// [data-theme="dark"]) in `@layer tokens`. Unlayered normal declarations
// always beat layered ones regardless of source order, so the unlayered
// inline tenant override wins. This guard pins three invariants so the
// cascade can't silently regress:
//   1. tokens.css declares `@layer tokens` and both brand-token blocks
//      (:root + [data-theme="dark"]) live INSIDE it.
//   2. The inline tenant `<style id="tenant-theme">` in layout.html is NOT
//      wrapped in a cascade layer (it must stay unlayered to win).
//   3. The inline tenant style still precedes the tokens.css <link> (the
//      head order the layer fix is written against).

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func TestTokensCSS_BrandTokensAreLayered(t *testing.T) {
	t.Parallel()

	mux := http.NewServeMux()
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServer(http.Dir("../../web/static"))))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/static/css/tokens.css", nil)
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 — tokens.css missing on disk", rec.Code)
	}
	body := rec.Body.String()

	open := strings.Index(body, "@layer tokens {")
	if open < 0 {
		t.Fatal("tokens.css must wrap its default token blocks in `@layer tokens {` so the unlayered inline tenant override wins (SIN-65115)")
	}
	close := strings.Index(body, "} /* @layer tokens */")
	if close < 0 {
		t.Fatal("tokens.css `@layer tokens` block must be closed with the `} /* @layer tokens */` marker")
	}
	if close < open {
		t.Fatalf("`@layer tokens` close marker (%d) precedes its open (%d)", close, open)
	}

	// Both brand-token-defining blocks must fall INSIDE the layer, else the
	// tenant override is still shadowed (in light or dark mode respectively).
	for _, sel := range []string{":root {", `[data-theme="dark"] {`} {
		idx := strings.Index(body, sel)
		if idx < 0 {
			t.Fatalf("tokens.css missing %q block entirely", sel)
		}
		if idx < open || idx > close {
			t.Errorf("%q block must be inside `@layer tokens` (open=%d close=%d, found=%d) so the tenant --color-primary override wins", sel, open, close, idx)
		}
	}

	// The dark re-bind redeclares --color-primary; it must be layered too,
	// otherwise the tenant palette is shadowed in dark mode.
	darkPrimary := strings.Index(body, "--color-primary: #6970dd")
	if darkPrimary >= 0 && (darkPrimary < open || darkPrimary > close) {
		t.Errorf("dark-mode --color-primary default must be inside `@layer tokens` (found=%d, layer %d..%d)", darkPrimary, open, close)
	}

	// The page-default element rules (html/body/a/…) and the reduced-motion
	// override must stay OUTSIDE the layer — they are not brand tokens and
	// don't compete with the tenant override.
	if html := strings.Index(body, "\nhtml {"); html >= 0 && html < close {
		t.Errorf("`html {` document defaults must stay outside `@layer tokens` (found=%d, layer closes at %d)", html, close)
	}
}

func TestLayout_TenantThemeStyleStaysUnlayered(t *testing.T) {
	t.Parallel()

	raw, err := os.ReadFile("../../internal/web/shell/layout.html")
	if err != nil {
		t.Fatalf("read layout.html: %v", err)
	}
	html := string(raw)

	styleIdx := strings.Index(html, `<style id="tenant-theme"`)
	if styleIdx < 0 {
		t.Fatal("layout.html must inject the inline `<style id=\"tenant-theme\">` tenant override")
	}
	linkIdx := strings.Index(html, `href="/static/css/tokens.css"`)
	if linkIdx < 0 {
		t.Fatal("layout.html must link /static/css/tokens.css")
	}

	// Head order the layer fix is written against: the unlayered inline
	// tenant <style> lands BEFORE the tokens.css <link>. (It only wins
	// because tokens.css' defaults are layered — see tokens.css guard.)
	if styleIdx > linkIdx {
		t.Errorf("inline tenant <style> (%d) must precede the tokens.css <link> (%d)", styleIdx, linkIdx)
	}

	// The inline tenant override must NOT be wrapped in a cascade layer —
	// unlayered is exactly what lets it beat the layered tokens defaults.
	if strings.Contains(html, "@layer") {
		t.Error("layout.html must not introduce an `@layer` around the tenant theme — the inline override must stay unlayered to win the cascade (SIN-65115)")
	}
}
