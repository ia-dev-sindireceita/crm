package inbox

// Render tests for the [SIN-65118] Peitho C1 emoji→{{icon}} sweep. The
// inbox message bubble previously embedded literal emoji (⛔ 📎 … for
// media chrome; ✓ ✓✓ ⚠ ⏱ for outbound delivery status). These tests
// assert the bubble now renders the Peitho inline-SVG Lucide helper
// instead, keeps the accessible text/aria-label for the decorative
// icons, and that no emoji codepoint leaks into the rendered chrome.

import (
	"strings"
	"testing"

	inboxusecase "github.com/pericles-luz/crm/internal/inbox/usecase"
)

// inboxChromeEmoji are the literal emoji the sweep removed; none may
// reappear in any rendered message bubble.
var inboxChromeEmoji = []string{"⛔", "📎", "✓", "⚠", "⏱"}

func assertNoChromeEmoji(t *testing.T, got string) {
	t.Helper()
	for _, e := range inboxChromeEmoji {
		if strings.Contains(got, e) {
			t.Errorf("rendered bubble still contains emoji %q; got: %s", e, got)
		}
	}
}

func baseOutbound(status string) inboxusecase.MessageView {
	m := baseInbound()
	m.Direction = "out"
	m.Status = status
	return m
}

func TestMessageBubble_MediaIcons_UsePeithoSVG(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		scanStatus string
		// wantPath is a fragment of the expected Lucide glyph geometry so
		// the test fails if the icon mapping silently changes.
		wantPath string
		wantText string
	}{
		{"infected", "infected", `d="M12 16h.01"`, "Conteúdo bloqueado por segurança"},
		{"clean", "clean", `d="m21.44 11.05`, "Anexo"},
		{"pending", "pending", `<circle cx="12" cy="12" r="10"/>`, "Verificando anexo"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			msg := baseInbound()
			msg.Media = &inboxusecase.MessageMediaView{ScanStatus: tc.scanStatus}
			if tc.scanStatus == "clean" {
				msg.Media.Hash = "abcd1234abcd1234abcd1234abcd1234abcd1234abcd1234abcd1234abcd1234"
				msg.Media.Format = "png"
			}
			got := renderBubble(t, msg)

			if !strings.Contains(got, `class="peitho-icon"`) {
				t.Errorf("media icon should render a Peitho SVG; got: %s", got)
			}
			if !strings.Contains(got, tc.wantPath) {
				t.Errorf("media icon missing expected glyph %q; got: %s", tc.wantPath, got)
			}
			// Decorative icon must keep its accessible text label.
			if !strings.Contains(got, tc.wantText) {
				t.Errorf("media block lost its accessible text %q; got: %s", tc.wantText, got)
			}
			assertNoChromeEmoji(t, got)
		})
	}
}

func TestMessageBubble_StatusBadge_UsesPeithoSVG(t *testing.T) {
	t.Parallel()
	cases := []struct {
		status   string
		wantPath string // fragment of the mapped Lucide glyph
		label    string
	}{
		{"pending", `<circle cx="12" cy="12" r="10"/>`, "Aguardando envio"},
		{"sent", `d="M20 6 9 17l-5-5"`, "Enviada"},
		{"delivered", `d="m22 10-7.5 7.5L13 16"`, "Entregue"},
		{"read", `d="m22 10-7.5 7.5L13 16"`, "Lida"},
		{"failed", `d="m21.73 18-8-14`, "Falha ao enviar"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.status, func(t *testing.T) {
			t.Parallel()
			got := renderBubble(t, baseOutbound(tc.status))

			if !strings.Contains(got, `class="message-bubble__status message-bubble__status--`+tc.status+`"`) {
				t.Errorf("missing status badge for %q; got: %s", tc.status, got)
			}
			if !strings.Contains(got, `class="peitho-icon"`) {
				t.Errorf("status badge should render a Peitho SVG; got: %s", got)
			}
			if !strings.Contains(got, tc.wantPath) {
				t.Errorf("status %q missing expected glyph %q; got: %s", tc.status, tc.wantPath, got)
			}
			// Accessible text the decorative glyph carries is preserved.
			if !strings.Contains(got, `aria-label="`+tc.label+`"`) {
				t.Errorf("status %q lost aria-label %q; got: %s", tc.status, tc.label, got)
			}
			assertNoChromeEmoji(t, got)
		})
	}
}

// SIN-65158: aria-label on a bare <span> is an aria-prohibited-attr
// violation — assistive tech ignores the name, so the delivery status
// ("Lida"/"Enviada"/"Entregue") goes silent. role="img" is a role that
// admits aria-label and matches the decorative SVG glyph, giving the
// badge a valid accessible name. Pin role and label on the same element.
func TestMessageBubble_StatusBadge_HasImgRole(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		status string
		label  string
	}{
		{"pending", "Aguardando envio"},
		{"sent", "Enviada"},
		{"delivered", "Entregue"},
		{"read", "Lida"},
		{"failed", "Falha ao enviar"},
	} {
		tc := tc
		t.Run(tc.status, func(t *testing.T) {
			t.Parallel()
			got := renderBubble(t, baseOutbound(tc.status))
			if !strings.Contains(got, `role="img" aria-label="`+tc.label+`"`) {
				t.Errorf("status %q badge must carry role=\"img\" alongside aria-label %q so the name is not aria-prohibited; got: %s", tc.status, tc.label, got)
			}
		})
	}
}

// Inbound bubbles never carry an outbound delivery badge — the status
// icon mapping must stay outbound-only after the sweep.
func TestMessageBubble_Inbound_HasNoStatusIcon(t *testing.T) {
	t.Parallel()
	got := renderBubble(t, baseInbound())
	if strings.Contains(got, "message-bubble__status") {
		t.Errorf("inbound bubble must not render a status badge; got: %s", got)
	}
	assertNoChromeEmoji(t, got)
}

// statusIcon is the outbound-only mapping; inbound/unknown collapse to
// "no badge" rather than a broken icon.
func TestStatusIcon_OutboundOnlyMapping(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"pending":   "clock",
		"sent":      "check",
		"delivered": "check-check",
		"read":      "check-check",
		"failed":    "triangle-alert",
		"":          "",
		"weird":     "",
	}
	for status, want := range cases {
		if got := statusIcon(status); got != want {
			t.Errorf("statusIcon(%q) = %q, want %q", status, got, want)
		}
	}
}
