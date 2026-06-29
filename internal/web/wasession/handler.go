package wasession

import (
	"context"
	"errors"
	"html/template"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/branding"
	"github.com/pericles-luz/crm/internal/http/middleware/csp"
	"github.com/pericles-luz/crm/internal/tenancy"
	"github.com/pericles-luz/crm/internal/web/shell"
	"github.com/pericles-luz/crm/internal/web/userlabel"
)

// BasePath is the surface root. The router enumerates this exact path plus
// the "/status", "/consent", "/connect" and "/disconnect" sub-paths.
const BasePath = "/settings/whatsapp-session"

// NoticeVersion stamps the ban-risk notice the operator consents to. Bump
// it when the notice copy changes materially: a recorded grant for an old
// version no longer counts as consent for the current notice, so the
// operator must re-accept before the session can be (re)activated.
const NoticeVersion = "wa-session-risk-2026-06-29"

// SessionSnapshot is the provisioning state the Provisioner reports.
// QRPayload is a bearer secret (the WhatsApp Web pairing code): the handler
// renders it into an inline SVG and never logs it.
type SessionSnapshot struct {
	// Status is the wasession.Status string ("unpaired", "pairing",
	// "connected", "disconnected", "banned") or "" when no session is
	// provisioned for the tenant.
	Status string
	// Active reports whether a session is currently provisioned (started)
	// for the tenant — false means the operator has not connected yet (or
	// has disconnected).
	Active bool
	// QRPayload is the current pairing code, non-empty only while the
	// session is pairing. Secret: rendered to SVG, never logged.
	QRPayload string
}

// Provisioner is the transport-facing port: it reports the session state
// and starts / stops the tenant's session. The cmd/server wire binds it to
// the Fase 1 Manager plus the QR cache; the handler never sees whatsmeow.
type Provisioner interface {
	Snapshot(ctx context.Context, tenantID uuid.UUID) (SessionSnapshot, error)
	// Connect activates (or reconnects) the tenant session, beginning QR
	// pairing when the session is not yet paired.
	Connect(ctx context.Context, tenantID uuid.UUID) error
	// Disconnect tears the tenant session down without clearing its
	// credentials.
	Disconnect(ctx context.Context, tenantID uuid.UUID) error
}

// ConsentState is the latest recorded ban-risk consent for a (tenant, user).
type ConsentState struct {
	Granted bool
	Version string
	At      time.Time
}

// ConsentInput is one informed-consent grant to persist.
type ConsentInput struct {
	TenantID  uuid.UUID
	UserID    uuid.UUID
	Version   string
	IP        netip.Addr
	UserAgent string
}

// ConsentGate is the audited consent port. The wire binds it to the
// internal/iam/consent RecordingRegistry (terms-of-service purpose), so a
// Record call writes both the consent row and an audit-log entry; Latest
// reads the most recent grant for gating.
type ConsentGate interface {
	Latest(ctx context.Context, tenantID, userID uuid.UUID) (ConsentState, error)
	Record(ctx context.Context, in ConsentInput) error
}

// CSRFTokenFn / UserIDFn mirror the dashboard surface: optional app-shell
// chrome collaborators sourced from the session by the auth middleware.
type CSRFTokenFn func(*http.Request) string

// UserIDFn returns the authenticated user id. Required: consent is recorded
// and gated per operator, so the surface refuses to render without it.
type UserIDFn func(*http.Request) uuid.UUID

// Deps bundles the handler collaborators. Provisioner, Consent and UserID
// are required; the rest default (Logger → slog.Default, Now → time.Now)
// or degrade gracefully (CSRFToken/UserLabels nil → shell fallbacks).
type Deps struct {
	Provisioner Provisioner
	Consent     ConsentGate
	UserID      UserIDFn
	Logger      *slog.Logger
	CSRFToken   CSRFTokenFn
	UserLabels  userlabel.Directory
	Now         func() time.Time
}

// Handler is the provisioning front controller.
type Handler struct {
	deps Deps
}

// New validates and wires the Handler.
func New(deps Deps) (*Handler, error) {
	if deps.Provisioner == nil {
		return nil, errors.New("web/wasession: Provisioner is required")
	}
	if deps.Consent == nil {
		return nil, errors.New("web/wasession: Consent is required")
	}
	if deps.UserID == nil {
		return nil, errors.New("web/wasession: UserID is required")
	}
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	if deps.Now == nil {
		deps.Now = time.Now
	}
	return &Handler{deps: deps}, nil
}

// Routes registers the surface on mux. Go 1.22 method+pattern syntax so the
// chi outer match and this inner mux agree on the verbs. The router gates
// every pattern behind RequireAuth + RequireAction(gerente).
func (h *Handler) Routes(mux *http.ServeMux) {
	mux.HandleFunc("GET "+BasePath, h.page)
	mux.HandleFunc("GET "+BasePath+"/status", h.statusFragment)
	mux.HandleFunc("POST "+BasePath+"/consent", h.recordConsent)
	mux.HandleFunc("POST "+BasePath+"/connect", h.connect)
	mux.HandleFunc("POST "+BasePath+"/disconnect", h.disconnect)
}

// page renders the full provisioning page inside the app shell.
func (h *Handler) page(w http.ResponseWriter, r *http.Request) {
	tenant, ok := h.tenant(w, r)
	if !ok {
		return
	}
	panel := h.buildPanel(r.Context(), tenant.ID, h.deps.UserID(r), "")
	data := h.newPageData(r, tenant, panel)
	h.render(w, layoutTmpl, data)
}

// statusFragment serves the HTMX poll target: the status badge + QR, which
// refreshes itself every few seconds while the session is active.
func (h *Handler) statusFragment(w http.ResponseWriter, r *http.Request) {
	tenant, ok := h.tenant(w, r)
	if !ok {
		return
	}
	st := h.snapshotView(r.Context(), tenant.ID)
	h.render(w, statusTmpl, st)
}

// recordConsent persists the operator's informed ban-risk consent. The
// checkbox must be checked (input validation at the boundary); the grant is
// written through the audited ConsentGate with the caller IP + UA.
func (h *Handler) recordConsent(w http.ResponseWriter, r *http.Request) {
	tenant, ok := h.tenant(w, r)
	if !ok {
		return
	}
	userID := h.deps.UserID(r)
	if err := r.ParseForm(); err != nil {
		h.renderPanel(w, r, tenant.ID, userID, "Não foi possível processar o formulário.")
		return
	}
	if strings.TrimSpace(r.PostFormValue("accept_risk")) == "" {
		h.renderPanel(w, r, tenant.ID, userID,
			"É necessário marcar a caixa de ciência do risco para registrar o consentimento.")
		return
	}
	in := ConsentInput{
		TenantID:  tenant.ID,
		UserID:    userID,
		Version:   NoticeVersion,
		IP:        clientIP(r),
		UserAgent: clampUA(r.UserAgent()),
	}
	if err := h.deps.Consent.Record(r.Context(), in); err != nil {
		h.deps.Logger.Error("web/wasession: record consent", "err", err,
			"tenant_id", tenant.ID.String())
		h.renderPanel(w, r, tenant.ID, userID, "Falha ao registrar o consentimento. Tente novamente.")
		return
	}
	h.renderPanel(w, r, tenant.ID, userID, "")
}

// connect activates the session — but only when a current-notice consent
// grant exists. Without it the session is NOT started and the panel
// re-renders the consent form (deny-by-default; the session active state
// depends on the recorded consent).
func (h *Handler) connect(w http.ResponseWriter, r *http.Request) {
	tenant, ok := h.tenant(w, r)
	if !ok {
		return
	}
	userID := h.deps.UserID(r)
	consent, err := h.deps.Consent.Latest(r.Context(), tenant.ID, userID)
	if err != nil {
		h.deps.Logger.Error("web/wasession: load consent", "err", err, "tenant_id", tenant.ID.String())
		h.renderPanel(w, r, tenant.ID, userID, "Não foi possível verificar o consentimento.")
		return
	}
	if !consentCurrent(consent) {
		h.renderPanel(w, r, tenant.ID, userID,
			"Registre o consentimento de risco antes de ativar a sessão.")
		return
	}
	if err := h.deps.Provisioner.Connect(r.Context(), tenant.ID); err != nil {
		h.deps.Logger.Error("web/wasession: connect", "err", err, "tenant_id", tenant.ID.String())
		h.renderPanel(w, r, tenant.ID, userID, "Falha ao ativar a sessão. Tente novamente.")
		return
	}
	h.renderPanel(w, r, tenant.ID, userID, "")
}

// disconnect tears the session down. It needs no consent check — stopping
// is always allowed.
func (h *Handler) disconnect(w http.ResponseWriter, r *http.Request) {
	tenant, ok := h.tenant(w, r)
	if !ok {
		return
	}
	userID := h.deps.UserID(r)
	if err := h.deps.Provisioner.Disconnect(r.Context(), tenant.ID); err != nil {
		h.deps.Logger.Error("web/wasession: disconnect", "err", err, "tenant_id", tenant.ID.String())
		h.renderPanel(w, r, tenant.ID, userID, "Falha ao desconectar a sessão. Tente novamente.")
		return
	}
	h.renderPanel(w, r, tenant.ID, userID, "")
}

// renderPanel re-renders just the #wa-session-panel partial (the HTMX swap
// target shared by the consent / connect / disconnect POSTs).
func (h *Handler) renderPanel(w http.ResponseWriter, r *http.Request, tenantID, userID uuid.UUID, actionErr string) {
	panel := h.buildPanel(r.Context(), tenantID, userID, actionErr)
	h.render(w, panelTmpl, panel)
}

// buildPanel assembles the panel view model: consent state + session
// status. A consent / snapshot read error degrades to a safe, inert panel
// (no controls) rather than leaking the error to the operator.
func (h *Handler) buildPanel(ctx context.Context, tenantID, userID uuid.UUID, actionErr string) panelView {
	v := panelView{
		BasePath:      BasePath,
		NoticeVersion: NoticeVersion,
		ActionError:   actionErr,
	}
	consent, err := h.deps.Consent.Latest(ctx, tenantID, userID)
	if err != nil {
		h.deps.Logger.Error("web/wasession: latest consent", "err", err, "tenant_id", tenantID.String())
		// Fail closed: treat as not consented so no controls are offered.
		v.ConsentError = "Não foi possível verificar o consentimento."
		return v
	}
	v.Consented = consentCurrent(consent)
	if !consent.At.IsZero() {
		v.ConsentAt = consent.At.Format("02/01/2006 15:04")
		v.ConsentVersion = consent.Version
	}
	v.SessionStatus = h.snapshotView(ctx, tenantID)
	return v
}

// snapshotView projects the Provisioner snapshot onto the status fragment
// view model, rendering the QR inline when pairing.
func (h *Handler) snapshotView(ctx context.Context, tenantID uuid.UUID) statusView {
	snap, err := h.deps.Provisioner.Snapshot(ctx, tenantID)
	if err != nil {
		h.deps.Logger.Error("web/wasession: snapshot", "err", err, "tenant_id", tenantID.String())
		return statusView{BasePath: BasePath, Status: "error", Label: "Estado indisponível", Tone: "neutral"}
	}
	st := statusView{
		BasePath: BasePath,
		Status:   normalizeStatus(snap.Status),
		Active:   snap.Active,
	}
	st.Label = statusLabel(st.Status)
	st.Tone = statusTone(st.Status)
	st.ShouldPoll = st.Active && st.Status != "banned"
	if st.Status == "pairing" && snap.QRPayload != "" {
		svg, qerr := qrSVG(snap.QRPayload)
		if qerr != nil {
			h.deps.Logger.Error("web/wasession: render qr", "err", qerr, "tenant_id", tenantID.String())
		} else {
			st.HasQR = true
			st.QRSVG = svg
		}
	}
	return st
}

// tenant resolves the tenant from context, writing a 500 and returning
// ok=false when it is absent (a wiring/middleware failure, not user input).
func (h *Handler) tenant(w http.ResponseWriter, r *http.Request) (*tenancy.Tenant, bool) {
	t, err := tenancy.FromContext(r.Context())
	if err != nil {
		h.deps.Logger.Error("web/wasession: tenant required", "err", err)
		http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		return nil, false
	}
	return t, true
}

func (h *Handler) render(w http.ResponseWriter, tmpl *template.Template, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := tmpl.Execute(w, data); err != nil {
		h.deps.Logger.Error("web/wasession: render", "err", err)
	}
}

// newPageData wraps the panel in the shell chrome view model.
func (h *Handler) newPageData(r *http.Request, tenant *tenancy.Tenant, panel panelView) pageData {
	return pageData{
		Panel:            panel,
		TenantThemeStyle: branding.ThemeStyleFromContext(r.Context()),
		CSPNonce:         csp.Nonce(r.Context()),
		TenantName:       tenant.Name,
		UserDisplayName:  userlabel.Resolve(r.Context(), h.deps.UserLabels, tenant.ID, h.deps.UserID(r)),
		NavItems:         buildNavItems(),
		UserMenuItems:    buildUserMenu(),
		CSRFToken:        h.csrfToken(r),
	}
}

func (h *Handler) csrfToken(r *http.Request) string {
	if h.deps.CSRFToken == nil {
		return ""
	}
	return h.deps.CSRFToken(r)
}

// consentCurrent reports whether the grant counts as consent for the
// current notice: granted AND for the current NoticeVersion.
func consentCurrent(c ConsentState) bool {
	return c.Granted && c.Version == NoticeVersion
}

// clientIP extracts the caller IP from RemoteAddr. A parse failure yields
// the zero Addr (the consent adapter then writes NULL).
func clientIP(r *http.Request) netip.Addr {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	addr, err := netip.ParseAddr(strings.TrimSpace(host))
	if err != nil {
		return netip.Addr{}
	}
	return addr
}

// clampUA bounds the user-agent to the 4 KiB the consent audit row stores.
func clampUA(ua string) string {
	const max = 4 << 10
	if len(ua) > max {
		return ua[:max]
	}
	return ua
}

// buildNavItems / buildUserMenu mirror the dashboard chrome so the surface
// renders inside the shared SidebarNav app-shell. No item is marked active
// (the provisioning page lives under settings, outside the primary nav).
func buildNavItems() []shell.NavItem {
	return []shell.NavItem{
		{Label: "Inbox", Path: "/inbox", Icon: "inbox"},
		{Label: "Funil", Path: "/funnel", Icon: "git-branch"},
		{Label: "Contatos", Path: "/contacts", Icon: "users"},
		{Label: "Painel", Path: "/dashboard", Icon: "bar-chart"},
	}
}

func buildUserMenu() []shell.UserMenuItem {
	return []shell.UserMenuItem{
		{Label: "Sair", Path: "/logout", Form: true},
	}
}
