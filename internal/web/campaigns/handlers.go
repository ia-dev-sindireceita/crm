package campaigns

import (
	"context"
	"errors"
	"html/template"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/adapter/httpapi/csrf"
	"github.com/pericles-luz/crm/internal/campaigns"
	"github.com/pericles-luz/crm/internal/tenancy"
)

// MaxNameLen caps the marketer-supplied name. The Postgres column
// itself is text (no length cap); this is defense in depth at the UI
// boundary.
const MaxNameLen = 200

// MaxRedirectURLLen caps the redirect target. URLs above ~2 KiB rarely
// survive proxies anyway; we stop way short of that.
const MaxRedirectURLLen = 1024

// MaxUTMLen caps each UTM field. Long UTM values are typically broken
// inputs (HTML pasted in by mistake); rejecting them at the form keeps
// the storage row sane.
const MaxUTMLen = 128

// MaxSlugLen caps the slug. The redirect handler builds the URL as
// /c/<slug>; an overlong slug would yield an unusable link.
const MaxSlugLen = 80

// detailClickLimit caps the per-detail-view click ledger. The detail
// page is for at-a-glance drilling, not bulk export; a "load more"
// affordance lands when pagination becomes necessary.
const detailClickLimit = 100

// CampaignReader is the read-side dependency. Tests substitute a
// fake; the production wire feeds *postgres/campaigns.Store.
type CampaignReader interface {
	ListByTenant(ctx context.Context, tenantID uuid.UUID) ([]*campaigns.Campaign, error)
	GetBySlug(ctx context.Context, tenantID uuid.UUID, slug string) (*campaigns.Campaign, error)
}

// CampaignWriter is the write-side dependency.
type CampaignWriter interface {
	CreateCampaign(ctx context.Context, c *campaigns.Campaign) error
}

// CampaignStatsReader returns the rolled-up click + attribution
// counters keyed by Campaign.ID. Implementations MAY omit campaigns
// that have zero clicks; the handler treats absent keys as the zero
// value.
type CampaignStatsReader interface {
	StatsByTenant(ctx context.Context, tenantID uuid.UUID) (map[uuid.UUID]campaigns.CampaignStats, error)
}

// CampaignClickLister returns the per-campaign click ledger newest-
// first, bounded by limit. The dashboard detail page uses this for
// the drill-down table that HTMX re-polls.
type CampaignClickLister interface {
	ListClicks(ctx context.Context, tenantID, campaignID uuid.UUID, limit int) ([]*campaigns.CampaignClick, error)
}

// CSRFTokenFn returns the request's CSRF token.
type CSRFTokenFn func(*http.Request) string

// UserIDFn returns the authenticated user id used as the actor on
// every mutation. uuid.Nil collapses to 401 — the audit row would be
// meaningless without an actor.
type UserIDFn func(*http.Request) uuid.UUID

// IDFn generates a new uuid for each created Campaign. Injectable so
// tests can pin the value; production wires uuid.New.
type IDFn func() uuid.UUID

// NowFn returns the current time. Injectable so tests can pin it;
// production wires time.Now().UTC.
type NowFn func() time.Time

// Deps bundles the handler collaborators. Every read/write port is
// required; ID, Now, and Logger default to safe values.
type Deps struct {
	Reader    CampaignReader
	Writer    CampaignWriter
	Stats     CampaignStatsReader
	Clicks    CampaignClickLister
	CSRFToken CSRFTokenFn
	UserID    UserIDFn
	ID        IDFn
	Now       NowFn
	Logger    *slog.Logger
}

// Handler is the HTMX campaign dashboard front controller.
type Handler struct {
	deps Deps
}

// New constructs a Handler. Missing required deps are rejected at
// wire time so cmd/server fails fast.
func New(deps Deps) (*Handler, error) {
	if deps.Reader == nil {
		return nil, errors.New("web/campaigns: Reader is required")
	}
	if deps.Writer == nil {
		return nil, errors.New("web/campaigns: Writer is required")
	}
	if deps.Stats == nil {
		return nil, errors.New("web/campaigns: Stats is required")
	}
	if deps.Clicks == nil {
		return nil, errors.New("web/campaigns: Clicks is required")
	}
	if deps.CSRFToken == nil {
		return nil, errors.New("web/campaigns: CSRFToken is required")
	}
	if deps.UserID == nil {
		return nil, errors.New("web/campaigns: UserID is required")
	}
	if deps.ID == nil {
		deps.ID = uuid.New
	}
	if deps.Now == nil {
		deps.Now = func() time.Time { return time.Now().UTC() }
	}
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	return &Handler{deps: deps}, nil
}

// Routes mounts the dashboard endpoints on mux.
func (h *Handler) Routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /campaigns", h.list)
	mux.HandleFunc("GET /campaigns/new", h.newForm)
	mux.HandleFunc("POST /campaigns", h.create)
	mux.HandleFunc("GET /campaigns/{slug}", h.detail)
	mux.HandleFunc("GET /campaigns/{slug}/clicks", h.clicksFragment)
}

// list renders the dashboard shell with the campaign × stats roll-up.
func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	tenant, err := tenancy.FromContext(r.Context())
	if err != nil {
		h.fail(w, http.StatusInternalServerError, "tenant required", err)
		return
	}
	rows, err := h.deps.Reader.ListByTenant(r.Context(), tenant.ID)
	if err != nil {
		h.fail(w, http.StatusInternalServerError, "list campaigns", err)
		return
	}
	stats, err := h.deps.Stats.StatsByTenant(r.Context(), tenant.ID)
	if err != nil {
		h.fail(w, http.StatusInternalServerError, "load stats", err)
		return
	}
	token := h.deps.CSRFToken(r)
	if token == "" {
		h.fail(w, http.StatusInternalServerError, "csrf token missing", errors.New("empty csrf token"))
		return
	}
	now := h.deps.Now().UTC()
	view := listView{
		Rows:        rowsFrom(rows, stats, tenant.Host, now),
		GeneratedAt: now.Format(time.RFC3339),
		CSRFMeta:    csrf.MetaTag(token),
		HXHeaders:   csrf.HXHeadersAttr(token),
	}
	h.writeHTML(w, http.StatusOK, listLayoutTmpl, view)
}

// newForm renders the create-campaign form.
func (h *Handler) newForm(w http.ResponseWriter, r *http.Request) {
	token := h.deps.CSRFToken(r)
	if token == "" {
		h.fail(w, http.StatusInternalServerError, "csrf token missing", errors.New("empty csrf token"))
		return
	}
	h.writeHTML(w, http.StatusOK, formLayoutTmpl, formView{
		CSRFMeta:  csrf.MetaTag(token),
		HXHeaders: csrf.HXHeadersAttr(token),
	})
}

// create validates the submitted form, constructs the Campaign via
// the domain constructor, persists it, and re-renders the list
// partial so HTMX swaps it back inline.
func (h *Handler) create(w http.ResponseWriter, r *http.Request) {
	tenant, err := tenancy.FromContext(r.Context())
	if err != nil {
		h.fail(w, http.StatusInternalServerError, "tenant required", err)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	in, verr := parseForm(r)
	if !verr.IsZero() {
		h.renderFormError(w, r, in, verr)
		return
	}
	actor := h.deps.UserID(r)
	if actor == uuid.Nil {
		h.fail(w, http.StatusUnauthorized, "missing actor", errors.New("nil user id"))
		return
	}
	now := h.deps.Now().UTC()
	c, derr := campaigns.NewCampaign(h.deps.ID(), tenant.ID, in.Name, in.Slug, in.RedirectURL, in.ExpiresAt, now)
	if derr != nil {
		h.renderFormError(w, r, in, domainCreateMessage(derr))
		return
	}
	c.WithUTM(in.UTMSource, in.UTMMedium, in.UTMCampaign, in.UTMTerm, in.UTMContent)
	if err := h.deps.Writer.CreateCampaign(r.Context(), c); err != nil {
		if errors.Is(err, campaigns.ErrSlugAlreadyExists) {
			h.renderFormError(w, r, in, formError{Field: "slug", Message: "slug já está em uso para este tenant"})
			return
		}
		h.fail(w, http.StatusInternalServerError, "create campaign", err)
		return
	}
	h.renderListPartial(w, r, tenant, http.StatusCreated)
}

// detail renders the per-campaign drill-down: campaign metadata,
// rolled-up counters, and the first page of the click ledger.
func (h *Handler) detail(w http.ResponseWriter, r *http.Request) {
	tenant, err := tenancy.FromContext(r.Context())
	if err != nil {
		h.fail(w, http.StatusInternalServerError, "tenant required", err)
		return
	}
	slug := strings.TrimSpace(r.PathValue("slug"))
	if slug == "" {
		http.Error(w, "invalid slug", http.StatusBadRequest)
		return
	}
	camp, err := h.deps.Reader.GetBySlug(r.Context(), tenant.ID, slug)
	if err != nil {
		if errors.Is(err, campaigns.ErrNotFound) {
			http.Error(w, http.StatusText(http.StatusNotFound), http.StatusNotFound)
			return
		}
		h.fail(w, http.StatusInternalServerError, "get campaign", err)
		return
	}
	stats, err := h.deps.Stats.StatsByTenant(r.Context(), tenant.ID)
	if err != nil {
		h.fail(w, http.StatusInternalServerError, "load stats", err)
		return
	}
	clicks, err := h.deps.Clicks.ListClicks(r.Context(), tenant.ID, camp.ID, detailClickLimit)
	if err != nil {
		h.fail(w, http.StatusInternalServerError, "list clicks", err)
		return
	}
	token := h.deps.CSRFToken(r)
	if token == "" {
		h.fail(w, http.StatusInternalServerError, "csrf token missing", errors.New("empty csrf token"))
		return
	}
	now := h.deps.Now().UTC()
	h.writeHTML(w, http.StatusOK, detailLayoutTmpl, detailView{
		Row:         rowFrom(camp, stats[camp.ID], tenant.Host, now),
		Clicks:      clicksFrom(clicks),
		CSRFMeta:    csrf.MetaTag(token),
		HXHeaders:   csrf.HXHeadersAttr(token),
		GeneratedAt: now.Format(time.RFC3339),
	})
}

// clicksFragment serves the click table partial for HTMX hx-trigger
// polling. AC #2 requires newly-recorded clicks (via C8 ingest) to
// surface within 30s; the detail page wires hx-trigger="every 10s" so
// three poll intervals are guaranteed to overlap the budget.
func (h *Handler) clicksFragment(w http.ResponseWriter, r *http.Request) {
	tenant, err := tenancy.FromContext(r.Context())
	if err != nil {
		h.fail(w, http.StatusInternalServerError, "tenant required", err)
		return
	}
	slug := strings.TrimSpace(r.PathValue("slug"))
	if slug == "" {
		http.Error(w, "invalid slug", http.StatusBadRequest)
		return
	}
	camp, err := h.deps.Reader.GetBySlug(r.Context(), tenant.ID, slug)
	if err != nil {
		if errors.Is(err, campaigns.ErrNotFound) {
			http.Error(w, http.StatusText(http.StatusNotFound), http.StatusNotFound)
			return
		}
		h.fail(w, http.StatusInternalServerError, "get campaign", err)
		return
	}
	clicks, err := h.deps.Clicks.ListClicks(r.Context(), tenant.ID, camp.ID, detailClickLimit)
	if err != nil {
		h.fail(w, http.StatusInternalServerError, "list clicks", err)
		return
	}
	stats, err := h.deps.Stats.StatsByTenant(r.Context(), tenant.ID)
	if err != nil {
		h.fail(w, http.StatusInternalServerError, "load stats", err)
		return
	}
	h.writeHTML(w, http.StatusOK, clicksTableTmpl, clicksTableView{
		Stats:  statsView{Clicks: stats[camp.ID].Clicks, Attributions: stats[camp.ID].Attributions},
		Clicks: clicksFrom(clicks),
	})
}

// renderListPartial re-fetches the dashboard rows and writes the
// list-rows partial back. Used by POST /campaigns so the new row
// surfaces inline without a full page reload.
func (h *Handler) renderListPartial(w http.ResponseWriter, r *http.Request, tenant *tenancy.Tenant, status int) {
	rows, err := h.deps.Reader.ListByTenant(r.Context(), tenant.ID)
	if err != nil {
		h.fail(w, http.StatusInternalServerError, "list campaigns", err)
		return
	}
	stats, err := h.deps.Stats.StatsByTenant(r.Context(), tenant.ID)
	if err != nil {
		h.fail(w, http.StatusInternalServerError, "load stats", err)
		return
	}
	now := h.deps.Now().UTC()
	h.writeHTML(w, status, listRowsTmpl, listView{
		Rows: rowsFrom(rows, stats, tenant.Host, now),
	})
}

// renderFormError re-renders the form with the previous input and an
// inline error message. 422 mirrors the catalog admin convention. The
// CSRF token is sourced from the session (not from the user-submitted
// form field) so the rerendered form always carries the canonical
// token even when the user POSTed an HTMX-driven request whose body
// did not include _csrf.
func (h *Handler) renderFormError(w http.ResponseWriter, r *http.Request, in formInput, ferr formError) {
	token := h.deps.CSRFToken(r)
	h.writeHTML(w, http.StatusUnprocessableEntity, formLayoutTmpl, formView{
		Input:     in,
		Error:     ferr,
		CSRFMeta:  csrf.MetaTag(token),
		HXHeaders: csrf.HXHeadersAttr(token),
	})
}

func (h *Handler) writeHTML(w http.ResponseWriter, status int, tmpl *template.Template, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	if err := tmpl.Execute(w, data); err != nil {
		h.deps.Logger.Error("web/campaigns: render", "template", tmpl.Name(), "err", err)
	}
}

func (h *Handler) fail(w http.ResponseWriter, status int, msg string, err error) {
	h.deps.Logger.Error("web/campaigns: "+msg, "err", err)
	http.Error(w, http.StatusText(status), status)
}

// ---------------------------------------------------------------------------
// form parsing
// ---------------------------------------------------------------------------

// formInput is the raw, validated-by-shape (not by domain) form
// payload. The handler builds it from r.PostForm; the domain
// constructor then enforces the deeper invariants.
type formInput struct {
	Name        string
	Slug        string
	RedirectURL string
	UTMSource   string
	UTMMedium   string
	UTMCampaign string
	UTMTerm     string
	UTMContent  string
	ExpiresAt   *time.Time
	ExpiresRaw  string
}

// formError carries the field/message pair the form template renders
// inline next to the offending input.
type formError struct {
	Field   string
	Message string
}

// IsZero reports whether ferr carries no error (for the template to
// skip the alert block).
func (e formError) IsZero() bool { return e.Field == "" && e.Message == "" }

// parseForm extracts and length-checks the create-form payload. It
// trims every text field; the slug stays raw because NormalizeSlug
// lowers it inside the domain constructor.
func parseForm(r *http.Request) (formInput, formError) {
	get := func(k string) string { return strings.TrimSpace(r.PostFormValue(k)) }
	in := formInput{
		Name:        get("name"),
		Slug:        get("slug"),
		RedirectURL: get("redirect_url"),
		UTMSource:   get("utm_source"),
		UTMMedium:   get("utm_medium"),
		UTMCampaign: get("utm_campaign"),
		UTMTerm:     get("utm_term"),
		UTMContent:  get("utm_content"),
		ExpiresRaw:  get("expires_at"),
	}
	switch {
	case in.Name == "":
		return in, formError{Field: "name", Message: "nome é obrigatório"}
	case len(in.Name) > MaxNameLen:
		return in, formError{Field: "name", Message: "nome excede o tamanho máximo"}
	case in.Slug == "":
		return in, formError{Field: "slug", Message: "slug é obrigatório"}
	case len(in.Slug) > MaxSlugLen:
		return in, formError{Field: "slug", Message: "slug excede o tamanho máximo"}
	case in.RedirectURL == "":
		return in, formError{Field: "redirect_url", Message: "URL de destino é obrigatória"}
	case len(in.RedirectURL) > MaxRedirectURLLen:
		return in, formError{Field: "redirect_url", Message: "URL excede o tamanho máximo"}
	}
	for _, utm := range []struct{ field, val string }{
		{"utm_source", in.UTMSource},
		{"utm_medium", in.UTMMedium},
		{"utm_campaign", in.UTMCampaign},
		{"utm_term", in.UTMTerm},
		{"utm_content", in.UTMContent},
	} {
		if len(utm.val) > MaxUTMLen {
			return in, formError{Field: utm.field, Message: "campo UTM excede o tamanho máximo"}
		}
	}
	if in.ExpiresRaw != "" {
		// Accept the HTML <input type="datetime-local"> default shape
		// (no timezone) and parse it as UTC so storage stays canonical.
		t, err := time.Parse("2006-01-02T15:04", in.ExpiresRaw)
		if err != nil {
			return in, formError{Field: "expires_at", Message: "data inválida"}
		}
		expires := t.UTC()
		in.ExpiresAt = &expires
	}
	return in, formError{}
}

// domainCreateMessage maps a domain validation error to a user-
// visible inline form message.
func domainCreateMessage(err error) formError {
	switch {
	case errors.Is(err, campaigns.ErrInvalidName):
		return formError{Field: "name", Message: "nome é obrigatório"}
	case errors.Is(err, campaigns.ErrInvalidSlug):
		return formError{Field: "slug", Message: "slug deve usar apenas a-z, 0-9, e hifens"}
	case errors.Is(err, campaigns.ErrInvalidRedirectURL):
		return formError{Field: "redirect_url", Message: "URL deve usar http ou https"}
	case errors.Is(err, campaigns.ErrInvalidTenant):
		return formError{Field: "", Message: "tenant inválido"}
	default:
		return formError{Field: "", Message: "não foi possível criar a campanha"}
	}
}

// ---------------------------------------------------------------------------
// view shaping
// ---------------------------------------------------------------------------

// listView is the data shape the dashboard layout consumes.
type listView struct {
	Rows        []rowView
	GeneratedAt string
	CSRFMeta    template.HTML
	HXHeaders   template.HTMLAttr
}

// detailView is the per-campaign drill-down shape.
type detailView struct {
	Row         rowView
	Clicks      []clickRow
	CSRFMeta    template.HTML
	HXHeaders   template.HTMLAttr
	GeneratedAt string
}

// formView is the create-form layout shape. Input carries the
// previous submission so 422 re-renders preserve user typing.
type formView struct {
	Input     formInput
	Error     formError
	CSRFMeta  template.HTML
	HXHeaders template.HTMLAttr
}

// clicksTableView is the partial used by the HTMX poll. Stats refresh
// alongside the click rows so the counter pills next to the page
// header also update.
type clicksTableView struct {
	Stats  statsView
	Clicks []clickRow
}

// rowView is one row in the dashboard table.
type rowView struct {
	ID           string
	Slug         string
	Name         string
	Link         string
	Clicks       int64
	Attributions int64
	Status       string
	ExpiresLabel string
	IsExpired    bool
	DetailURL    string
}

// clickRow is one row in the click ledger drill-down.
type clickRow struct {
	When      string
	ClickID   string
	ContactID string
	IP        string
	UserAgent string
	Referrer  string
}

// statsView is the counter pair the HTMX poll refreshes.
type statsView struct {
	Clicks       int64
	Attributions int64
}

// rowsFrom merges the campaigns list with the per-campaign stats and
// renders each row's public link given the tenant host. A campaign
// absent from stats degrades to zero counters silently.
func rowsFrom(list []*campaigns.Campaign, stats map[uuid.UUID]campaigns.CampaignStats, host string, now time.Time) []rowView {
	out := make([]rowView, 0, len(list))
	for _, c := range list {
		out = append(out, rowFrom(c, stats[c.ID], host, now))
	}
	return out
}

// rowFrom renders one row.
func rowFrom(c *campaigns.Campaign, s campaigns.CampaignStats, host string, now time.Time) rowView {
	row := rowView{
		ID:           c.ID.String(),
		Slug:         c.Slug,
		Name:         c.Name,
		Link:         buildPublicLink(host, c),
		Clicks:       s.Clicks,
		Attributions: s.Attributions,
		Status:       string(c.Status),
		DetailURL:    "/campaigns/" + c.Slug,
	}
	if c.IsExpired(now) {
		row.IsExpired = true
		row.Status = string(campaigns.StatusExpired)
	}
	if c.ExpiresAt != nil {
		row.ExpiresLabel = c.ExpiresAt.UTC().Format("02/01/2006 15:04 MST")
	} else {
		row.ExpiresLabel = "sempre"
	}
	return row
}

// buildPublicLink renders the marketer-facing URL for a campaign.
// The redirect handler endpoint lives at /c/<slug>; UTM tags ship
// alongside so a click on the rendered link surfaces the same
// attribution the redirect would. host is the tenant.Host resolved
// by the TenantScope middleware; the scheme is always https in
// production (caller-supplied env-derived configuration is out of
// scope for this dashboard).
func buildPublicLink(host string, c *campaigns.Campaign) string {
	if host == "" {
		host = "crm.invalid"
	}
	q := url.Values{}
	if c.UTMSource != "" {
		q.Set("utm_source", c.UTMSource)
	}
	if c.UTMMedium != "" {
		q.Set("utm_medium", c.UTMMedium)
	}
	if c.UTMCampaign != "" {
		q.Set("utm_campaign", c.UTMCampaign)
	}
	if c.UTMTerm != "" {
		q.Set("utm_term", c.UTMTerm)
	}
	if c.UTMContent != "" {
		q.Set("utm_content", c.UTMContent)
	}
	base := "https://" + host + "/c/" + c.Slug
	if encoded := q.Encode(); encoded != "" {
		return base + "?" + encoded
	}
	return base
}

// clicksFrom renders the click ledger rows.
func clicksFrom(rows []*campaigns.CampaignClick) []clickRow {
	out := make([]clickRow, 0, len(rows))
	for _, c := range rows {
		row := clickRow{
			When:      c.CreatedAt.UTC().Format("02/01/2006 15:04:05"),
			ClickID:   c.ClickID,
			UserAgent: truncate(c.UserAgent, 64),
			Referrer:  truncate(c.Referrer, 64),
		}
		if c.ContactID != nil {
			row.ContactID = c.ContactID.String()
		} else {
			row.ContactID = "—"
		}
		if c.IP.IsValid() {
			row.IP = c.IP.String()
		} else {
			row.IP = "—"
		}
		out = append(out, row)
	}
	return out
}

func truncate(s string, n int) string {
	if n <= 0 || len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
