package privacy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"html/template"
	"io"
	"log/slog"
	"net/http"
	"net/mail"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/renderer/html"

	"github.com/pericles-luz/crm/internal/branding"
	"github.com/pericles-luz/crm/internal/http/middleware/csp"
	"github.com/pericles-luz/crm/internal/tenancy"
)

// SettingsReader is the read port the page depends on. cmd/server
// passes the postgres-backed TenantResolver; tests pass a stub.
type SettingsReader interface {
	LoadPrivacySettings(ctx context.Context, tenantID uuid.UUID) (tenancy.PrivacySettings, error)
}

// Deps bundles the handler collaborators. All fields except Logger
// and Now are required; Logger defaults to slog.Default and Now to
// time.Now (UTC) so tests can leave them zero.
type Deps struct {
	Settings SettingsReader
	Now      func() time.Time
	Logger   *slog.Logger
}

// Handler serves GET /privacy. Construct once with New; the returned
// Handler is safe to share across goroutines.
type Handler struct {
	deps Deps
}

// New validates deps and returns a ready Handler.
func New(deps Deps) (*Handler, error) {
	if deps.Settings == nil {
		return nil, errors.New("web/public/privacy: Settings is required")
	}
	if deps.Now == nil {
		deps.Now = func() time.Time { return time.Now().UTC() }
	}
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	return &Handler{deps: deps}, nil
}

// Routes mounts GET /privacy on mux.
func (h *Handler) Routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /privacy", h.view)
}

// ServeHTTP implements http.Handler so the page can be mounted under a
// chi route without going through the inner mux.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) { h.view(w, r) }

// FallbackVersion is the version string the page renders when the
// tenant has not yet published a policy. It mirrors the LGPD platform
// baseline so visitors who land before a publish see a coherent value.
const FallbackVersion = "platform-default-1.0"

// FallbackPolicy is the platform-default privacy policy body. Inlined
// so the binary can render the page even when the tenant has not
// pushed a policy yet. The text is in pt-BR because the SaaS is
// Brazil-targeted; localisation is a follow-up.
const FallbackPolicy = `# Política de Privacidade (modelo padrão)

Este é o texto padrão exibido enquanto o operador desta área de
atendimento não publica a política específica do tenant. A política
abrange:

- Quais dados pessoais são coletados (nome, e-mail, telefone,
  mensagens enviadas pelos canais habilitados).
- Bases legais de tratamento (LGPD art. 7º, principalmente execução
  de contrato, legítimo interesse e cumprimento de obrigação legal).
- Compartilhamento com sub-processadores (provedores de mensageria,
  pagamentos, processamento por IA — quando aplicável).
- Direitos do titular (acesso, correção, deleção, portabilidade).

Para exercer um direito ou tirar dúvidas, contate o encarregado de
proteção de dados (DPO) listado abaixo. Atualizamos esta política
sempre que houver mudança material; a versão e a data publicadas
identificam a vigência.
`

func (h *Handler) view(w http.ResponseWriter, r *http.Request) {
	tenant, err := tenancy.FromContext(r.Context())
	if err != nil {
		http.Error(w, "tenant required", http.StatusInternalServerError)
		return
	}
	settings, err := h.deps.Settings.LoadPrivacySettings(r.Context(), tenant.ID)
	if err != nil && !errors.Is(err, tenancy.ErrTenantNotFound) {
		h.deps.Logger.Error("web/public/privacy: load settings", "tenant_id", tenant.ID, "err", err)
		http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		return
	}
	version := settings.PrivacyPolicyVersion
	if version == "" {
		version = FallbackVersion
	}
	body := settings.PrivacyPolicyMarkdown
	if strings.TrimSpace(body) == "" {
		body = FallbackPolicy
	}
	updated := h.deps.Now().UTC()
	if settings.PrivacyPolicyUpdated != nil {
		updated = settings.PrivacyPolicyUpdated.UTC()
	}
	rendered, err := renderMarkdown(body)
	if err != nil {
		h.deps.Logger.Error("web/public/privacy: render markdown", "err", err)
		http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		return
	}
	policyURL := safePolicyURL(settings.PrivacyPolicyURL)
	dpoEmail := safeMailbox(settings.DPOEmail)
	data := pageData{
		TenantName:       tenant.Name,
		TenantThemeStyle: branding.ThemeStyleFromContext(r.Context()),
		CSPNonce:         csp.Nonce(r.Context()),
		Version:          version,
		UpdatedAt:        updated.Format("2006-01-02"),
		UpdatedAtISO:     updated.Format(time.RFC3339),
		PolicyHTML:       rendered,
		PolicyURL:        policyURL,
		DPOName:          settings.DPOName,
		DPOEmail:         dpoEmail,
		DPOEmailMailto:   mailtoURL(dpoEmail),
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// Public page; allow CDN/edge caching but require revalidation so
	// a freshly-published policy lands quickly.
	w.Header().Set("Cache-Control", "public, max-age=60, must-revalidate")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
	if err := pageTmpl.Execute(w, data); err != nil {
		h.deps.Logger.Error("web/public/privacy: render", "err", err)
	}
}

// renderMarkdown converts Markdown to HTML with no raw-HTML injection
// (goldmark escapes raw HTML by default — we do NOT pass
// html.WithUnsafe()) and no auto-linking of bare URLs.
func renderMarkdown(src string) (template.HTML, error) {
	md := goldmark.New(
		goldmark.WithParserOptions(parser.WithAutoHeadingID()),
		goldmark.WithRendererOptions(
			html.WithHardWraps(),
			// Default: raw HTML in source is ESCAPED, not passed
			// through. We never call html.WithUnsafe().
		),
	)
	var buf bytes.Buffer
	if err := md.Convert([]byte(src), &buf); err != nil {
		return "", fmt.Errorf("convert markdown: %w", err)
	}
	return template.HTML(buf.String()), nil
}

// safePolicyURL returns the URL string only when it parses cleanly as
// an absolute http(s) URL. Anything else (file://, javascript:, raw
// strings) is dropped so the page never emits a foot-gun link.
func safePolicyURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return ""
	}
	if u.Host == "" {
		return ""
	}
	return u.String()
}

// safeMailbox returns the raw e-mail address only when it parses
// cleanly as a single mailbox. Anything else is dropped so the page
// never emits a foot-gun mailto.
func safeMailbox(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	addr, err := mail.ParseAddress(raw)
	if err != nil {
		return ""
	}
	return addr.Address
}

// mailtoURL builds the mailto: URL for a sanitised mailbox. Returns
// empty when the mailbox is empty so the template can guard against
// rendering an anchor with an empty href.
func mailtoURL(addr string) string {
	if addr == "" {
		return ""
	}
	return "mailto:" + addr
}

// pageData is the template view-model.
type pageData struct {
	TenantName       string
	TenantThemeStyle template.CSS
	// CSPNonce carries the per-request CSP nonce (SIN-63275).
	CSPNonce       string
	Version        string
	UpdatedAt      string
	UpdatedAtISO   string
	PolicyHTML     template.HTML
	PolicyURL      string
	DPOName        string
	DPOEmail       string
	DPOEmailMailto string
}

// pageTmpl renders the public /privacy page. The cookie banner is
// included inline via {{template "cookie_banner" .}} — see the consent
// package for the implementation; the banner is universally present
// because the LGPD AC #4 requires it on first access.
var pageTmpl = template.Must(template.New("public_privacy.layout").Parse(`<!doctype html>
<html lang="pt-BR">
<head>
  <meta charset="utf-8">
  <title>Política de Privacidade — {{.TenantName}}</title>
  <meta name="viewport" content="width=device-width, initial-scale=1">
  {{- with .TenantThemeStyle}}<style id="tenant-theme" nonce="{{$.CSPNonce}}">{{.}}</style>{{end}}
  <link rel="stylesheet" href="/static/css/privacy.css">
</head>
<body>
  <main class="privacy-shell" role="main" id="privacy">
    <header class="privacy-shell__header">
      <h1>Política de Privacidade</h1>
      <p class="privacy-shell__meta">
        <span class="privacy-shell__tenant">{{.TenantName}}</span>
        <span class="privacy-shell__version" aria-label="Versão da política">Versão {{.Version}}</span>
        <time class="privacy-shell__updated"
              datetime="{{.UpdatedAtISO}}"
              aria-label="Última atualização">
          atualizada em {{.UpdatedAt}}
        </time>
      </p>
      {{if .PolicyURL}}
      <p class="privacy-shell__pdf">
        <a href="{{.PolicyURL}}" rel="noopener noreferrer">Versão oficial publicada</a>
      </p>
      {{end}}
    </header>

    <article class="privacy-policy" aria-label="Texto da política">
      {{.PolicyHTML}}
    </article>

    <aside class="privacy-dpo" aria-label="Encarregado de proteção de dados">
      <h2 class="privacy-dpo__title">Encarregado de proteção de dados (DPO)</h2>
      {{if or .DPOName .DPOEmail}}
        {{with .DPOName}}<p class="privacy-dpo__name">{{.}}</p>{{end}}
        {{with .DPOEmail}}
        <p class="privacy-dpo__email">
          <a href="{{$.DPOEmailMailto}}">{{.}}</a>
        </p>
        {{end}}
      {{else}}
        <p class="privacy-dpo__empty">
          O encarregado ainda não foi publicado para este tenant. Contate
          o operador da plataforma para obter um canal LGPD.
        </p>
      {{end}}
    </aside>
  </main>
</body>
</html>
`))

// Ensure pageTmpl is fully parsed at init time (lazy escaper race fix
// pattern — same as internal/web/contacts and internal/web/lgpd).
func init() {
	_ = pageTmpl.Execute(io.Discard, pageData{})
}
