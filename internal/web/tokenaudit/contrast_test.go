package tokenaudit_test

// SIN-65116 WCAG AA contrast guard. Tranche-D QA (SIN-65085) measured
// four token-level contrast misses on the Pitho app background:
//
//	A1  --text-muted / --color-secondary (#6b7280) = 4.43:1 on --surface-1
//	A2  --color-success (#268560) used as TEXT       = 4.17:1 on --surface-1
//	A3  --color-primary (#5b63d3) on --color-primary-soft = 4.40:1
//	A4  (dark) --color-primary (#6970dd) on dark --color-primary-soft = 3.71:1
//
// This test parses the real token values out of web/static/css/tokens.css
// (light :root + dark [data-theme="dark"] rebind) and fails if any of the
// rendered foreground/background pairs falls below WCAG 2.1 AA for body
// text (4.5:1). It is the deterministic, browser-free counterpart to the
// axe-core re-run named in the issue AC, and guards against future token
// edits silently regressing contrast.

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/pericles-luz/crm/internal/branding"
)

// aaBody is the WCAG 2.x contrast floor for normal-size text
// (Success Criterion 1.4.3).
const aaBody = branding.WCAGAANormalText // 4.5

// repoRoot walks up from the test's cwd until it finds the module's
// go.mod. The cwd is the package directory when `go test` runs, so the
// walk is short (4 hops). Mirrors internal/web/cspaudit.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := filepath.Abs(".")
	if err != nil {
		t.Fatalf("abs cwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("could not locate repo root from %q", dir)
		}
		dir = parent
	}
}

// tokenPattern matches `--name: #rrggbb` custom-property declarations.
// Non-hex tokens (spacing, motion, rgba focus rings) never match.
var tokenPattern = regexp.MustCompile(`--([a-z0-9-]+):\s*(#[0-9a-fA-F]{6})\b`)

// darkSelector marks the start of the [data-theme="dark"] rebind block.
// The trailing " {" pins it to the real rule — the token-file doc comment
// also mentions the bare `[data-theme="dark"]` string.
const darkSelector = `[data-theme="dark"] {`

// parseTokens splits tokens.css into the light region (everything before
// the dark selector — the single :root block) and the dark region (the
// [data-theme="dark"] block) and returns name->hex maps for each. Last
// declaration wins, matching the CSS cascade within a single selector.
func parseTokens(t *testing.T, css string) (light, dark map[string]string) {
	t.Helper()
	idx := strings.Index(css, darkSelector)
	if idx < 0 {
		t.Fatalf("tokens.css: %q block not found", darkSelector)
	}
	// The dark block runs from the selector to the first line that is
	// exactly "}" (column 0) — the close of the [data-theme="dark"] rule.
	rest := css[idx:]
	end := strings.Index(rest, "\n}")
	if end < 0 {
		t.Fatalf("tokens.css: unterminated %q block", darkSelector)
	}
	lightCSS := css[:idx]
	darkCSS := rest[:end]

	parse := func(region string) map[string]string {
		m := make(map[string]string)
		for _, mt := range tokenPattern.FindAllStringSubmatch(region, -1) {
			m[mt[1]] = strings.ToLower(mt[2])
		}
		return m
	}
	light, dark = parse(lightCSS), parse(darkCSS)
	if len(light) == 0 || len(dark) == 0 {
		t.Fatalf("tokens.css: parsed light=%d dark=%d tokens", len(light), len(dark))
	}
	return light, dark
}

// hexToRGB converts a "#rrggbb" literal to a branding.RGB so we can reuse
// the production WCAG contrast helper.
func hexToRGB(t *testing.T, hex string) branding.RGB {
	t.Helper()
	h := strings.TrimPrefix(hex, "#")
	if len(h) != 6 {
		t.Fatalf("hexToRGB: want #rrggbb, got %q", hex)
	}
	v, err := strconv.ParseUint(h, 16, 32)
	if err != nil {
		t.Fatalf("hexToRGB %q: %v", hex, err)
	}
	return branding.RGB{R: uint8(v >> 16), G: uint8(v >> 8), B: uint8(v)}
}

func loadTokens(t *testing.T) (light, dark map[string]string) {
	t.Helper()
	path := filepath.Join(repoRoot(t), "web", "static", "css", "tokens.css")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read tokens.css: %v", err)
	}
	return parseTokens(t, string(raw))
}

// contrastCase is one foreground-on-background assertion. fg/bg are token
// names resolved against the active theme map.
type contrastCase struct {
	name string
	fg   string
	bg   string
}

func TestTokenContrastLightAA(t *testing.T) {
	light, _ := loadTokens(t)
	cases := []contrastCase{
		// A1 — muted / secondary text on the gray app background and on
		// the slightly darker hover surface.
		{"A1 text-muted on surface-1", "text-muted", "surface-1"},
		{"A1 text-muted on surface-2", "text-muted", "surface-2"},
		{"A1 color-secondary on surface-1", "color-secondary", "surface-1"},
		// A2 — success-colored text variant on app bg + on the won-badge tint.
		{"A2 success-strong on surface-1", "color-success-strong", "surface-1"},
		{"A2 success-strong on success-surface", "color-success-strong", "color-success-surface"},
		// SIN-65127 — warning-colored text variant (inbox waiting badge) on
		// app bg + on the amber waiting-badge tint.
		{"warning-strong on surface-1", "color-warning-strong", "surface-1"},
		{"warning-strong on warning-surface", "color-warning-strong", "color-warning-surface"},
		// A3 — accent text on the primary-soft tint (selected nav / badges).
		{"A3 on-primary-soft on primary-soft", "color-on-primary-soft", "color-primary-soft"},
		// Sanity guards for the existing text/link tokens we rely on.
		{"text-default on surface-1", "text-default", "surface-1"},
		{"text-strong on surface-1", "text-strong", "surface-1"},
		{"color-link on surface-1", "color-link", "surface-1"},
	}
	runContrastCases(t, "light", light, cases)
}

func TestTokenContrastDarkAA(t *testing.T) {
	_, dark := loadTokens(t)
	cases := []contrastCase{
		// A4 — accent text on the dark primary-soft tint.
		{"A4 on-primary-soft on primary-soft (dark)", "color-on-primary-soft", "color-primary-soft"},
		// success-as-text + muted text still clear AA on the dark app bg.
		{"success-strong on surface-1 (dark)", "color-success-strong", "surface-1"},
		{"warning-strong on surface-1 (dark)", "color-warning-strong", "surface-1"},
		{"warning-strong on warning-surface (dark)", "color-warning-strong", "color-warning-surface"},
		{"text-muted on surface-1 (dark)", "text-muted", "surface-1"},
		{"color-link on surface-1 (dark)", "color-link", "surface-1"},
		{"text-default on surface-1 (dark)", "text-default", "surface-1"},
	}
	runContrastCases(t, "dark", dark, cases)
}

func runContrastCases(t *testing.T, theme string, tokens map[string]string, cases []contrastCase) {
	t.Helper()
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fgHex, ok := tokens[c.fg]
			if !ok {
				t.Fatalf("%s theme: token --%s not defined in tokens.css", theme, c.fg)
			}
			bgHex, ok := tokens[c.bg]
			if !ok {
				t.Fatalf("%s theme: token --%s not defined in tokens.css", theme, c.bg)
			}
			ratio := hexToRGB(t, fgHex).Contrast(hexToRGB(t, bgHex))
			if ratio < aaBody {
				t.Errorf("%s: --%s (%s) on --%s (%s) = %.2f:1, want >= %.1f:1 (WCAG AA body)",
					c.name, c.fg, fgHex, c.bg, bgHex, ratio, aaBody)
			}
		})
	}
}
