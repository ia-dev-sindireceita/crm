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

	"github.com/pericles-luz/crm/internal/adapter/httpapi/csrf"
	"github.com/pericles-luz/crm/internal/billing"
	"github.com/pericles-luz/crm/internal/billing/dunning"
	"github.com/pericles-luz/crm/internal/billing/pix"
	"github.com/pericles-luz/crm/internal/branding"
	"github.com/pericles-luz/crm/internal/http/middleware/csp"
	"github.com/pericles-luz/crm/internal/tenancy"
)

// pollIntervalPending is the HTMX hx-trigger interval the detail page
// embeds while the PIX charge is pending. Once the status reaches a
// terminal value the partial omits the trigger so polling stops
// cleanly (AC #3 — no recursos em paid/expired/failed).
const pollIntervalPending = "every 10s"

// listLimit caps the rows the list page renders. The invoice table
// is small (one row per billing period); a flat cap is fine until
// pagination becomes necessary.
const listLimit = 50

// InvoiceLister is the read port the list page consults. Tenant-
// scoped via RLS; "no rows" returns an empty slice.
type InvoiceLister interface {
	ListByTenant(ctx context.Context, tenantID uuid.UUID) ([]*billing.Invoice, error)
}

// InvoiceGetter is the read port the detail page consults. Returns
// billing.ErrNotFound for unknown ids; tenant-scoped via RLS so a
// cross-tenant id resolves to ErrNotFound as well.
type InvoiceGetter interface {
	GetByID(ctx context.Context, tenantID, invoiceID uuid.UUID) (*billing.Invoice, error)
}

// PIXChargeLister is the read port for the QR block on the detail
// page. LatestForInvoice returns pix.ErrNotFound when the charge has
// not yet been issued by the PSP; the handler treats this as
// "cobrança em processamento" and keeps polling.
type PIXChargeLister interface {
	LatestForInvoice(ctx context.Context, tenantID, invoiceID uuid.UUID) (*pix.PIXCharge, error)
}

// DunningStateReader resolves the tenant's current dunning state.
// (nil, nil) means "no row yet" (treated as StateCurrent — no
// banner). Errors propagate to the caller.
type DunningStateReader interface {
	CurrentForTenant(ctx context.Context, tenantID uuid.UUID) (*dunning.DunningState, error)
}

// CSRFTokenFn returns the request's CSRF token.
type CSRFTokenFn func(*http.Request) string

// UserIDFn returns the authenticated user id. uuid.Nil collapses to
// 401 — the audit row would be meaningless without an actor.
type UserIDFn func(*http.Request) uuid.UUID

// NowFn returns the current time. Injectable so tests can pin it.
type NowFn func() time.Time

// Deps bundles the handler collaborators. Every port is required;
// when a backing adapter is not yet wired (e.g. the PIX postgres
// adapter lands in C7) inject a small adapter that returns
// pix.ErrNotFound rather than passing a nil dependency.
type Deps struct {
	Invoices  InvoiceLister
	Invoice   InvoiceGetter
	Charges   PIXChargeLister
	Dunning   DunningStateReader
	CSRFToken CSRFTokenFn
	UserID    UserIDFn
	Now       NowFn
	Logger    *slog.Logger
}

// Handler is the HTMX billing-invoices front controller.
type Handler struct {
	deps Deps
}

// New constructs a Handler. Missing required deps are rejected so
// cmd/server fails fast.
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

// Routes mounts the surface endpoints on mux.
func (h *Handler) Routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /billing/invoices", h.list)
	mux.HandleFunc("GET /billing/invoices/{id}", h.detail)
	mux.HandleFunc("GET /billing/invoices/{id}/status", h.statusFragment)
	mux.HandleFunc("GET /billing/dunning-banner", h.bannerFragment)
}

// list renders the invoice list with the dunning banner inline.
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
	banner, err := h.bannerFor(r.Context(), tenant.ID)
	if err != nil {
		h.fail(w, http.StatusInternalServerError, "load dunning", err)
		return
	}
	now := h.deps.Now().UTC()
	view := listView{
		Banner:           banner,
		Rows:             listRows(rows, listLimit),
		GeneratedAt:      now.Format(time.RFC3339),
		CSRFMeta:         csrf.MetaTag(token),
		HXHeaders:        csrf.HXHeadersAttr(token),
		TenantThemeStyle: branding.ThemeStyleFromContext(r.Context()),
		CSPNonce:         csp.Nonce(r.Context()),
	}
	h.writeHTML(w, http.StatusOK, listLayoutTmpl, view)
}

// detail renders the per-invoice page: metadata, dunning banner, QR
// + copia-e-cola when the PIX charge is present, and the status
// badge that HTMX polls every 10s while pending.
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
		CSRFMeta:         csrf.MetaTag(token),
		HXHeaders:        csrf.HXHeadersAttr(token),
		GeneratedAt:      now.Format(time.RFC3339),
		TenantThemeStyle: branding.ThemeStyleFromContext(r.Context()),
		CSPNonce:         csp.Nonce(r.Context()),
	}
	h.writeHTML(w, http.StatusOK, detailLayoutTmpl, view)
}

// statusFragment renders the status-badge partial used by the HTMX
// poll. The partial omits hx-trigger once the charge reaches a
// terminal status so polling stops (AC #3).
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

// bannerFragment is the standalone dunning-banner partial. Other
// pages can hx-get it to surface the banner without redirecting to
// /billing/invoices.
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
// view shaping
// ---------------------------------------------------------------------------

type listView struct {
	Banner           bannerView
	Rows             []invoiceRow
	GeneratedAt      string
	CSRFMeta         template.HTML
	HXHeaders        template.HTMLAttr
	TenantThemeStyle template.CSS
	// CSPNonce carries the per-request CSP nonce (SIN-63275).
	CSPNonce string
}

type detailView struct {
	Banner           bannerView
	Invoice          invoiceRow
	Charge           chargeView
	Status           statusFragment
	CSRFMeta         template.HTML
	HXHeaders        template.HTMLAttr
	GeneratedAt      string
	TenantThemeStyle template.CSS
	// CSPNonce carries the per-request CSP nonce (SIN-63275).
	CSPNonce string
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
	HasCharge bool         // true once a PIXCharge exists
	Pending   bool         // true when there is no PIX charge yet
	QRDataURI template.URL // safe data URI for <img src>
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
	Severity string
	Title    string
	Message  string
	Visible  bool
}

func listRows(invs []*billing.Invoice, limit int) []invoiceRow {
	if limit > 0 && len(invs) > limit {
		invs = invs[:limit]
	}
	out := make([]invoiceRow, 0, len(invs))
	for _, inv := range invs {
		out = append(out, invoiceRowFrom(inv))
	}
	return out
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

// qrDataURI converts the stored qr_code payload into a data URI safe
// for rendering as <img src>. The PIX domain documents qr_code as
// "base64-encoded PNG/SVG"; this helper sniffs the decoded leading
// bytes to pick the correct MIME and falls back to SVG when the
// signature is unrecognised. If the stored value already begins with
// `data:` the helper passes it through as-is.
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
		// No charge yet → render as pending so the page keeps polling
		// until the PSP responds (AC #3 — poll only while pending).
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

// bannerViewFrom renders the dunning banner for the given state. A
// nil state or an active courtesy override collapses to "no banner"
// so brand-new tenants and tenants with a current free-period grant
// see a clean top-bar.
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
			Severity: "warn",
			Title:    "Pagamento pendente",
			Message:  "Sua última fatura está atrasada. Quite a cobrança PIX para evitar suspensão.",
			Visible:  true,
		}
	case dunning.StateSuspendedOutbound:
		return bannerView{
			Severity: "outbound",
			Title:    "Envios suspensos",
			Message:  "O envio de mensagens foi suspenso até a regularização do pagamento.",
			Visible:  true,
		}
	case dunning.StateSuspendedFull:
		return bannerView{
			Severity: "full",
			Title:    "Conta em modo leitura",
			Message:  "Sua conta está em modo somente leitura. Quite a cobrança PIX para reativar o acesso completo.",
			Visible:  true,
		}
	default:
		return bannerView{}
	}
}

// parseID parses a uuid path value and returns false for invalid input.
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
