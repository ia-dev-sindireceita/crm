// Package dashboard is the HTMX managerial dashboard UI (SIN-65008,
// frontend half of the Dashboard / relatórios epic SIN-64963). It renders
// the read-only indicators produced by the metrics read-model
// (internal/metrics, SIN-65007) and offers a CSV export of the channel-
// volume + conversation-counter report.
//
// The handler depends only on a tiny SnapshotUseCase port (satisfied by
// *internal/metrics/usecase.GetDashboard) plus the tenancy / branding /
// CSP context helpers — never on a storage driver, so it stays on the
// right side of the hexagonal boundary. Every surface is a GET (read-
// only): there are no forms, no inline on*= handlers (the strict CSP of
// SIN-63977 would render-but-never-execute them), and therefore no CSRF
// token to thread.
package dashboard

import (
	"context"
	"encoding/csv"
	"errors"
	"html/template"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/branding"
	"github.com/pericles-luz/crm/internal/http/middleware/csp"
	"github.com/pericles-luz/crm/internal/metrics"
	"github.com/pericles-luz/crm/internal/tenancy"
	"github.com/pericles-luz/crm/internal/web/shell"
)

// SnapshotUseCase is the read port the dashboard depends on. The concrete
// *metrics/usecase.GetDashboard satisfies it. Passing the zero time.Time
// lets the use case apply its default 30-day window, so the handler never
// has to own the window policy.
type SnapshotUseCase interface {
	Execute(ctx context.Context, tenantID uuid.UUID, since time.Time) (metrics.DashboardMetrics, error)
}

// CSRFTokenFn returns the request's CSRF token (sourced from the session
// by the auth middleware). The dashboard has no forms of its own, but the
// shared app-shell renders the logout form, which needs the token; an
// empty token degrades to a logout form without the hidden field rather
// than failing the read-only page.
type CSRFTokenFn func(*http.Request) string

// UserIDFn returns the authenticated user id for the app-shell user-menu
// label. Returning uuid.Nil renders the "Conta" placeholder.
type UserIDFn func(*http.Request) uuid.UUID

// Deps bundles the handler's collaborators. Snapshot is required; Logger
// defaults to slog.Default() when nil. CSRFToken and UserID are optional
// app-shell chrome collaborators (SIN-65122): when nil the page still
// renders, with the logout form omitting its CSRF hidden field and the
// user-menu showing the "Conta" placeholder.
type Deps struct {
	Snapshot  SnapshotUseCase
	Logger    *slog.Logger
	CSRFToken CSRFTokenFn
	UserID    UserIDFn
}

// Handler is the dashboard front controller. It is mounted on the
// authenticated mux at /dashboard and /dashboard/export.csv — see Routes.
type Handler struct {
	deps Deps
}

// New wires the Handler. Returns an error when the required Snapshot
// dependency is missing; the composition root treats that as a
// programming error.
func New(deps Deps) (*Handler, error) {
	if deps.Snapshot == nil {
		return nil, errors.New("web/dashboard: Snapshot is required")
	}
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	return &Handler{deps: deps}, nil
}

// Routes registers the dashboard handlers on mux. Go 1.22 ServeMux
// method+pattern syntax so chi's outer match and the inner mux agree on
// the verbs. Both routes are reads.
func (h *Handler) Routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /dashboard", h.page)
	mux.HandleFunc("GET /dashboard/export.csv", h.exportCSV)
}

// page renders the full dashboard page: conversation counters, SLA
// percentiles (first-response + the resolution proxy), volume per channel
// and the funnel-stage distribution. The aggregation window defaults to
// the use case's 30-day lookback (zero time.Time).
func (h *Handler) page(w http.ResponseWriter, r *http.Request) {
	tenant, err := tenancy.FromContext(r.Context())
	if err != nil {
		h.fail(w, http.StatusInternalServerError, "tenant required", err)
		return
	}
	snap, err := h.deps.Snapshot.Execute(r.Context(), tenant.ID, time.Time{})
	if err != nil {
		h.fail(w, http.StatusInternalServerError, "snapshot", err)
		return
	}

	data := newPageData(snap, r)
	data.TenantName = tenant.Name
	data.UserDisplayName = displayNameForUser(h.userID(r))
	data.NavItems = buildDashboardNavItems()
	data.UserMenuItems = buildDashboardUserMenu()
	data.CSRFToken = h.csrfToken(r)

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := dashboardLayoutTmpl.Execute(w, data); err != nil {
		h.deps.Logger.Error("web/dashboard: render page", "err", err)
	}
}

// userID resolves the session user id through the optional UserID dep,
// returning uuid.Nil (→ "Conta" in the shell) when it is not wired.
func (h *Handler) userID(r *http.Request) uuid.UUID {
	if h.deps.UserID == nil {
		return uuid.Nil
	}
	return h.deps.UserID(r)
}

// csrfToken resolves the session CSRF token through the optional CSRFToken
// dep, returning "" (logout form renders without the hidden field) when it
// is not wired.
func (h *Handler) csrfToken(r *http.Request) string {
	if h.deps.CSRFToken == nil {
		return ""
	}
	return h.deps.CSRFToken(r)
}

// buildDashboardNavItems returns the SidebarNav primary nav for the
// dashboard page (SIN-65122). It mirrors the inbox/funnel/contacts set so
// the seed-role (atendente) post-login surfaces share one persistent nav,
// with "Painel" marked active so the shell stamps aria-current="page".
// The brand link back to /hello-tenant is owned by the shell layout.
func buildDashboardNavItems() []shell.NavItem {
	return []shell.NavItem{
		{Label: "Inbox", Path: "/inbox"},
		{Label: "Funil", Path: "/funnel"},
		{Label: "Contatos", Path: "/contacts"},
		{Label: "Painel", Path: "/dashboard", Active: true},
	}
}

// buildDashboardUserMenu returns the user-menu dropdown entries common to
// authenticated dashboard sessions (logout only, matching inbox/funnel).
func buildDashboardUserMenu() []shell.UserMenuItem {
	return []shell.UserMenuItem{
		{Label: "Sair", Path: "/logout", Form: true},
	}
}

// displayNameForUser is the placeholder display formatter for the
// user-menu button. The session does not (yet) carry a human label, so we
// render the uuid prefix — replace once a user-name resolver lands.
// Mirrors internal/web/inbox.displayNameForUser; kept local because the
// two web packages do not share a helper module.
func displayNameForUser(userID uuid.UUID) string {
	if userID == uuid.Nil {
		return "Conta"
	}
	s := userID.String()
	if len(s) > 8 {
		return s[:8]
	}
	return s
}

// exportCSV streams the channel-volume + conversation-counter report as a
// downloadable CSV (AC: "ao menos um relatório exportável"). It uses a
// long format — section,label,value — so a single sheet carries the
// counters, the per-channel volume, the SLA percentiles (in seconds) and
// the funnel distribution without a ragged table. Content-Disposition
// marks it as an attachment so browsers download rather than render it.
func (h *Handler) exportCSV(w http.ResponseWriter, r *http.Request) {
	tenant, err := tenancy.FromContext(r.Context())
	if err != nil {
		h.fail(w, http.StatusInternalServerError, "tenant required", err)
		return
	}
	snap, err := h.deps.Snapshot.Execute(r.Context(), tenant.ID, time.Time{})
	if err != nil {
		h.fail(w, http.StatusInternalServerError, "snapshot", err)
		return
	}

	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="dashboard.csv"`)
	w.Header().Set("Cache-Control", "no-store")

	cw := csv.NewWriter(w)
	// Header row keeps the export self-describing for spreadsheet imports.
	rows := [][]string{{"section", "label", "value"}}

	// Conversation counters (open / closed).
	for _, s := range snap.ConversationsByState {
		rows = append(rows, []string{"conversations", stateLabel(s.State), strconv.FormatInt(s.Count, 10)})
	}
	// Volume per channel — the minimum the AC requires.
	for _, c := range snap.VolumeByChannel {
		rows = append(rows, []string{"channel", c.Channel, strconv.FormatInt(c.Count, 10)})
	}
	// SLA percentiles, emitted in whole seconds so the cell is numeric.
	rows = append(rows,
		[]string{"first_response", "p50_seconds", secondsCell(snap.FirstResponse.P50)},
		[]string{"first_response", "p90_seconds", secondsCell(snap.FirstResponse.P90)},
		[]string{"resolution_proxy", "p50_seconds", secondsCell(snap.Resolution.P50)},
		[]string{"resolution_proxy", "p90_seconds", secondsCell(snap.Resolution.P90)},
	)
	// Funnel distribution, ordered by the adapter (stage position).
	for _, s := range snap.FunnelByStage {
		rows = append(rows, []string{"funnel", s.Label, strconv.FormatInt(s.Count, 10)})
	}

	if err := cw.WriteAll(rows); err != nil {
		// WriteAll already flushed what it could; the header is sent, so we
		// can only log the truncation rather than change the status code.
		h.deps.Logger.Error("web/dashboard: write csv", "err", err)
		return
	}
	if err := cw.Error(); err != nil {
		h.deps.Logger.Error("web/dashboard: flush csv", "err", err)
	}
}

// secondsCell renders a duration as a whole-second integer for the CSV.
// A zero duration (empty sample) renders "0" so the column stays numeric
// — the human-facing "—" treatment lives in the HTML view, not the data
// export.
func secondsCell(d time.Duration) string {
	return strconv.FormatInt(int64(d/time.Second), 10)
}

// fail centralises the error path: the underlying error goes to the log,
// the response body carries only the generic status text.
func (h *Handler) fail(w http.ResponseWriter, status int, msg string, err error) {
	h.deps.Logger.Error("web/dashboard: "+msg, "err", err)
	http.Error(w, http.StatusText(status), status)
}

// pageData is the template-facing view model. It carries the raw read-
// model slices (the template formats them via templateFuncs) plus the
// per-request chrome (theme style + CSP nonce) so the page renders inside
// the tenant's palette without an inline-style CSP violation.
type pageData struct {
	Since                string
	ConversationsByState []metrics.StateCount
	VolumeByChannel      []metrics.ChannelCount
	FirstResponse        metrics.Percentiles
	Resolution           metrics.Percentiles
	FunnelByStage        []metrics.StageCount
	HasFunnel            bool
	HasChannels          bool
	HasStates            bool
	TenantThemeStyle     template.CSS
	CSPNonce             string

	// Derived metric-card values — pre-computed so the template stays simple.
	OpenCount   int64
	ClosedCount int64

	// shell.Data chrome fields (SIN-65122) — read by shell.Layout's
	// reflection helpers (shellTenantName, shellNavItems, …) so the
	// dashboard renders inside the global SidebarNav app-shell.
	TenantName      string
	TenantLogo      string
	UserDisplayName string
	NavItems        []shell.NavItem
	UserMenuItems   []shell.UserMenuItem
	CSRFToken       string
}

// newPageData projects a read-model snapshot + request onto the template
// view model. The Has* flags drive the empty-state copy so an empty
// tenant renders a friendly "sem dados" block instead of a bare table.
func newPageData(snap metrics.DashboardMetrics, r *http.Request) pageData {
	return pageData{
		Since:                snap.Since.Format("02/01/2006"),
		ConversationsByState: snap.ConversationsByState,
		VolumeByChannel:      snap.VolumeByChannel,
		FirstResponse:        snap.FirstResponse,
		Resolution:           snap.Resolution,
		FunnelByStage:        snap.FunnelByStage,
		HasFunnel:            len(snap.FunnelByStage) > 0,
		HasChannels:          len(snap.VolumeByChannel) > 0,
		HasStates:            len(snap.ConversationsByState) > 0,
		TenantThemeStyle:     branding.ThemeStyleFromContext(r.Context()),
		CSPNonce:             csp.Nonce(r.Context()),
		OpenCount:            stateCount(snap.ConversationsByState, "open"),
		ClosedCount:          stateCount(snap.ConversationsByState, "closed"),
	}
}

// stateCount extracts the conversation count for a specific lifecycle
// state from the slice returned by the metrics read model.
func stateCount(states []metrics.StateCount, state string) int64 {
	for _, s := range states {
		if s.State == state {
			return s.Count
		}
	}
	return 0
}
