package handler_test

// SIN-63940 / UX-F3 — /hello-tenant landing must filter the surfaces
// index, the dashboard cards, AND the top-bar nav by the principal's
// role. The tests below pin the four-role contract:
//
//   - atendente sees inbox + funnel + privacy + 2FA setup; everything
//     gerente-only is omitted (catálogo, política de IA, branding,
//     LGPD, custom-domain),
//   - common (the legacy browse role) sees only the inbox-less subset
//     (funnel + privacy + 2FA setup),
//   - gerente sees every tenant-scope surface,
//   - master is short-circuited and sees the same superset as gerente
//     plus, transitively, every master-only Future link.
//
// The fixtures use the FULL extended deps (all flags true) so the
// only thing being measured is the role gate. Path-level wireup is
// pinned separately in hello_index_test.go.

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/adapter/httpapi/handler"
	"github.com/pericles-luz/crm/internal/adapter/httpapi/middleware"
	"github.com/pericles-luz/crm/internal/iam"
	"github.com/pericles-luz/crm/internal/tenancy"
)

// allFlagsTrueDeps wires every Fase 3–6 surface to "live". Role-only
// tests use this so a hidden card in the rendered body proves the role
// gate omitted it, not that a stray deps.WebX dropped it.
func allFlagsTrueDeps() handler.HelloTenantDeps {
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
		},
	}
}

// roleHelloRequest builds an authenticated GET /hello-tenant request
// whose session carries the supplied iam.Role. The test request goes
// through the same RequireAuth fixture as the other handler tests but
// the role is set explicitly so filterSurfacesByRole exercises the
// non-legacy code path.
func roleHelloRequest(t *testing.T, role iam.Role) *http.Request {
	t.Helper()
	tenantID := uuid.New()
	userID := uuid.New()
	tenant := &tenancy.Tenant{ID: tenantID, Name: "acme", Host: "acme.crm.local"}
	r := tenantedRequest(t, http.MethodGet, "/hello-tenant", nil, tenant)
	return r.WithContext(middleware.WithSession(r.Context(), iam.Session{
		ID: uuid.New(), UserID: userID, TenantID: tenantID,
		Role:      role,
		CSRFToken: "role-test-csrf",
		ExpiresAt: time.Now().Add(time.Hour),
	}))
}

// rolePaths bundles the per-role expectations the tests below pin.
// Visible/Hidden are the surface paths that MUST render an anchor (in
// either the surfaces nav OR the dashboard card link) and that MUST
// NOT appear as anchors anywhere, respectively. Surfaces a role does
// not see render neither as <a> link nor as the disabled hint — they
// are dropped entirely so the operator does not get teased with a
// surface the role gate would forbid.
type rolePaths struct {
	role    iam.Role
	visible []string // path → must appear as `<a href="path"` somewhere
	hidden  []string // path → MUST NOT appear as anchor anywhere
}

// roleMatrix is the application-layer mirror of the ADR 0090 RBAC
// matrix for /hello-tenant entries. Atendente sees the operator-side
// inbox + funnel; gerente sees the full configuration surface; master
// short-circuits to "see everything" inside filterSurfacesByRole;
// common is the legacy browse role and sees only what every role can
// reach (funnel browse + privacy + 2FA setup).
var roleMatrix = []rolePaths{
	{
		role: iam.RoleTenantAtendente,
		visible: []string{
			"/inbox",
			"/funnel",
			"/settings/privacy",
			"/admin/2fa/setup",
		},
		hidden: []string{
			"/funnel/rules",
			"/catalog",
			"/campaigns",
			"/settings/ai-policy",
			"/consent/cookies-banner",
			"/branding",
			"/billing/invoices",
			"/admin/lgpd/requests",
			"/tenant/custom-domains",
		},
	},
	{
		role: iam.RoleTenantCommon,
		visible: []string{
			"/funnel",
			"/settings/privacy",
			"/admin/2fa/setup",
		},
		hidden: []string{
			"/inbox", // SIN-63808 — common cannot read the inbox
			"/funnel/rules",
			"/catalog",
			"/campaigns",
			"/settings/ai-policy",
			"/consent/cookies-banner",
			"/branding",
			"/billing/invoices",
			"/admin/lgpd/requests",
			"/tenant/custom-domains",
		},
	},
	{
		role: iam.RoleTenantGerente,
		visible: []string{
			"/inbox",
			"/funnel",
			"/funnel/rules",
			"/catalog",
			"/campaigns",
			"/settings/privacy",
			"/settings/ai-policy",
			"/consent/cookies-banner",
			"/branding",
			"/billing/invoices",
			"/admin/lgpd/requests",
			"/tenant/custom-domains",
			"/admin/2fa/setup",
		},
		hidden: []string{},
	},
	{
		role: iam.RoleMaster,
		visible: []string{
			"/inbox",
			"/funnel",
			"/funnel/rules",
			"/catalog",
			"/campaigns",
			"/settings/privacy",
			"/settings/ai-policy",
			"/consent/cookies-banner",
			"/branding",
			"/billing/invoices",
			"/admin/lgpd/requests",
			"/tenant/custom-domains",
			"/admin/2fa/setup",
		},
		hidden: []string{},
	},
}

func TestNewHelloTenant_Role_FiltersSurfacesAndCards(t *testing.T) {
	t.Parallel()
	deps := allFlagsTrueDeps()
	for _, tc := range roleMatrix {
		tc := tc
		t.Run(string(tc.role), func(t *testing.T) {
			t.Parallel()
			rec := httptest.NewRecorder()
			handler.NewHelloTenant(deps)(rec, roleHelloRequest(t, tc.role))
			if rec.Code != http.StatusOK {
				t.Fatalf("status=%d, want 200 (body=%q)", rec.Code, rec.Body.String())
			}
			body := rec.Body.String()
			for _, p := range tc.visible {
				if !strings.Contains(body, `href="`+p+`"`) {
					t.Errorf("role %s: surface %q must render an anchor, body=%q", tc.role, p, body)
				}
			}
			for _, p := range tc.hidden {
				if strings.Contains(body, `href="`+p+`"`) {
					t.Errorf("role %s: surface %q must be filtered out, body=%q", tc.role, p, body)
				}
				if strings.Contains(body, "card-"+p) {
					t.Errorf("role %s: card for %q must be filtered out, body=%q", tc.role, p, body)
				}
			}
		})
	}
}

func TestNewHelloTenant_Role_TopBarNavFiltered(t *testing.T) {
	t.Parallel()
	deps := allFlagsTrueDeps()
	// Atendente: top-bar nav has ONLY Inbox + Funil per AC §2. Privacy
	// and 2FA setup are body-only surfaces (TopNav=false) so the chrome
	// stays scannable. Everything gerente-only is omitted by role gate.
	rec := httptest.NewRecorder()
	handler.NewHelloTenant(deps)(rec, roleHelloRequest(t, iam.RoleTenantAtendente))
	body := rec.Body.String()
	for _, want := range []string{
		`<a href="/inbox">Inbox</a>`,
		`<a href="/funnel">Funil</a>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("atendente top-bar missing %q", want)
		}
	}
	// Body-only surfaces must not appear as top-bar anchors.
	for _, off := range []string{
		`<a href="/settings/privacy">Privacidade</a>`,
		`<a href="/admin/2fa/setup">2FA</a>`,
		`<a href="/catalog">Catálogo</a>`,
		`<a href="/branding">Branding</a>`,
		`<a href="/admin/lgpd/requests">LGPD</a>`,
		`<a href="/tenant/custom-domains">Domínio</a>`,
	} {
		if strings.Contains(body, off) {
			t.Errorf("atendente top-bar leaked %q", off)
		}
	}
}

func TestNewHelloTenant_Role_DashboardCardLabels(t *testing.T) {
	t.Parallel()
	// Atendente lands on a Suas conversas card with the JTBD body
	// line and an Abrir link. The dashboard is the primary landing
	// per SIN-63940; if this regression-tests fails the atendente
	// no longer sees the inbox CTA above the surfaces-nav fold.
	rec := httptest.NewRecorder()
	handler.NewHelloTenant(allFlagsTrueDeps())(rec, roleHelloRequest(t, iam.RoleTenantAtendente))
	body := rec.Body.String()
	if !strings.Contains(body, `data-testid="dashboard-cards"`) {
		t.Fatalf("dashboard cards section missing: %q", body)
	}
	if !strings.Contains(body, `<h2>Suas conversas</h2>`) {
		t.Errorf("atendente landing missing Suas conversas card heading: %q", body)
	}
	if !strings.Contains(body, "Atender clientes vindos de WhatsApp") {
		t.Errorf("atendente Suas conversas card missing JTBD line: %q", body)
	}
	if !strings.Contains(body, `<a class="hello-tenant__card-link" href="/inbox">Abrir</a>`) {
		t.Errorf("atendente Suas conversas card missing Abrir link: %q", body)
	}
}

func TestNewHelloTenant_HumanGreetingAndTenant(t *testing.T) {
	t.Parallel()
	// SIN-63940 AC #3 — the landing greets the user by display name,
	// keeps the tenant name as the secondary context line, and hides
	// the user-id behind a hidden <span> so debug callers can still
	// find it without polluting the visible chrome.
	rec := httptest.NewRecorder()
	handler.NewHelloTenant(allFlagsTrueDeps())(rec, roleHelloRequest(t, iam.RoleTenantGerente))
	body := rec.Body.String()
	if !strings.Contains(body, "Olá, <strong>") {
		t.Errorf("body missing Olá greeting: %q", body)
	}
	if !strings.Contains(body, `<strong data-testid="tenant-name">acme</strong>`) {
		t.Errorf("body missing tenant-name strong: %q", body)
	}
	// User ID stays hidden but reachable (so handlers_test.go
	// TestHelloTenant_RendersTenantName still passes against the
	// legacy shim).
	if !strings.Contains(body, `<span hidden data-testid="user-id">`) {
		t.Errorf("body missing hidden user-id span: %q", body)
	}
	if strings.Contains(body, "User ID:") {
		t.Errorf("body still contains debug 'User ID:' label: %q", body)
	}
}

func TestNewHelloTenant_DisabledCardCarriesExplanation(t *testing.T) {
	t.Parallel()
	// SIN-63940 AC #6 — when the underlying handler is unmounted the
	// card body MUST carry the longer "Indisponível neste ambiente —
	// verifique configuração do servidor." copy instead of leaving
	// the operator with no signal about why the link is dead.
	deps := allFlagsTrueDeps()
	deps.Extended.InboxEnabled = false
	rec := httptest.NewRecorder()
	handler.NewHelloTenant(deps)(rec, roleHelloRequest(t, iam.RoleTenantGerente))
	body := rec.Body.String()
	const want = "Indisponível neste ambiente — verifique configuração do servidor."
	if !strings.Contains(body, want) {
		t.Errorf("disabled inbox card missing explanation %q\nbody=%q", want, body)
	}
}

func TestNewHelloTenant_BackCompat_NilExtendedKeepsLegacyIndex(t *testing.T) {
	t.Parallel()
	// SIN-63940 — when the wire layer has not migrated (Extended is
	// nil) the handler MUST keep emitting the SIN-63774 7-entry
	// baseline in the surfaces nav so router_test.go fixtures stay
	// green. The user-menu 2FA link is the shell-level chrome that
	// every authenticated page carries and is NOT part of the index
	// — it stays regardless of Extended, mirrored by the existing
	// TestNewHelloTenant_ShellUserMenuLogoutAndTwoFA pin.
	deps := handler.HelloTenantDeps{
		FunnelEnabled:    true,
		CatalogEnabled:   true,
		CampaignsEnabled: true,
		// Extended deliberately left nil.
	}
	rec := httptest.NewRecorder()
	handler.NewHelloTenant(deps)(rec, roleHelloRequest(t, iam.RoleTenantGerente))
	body := rec.Body.String()
	for _, p := range []string{
		"/inbox", "/branding", "/billing/invoices",
		"/admin/lgpd/requests", "/tenant/custom-domains",
	} {
		if strings.Contains(body, `href="`+p+`"`) {
			t.Errorf("legacy index leaked %q: %q", p, body)
		}
	}
}
