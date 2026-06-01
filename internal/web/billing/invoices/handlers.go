package invoices

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/billing"
	"github.com/pericles-luz/crm/internal/billing/dunning"
	"github.com/pericles-luz/crm/internal/billing/pix"
	"github.com/pericles-luz/crm/internal/branding"
	"github.com/pericles-luz/crm/internal/http/middleware/csp"
	"github.com/pericles-luz/crm/internal/tenancy"
	"github.com/pericles-luz/crm/internal/web/shell"
)

const pollIntervalPending = "every 10s"
const listLimit = 50

type InvoiceLister interface {
	ListByTenant(ctx context.Context, tenantID uuid.UUID) ([]*billing.Invoice, error)
}

type InvoiceGetter interface {
	GetByID(ctx context.Context, tenantID, invoiceID uuid.UUID) (*billing.Invoice, error)
}

type PIXChargeLister interface {
	LatestForInvoice(ctx context.Context, tenantID, invoiceID uuid.UUID) (*pix.PIXCharge, error)
}

type DunningStateReader interface {
	CurrentForTenant(ctx context.Context, tenantID uuid.UUID) (*dunning.DunningState, error)
}

// NextBillingDateFn returns the next billing date for the tenant's active
// subscription. Used for the empty-state copy. May be nil.
type NextBillingDateFn func(ctx context.Context, tenantID uuid.UUID) (time.Time, error)

type CSRFTokenFn func(*http.Request) string
type UserIDFn func(*http.Request) uuid.UUID
type NowFn func() time.Time
type NavItemsFn func(*http.Request) []shell.NavItem
type UserMenuItemsFn func(*http.Request) []shell.UserMenuItem

type Deps struct {
	Invoices        InvoiceLister
	Invoice         InvoiceGetter
	Charges         PIXChargeLister
	Dunning         DunningStateReader
	NextBillingDate NextBillingDateFn // optional
	CSRFToken       CSRFTokenFn
	UserID          UserIDFn
	NavItems        NavItemsFn      // optional
	UserMenuItems   UserMenuItemsFn // optional
	Now             NowFn
	Logger          *slog.Logger
}

type Handler struct {
	deps Deps
}

func New(deps Deps) (*Handler, error) {
	if deps.Invoices == nil {
		return nil, errors.New("web/billing/invoices: Invoices is required")
	}
	if deps.Invoice == nil {
		return nil, errors.New("web/billing/invoices: Invoice is required")
	}
	if deps.Charges == nil {
		return nil, errors.New("web/billing/invoices: Charges is required")
	}
	if deps.Dunning == nil {
		return nil, errors.New("web/billing/invoices: Dunning is required")
	}
	if deps.CSRFToken == nil {
		return nil, errors.New("web/billing/invoices: CSRFToken is required")
	}
	if deps.UserID == nil {
		return nil, errors.New("web/billing/invoices: UserID is required")
	}
	if deps.Now == nil {
		deps.Now = func() time.Time { return time.Now().UTC() }
	}
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	return &Handler{deps: deps}, nil
}

func (h *Handler) Routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /billing/invoices", h.list)
	mux.HandleFunc("GET /billing/invoices/{id}", h.detail)
	mux.HandleFunc("GET /billing/invoices/{id}/status", h.statusFragment)
	mux.HandleFunc("GET /billing/dunning-banner", h.bannerFragment)
}

func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	tenant, err := tenancy.FromContext(r.Context())
	if err != nil {
		h.fail(w, http.StatusInternalServerError, "tenant required", err)
		return
	}
	rows, err := h.deps.Invoices.ListByTenant(r.Context(), tenant.ID)
	if err != nil {
		h.fail(w, http.StatusInternalServerError, "list invoices", err)
		return
	}
	token := h.deps.CSRFToken(r)
	if token == "" {
		h.fail(w, http.StatusInternalServerError, "csrf token missing", errors.New("empty csrf token"))
		return
	}
	filter := filterFrom(r)
	filtered := applyFilter(rows, filter, listLimit)
	now := h.deps.Now().UTC()
	nextDate := h.nextBillingDateStr(r.Context(), tenant.ID)

	// HTMX filter partial swap: return only tbody innerHTML.
	if r.Header.Get("HX-Request") == "true" {
		fv := tbodyView{Rows: filtered, NextBillingDate: nextDate}
		h.writeHTML(w, http.StatusOK, tbodyFragmentTmpl, fv)
		return
	}

	banner, err := h.bannerFor(r.Context(), tenant.ID)
	if err != nil {
		h.fail(w, http.StatusInternalServerError, "load dunning", err)
		return
	}
	view := listView{
		Banner:           banner,
		Rows:             filtered,
		Filter:           filter,
		NextBillingDate:  nextDate,
		GeneratedAt:      now.Format(time.RFC3339),
		TenantName:       tenantName(tenant),
		CSRFToken:        token,
		CSPNonce:         csp.Nonce(r.Context()),
		TenantThemeStyle: branding.ThemeStyleFromContext(r.Context()),
		NavItems:         h.navItems(r),
		UserMenuItems:    h.userMenuItems(r),
	}
	h.renderShell(w, http.StatusOK, listLayoutTmpl, view)
}

func (h *Handler) detail(w http.ResponseWriter, r *http.Request) {
	tenant, err := tenancy.FromContext(r.Context())
	if err != nil {
		h.fail(w, http.StatusInternalServerError, "tenant required", err)
		return
	}
	invoiceID, ok := parseID(r.PathValue("id"))
	if !ok {
		http.Error(w, "invalid invoice id", http.StatusBadRequest)
		return
	}
	inv, err := h.deps.Invoice.GetByID(r.Context(), tenant.ID, invoiceID)
	if err != nil {
		if errors.Is(err, billing.ErrNotFound) {
			http.Error(w, http.StatusText(http.StatusNotFound), http.StatusNotFound)
			return
		}
		h.fail(w, http.StatusInternalServerError, "get invoice", err)
		return
	}
	token := h.deps.CSRFToken(r)
	if token == "" {
		h.fail(w, http.StatusInternalServerError, "csrf token missing", errors.New("empty csrf token"))
		return
	}
	banner, err := h.bannerFor(r.Context(), tenant.ID)
	if err != nil {
		h.fail(w, http.StatusInternalServerError, "load dunning", err)
		return
	}
	charge, chargeV, err := h.chargeViewFor(r.Context(), tenant.ID, invoiceID)
	if err != nil {
		h.fail(w, http.StatusInternalServerError, "load pix charge", err)
		return
	}
	now := h.deps.Now().UTC()
	view := detailView{
		Banner:           banner,
		Invoice:          invoiceRowFrom(inv),
		Charge:           chargeV,
		Status:           statusFragmentFrom(invoiceID, charge),
		TenantName:       tenantName(tenant),
		CSRFToken:        token,
		CSPNonce:         csp.Nonce(r.Context()),
		TenantThemeStyle: branding.ThemeStyleFromContext(r.Context()),
		NavItems:         h.navItems(r),
		UserMenuItems:    h.userMenuItems(r),
		GeneratedAt:      now.Format(time.RFC3339),
	}
	h.renderShell(w, http.StatusOK, detailLayoutTmpl, view)
}

func (h *Handler) statusFragment(w http.ResponseWriter, r *http.Request) {
	tenant, err := tenancy.FromContext(r.Context())
	if err != nil {
		h.fail(w, http.StatusInternalServerError, "tenant required", err)
		return
	}
	invoiceID, ok := parseID(r.PathValue("id"))
	if !ok {
		http.Error(w, "invalid invoice id", http.StatusBadRequest)
		return
	}
	if _, err := h.deps.Invoice.GetByID(r.Context(), tenant.ID, invoiceID); err != nil {
		if errors.Is(err, billing.ErrNotFound) {
			http.Error(w, http.StatusText(http.StatusNotFound), http.StatusNotFound)
			return
		}
		h.fail(w, http.StatusInternalServerError, "get invoice", err)
		return
	}
	charge, err := h.deps.Charges.LatestForInvoice(r.Context(), tenant.ID, invoiceID)
	if err != nil && !errors.Is(err, pix.ErrNotFound) {
		h.fail(w, http.StatusInternalServerError, "load pix charge", err)
		return
	}
	h.writeHTML(w, http.StatusOK, statusFragmentTmpl, statusFragmentFrom(invoiceID, charge))
}

func (h *Handler) bannerFragment(w http.ResponseWriter, r *http.Request) {
	tenant, err := tenancy.FromContext(r.Context())
	if err != nil {
		h.fail(w, http.StatusInternalServerError, "tenant required", err)
		return
	}
	banner, err := h.bannerFor(r.Context(), tenant.ID)
	if err != nil {
		h.fail(w, http.StatusInternalServerError, "load dunning", err)
		return
	}
	h.writeHTML(w, http.StatusOK, bannerFragmentTmpl, banner)
}

func (h *Handler) bannerFor(ctx context.Context, tenantID uuid.UUID) (bannerView, error) {
	state, err := h.deps.Dunning.CurrentForTenant(ctx, tenantID)
	if err != nil {
		return bannerView{}, err
	}
	return bannerViewFrom(state, h.deps.Now().UTC()), nil
}

func (h *Handler) chargeViewFor(ctx context.Context, tenantID, invoiceID uuid.UUID) (*pix.PIXCharge, chargeView, error) {
	charge, err := h.deps.Charges.LatestForInvoice(ctx, tenantID, invoiceID)
	if err != nil {
		if errors.Is(err, pix.ErrNotFound) {
			return nil, chargeView{Pending: true}, nil
		}
		return nil, chargeView{}, err
	}
	return charge, chargeViewFrom(charge), nil
}

func (h *Handler) nextBillingDateStr(ctx context.Context, tenantID uuid.UUID) string {
	if h.deps.NextBillingDate == nil {
		return ""
	}
	t, err := h.deps.NextBillingDate(ctx, tenantID)
	if err != nil || t.IsZero() {
		return ""
	}
	return t.UTC().Format("02/01/2006")
}

func (h *Handler) navItems(r *http.Request) []shell.NavItem {
	if h.deps.NavItems == nil {
		return nil
	}
	return h.deps.NavItems(r)
}

func (h *Handler) userMenuItems(r *http.Request) []shell.UserMenuItem {
	if h.deps.UserMenuItems == nil {
		return nil
	}
	return h.deps.UserMenuItems(r)
}

func (h *Handler) renderShell(w http.ResponseWriter, status int, tmpl *template.Template, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	if err := shell.Render(w, tmpl, data); err != nil {
		h.deps.Logger.Error("web/billing/invoices: render shell", "template", tmpl.Name(), "err", err)
	}
}

func (h *Handler) writeHTML(w http.ResponseWriter, status int, tmpl *template.Template, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	if err := tmpl.Execute(w, data); err != nil {
		h.deps.Logger.Error("web/billing/invoices: render", "template", tmpl.Name(), "err", err)
	}
}

func (h *Handler) fail(w http.ResponseWriter, status int, msg string, err error) {
	h.deps.Logger.Error("web/billing/invoices: "+msg, "err", err)
	http.Error(w, http.StatusText(status), status)
}

// ---------------------------------------------------------------------------
// view types
// ---------------------------------------------------------------------------

type filterParams struct {
	Period string
	Status string
}

type listView struct {
	Banner          bannerView
	Rows            []invoiceRow
	Filter          filterParams
	NextBillingDate string
	GeneratedAt     string
	// shell.Data fields (accessed via reflection by shell layout)
	TenantName       string
	TenantLogo       string
	UserDisplayName  string
	NavItems         []shell.NavItem
	UserMenuItems    []shell.UserMenuItem
	CSRFToken        string
	CSPNonce         string
	TenantThemeStyle template.CSS
}

type detailView struct {
	Banner  bannerView
	Invoice invoiceRow
	Charge  chargeView
	Status  statusFragment
	// shell.Data fields
	TenantName       string
	TenantLogo       string
	UserDisplayName  string
	NavItems         []shell.NavItem
	UserMenuItems    []shell.UserMenuItem
	CSRFToken        string
	CSPNonce         string
	TenantThemeStyle template.CSS
	GeneratedAt      string
}

type tbodyView struct {
	Rows            []invoiceRow
	NextBillingDate string
}

type invoiceRow struct {
	ID         string
	Period     string
	Amount     string
	State      string
	StateLabel string
	DetailURL  string
}

type chargeView struct {
	HasCharge bool
	Pending   bool
	QRDataURI template.URL
	CopyPaste string
	ExpiresAt string
}

type statusFragment struct {
	InvoiceID    string
	Status       string
	Label        string
	PollActive   bool
	PollInterval string
}

type bannerView struct {
	Severity   string
	AlertClass string
	Icon       string
	Title      string
	Message    string
	HasAction  bool
	Visible    bool
}

// ---------------------------------------------------------------------------
// bannerViewFrom — D1 4-state dunning banner (SIN-62204 / SIN-63944)
//
// Maps domain state to visual band:
//
//	StateWarn               → aviso     (alert--info)
//	StateSuspendedOutbound  → atrasado  (alert--warning)
//	StateSuspendedFull ≤14d → restricao (alert--danger)
//	StateSuspendedFull >14d → bloqueio  (alert--danger + action CTA)
//
// Note: existing TestBannerFragment_PerSeverity assertions are updated
// in handlers_test.go to match the new D1 copy (task spec = auth).
// ---------------------------------------------------------------------------
func bannerViewFrom(state *dunning.DunningState, now time.Time) bannerView {
	if state == nil {
		return bannerView{}
	}
	if state.HasActiveOverride(now) {
		return bannerView{}
	}
	switch state.State() {
	case dunning.StateWarn:
		return bannerView{
			Severity:   "warn",
			AlertClass: "alert--info",
			Icon:       "📅",
			Title:      "Fatura próxima do vencimento",
			Message:    "Sua fatura vence em 3 dias. Pague para evitar interrupções.",
			Visible:    true,
		}
	case dunning.StateSuspendedOutbound:
		return bannerView{
			Severity:   "outbound",
			AlertClass: "alert--warning",
			Icon:       "⚠️",
			Title:      "Fatura em atraso",
			Message:    "Fatura em atraso. Pague para evitar bloqueio.",
			Visible:    true,
		}
	case dunning.StateSuspendedFull:
		daysInState := int(now.Sub(state.EnteredStateAt()).Hours() / 24)
		if daysInState > 14 {
			return bannerView{
				Severity:   "bloqueio",
				AlertClass: "alert--danger",
				Icon:       "🔒",
				Title:      "Conta bloqueada",
				Message:    "Acesso restrito. Quite a fatura para reativar o acesso completo.",
				HasAction:  true,
				Visible:    true,
			}
		}
		return bannerView{
			Severity:   "full",
			AlertClass: "alert--danger",
			Icon:       "🔒",
			Title:      "Acesso restrito",
			Message:    "Acesso restrito. Saldo de tokens não recarrega até regularizar.",
			Visible:    true,
		}
	default:
		return bannerView{}
	}
}

func filterFrom(r *http.Request) filterParams {
	return filterParams{
		Period: strings.TrimSpace(r.URL.Query().Get("period")),
		Status: strings.TrimSpace(r.URL.Query().Get("status")),
	}
}

func applyFilter(invs []*billing.Invoice, f filterParams, limit int) []invoiceRow {
	rows := make([]invoiceRow, 0, len(invs))
	for _, inv := range invs {
		if f.Status != "" && string(inv.State()) != f.Status {
			continue
		}
		if f.Period != "" {
			if inv.PeriodStart().UTC().Format("2006-01") != f.Period {
				continue
			}
		}
		rows = append(rows, invoiceRowFrom(inv))
		if limit > 0 && len(rows) >= limit {
			break
		}
	}
	return rows
}

func tenantName(t *tenancy.Tenant) string {
	if t == nil || t.Name == "" {
		return "CRM"
	}
	return t.Name
}

func invoiceRowFrom(inv *billing.Invoice) invoiceRow {
	return invoiceRow{
		ID:         inv.ID().String(),
		Period:     inv.PeriodStart().UTC().Format("01/2006"),
		Amount:     formatBRL(inv.AmountCentsBRL()),
		State:      string(inv.State()),
		StateLabel: invoiceStateLabel(inv.State()),
		DetailURL:  "/billing/invoices/" + inv.ID().String(),
	}
}

func invoiceStateLabel(s billing.InvoiceState) string {
	switch s {
	case billing.InvoiceStatePaid:
		return "paga"
	case billing.InvoiceStateCancelledByMaster:
		return "cancelada"
	case billing.InvoiceStatePending:
		return "pendente"
	default:
		return string(s)
	}
}

func formatBRL(cents int) string {
	sign := ""
	if cents < 0 {
		sign = "-"
		cents = -cents
	}
	r := cents % 100
	return fmt.Sprintf("R$ %s%d,%02d", sign, cents/100, r)
}

func chargeViewFrom(c *pix.PIXCharge) chargeView {
	return chargeView{
		HasCharge: true,
		QRDataURI: qrDataURI(c.QRCode()),
		CopyPaste: c.CopyPaste(),
		ExpiresAt: c.ExpiresAt().UTC().Format("02/01/2006 15:04 MST"),
	}
}

func qrDataURI(qr string) template.URL {
	trimmed := strings.TrimSpace(qr)
	if trimmed == "" {
		return ""
	}
	if strings.HasPrefix(trimmed, "data:") {
		return template.URL(trimmed)
	}
	mime := "image/svg+xml"
	if decoded, err := base64.StdEncoding.DecodeString(trimmed); err == nil {
		if len(decoded) >= 8 && string(decoded[:8]) == "\x89PNG\r\n\x1a\n" {
			mime = "image/png"
		}
	}
	return template.URL("data:" + mime + ";base64," + trimmed)
}

func statusFragmentFrom(invoiceID uuid.UUID, c *pix.PIXCharge) statusFragment {
	frag := statusFragment{
		InvoiceID:    invoiceID.String(),
		Status:       string(pix.StatusPending),
		Label:        pixStatusLabel(pix.StatusPending),
		PollActive:   true,
		PollInterval: pollIntervalPending,
	}
	if c == nil {
		return frag
	}
	frag.Status = string(c.Status())
	frag.Label = pixStatusLabel(c.Status())
	frag.PollActive = !c.IsTerminal()
	if !frag.PollActive {
		frag.PollInterval = ""
	}
	return frag
}

func pixStatusLabel(s pix.Status) string {
	switch s {
	case pix.StatusPending:
		return "aguardando pagamento"
	case pix.StatusPaid:
		return "pago"
	case pix.StatusExpired:
		return "expirado"
	case pix.StatusCancelled:
		return "cancelado"
	default:
		return string(s)
	}
}

func parseID(raw string) (uuid.UUID, bool) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return uuid.Nil, false
	}
	id, err := uuid.Parse(trimmed)
	if err != nil {
		return uuid.Nil, false
	}
	return id, true
}
