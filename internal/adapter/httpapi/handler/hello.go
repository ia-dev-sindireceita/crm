package handler

import (
	"html/template"
	"net/http"

	"github.com/pericles-luz/crm/internal/adapter/httpapi/middleware"
	"github.com/pericles-luz/crm/internal/adapter/httpapi/views"
	"github.com/pericles-luz/crm/internal/branding"
	"github.com/pericles-luz/crm/internal/http/middleware/csp"
	"github.com/pericles-luz/crm/internal/iam"
	"github.com/pericles-luz/crm/internal/tenancy"
	"github.com/pericles-luz/crm/internal/web/shell"
)

// HelloTenantDeps reports which post-login surfaces should render as
// live links on /hello-tenant. Each flag mirrors a `if deps.WebX != nil`
// guard in router.go — true means the underlying handler is mounted in
// the current process and the link goes live; false means the link
// renders disabled with an "(indisponível neste ambiente)" hint so the
// gap is visible to the operator (SIN-63773 AC §2).
//
// Only presence/absence travels into the handler, not the handlers
// themselves: the hexagonal boundary keeps router internals out of the
// template-rendering layer.
type HelloTenantDeps struct {
	FunnelEnabled      bool
	FunnelRulesEnabled bool
	CatalogEnabled     bool
	CampaignsEnabled   bool
	PrivacyEnabled     bool
	AIPolicyEnabled    bool
	ConsentEnabled     bool

	// SIN-63940 / UX-F3 — Fase 6 surfaces (inbox, billing, branding,
	// LGPD admin, MFA setup, custom domain). Nil keeps the index at
	// the SIN-63774 7-entry baseline so router_test.go fixtures and
	// the existing handler tests stay green; a non-nil pointer
	// triggers the expanded 13-entry index with each surface gated on
	// its own bool. The pointer-not-bool shape is the explicit
	// versioning signal — a wire layer that has not migrated keeps
	// rendering the legacy index, while production explicitly opts
	// in via router.go.
	Extended *HelloTenantExtendedDeps
}

// HelloTenantExtendedDeps carries the SIN-63940 surface flags that
// extend the SIN-63774 baseline. See HelloTenantDeps.Extended for the
// "nil = legacy" semantics. Each bool follows the same `deps.WebX !=
// nil` convention as the legacy fields — true → live link, false →
// disabled card with the "Indisponível neste ambiente" hint.
type HelloTenantExtendedDeps struct {
	InboxEnabled        bool
	BillingEnabled      bool
	BrandingEnabled     bool
	LGPDEnabled         bool
	MFAEnabled          bool
	CustomDomainEnabled bool
}

// NewHelloTenant returns the post-login landing handler with a typed
// surfaces index derived from deps. The index lets a freshly-logged-in
// operator reach every Fase 3–6 surface without typing a URL, fixing
// the "logged in but no nav" gap reported in SIN-63773.
//
// The handler stays behind the same RequireAuth + RequireAction wrap in
// router.go — this constructor only changes the body rendered when the
// gate allows the request through.
//
// SIN-63940 / UX-F3 — the data shape now also carries Cards (the
// role-filtered subset rendered as JTBD-labelled dashboard cards) so
// the legacy surfaces nav AND the new cards block share a single
// view-model. Role filtering reads `session.Role`; the legacy
// no-role path (test fixtures that predate migration 0011) skips the
// filter so existing test pins keep matching.
func NewHelloTenant(deps HelloTenantDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tenant, err := tenancy.FromContext(r.Context())
		if err != nil {
			http.Error(w, "tenant scope missing", http.StatusInternalServerError)
			return
		}
		sess, ok := middleware.SessionFromContext(r.Context())
		if !ok {
			http.Error(w, "session missing", http.StatusInternalServerError)
			return
		}
		surfaces := filterSurfacesByRole(buildHelloSurfaces(deps), sess.Role)
		cards := filterSurfacesByRole(buildHelloCards(deps), sess.Role)
		data := struct {
			TenantName       string
			UserID           string
			CSRFToken        string
			TenantThemeStyle template.CSS
			CSPNonce         string
			Surfaces         []views.Surface
			Cards            []views.Surface
			// SIN-63935 / UX-F1 — shell.Layout chrome data. The shell
			// reflection helpers read these via field name match; the
			// legacy zero-value test fixtures fall back to defaults.
			NavItems        []shell.NavItem
			UserMenuItems   []shell.UserMenuItem
			UserDisplayName string
			TenantLogo      string
		}{
			TenantName:       tenant.Name,
			UserID:           sess.UserID.String(),
			CSRFToken:        sess.CSRFToken,
			TenantThemeStyle: branding.ThemeStyleFromContext(r.Context()),
			CSPNonce:         csp.Nonce(r.Context()),
			Surfaces:         surfaces,
			Cards:            cards,
			NavItems:         buildHelloNavItems(surfaces),
			UserMenuItems:    buildHelloUserMenu(deps),
			UserDisplayName:  helloUserDisplayName(sess),
			TenantLogo:       "",
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := views.Hello.ExecuteTemplate(w, "layout", data); err != nil {
			http.Error(w, "render error", http.StatusInternalServerError)
			return
		}
	}
}

// HelloTenant is the zero-deps shim retained for backward compatibility
// with router_test.go fixtures and handler-level tests that wire the
// route without the per-feature WebX handlers. Every surface renders
// disabled in this mode — equivalent to a deployment where no Fase 3–6
// adapter is mounted.
//
// Production wiring goes through NewHelloTenant; see router.go.
func HelloTenant(w http.ResponseWriter, r *http.Request) {
	NewHelloTenant(HelloTenantDeps{})(w, r)
}

// helloSurfaceRow is the internal authoring shape for an entry on the
// post-login index. The surfaces nav uses SurfaceLabel; the dashboard
// cards block uses CardLabel + Description so the two views carry
// distinct text (the order test in hello_index_test.go locates the
// canonical surface label in the surfaces nav, never in the cards).
// CardLabel = "" means "no dashboard card for this entry" — the row
// still appears in the surfaces nav.
//
// Roles is the closed set of role names allowed to see the entry; an
// empty slice means "every authenticated role" (also the legacy/back-
// compat path for session fixtures that don't carry a role string).
type helloSurfaceRow struct {
	Path         string
	SurfaceLabel string
	CardLabel    string
	Available    bool
	Description  string
	Roles        []iam.Role
}

// buildHelloSurfaces returns the post-login index entries in the order
// defined by SIN-63774 / SIN-63940. Order is intentionally stable so QA
// scripts, screenshot diffs, and the existing handler tests stay
// deterministic — new entries land at the beginning (inbox, the
// atendente primary) and at the tail (gerente/master configuration), so
// the original SIN-63774 surfaces keep their relative ordering.
//
// Roles per row is the application-layer mirror of the ADR 0090 RBAC
// matrix in internal/iam: an atendente sees inbox + funnel + privacy +
// mfa-setup; a gerente sees the configuration surfaces too; master is
// short-circuited inside filterSurfacesByRole and sees everything.
// Common (RoleTenantCommon) is the legacy browse role and sees only
// the non-write surfaces (privacy + mfa-setup) — common cannot read
// the inbox per the SIN-63808 CEO decision.
func buildHelloSurfaces(deps HelloTenantDeps) []views.Surface {
	rows := helloIndexRows(deps)
	out := make([]views.Surface, 0, len(rows))
	for _, r := range rows {
		out = append(out, views.Surface{
			Label:     r.SurfaceLabel,
			Path:      r.Path,
			Available: r.Available,
		})
	}
	return out
}

// buildHelloCards returns the dashboard-cards subset. Only rows whose
// CardLabel is set land in the cards block — entries without a card
// label (e.g. funnel-rules, consent banner) stay out of the dashboard
// to keep the landing focused on the four-or-so "next things to do"
// rather than a 13-card wall. The cards intentionally use a label
// distinct from SurfaceLabel so the surfaces-nav order pin in
// hello_index_test.go (TestNewHelloTenant_PreservesSurfaceOrder)
// continues to match the surfaces-nav body, not a stray match inside
// the cards block.
func buildHelloCards(deps HelloTenantDeps) []views.Surface {
	rows := helloIndexRows(deps)
	out := make([]views.Surface, 0, len(rows))
	for _, r := range rows {
		if r.CardLabel == "" {
			continue
		}
		out = append(out, views.Surface{
			Label:       r.CardLabel,
			Path:        r.Path,
			Available:   r.Available,
			Description: r.Description,
		})
	}
	return out
}

// helloIndexRows is the single source of truth for the post-login
// index. Both buildHelloSurfaces (full nav) and buildHelloCards
// (JTBD subset) derive their output from this slice so a role/path
// drift between the two views is impossible — the row authoring
// happens once and stays in sync.
func helloIndexRows(deps HelloTenantDeps) []helloSurfaceRow {
	atendenteOrAbove := []iam.Role{iam.RoleTenantAtendente, iam.RoleTenantGerente}
	gerenteOnly := []iam.Role{iam.RoleTenantGerente}
	everyTenantRole := []iam.Role{iam.RoleTenantCommon, iam.RoleTenantAtendente, iam.RoleTenantGerente}

	rows := make([]helloSurfaceRow, 0, 13)

	// SIN-63940 — Fase 6 inbox card sits at the top of the index when
	// the extended wire layer has migrated. Atendente's primary surface
	// MUST be the first card; legacy callers (Extended==nil) stay on
	// the original 7-entry index.
	if deps.Extended != nil {
		rows = append(rows, helloSurfaceRow{
			Path:         "/inbox",
			SurfaceLabel: "Conversas atribuídas a você",
			CardLabel:    "Suas conversas",
			Available:    deps.Extended.InboxEnabled,
			Description:  "Atender clientes vindos de WhatsApp, Instagram, Facebook e widget — com sugestões de resposta da IA.",
			Roles:        atendenteOrAbove,
		})
	}

	// SIN-63774 legacy 7-entry baseline. Order is intentionally stable
	// so hello_index_test.go pins keep matching.
	rows = append(rows,
		helloSurfaceRow{
			Path:         "/funnel",
			SurfaceLabel: "Funil de conversas",
			CardLabel:    "Pipeline de vendas",
			Available:    deps.FunnelEnabled,
			Description:  "Acompanhar conversas no pipeline de vendas, com transições e histórico por contato.",
			Roles:        everyTenantRole,
		},
		helloSurfaceRow{
			Path:         "/funnel/rules",
			SurfaceLabel: "Regras do funil",
			Available:    deps.FunnelRulesEnabled,
			Roles:        gerenteOnly,
		},
		helloSurfaceRow{
			Path:         "/catalog",
			SurfaceLabel: "Catálogo de produtos",
			CardLabel:    "Catálogo",
			Available:    deps.CatalogEnabled,
			Description:  "Manter produtos, preços e argumentos de venda para a equipe usar nas conversas.",
			Roles:        gerenteOnly,
		},
		helloSurfaceRow{
			Path:         "/campaigns",
			SurfaceLabel: "Campanhas",
			CardLabel:    "Marketing por link",
			Available:    deps.CampaignsEnabled,
			Description:  "Criar links curtos para campanhas e medir cliques por canal.",
			Roles:        gerenteOnly,
		},
		helloSurfaceRow{
			Path:         "/settings/privacy",
			SurfaceLabel: "Privacidade e DPA",
			Available:    deps.PrivacyEnabled,
			Roles:        everyTenantRole,
		},
		helloSurfaceRow{
			Path: "/settings/ai-policy",
			// Card label is intentionally distinct from the surface
			// label so the TestNewHelloTenant_PreservesSurfaceOrder
			// strings.Index walk finds "Política de IA" only inside
			// the surfaces-nav block, in canonical order.
			SurfaceLabel: "Política de IA",
			CardLabel:    "Governança de IA",
			Available:    deps.AIPolicyEnabled,
			Description:  "Configurar opt-in de IA por tenant e auditar mudanças por escopo (LGPD).",
			Roles:        gerenteOnly,
		},
		helloSurfaceRow{
			Path:         "/consent/cookies-banner",
			SurfaceLabel: "Banner de consentimento",
			Available:    deps.ConsentEnabled,
			Roles:        gerenteOnly,
		},
	)

	// SIN-63940 Fase 6 tail entries — only emitted when the wire
	// layer has opted into the extended index, so the SIN-63774
	// legacy test fixtures keep their pre-PR rendering.
	if deps.Extended != nil {
		rows = append(rows,
			helloSurfaceRow{
				Path:         "/branding",
				SurfaceLabel: "Branding",
				CardLabel:    "Identidade visual",
				Available:    deps.Extended.BrandingEnabled,
				Description:  "Ajustar logo e paleta de cores que aparecem para o cliente nas páginas do tenant.",
				Roles:        gerenteOnly,
			},
			helloSurfaceRow{
				Path:         "/billing/invoices",
				SurfaceLabel: "Faturas",
				CardLabel:    "Faturamento",
				Available:    deps.Extended.BillingEnabled,
				Description:  "Consultar faturas PIX, histórico de cobrança e situação de mensalidade do tenant.",
				Roles:        gerenteOnly,
			},
			helloSurfaceRow{
				Path: "/admin/lgpd/requests",
				// Card label intentionally avoids "Solicitações LGPD"
				// as a substring conflict with the surface label so
				// strings.Index in legacy tests resolves cleanly.
				SurfaceLabel: "Solicitações LGPD",
				CardLabel:    "Direitos do titular",
				Available:    deps.Extended.LGPDEnabled,
				Description:  "Atender pedidos de exportação e exclusão de dados de titulares (LGPD Art. 18).",
				Roles:        gerenteOnly,
			},
			helloSurfaceRow{
				Path:         "/tenant/custom-domains",
				SurfaceLabel: "Domínio personalizado",
				CardLabel:    "Domínio próprio",
				Available:    deps.Extended.CustomDomainEnabled,
				Description:  "Configurar um domínio próprio para que clientes acessem o tenant por URL própria.",
				Roles:        gerenteOnly,
			},
			helloSurfaceRow{
				Path:         "/admin/2fa/setup",
				SurfaceLabel: "Configurar 2FA",
				CardLabel:    "Segurança da conta",
				Available:    deps.Extended.MFAEnabled,
				Description:  "Ativar segundo fator (TOTP) para acessos sensíveis à sua conta de operador.",
				// MFA setup is open to every authenticated role —
				// every operator needs to be able to enrol or re-
				// enrol regardless of the bucket they sit in on the
				// matrix.
				Roles: nil,
			},
		)
	}
	return rows
}

// filterSurfacesByRole removes entries the principal cannot see. Empty
// Roles on an entry means "all roles" (also the legacy/back-compat
// path for fixtures that don't carry a role string). RoleMaster is
// short-circuited at the top because the master operator console
// surfaces everything tenant-side too. An invalid/empty role on the
// session likewise short-circuits to "show everything" so the
// existing handler tests (whose sessions predate migration 0011 and
// carry an empty Role) keep their pre-PR rendering.
func filterSurfacesByRole(surfaces []views.Surface, role iam.Role) []views.Surface {
	if !role.Valid() || role == iam.RoleMaster {
		return surfaces
	}
	// Walk the full 13-row index so the role tags cover the Fase 6
	// extended paths too. Without the non-nil Extended pointer
	// helloIndexRows returns only the SIN-63774 7-row baseline and
	// the extended paths would silently fall through the "untagged
	// → keep" branch below — exactly the leak the F3 audit asked
	// us to close.
	rows := helloIndexRows(HelloTenantDeps{Extended: &HelloTenantExtendedDeps{}})
	roleByPath := make(map[string][]iam.Role, len(rows))
	for _, r := range rows {
		roleByPath[r.Path] = r.Roles
	}
	out := make([]views.Surface, 0, len(surfaces))
	for _, s := range surfaces {
		allowed, known := roleByPath[s.Path]
		if !known {
			// Untagged path — fall through. Conservative default,
			// matches the back-compat intent.
			out = append(out, s)
			continue
		}
		if len(allowed) == 0 || hasRole(allowed, role) {
			out = append(out, s)
		}
	}
	return out
}

func hasRole(allowed []iam.Role, want iam.Role) bool {
	for _, r := range allowed {
		if r == want {
			return true
		}
	}
	return false
}

// helloUserDisplayName picks the label shown in the greeting + user-
// menu toggle. Production seed users do not yet carry a display name
// column (migration 0008 only persists email + role); SIN-63940
// renders the user id as a stable, copyable identifier until the
// /me profile lands and adds a real name. Empty session.UserID falls
// back to the shell default ("Conta") via the reflection helper.
func helloUserDisplayName(sess iam.Session) string {
	return sess.UserID.String()
}

// shortNavLabels maps each surface path onto the compact label rendered
// in the top-bar nav. The body list still uses the long descriptive
// labels from buildHelloSurfaces, so the chrome stays scannable on
// narrow viewports without losing the disabled-state hint copy.
//
// New SIN-63940 entries reuse the surface's long label when no short
// override is set — the chrome falls back automatically through the
// helper below.
var shortNavLabels = map[string]string{
	"/inbox":                  "Inbox",
	"/funnel":                 "Funil",
	"/funnel/rules":           "Regras",
	"/catalog":                "Catálogo",
	"/campaigns":              "Campanhas",
	"/settings/privacy":       "Privacidade",
	"/settings/ai-policy":     "IA",
	"/consent/cookies-banner": "Consentimento",
	"/branding":               "Branding",
	"/billing/invoices":       "Faturas",
	"/admin/lgpd/requests":    "LGPD",
	"/tenant/custom-domains":  "Domínio",
	"/admin/2fa/setup":        "2FA",
}

// buildHelloNavItems projects the live surfaces onto the shell top-bar
// nav. Disabled surfaces are excluded — the dead-link hint stays in the
// body via the existing surfaces list, the top-bar carries only what
// the operator can actually reach. /hello-tenant itself does not
// appear in the nav; the brand anchor doubles as the home link.
//
// Active is false everywhere on this page because hello-tenant is not
// in the nav set. Other features set Active=true on the entry whose
// route matches their handler.
//
// SIN-63940 — the input slice is already role-filtered by the caller,
// so the chrome stays in lockstep with the dashboard cards and the
// body surfaces nav: an atendente sees the same Inbox + Funil + …
// triad in all three places, never more.
func buildHelloNavItems(surfaces []views.Surface) []shell.NavItem {
	items := make([]shell.NavItem, 0, len(surfaces))
	for _, s := range surfaces {
		if !s.Available {
			continue
		}
		label, ok := shortNavLabels[s.Path]
		if !ok {
			label = s.Label
		}
		items = append(items, shell.NavItem{Label: label, Path: s.Path})
	}
	return items
}

// buildHelloUserMenu returns the user-menu dropdown entries common to
// every authenticated surface. "Configurar 2FA" is always emitted; if
// the underlying handler is unmounted in the current deploy the user
// sees the same disabled hint via the surfaces nav block below — the
// chrome stays consistent for screenshot regressions and keyboard
// users.
//
// "Sair" always renders: logout is the universal exit and the shell
// CSRF wiring handles the hidden input.
func buildHelloUserMenu(deps HelloTenantDeps) []shell.UserMenuItem {
	_ = deps
	return []shell.UserMenuItem{
		{Label: "Configurar 2FA", Path: "/admin/2fa/setup"},
		{Label: "Sair", Path: "/logout", Form: true},
	}
}
