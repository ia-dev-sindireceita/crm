package master_test

// SIN-65103 / Peitho C8 — the master console chrome must render the
// Peitho inline-SVG {{icon}} helper instead of Unicode emoji, which the
// "no emoji in chrome" rule forbids (emoji are inconsistent across
// platforms and not themable). These tests pin the swap so a future
// edit can't silently reintroduce an emoji glyph.

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/tenancy"
	"github.com/pericles-luz/crm/internal/web/master"
)

// emojiGlyphs are the specific chrome emoji this sweep replaced. None
// may appear in rendered master output.
var emojiGlyphs = []string{"🛑", "⚡", "🔒", "✔", "⚠"}

func assertNoEmoji(t *testing.T, where, rendered string) {
	t.Helper()
	for _, g := range emojiGlyphs {
		if strings.Contains(rendered, g) {
			t.Errorf("%s: emoji %q leaked into rendered chrome (must use {{icon}})", where, g)
		}
	}
}

func TestImpersonationBanner_RendersPeithoIconsNoEmoji(t *testing.T) {
	t.Parallel()
	ctx, _ := bannerCtx(t, "Acme", "acme")
	r := httptest.NewRequest(http.MethodGet, "/master/tenants", nil).WithContext(ctx)
	now := time.Date(2026, 6, 17, 13, 5, 0, 0, time.UTC)
	resolver := &stubTenantsByID{tenant: &tenancy.Tenant{Name: "Acme Saúde Ltda", Host: "acme.crm.local"}}
	bctx := master.BuildImpersonationContext(r, resolver, "tk", func() time.Time { return now })

	rendered := renderTenantsListWithBanner(t, bctx)

	// The impersonation pill (🛑) and audit-feed chip (⚡) now render
	// inline Lucide SVG via the shared icon helper.
	if !strings.Contains(rendered, `class="peitho-icon"`) {
		t.Fatalf("expected inline peitho-icon SVG in banner chrome, got: %q", rendered)
	}
	// octagon-alert geometry (the halt pill) and zap geometry (auditoria)
	// are stamped from the curated icon set.
	if !strings.Contains(rendered, `M15.312 2a2 2 0 0 1 1.414.586`) {
		t.Error("octagon-alert icon missing from impersonation pill")
	}
	if !strings.Contains(rendered, `M4 14a1 1 0 0 1-.78-1.63`) {
		t.Error("zap icon missing from audit-feed chip")
	}
	assertNoEmoji(t, "impersonation banner", rendered)
}

func TestGrantRequestDetail_RendersPeithoIconsNoEmoji(t *testing.T) {
	t.Parallel()
	requester := uuid.MustParse("77777777-7777-7777-7777-777777777777")
	other := uuid.MustParse("88888888-8888-8888-8888-888888888888")
	req := master.GrantRequest{
		ID:          uuid.New(),
		CreatedByID: requester,
		State:       master.GrantRequestStateAwaiting,
		Kind:        master.GrantKindExtraTokens,
		Amount:      25_000_000,
		Reason:      "peitho icon sweep",
		CreatedAt:   time.Now().UTC(),
	}

	// A different reviewer: requester sigil (🔒) + reviewer sigil (✔)
	// both render as icons; no self-guard branch.
	rendered := renderGrantRequestDetailPanel(t, req, other, "")
	if !strings.Contains(rendered, `class="peitho-icon"`) {
		t.Fatalf("expected peitho-icon SVG on the 4-eyes sigils, got: %q", rendered)
	}
	if !strings.Contains(rendered, `M7 11V7a5 5 0 0 1 10 0v4`) {
		t.Error("lock icon missing from SOLICITANTE sigil")
	}
	if !strings.Contains(rendered, `M20 6 9 17l-5-5`) {
		t.Error("check icon missing from REVISOR sigil")
	}
	assertNoEmoji(t, "grant request detail (reviewer)", rendered)

	// Self-creator branch: the ⚠ self-approve guard now renders an
	// octagon-alert icon. The 4-eyes guard text + data attr stay intact.
	selfRendered := renderGrantRequestDetailPanel(t, req, requester, "")
	if !strings.Contains(selfRendered, `data-self-approve-guard="true"`) {
		t.Fatal("self-approve guard must still render (4-eyes behavior preserved)")
	}
	if !strings.Contains(selfRendered, `M15.312 2a2 2 0 0 1 1.414.586`) {
		t.Error("octagon-alert icon missing from self-approve guard")
	}
	assertNoEmoji(t, "grant request detail (self)", selfRendered)
}
