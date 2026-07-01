package handler_test

// SIN-66391 (P2) — the /settings/channels admin surface MUST appear on the
// post-login index for gerente (and be filtered out for atendente), so a
// freshly-logged-in gerente can reach the channel manager without typing a
// URL (memory feedback_hello_tenant_sync_on_mount). New test — the
// existing roleMatrix is subset-based and untouched.

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/pericles-luz/crm/internal/adapter/httpapi/handler"
	"github.com/pericles-luz/crm/internal/iam"
)

func channelsEnabledDeps() handler.HelloTenantDeps {
	deps := allFlagsTrueDeps()
	deps.Extended.ChannelsEnabled = true
	return deps
}

func TestNewHelloTenant_ChannelsSurface_GerenteSeesLink(t *testing.T) {
	t.Parallel()
	rec := httptest.NewRecorder()
	handler.NewHelloTenant(channelsEnabledDeps())(rec, roleHelloRequest(t, iam.RoleTenantGerente))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `<a href="/settings/channels">Canais</a>`) {
		t.Fatalf("gerente landing missing Canais link\nbody=%s", rec.Body.String())
	}
}

// SIN-66444 — the board reported "não tem a opção no menu": the surface
// was TopNav:false, so Canais rendered only as a body card (scrolled off
// screen) and never in the persistent left-nav. It must appear in the
// shell nav for gerente, at parity with Branding/Faturas.
func TestNewHelloTenant_ChannelsSurface_InShellNav(t *testing.T) {
	t.Parallel()
	rec := httptest.NewRecorder()
	handler.NewHelloTenant(channelsEnabledDeps())(rec, roleHelloRequest(t, iam.RoleTenantGerente))
	body := rec.Body.String()
	navStart := strings.Index(body, `id="app-shell-nav"`)
	if navStart < 0 {
		t.Fatalf("shell nav block absent\nbody=%s", body)
	}
	navEnd := strings.Index(body[navStart:], "</nav>")
	if navEnd < 0 {
		t.Fatalf("shell nav block unterminated")
	}
	nav := body[navStart : navStart+navEnd]
	if !strings.Contains(nav, `<a href="/settings/channels">Canais</a>`) {
		t.Fatalf("Canais missing from left-nav (SIN-66444)\nnav=%s", nav)
	}
}

func TestNewHelloTenant_ChannelsSurface_AtendenteHidden(t *testing.T) {
	t.Parallel()
	rec := httptest.NewRecorder()
	handler.NewHelloTenant(channelsEnabledDeps())(rec, roleHelloRequest(t, iam.RoleTenantAtendente))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d", rec.Code)
	}
	if strings.Contains(rec.Body.String(), `href="/settings/channels"`) {
		t.Fatalf("atendente landing leaked the gerente-only Canais link")
	}
}

func TestNewHelloTenant_ChannelsSurface_DisabledWhenUnmounted(t *testing.T) {
	t.Parallel()
	deps := allFlagsTrueDeps() // ChannelsEnabled defaults false
	rec := httptest.NewRecorder()
	handler.NewHelloTenant(deps)(rec, roleHelloRequest(t, iam.RoleTenantGerente))
	body := rec.Body.String()
	// The surface still appears for gerente, but as the disabled hint
	// rather than a live link (parity with wallet/dashboard treatment).
	if strings.Contains(body, `<a href="/settings/channels">Canais</a>`) {
		t.Fatalf("Canais should be disabled when WebChannels is unmounted")
	}
	if !strings.Contains(body, "Canais") {
		t.Fatalf("Canais entry should still render (disabled) for gerente")
	}
}
