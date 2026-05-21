package lgpd

// SIN-63191 / Fase 6 PR4 — HTMX UI for the LGPD admin surface.
//
// Adds two HTML pages on top of the SIN-63186 JSON/ZIP endpoints:
//
//   GET /admin/contacts/{contactID}/lgpd
//   GET /admin/lgpd/requests[?status=...]
//
// The export form on the contact page targets the existing
// GET /admin/lgpd/export?contact_id=... — same handler, no new
// transport. The delete form posts a regular x-www-form-urlencoded
// body so the surface degrades to a non-HTMX POST when JS is off
// (progressive enhancement lens).

import (
	"errors"
	"fmt"
	"html/template"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/adapter/httpapi/csrf"
	"github.com/pericles-luz/crm/internal/branding"
	"github.com/pericles-luz/crm/internal/iam"
	domainaudit "github.com/pericles-luz/crm/internal/iam/audit"
	domain "github.com/pericles-luz/crm/internal/lgpd"
	"github.com/pericles-luz/crm/internal/tenancy"
)

// UIDeps bundles the collaborators the HTML pages need on top of the
// existing JSON/ZIP handlers.
type UIDeps struct {
	// Deletions is the same DeletionRepository used by the JSON
	// endpoints. The HTMX delete-form handler calls Upsert with the
	// same retention policy as POST /admin/lgpd/delete.
	Deletions domain.DeletionRepository

	// Lister backs the /admin/lgpd/requests page. Production wires
	// it to the same postgres Store that satisfies Deletions; the
	// port is split so existing repository consumers do not have to
	// satisfy a new method (accept-broad / return-narrow).
	Lister domain.DeletionLister

	// Audit writes one row per HTMX delete submission so the AC #6
	// audit obligation is satisfied at the same call site as the
	// JSON delete handler. Nil falls back to a no-op writer — tests
	// already exercise the wired path via the JSON handler.
	Audit AuditWriter

	// Policy supplies the fiscal retention window for HTMX-driven
	// delete submissions. Defaults to the LGPD fiscal default.
	Policy domain.RetentionPolicy

	// CSRFToken returns the request's CSRF token for the form post.
	CSRFToken func(*http.Request) string

	// Now is the wall-clock source the requests-list page uses to
	// label rows as in_retention vs ready. Defaults to time.Now.
	Now func() time.Time
}

// UIHandler renders the LGPD admin HTML pages.
type UIHandler struct {
	h    *Handler
	deps UIDeps
}

// NewUI constructs the HTMX page handler. The inner h supplies the
// JSON/ZIP handlers via Export/Delete (same security envelope, same
// audit writer, same retention policy). Returns an error when a
// required dependency is missing so cmd/server fails fast.
func NewUI(h *Handler, deps UIDeps) (*UIHandler, error) {
	if h == nil {
		return nil, errors.New("web/lgpd: parent handler is required")
	}
	if deps.Deletions == nil {
		return nil, errors.New("web/lgpd: UI Deletions is required")
	}
	if deps.Lister == nil {
		return nil, errors.New("web/lgpd: UI Lister is required")
	}
	if deps.CSRFToken == nil {
		return nil, errors.New("web/lgpd: UI CSRFToken is required")
	}
	if deps.Policy.FiscalYears == 0 {
		deps.Policy = domain.RetentionPolicy{FiscalYears: domain.DefaultFiscalRetentionYears}
	}
	if deps.Now == nil {
		deps.Now = func() time.Time { return time.Now().UTC() }
	}
	return &UIHandler{h: h, deps: deps}, nil
}

// Routes mounts the HTML pages on mux.
//
//	GET /admin/contacts/{contactID}/lgpd
//	GET /admin/lgpd/requests
//	POST /admin/lgpd/delete-form
//
// The POST is the form-encoded twin of POST /admin/lgpd/delete; it
// lets the page submit a regular HTML <form> when JS is off (AC #6
// progressive enhancement).
func (u *UIHandler) Routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /admin/contacts/{contactID}/lgpd", u.contactPage)
	mux.HandleFunc("GET /admin/lgpd/requests", u.requestsPage)
	mux.HandleFunc("POST /admin/lgpd/delete-form", u.deleteForm)
}

// ContactPage and RequestsPage are exported so router tests can drive
// the handlers directly without going through the inner mux.
func (u *UIHandler) ContactPage(w http.ResponseWriter, r *http.Request)  { u.contactPage(w, r) }
func (u *UIHandler) RequestsPage(w http.ResponseWriter, r *http.Request) { u.requestsPage(w, r) }
func (u *UIHandler) DeleteForm(w http.ResponseWriter, r *http.Request)   { u.deleteForm(w, r) }

func (u *UIHandler) contactPage(w http.ResponseWriter, r *http.Request) {
	tenant, err := tenancy.FromContext(r.Context())
	if err != nil {
		http.Error(w, "tenant required", http.StatusInternalServerError)
		return
	}
	contactID, err := uuid.Parse(r.PathValue("contactID"))
	if err != nil {
		http.Error(w, "invalid contact id", http.StatusBadRequest)
		return
	}
	token := u.deps.CSRFToken(r)
	if token == "" {
		http.Error(w, "csrf token missing", http.StatusInternalServerError)
		return
	}
	data := contactPageData{
		CSRFMeta:         csrf.MetaTag(token),
		HXHeaders:        csrf.HXHeadersAttr(token),
		TenantThemeStyle: branding.ThemeStyleFromContext(r.Context()),
		Panel: contactPanelData{
			ContactID: contactID,
			TenantID:  tenant.ID,
			CSRFInput: csrf.FormHidden(token),
		},
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "private, no-store")
	if err := contactLayoutTmpl.Execute(w, data); err != nil {
		u.h.deps.Logger.Error("web/lgpd: render contact page", "err", err)
	}
}

func (u *UIHandler) requestsPage(w http.ResponseWriter, r *http.Request) {
	tenant, err := tenancy.FromContext(r.Context())
	if err != nil {
		http.Error(w, "tenant required", http.StatusInternalServerError)
		return
	}
	raw := strings.TrimSpace(r.URL.Query().Get("status"))
	filter, label, err := parseStatusFilter(raw)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	now := u.deps.Now()
	// "in_retention" is a synthetic UI status: we ask the repo for the
	// pending rows and partition by retention_until in the renderer.
	queryStatus := filter
	if filter == domain.InRetention {
		queryStatus = string(domain.DeletionStatusPending)
	}
	rows, err := u.deps.Lister.ListByTenant(r.Context(), tenant.ID, domain.DeletionStatus(queryStatus), requestListLimit)
	if err != nil {
		u.h.deps.Logger.Error("web/lgpd: list requests", "err", err.Error())
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	view := make([]requestRow, 0, len(rows))
	for _, row := range rows {
		display := derivedStatus(row, now)
		if filter == domain.InRetention && display != domain.InRetention {
			continue
		}
		view = append(view, requestRow{
			ID:             row.ID,
			ContactID:      row.ContactID,
			Justification:  row.Justification,
			Status:         display,
			Label:          statusLabel(display),
			CreatedAt:      row.CreatedAt,
			RetentionUntil: row.RetentionUntil,
			CompletedAt:    row.CompletedAt,
		})
	}
	token := u.deps.CSRFToken(r)
	if token == "" {
		http.Error(w, "csrf token missing", http.StatusInternalServerError)
		return
	}
	data := requestsPageData{
		CSRFMeta:         csrf.MetaTag(token),
		HXHeaders:        csrf.HXHeadersAttr(token),
		TenantThemeStyle: branding.ThemeStyleFromContext(r.Context()),
		Panel: requestsPanelData{
			Status:      filter,
			StatusLabel: label,
			Filters:     statusFilters(),
			Rows:        view,
		},
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "private, no-store")
	if err := requestsLayoutTmpl.Execute(w, data); err != nil {
		u.h.deps.Logger.Error("web/lgpd: render requests page", "err", err)
	}
}

// deleteForm is the form-encoded mirror of POST /admin/lgpd/delete. The
// JSON endpoint stays in place for API callers; this one lets the page
// degrade to a regular HTML form when JS is off (progressive
// enhancement lens). Same audit row, same retention policy, same
// Idempotent upsert — only the wire format and the redirect-back shape
// differ.
func (u *UIHandler) deleteForm(w http.ResponseWriter, r *http.Request) {
	tenant, err := tenancy.FromContext(r.Context())
	if err != nil {
		http.Error(w, "tenant required", http.StatusInternalServerError)
		return
	}
	principal, _ := iam.PrincipalFromContext(r.Context())
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	contactID, err := uuid.Parse(strings.TrimSpace(r.PostFormValue("contact_id")))
	if err != nil {
		http.Error(w, "invalid contact id", http.StatusBadRequest)
		return
	}
	just := strings.TrimSpace(r.PostFormValue("justification"))
	if just == "" {
		http.Error(w, "justification is required", http.StatusBadRequest)
		return
	}
	if len(just) > 4096 {
		http.Error(w, "justification too long", http.StatusBadRequest)
		return
	}
	now := u.deps.Now()
	dr := domain.DeletionRequest{
		TenantID:          tenant.ID,
		ContactID:         contactID,
		RequestedByUserID: principal.UserID,
		Justification:     just,
		Status:            domain.DeletionStatusPending,
		RetentionUntil:    u.deps.Policy.RetentionUntil(now),
		CreatedAt:         now,
	}
	out, err := u.deps.Deletions.Upsert(r.Context(), dr)
	if err != nil {
		u.h.deps.Logger.Error("web/lgpd: ui delete upsert", "err", err.Error())
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if u.deps.Audit != nil {
		_ = u.deps.Audit.WriteData(r.Context(), domainaudit.DataAuditEvent{
			Event:       domainaudit.DataEventLGPDForget,
			ActorUserID: principal.UserID,
			TenantID:    tenant.ID,
			Target: map[string]any{
				"contact_id": contactID.String(),
				"actor_ip":   clientIP(r),
				"source":     "ui",
			},
			OccurredAt: now,
		})
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "private, no-store")
	frag := deleteAckFragment{
		RequestID:      out.ID,
		ContactID:      out.ContactID,
		RetentionUntil: out.RetentionUntil,
	}
	if err := deleteAckTmpl.Execute(w, frag); err != nil {
		u.h.deps.Logger.Error("web/lgpd: render delete ack", "err", err)
	}
}

// requestListLimit caps the requests-page query so a tenant with a
// long history (LGPD requests are forever-retained for audit) does not
// hand the operator an unbounded list. Pagination is a follow-up; the
// cap mirrors the worker's ListReady default.
const requestListLimit = 200

// requestRow is the projection the requests template iterates over.
type requestRow struct {
	ID             uuid.UUID
	ContactID      uuid.UUID
	Justification  string
	Status         string
	Label          string
	CreatedAt      time.Time
	RetentionUntil time.Time
	CompletedAt    *time.Time
}

// statusFilter is one selectable row in the filter dropdown.
type statusFilter struct {
	Value string
	Label string
}

// parseStatusFilter normalises the ?status= query value into either an
// empty string ("all"), one of the persisted vocabulary values, or the
// synthetic "in_retention" label. An unknown value returns a 4xx so
// the page never silently filters by garbage.
func parseStatusFilter(raw string) (filterValue string, label string, err error) {
	switch raw {
	case "", "all":
		return "", "Todos", nil
	case string(domain.DeletionStatusPending):
		return raw, "Pendentes", nil
	case string(domain.DeletionStatusCompleted):
		return raw, "Concluídos", nil
	case string(domain.DeletionStatusFailed):
		return raw, "Falhados", nil
	case domain.InRetention:
		return raw, "Em retenção", nil
	default:
		return "", "", fmt.Errorf("invalid status filter %q", raw)
	}
}

// statusFilters lists the four selectable statuses for the dropdown.
// Kept fixed to the controlled vocabulary so a new persisted status
// cannot silently appear in the UI without a code change.
func statusFilters() []statusFilter {
	return []statusFilter{
		{Value: "", Label: "Todos"},
		{Value: string(domain.DeletionStatusPending), Label: "Pendentes"},
		{Value: domain.InRetention, Label: "Em retenção"},
		{Value: string(domain.DeletionStatusCompleted), Label: "Concluídos"},
		{Value: string(domain.DeletionStatusFailed), Label: "Falhados"},
	}
}

// statusLabel maps a persisted or synthetic status to a Portuguese
// label for the table cell.
func statusLabel(s string) string {
	switch s {
	case string(domain.DeletionStatusPending):
		return "Pendente"
	case string(domain.DeletionStatusCompleted):
		return "Concluído"
	case string(domain.DeletionStatusFailed):
		return "Falhou"
	case domain.InRetention:
		return "Em retenção"
	default:
		return s
	}
}

// derivedStatus maps the (Status, RetentionUntil) tuple onto the
// status string the UI surfaces. A pending row whose retention window
// has not elapsed is "in_retention"; a pending row past its retention
// window is "pending" (worker has not yet finalised but the wait is
// over). The rest pass through.
func derivedStatus(row domain.DeletionRequest, now time.Time) string {
	if row.Status == domain.DeletionStatusPending && row.RetentionUntil.After(now) {
		return domain.InRetention
	}
	return string(row.Status)
}

// contactPanelData drives the inner contact panel.
type contactPanelData struct {
	ContactID uuid.UUID
	TenantID  uuid.UUID
	CSRFInput template.HTML
}

// contactPageData drives the full-page contact view.
type contactPageData struct {
	CSRFMeta         template.HTML
	HXHeaders        template.HTMLAttr
	TenantThemeStyle template.CSS
	Panel            contactPanelData
}

// requestsPanelData drives the inner requests panel.
type requestsPanelData struct {
	Status      string
	StatusLabel string
	Filters     []statusFilter
	Rows        []requestRow
}

// requestsPageData drives the full-page requests view.
type requestsPageData struct {
	CSRFMeta         template.HTML
	HXHeaders        template.HTMLAttr
	TenantThemeStyle template.CSS
	Panel            requestsPanelData
}

// deleteAckFragment is the post-submit HTMX swap target.
type deleteAckFragment struct {
	RequestID      uuid.UUID
	ContactID      uuid.UUID
	RetentionUntil time.Time
}

// templateFuncs are the small helper set both templates use.
var templateFuncs = template.FuncMap{
	"fmtTime": formatTimeUTC,
	"fmtTimePtr": func(t *time.Time) string {
		if t == nil {
			return ""
		}
		return formatTimeUTC(*t)
	},
	"shortUUID": func(u uuid.UUID) string {
		s := u.String()
		if len(s) >= 8 {
			return s[:8]
		}
		return s
	},
}

// formatTimeUTC renders timestamps in a human-readable UTC form.
func formatTimeUTC(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format("2006-01-02 15:04 UTC")
}

// contactLayoutTmpl is the full-page shell for /admin/contacts/{id}/lgpd.
// The page never reloads the parent on submit — the delete form posts
// to /admin/lgpd/delete-form and the response replaces #lgpd-ack with
// the acknowledgement fragment. The export button is a plain GET form
// that hits the existing JSON/ZIP endpoint.
//
// hx-confirm carries the destructive-action confirmation modal so no
// extra JS is needed (AC #1). Accessibility: every button has a
// visible label and an aria-label fallback, focus is preserved by the
// browser default (form submit), and the destructive button uses
// aria-describedby to point at the confirm helper text.
var contactLayoutTmpl = template.Must(template.New("contact_lgpd.layout").Funcs(templateFuncs).Parse(`<!doctype html>
<html lang="pt-BR">
<head>
  <meta charset="utf-8">
  <title>LGPD — controles do contato</title>
  {{.CSRFMeta}}
  {{- with .TenantThemeStyle}}<style id="tenant-theme">{{.}}</style>{{end}}
  <link rel="stylesheet" href="/static/css/lgpd.css">
  <script src="/static/vendor/htmx/2.0.9/htmx.min.js" defer></script>
</head>
<body {{.HXHeaders}}>
  <main class="lgpd-shell" role="main">
    <header class="lgpd-shell__header">
      <h1>Controles LGPD do contato</h1>
      <p class="lgpd-shell__hint">
        Exporte os dados pessoais associados ao contato ou solicite a
        deleção definitiva. A deleção respeita a retenção fiscal
        obrigatória e é registrada em <em>audit_log_data</em>.
      </p>
    </header>
    {{template "contact_panel" .Panel}}
    <section id="lgpd-ack" aria-live="polite" class="lgpd-ack" data-empty="true"></section>
  </main>
</body>
</html>
`))

// contactPanelTmpl is the inner panel. Mounted as a named template so
// the layout includes it via {{template "contact_panel" .Panel}}.
var contactPanelTmpl = template.Must(template.New("contact_panel").Funcs(templateFuncs).Parse(`<section id="lgpd-panel" class="lgpd-panel" aria-label="Controles LGPD">
  <header class="lgpd-panel__header">
    <h2 class="lgpd-panel__title">Contato {{shortUUID .ContactID}}</h2>
  </header>
  <div class="lgpd-panel__actions">
    <form class="lgpd-panel__export"
          method="get"
          action="/admin/lgpd/export"
          aria-label="Exportar dados do contato">
      <input type="hidden" name="contact_id" value="{{.ContactID}}">
      <button type="submit" class="lgpd-btn lgpd-btn--primary"
              aria-label="Exportar dados pessoais do contato como ZIP">
        Exportar dados
      </button>
    </form>
    <form class="lgpd-panel__delete"
          method="post"
          action="/admin/lgpd/delete-form"
          hx-post="/admin/lgpd/delete-form"
          hx-target="#lgpd-ack"
          hx-swap="innerHTML"
          hx-confirm="Tem certeza? A deleção é definitiva — apenas dados fiscais permanecem retidos por força de lei."
          aria-describedby="lgpd-delete-help">
      {{.CSRFInput}}
      <input type="hidden" name="contact_id" value="{{.ContactID}}">
      <label class="lgpd-field">
        <span class="lgpd-field__label">Justificativa (obrigatória)</span>
        <textarea name="justification"
                  required
                  minlength="4"
                  maxlength="4096"
                  rows="3"
                  aria-required="true"
                  class="lgpd-field__input"></textarea>
      </label>
      <p id="lgpd-delete-help" class="lgpd-help">
        A solicitação inicia em "Em retenção" e é finalizada pelo worker
        depois do fim da janela de retenção fiscal.
      </p>
      <button type="submit" class="lgpd-btn lgpd-btn--danger"
              aria-label="Solicitar deleção definitiva do contato">
        Solicitar deleção
      </button>
    </form>
  </div>
</section>
`))

// requestsLayoutTmpl is the full-page shell for /admin/lgpd/requests.
var requestsLayoutTmpl = template.Must(template.New("lgpd_requests.layout").Funcs(templateFuncs).Parse(`<!doctype html>
<html lang="pt-BR">
<head>
  <meta charset="utf-8">
  <title>LGPD — solicitações de deleção</title>
  {{.CSRFMeta}}
  {{- with .TenantThemeStyle}}<style id="tenant-theme">{{.}}</style>{{end}}
  <link rel="stylesheet" href="/static/css/lgpd.css">
  <script src="/static/vendor/htmx/2.0.9/htmx.min.js" defer></script>
</head>
<body {{.HXHeaders}}>
  <main class="lgpd-shell" role="main">
    <header class="lgpd-shell__header">
      <h1>Solicitações de deleção</h1>
      <p class="lgpd-shell__hint">
        Histórico de solicitações LGPD do tenant. Use o filtro para ver
        apenas pendentes, em retenção, concluídas ou falhadas.
      </p>
    </header>
    {{template "requests_panel" .Panel}}
  </main>
</body>
</html>
`))

// requestsPanelTmpl renders the filter + the table. The filter form is
// a plain GET that re-renders the page; HTMX is layered as an
// enhancement that swaps just the panel.
var requestsPanelTmpl = template.Must(template.New("requests_panel").Funcs(templateFuncs).Parse(`<section id="lgpd-requests-panel" class="lgpd-panel" aria-label="Solicitações LGPD">
  <form class="lgpd-filters" method="get" action="/admin/lgpd/requests"
        hx-get="/admin/lgpd/requests"
        hx-target="#lgpd-requests-panel"
        hx-swap="outerHTML"
        aria-label="Filtrar por status">
    <label class="lgpd-field lgpd-field--inline">
      <span class="lgpd-field__label">Status</span>
      <select name="status" class="lgpd-field__input"
              onchange="this.form.requestSubmit()"
              aria-label="Filtrar status">
        {{range .Filters}}
          <option value="{{.Value}}"{{if eq .Value $.Status}} selected{{end}}>{{.Label}}</option>
        {{end}}
      </select>
    </label>
    <noscript>
      <button type="submit" class="lgpd-btn lgpd-btn--primary">Filtrar</button>
    </noscript>
  </form>

  {{if .Rows}}
  <table class="lgpd-requests" role="table" aria-label="Solicitações ({{.StatusLabel}})">
    <thead>
      <tr>
        <th scope="col">ID</th>
        <th scope="col">Contato</th>
        <th scope="col">Status</th>
        <th scope="col">Justificativa</th>
        <th scope="col">Criado em</th>
        <th scope="col">Retenção até</th>
        <th scope="col">Concluído em</th>
      </tr>
    </thead>
    <tbody>
      {{range .Rows}}
      <tr data-status="{{.Status}}">
        <td>{{shortUUID .ID}}</td>
        <td>{{shortUUID .ContactID}}</td>
        <td><span class="lgpd-badge lgpd-badge--{{.Status}}">{{.Label}}</span></td>
        <td class="lgpd-justification">{{.Justification}}</td>
        <td><time datetime="{{.CreatedAt.Format "2006-01-02T15:04:05Z07:00"}}">{{fmtTime .CreatedAt}}</time></td>
        <td><time datetime="{{.RetentionUntil.Format "2006-01-02T15:04:05Z07:00"}}">{{fmtTime .RetentionUntil}}</time></td>
        <td>{{fmtTimePtr .CompletedAt}}</td>
      </tr>
      {{end}}
    </tbody>
  </table>
  {{else}}
  <p class="lgpd-empty" role="status">Nenhuma solicitação para o filtro "{{.StatusLabel}}".</p>
  {{end}}
</section>
`))

// deleteAckTmpl is rendered into #lgpd-ack after a successful delete
// submission. Lives in this file so the entire flow stays inline.
var deleteAckTmpl = template.Must(template.New("lgpd_delete_ack").Funcs(templateFuncs).Parse(`<article class="lgpd-ack__msg" role="alert" aria-live="assertive">
  <h2 class="lgpd-ack__title">Solicitação registrada</h2>
  <p class="lgpd-ack__id">ID: {{shortUUID .RequestID}}</p>
  <p class="lgpd-ack__contact">Contato {{shortUUID .ContactID}}</p>
  <p class="lgpd-ack__retention">Retenção até {{fmtTime .RetentionUntil}}.</p>
</article>
`))

func init() {
	// Cross-register named templates so the layouts can include them.
	if _, err := contactLayoutTmpl.AddParseTree(contactPanelTmpl.Name(), contactPanelTmpl.Tree); err != nil {
		panic("web/lgpd: register contact_panel: " + err.Error())
	}
	if _, err := requestsLayoutTmpl.AddParseTree(requestsPanelTmpl.Name(), requestsPanelTmpl.Tree); err != nil {
		panic("web/lgpd: register requests_panel: " + err.Error())
	}
	// Prime html/template's lazy escaper (SIN-62774 race fix pattern).
	for _, t := range []*template.Template{
		contactPanelTmpl, contactLayoutTmpl,
		requestsPanelTmpl, requestsLayoutTmpl,
		deleteAckTmpl,
	} {
		_ = t.Execute(io.Discard, nil)
	}
}
