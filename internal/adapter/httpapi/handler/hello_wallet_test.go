package handler_test

// SIN-64988 — close the test gap on the wallet surface in the
// post-login index. The wallet row (SIN-63942 / UX-F5, "/wallet" →
// "Saldo de tokens") was added to helloIndexRows but, unlike inbox and
// billing/invoices, never had its presence/absence/role-gate pinned:
// allFlagsTrueDeps() forgot to set WalletEnabled and roleMatrix never
// listed "/wallet". A refactor could silently drop the wallet link with
// no test failing. These tests pin the four contracts that matter:
//
//   - gerente + WalletEnabled=true  → live <a href="/wallet"> link,
//   - gerente + WalletEnabled=false → disabled aria span, no dead link,
//   - atendente / common            → wallet filtered out entirely
//                                     (Roles: gerenteOnly),
//   - Extended==nil (legacy wire)   → wallet absent (back-compat).
//
// The fixtures are self-contained (own deps builders) so this file adds
// coverage without touching the existing shared helpers.

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/pericles-luz/crm/internal/adapter/httpapi/handler"
	"github.com/pericles-luz/crm/internal/iam"
)

const (
	walletPath        = "/wallet"
	walletLabel       = "Saldo de tokens"
	walletLiveLink    = `<a href="/wallet">` + walletLabel + `</a>`
	walletDisabledTag = `<span aria-disabled="true">` + walletLabel + ` (indisponível neste ambiente)</span>`
	walletDeadLink    = `<a href="/wallet">`
)

// walletDeps returns extended deps with every flag live and the wallet
// flag set to walletEnabled. Mirrors allFlagsTrueDeps() but is local to
// this file (and, unlike that helper, actually wires WalletEnabled) so
// the wallet contract is measured in isolation from the role gate.
func walletDeps(walletEnabled bool) handler.HelloTenantDeps {
	return handler.HelloTenantDeps{
		FunnelEnabled:      true,
		FunnelRulesEnabled: true,
		CatalogEnabled:     true,
		CampaignsEnabled:   true,
		PrivacyEnabled:     true,
		AIPolicyEnabled:    true,
		ConsentEnabled:     true,
		Extended: &handler.HelloTenantExtendedDeps{
			InboxEnabled:        true,
			BillingEnabled:      true,
			BrandingEnabled:     true,
			LGPDEnabled:         true,
			MFAEnabled:          true,
			CustomDomainEnabled: true,
			WalletEnabled:       walletEnabled,
		},
	}
}

func TestNewHelloTenant_Wallet_GerenteEnabledRendersLink(t *testing.T) {
	t.Parallel()
	rec := httptest.NewRecorder()
	handler.NewHelloTenant(walletDeps(true))(rec, roleHelloRequest(t, iam.RoleTenantGerente))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200 (body=%q)", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, walletLiveLink) {
		t.Errorf("gerente wallet must render live link %q\nbody=%q", walletLiveLink, body)
	}
}

func TestNewHelloTenant_Wallet_GerenteDisabledRendersSpan(t *testing.T) {
	t.Parallel()
	rec := httptest.NewRecorder()
	handler.NewHelloTenant(walletDeps(false))(rec, roleHelloRequest(t, iam.RoleTenantGerente))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200 (body=%q)", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, walletDisabledTag) {
		t.Errorf("gerente wallet (disabled) must render aria span %q\nbody=%q", walletDisabledTag, body)
	}
	if strings.Contains(body, walletDeadLink) {
		t.Errorf("disabled wallet must not render a dead link %q\nbody=%q", walletDeadLink, body)
	}
}

func TestNewHelloTenant_Wallet_NonGerenteFilteredOut(t *testing.T) {
	t.Parallel()
	// Wallet is Roles: gerenteOnly — surfacing it to a role without
	// ActionTenantWalletView would be a guaranteed 403 on click. The
	// role gate must drop it entirely (no link AND no card) for
	// atendente and common.
	for _, role := range []iam.Role{iam.RoleTenantAtendente, iam.RoleTenantCommon} {
		role := role
		t.Run(string(role), func(t *testing.T) {
			t.Parallel()
			rec := httptest.NewRecorder()
			handler.NewHelloTenant(walletDeps(true))(rec, roleHelloRequest(t, role))
			if rec.Code != http.StatusOK {
				t.Fatalf("status=%d, want 200 (body=%q)", rec.Code, rec.Body.String())
			}
			body := rec.Body.String()
			if strings.Contains(body, `href="`+walletPath+`"`) {
				t.Errorf("role %s: wallet must be filtered out, body=%q", role, body)
			}
			if strings.Contains(body, walletDisabledTag) {
				t.Errorf("role %s: wallet must not even render a disabled span, body=%q", role, body)
			}
		})
	}
}

func TestNewHelloTenant_Wallet_NilExtendedOmitsSurface(t *testing.T) {
	t.Parallel()
	// Legacy wire layer (Extended==nil) keeps the SIN-63774 7-entry
	// baseline, so the wallet surface must not leak into the index even
	// for a gerente.
	deps := handler.HelloTenantDeps{
		FunnelEnabled:    true,
		CatalogEnabled:   true,
		CampaignsEnabled: true,
		// Extended deliberately left nil.
	}
	rec := httptest.NewRecorder()
	handler.NewHelloTenant(deps)(rec, roleHelloRequest(t, iam.RoleTenantGerente))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200 (body=%q)", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if strings.Contains(body, `href="`+walletPath+`"`) {
		t.Errorf("legacy nil-Extended index leaked wallet link: %q", body)
	}
}
