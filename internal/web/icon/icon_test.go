package icon_test

import (
	"html/template"
	"strings"
	"testing"

	"github.com/pericles-luz/crm/internal/web/icon"
)

func TestSVG_KnownIconStrokeDefaults(t *testing.T) {
	got := string(icon.SVG("search", 16))

	for _, want := range []string{
		`<svg class="peitho-icon"`,
		`width="16"`,
		`height="16"`,
		`viewBox="0 0 24 24"`,
		`fill="none"`,
		`stroke="currentColor"`,
		`stroke-width="2"`,
		`stroke-linecap="round"`,
		`stroke-linejoin="round"`,
		`aria-hidden="true"`,
		`<circle cx="11" cy="11" r="8"/>`,
		`</svg>`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("SVG(search) missing %q\ngot: %s", want, got)
		}
	}
}

func TestSVG_CSPSafeNoStyleOrScript(t *testing.T) {
	// CSP ships style-src/script-src without 'unsafe-inline'. The icon
	// must never emit an inline style= attribute, a <script>, or an
	// on*= handler, or it would silently break / get blocked.
	for _, name := range icon.Names() {
		out := strings.ToLower(string(icon.SVG(name, 20)))
		for _, banned := range []string{"style=", "<script", "onload", "onclick", "javascript:", "<use", "href="} {
			if strings.Contains(out, banned) {
				t.Errorf("icon %q emitted banned token %q: %s", name, banned, out)
			}
		}
	}
}

func TestSVG_FilledIconUsesFill(t *testing.T) {
	got := string(icon.SVG("circle", 24))
	if !strings.Contains(got, `fill="currentColor"`) || !strings.Contains(got, `stroke="none"`) {
		t.Errorf("filled icon circle should fill, not stroke; got: %s", got)
	}
}

func TestSVG_UnknownIconIsEmpty(t *testing.T) {
	if got := icon.SVG("does-not-exist", 16); got != "" {
		t.Errorf("unknown icon should render empty, got %q", got)
	}
}

func TestSVG_NonPositiveSizeFallsBackToDefault(t *testing.T) {
	for _, size := range []int{0, -5} {
		got := string(icon.SVG("plus", size))
		if !strings.Contains(got, `width="16"`) || !strings.Contains(got, `height="16"`) {
			t.Errorf("size %d should fall back to 16px, got: %s", size, got)
		}
	}
}

func TestSVG_CustomSize(t *testing.T) {
	got := string(icon.SVG("plus", 28))
	if !strings.Contains(got, `width="28"`) || !strings.Contains(got, `height="28"`) {
		t.Errorf("custom size 28 not applied, got: %s", got)
	}
}

func TestHas(t *testing.T) {
	if !icon.Has("octagon-alert") {
		t.Error("octagon-alert should be a known icon (used by impersonation banner)")
	}
	if icon.Has("totally-made-up") {
		t.Error("unknown name should report Has=false")
	}
}

func TestNames_SortedAndNonEmpty(t *testing.T) {
	names := icon.Names()
	if len(names) == 0 {
		t.Fatal("Names() returned no icons")
	}
	for i := 1; i < len(names); i++ {
		if names[i-1] > names[i] {
			t.Errorf("Names() not sorted at index %d: %q > %q", i, names[i-1], names[i])
		}
	}
	// every advertised name must render.
	for _, n := range names {
		if icon.SVG(n, 16) == "" {
			t.Errorf("advertised icon %q rendered empty", n)
		}
	}
}

func TestFuncMap_DefaultAndExplicitSize(t *testing.T) {
	tmpl := template.Must(
		template.New("t").Funcs(icon.FuncMap()).Parse(
			`{{icon "search"}}|{{icon "search" 24}}`,
		),
	)
	var b strings.Builder
	if err := tmpl.Execute(&b, nil); err != nil {
		t.Fatalf("execute: %v", err)
	}
	out := b.String()
	parts := strings.SplitN(out, "|", 2)
	if len(parts) != 2 {
		t.Fatalf("expected two renders, got %q", out)
	}
	if !strings.Contains(parts[0], `width="16"`) {
		t.Errorf("default size render should be 16px, got %q", parts[0])
	}
	if !strings.Contains(parts[1], `width="24"`) {
		t.Errorf("explicit size render should be 24px, got %q", parts[1])
	}
}

func TestFuncMap_UnknownRendersEmptyInTemplate(t *testing.T) {
	tmpl := template.Must(
		template.New("t").Funcs(icon.FuncMap()).Parse(`[{{icon "nope"}}]`),
	)
	var b strings.Builder
	if err := tmpl.Execute(&b, nil); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if got := b.String(); got != "[]" {
		t.Errorf("unknown icon should render empty in template, got %q", got)
	}
}
