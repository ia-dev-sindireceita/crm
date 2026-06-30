package wasession

import (
	"html/template"
	"io"

	"github.com/pericles-luz/crm/internal/web/icon"
	"github.com/pericles-luz/crm/internal/web/shell"
)

// pageData is the full-page view model: the panel plus the shell chrome
// fields the layout reads by name via reflection.
type pageData struct {
	Panel panelView

	TenantThemeStyle template.CSS
	CSPNonce         string
	TenantName       string
	TenantLogo       string
	UserDisplayName  string
	NavItems         []shell.NavItem
	UserMenuItems    []shell.UserMenuItem
	CSRFToken        string
}

// panelView is the #wa-session-panel partial: consent state + session
// controls. It is the HTMX swap target for the consent / connect /
// disconnect POSTs.
type panelView struct {
	BasePath      string
	NoticeVersion string

	Consented      bool
	ConsentAt      string
	ConsentVersion string
	ConsentError   string
	ActionError    string

	SessionStatus statusView
}

// statusView is the #wa-session-status fragment: the live status badge and
// the QR. It re-fetches itself every few seconds while the session is
// active (ShouldPoll).
type statusView struct {
	BasePath   string
	Status     string
	Label      string
	Tone       string
	Active     bool
	HasQR      bool
	QRSVG      template.HTML
	ShouldPoll bool
}

// normalizeStatus maps the raw Provisioner status onto the closed set the
// templates key on. An empty status (no session provisioned) is "inactive";
// an unknown value falls back to "inactive" so the page stays coherent.
func normalizeStatus(s string) string {
	switch s {
	case "unpaired", "pairing", "connected", "disconnected", "banned", "error":
		return s
	default:
		return "inactive"
	}
}

// statusLabel renders the pt-BR operator-facing status label.
func statusLabel(status string) string {
	switch status {
	case "connected":
		return "Conectado"
	case "pairing":
		return "Pareando — escaneie o QR"
	case "disconnected":
		return "Desconectado — reconectando"
	case "banned":
		return "Banido — refaça o pareamento"
	case "unpaired":
		return "Não pareado"
	case "error":
		return "Estado indisponível"
	default:
		return "Inativo"
	}
}

// statusTone maps the status onto the Pitho StatusBadge tone modifier.
func statusTone(status string) string {
	switch status {
	case "connected":
		return "success"
	case "pairing":
		return "accent"
	case "disconnected":
		return "warning"
	case "banned":
		return "danger"
	default:
		return "neutral"
	}
}

// funcs are the template helpers. The icon helper is shared with the shell
// so fragments rendered standalone (status / panel) still resolve {{icon}}.
var funcs = func() template.FuncMap {
	fm := template.FuncMap{}
	for k, v := range icon.FuncMap() {
		fm[k] = v
	}
	return fm
}()

// fragmentDefs defines the "panel" and "status" sub-templates shared by the
// full page and the standalone partial renders.
const fragmentDefs = `
{{define "panel"}}
<div id="wa-session-panel" class="wa-session__panel">
  {{if .ConsentError}}<p class="wa-session__alert wa-session__alert--error" role="alert">{{.ConsentError}}</p>{{end}}
  {{if .ActionError}}<p class="wa-session__alert wa-session__alert--error" role="alert">{{.ActionError}}</p>{{end}}

  {{if not .Consented}}
  <section class="wa-session__consent card" aria-labelledby="wa-consent-title">
    <h2 id="wa-consent-title" class="wa-session__subtitle">Consentimento de risco</h2>
    <p class="wa-session__consent-text">
      A sessão não oficial do WhatsApp viola os Termos de Serviço do WhatsApp e
      <strong>pode resultar no banimento permanente</strong> do número conectado, sem aviso
      prévio e sem possibilidade de recuperação das conversas. Use apenas em números
      dedicados e cientes do risco.
    </p>
    <form class="wa-session__consent-form" hx-post="{{.BasePath}}/consent"
          hx-target="#wa-session-panel" hx-swap="outerHTML">
      <label class="wa-session__consent-check">
        <input type="checkbox" name="accept_risk" value="on" required>
        <span>Li e estou ciente do risco de banimento e aceito ativar a sessão não oficial.</span>
      </label>
      <button type="submit" class="btn btn--primary" data-testid="wa-consent-submit">
        Registrar consentimento
      </button>
    </form>
  </section>
  {{else}}
  <section class="wa-session__controls card" aria-labelledby="wa-controls-title">
    <h2 id="wa-controls-title" class="wa-session__subtitle">Sessão</h2>
    <p class="wa-session__consent-note">
      Consentimento de risco registrado em {{.ConsentAt}} (aviso {{.ConsentVersion}}).
    </p>

    {{template "status" .SessionStatus}}

    <div class="wa-session__actions">
      <button class="btn btn--primary" hx-post="{{.BasePath}}/connect"
              hx-target="#wa-session-panel" hx-swap="outerHTML" data-testid="wa-connect">
        {{if .SessionStatus.Active}}Reconectar{{else}}Conectar{{end}}
      </button>
      {{if .SessionStatus.Active}}
      <button class="btn btn--secondary" hx-post="{{.BasePath}}/disconnect"
              hx-target="#wa-session-panel" hx-swap="outerHTML" data-testid="wa-disconnect">
        Desconectar
      </button>
      {{end}}
    </div>
  </section>
  {{end}}
</div>
{{end}}

{{define "status"}}
<div id="wa-session-status" class="wa-session__status"
     {{if .ShouldPoll}}hx-get="{{.BasePath}}/status" hx-trigger="every 3s" hx-target="#wa-session-status" hx-swap="outerHTML"{{end}}
     data-status="{{.Status}}" data-testid="wa-status">
  <p class="wa-session__status-line">
    <span class="status-badge--pitho badge--{{.Tone}}" role="img" aria-label="Estado da sessão: {{.Label}}">{{.Label}}</span>
  </p>
  {{if .HasQR}}
  <figure class="wa-session__qr">
    {{.QRSVG}}
    <figcaption class="wa-session__qr-caption">
      Abra o WhatsApp no celular → Aparelhos conectados → Conectar um aparelho, e escaneie o código.
    </figcaption>
  </figure>
  {{end}}
</div>
{{end}}
`

// panelTmpl renders the panel partial standalone (POST responses).
var panelTmpl = mustFragment("wa-panel", `{{template "panel" .}}`)

// statusTmpl renders the status fragment standalone (GET /status poll).
var statusTmpl = mustFragment("wa-status", `{{template "status" .}}`)

// mustFragment builds a standalone fragment template whose root executes the
// named sub-template against the passed view model. The root body is parsed
// first so it owns rootName; the shared define blocks are then added to the
// same tree and resolved at execute time.
func mustFragment(rootName, rootBody string) *template.Template {
	t := template.Must(template.New(rootName).Funcs(funcs).Parse(rootBody))
	template.Must(t.Parse(fragmentDefs))
	// Prime the lazy escaper before any concurrent request (mirrors the
	// dashboard/inbox precedent); a nil-data prime errors harmlessly.
	_ = t.Execute(io.Discard, nil)
	return t
}

// layoutTmpl is the full provisioning page on the global SidebarNav shell.
// The risk notice + #wa-session-panel live in the shell "content" slot; the
// page stylesheet is injected via "head_extra".
var layoutTmpl = func() *template.Template {
	t := shell.MustParse(funcs, nil)
	template.Must(t.Parse(fragmentDefs))
	template.Must(t.Parse(`
{{define "title"}}WhatsApp (sessão) — provisionamento{{end}}
{{define "head_extra"}}
  <link rel="stylesheet" href="/static/css/wa-session.css">
  <script src="/static/vendor/htmx/2.0.9/htmx.min.js" nonce="{{shellCSPNonce .}}" defer></script>
{{end}}
{{define "content"}}
  <div class="wa-session" data-testid="wa-session">
    <div class="wa-session__header">
      <h1 class="wa-session__title">WhatsApp (sessão não oficial)</h1>
      <p class="wa-session__lead">
        Conecte um número de WhatsApp por QR para atender pelo inbox sem a API oficial.
        Leia o aviso de risco antes de ativar.
      </p>
    </div>

    <aside class="wa-session__risk card" role="note" aria-label="Aviso de risco">
      <span class="wa-session__risk-icon" aria-hidden="true">{{icon "alert-triangle" 20}}</span>
      <div class="wa-session__risk-body">
        <strong>Risco de banimento.</strong>
        A sessão não oficial contraria os Termos de Serviço do WhatsApp e pode levar ao
        banimento permanente do número. O uso é por sua conta e risco.
      </div>
    </aside>

    {{template "panel" .Panel}}
  </div>
{{end}}
`))
	_ = t.Execute(io.Discard, nil)
	return t.Lookup("layout")
}()
