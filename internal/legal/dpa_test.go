package legal

import (
	"strings"
	"testing"
)

// TestDPAVersion_Semver ensures DPAVersion stays in MAJOR.MINOR.PATCH
// form so the 30-day notice automation (and humans reading the page)
// can compare versions deterministically.
func TestDPAVersion_Semver(t *testing.T) {
	parts := strings.Split(DPAVersion, ".")
	if len(parts) != 3 {
		t.Fatalf("DPAVersion %q: expected 3 dot-separated parts, got %d", DPAVersion, len(parts))
	}
	for i, p := range parts {
		if p == "" {
			t.Fatalf("DPAVersion %q: part %d is empty", DPAVersion, i)
		}
		for _, r := range p {
			if r < '0' || r > '9' {
				t.Fatalf("DPAVersion %q: part %d contains non-digit %q", DPAVersion, i, r)
			}
		}
	}
}

// TestDPAMarkdown_MentionsOpenRouter is the AC #2 invariant from
// SIN-62354: the DPA shipped to tenants must explicitly call out
// OpenRouter as a sub-processor. Decisão #8 (SIN-62203) makes this a
// release-blocking requirement.
func TestDPAMarkdown_MentionsOpenRouter(t *testing.T) {
	if !DPAMentionsOpenRouter() {
		t.Fatalf("DPA markdown missing required mention of \"OpenRouter\"")
	}
	if !strings.Contains(DPAMarkdown(), "openrouter.ai/privacy") {
		t.Fatalf("DPA markdown missing required link to OpenRouter privacy policy")
	}
}

// TestDPAMarkdown_MentionsPurposeAndData ensures the two LGPD-required
// fields are present in the OpenRouter section: finalidade and dados
// tratados. The literal Portuguese strings are the ones that appear in
// the DPA template; if someone reorganises the document, this test
// will surface the loss before merge.
func TestDPAMarkdown_MentionsPurposeAndData(t *testing.T) {
	md := DPAMarkdown()
	for _, want := range []string{
		"Resumir conversa e sugerir argumentação de venda",
		"PII estruturada mascarada",
	} {
		if !strings.Contains(md, want) {
			t.Errorf("DPA markdown missing required phrase %q", want)
		}
	}
}

// TestSubprocessors_Names asserts the canonical sub-processor list
// shape. A silent edit that drops OpenRouter or Meta would fail this
// test — that is the intended early-warning so a PR that breaks the
// LGPD disclosure is impossible to land green.
func TestSubprocessors_Names(t *testing.T) {
	list := Subprocessors()
	if len(list) != 4 {
		t.Fatalf("expected exactly 4 sub-processors (AI, messaging, email, payments), got %d", len(list))
	}

	byKind := map[SubprocessorKind]Subprocessor{}
	for _, s := range list {
		if _, dup := byKind[s.Kind]; dup {
			t.Errorf("duplicate kind %q in sub-processor list", s.Kind)
		}
		byKind[s.Kind] = s
	}

	for _, want := range []SubprocessorKind{KindAI, KindMessaging, KindEmail, KindPayment} {
		if _, ok := byKind[want]; !ok {
			t.Errorf("missing sub-processor of kind %q", want)
		}
	}

	openrouter := byKind[KindAI]
	if !strings.Contains(openrouter.Name, "OpenRouter") {
		t.Errorf("AI sub-processor expected to be OpenRouter, got %q", openrouter.Name)
	}
	if openrouter.Status != "active" {
		t.Errorf("OpenRouter expected status=active, got %q", openrouter.Status)
	}
	if openrouter.PolicyURL == "" {
		t.Errorf("OpenRouter expected non-empty PolicyURL")
	}
	if !strings.Contains(openrouter.DataHandled, "mascarada") {
		t.Errorf("OpenRouter DataHandled must mention masking (ADR-0041 invariant), got %q", openrouter.DataHandled)
	}

	pix := byKind[KindPayment]
	if pix.Status != "pending" {
		t.Errorf("PIX PSP expected status=pending until decisão D2, got %q", pix.Status)
	}
}

// TestSubprocessors_ReturnsCopy guards against an accidental mutation
// of the package-level truth via the returned slice. A handler that
// sorts or filters the result MUST NOT corrupt the source.
func TestSubprocessors_ReturnsCopy(t *testing.T) {
	a := Subprocessors()
	if len(a) == 0 {
		t.Fatal("Subprocessors() returned empty slice")
	}
	original := a[0].Name
	a[0].Name = "TAMPERED"
	b := Subprocessors()
	if b[0].Name != original {
		t.Fatalf("Subprocessors() returned aliased slice — mutation leaked back to source. got %q want %q", b[0].Name, original)
	}
}

// TestDPAFilename_VersionEmbedded ensures the filename suffix carries
// the version so a tenant can keep historical copies on disk.
func TestDPAFilename_VersionEmbedded(t *testing.T) {
	got := DPAFilename()
	if !strings.Contains(got, DPAVersion) {
		t.Errorf("DPAFilename %q must contain DPAVersion %q", got, DPAVersion)
	}
	if !strings.HasSuffix(got, ".md") {
		t.Errorf("DPAFilename %q must end with .md", got)
	}
}

// TestDPAContentType is the RFC 7763 media type for markdown. A silent
// switch to text/plain would break the browser's "save as .md" UX.
func TestDPAContentType(t *testing.T) {
	if !strings.HasPrefix(DPAContentType, "text/markdown") {
		t.Fatalf("DPAContentType expected text/markdown, got %q", DPAContentType)
	}
	if !strings.Contains(DPAContentType, "charset=utf-8") {
		t.Errorf("DPAContentType should set charset=utf-8, got %q", DPAContentType)
	}
}
