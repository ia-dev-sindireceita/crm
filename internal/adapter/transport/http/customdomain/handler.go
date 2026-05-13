package customdomain

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/customdomain/management"
	"github.com/pericles-luz/crm/internal/http/middleware/csp"
)

// Handler is the HTTP boundary for SIN-62259. Construct via New and
// register on a Go 1.22 *http.ServeMux via Register.
type Handler struct {
	uc            UseCase
	csrf          CSRFConfig
	now           func() time.Time
	primaryDomain string
	logger        *slog.Logger
}

// UseCase is the narrow management surface the handler relies on.
// Defining it here keeps the handler unit-testable with a fake.
type UseCase interface {
	List(ctx context.Context, tenantID uuid.UUID) ([]management.Domain, error)
	Get(ctx context.Context, tenantID, id uuid.UUID) (management.Domain, error)
	Enroll(ctx context.Context, tenantID uuid.UUID, host string) (management.EnrollResult, error)
	Verify(ctx context.Context, tenantID, id uuid.UUID) (management.VerifyOutcome, error)
	SetPaused(ctx context.Context, tenantID, id uuid.UUID, paused bool) (management.Domain, error)
	Delete(ctx context.Context, tenantID, id uuid.UUID) error
}

// Config groups the handler's collaborators. PrimaryDomain is rendered
// in the helper text about static.<primary> assets.
type Config struct {
	UseCase       UseCase
	CSRF          CSRFConfig
	Now           func() time.Time
	PrimaryDomain string
	Logger        *slog.Logger
}

// New returns a Handler. Returns an error for missing required deps.
func New(cfg Config) (*Handler, error) {
	if cfg.UseCase == nil {
		return nil, errors.New("customdomain: UseCase is required")
	}
	if len(cfg.CSRF.Secret) < 32 {
		return nil, errors.New("customdomain: CSRF secret must be at least 32 bytes")
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	primary := cfg.PrimaryDomain
	if primary == "" {
		primary = "exemplo.com"
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	// Eagerly parse templates so a manifest-load error (or a
	// missing-vendor-asset panic) surfaces at server boot instead of
	// the first request. SIN-62535.
	if _, err := loadTemplates(); err != nil {
		return nil, fmt.Errorf("customdomain: load templates: %w", err)
	}
	return &Handler{
		uc:            cfg.UseCase,
		csrf:          cfg.CSRF,
		now:           now,
		primaryDomain: primary,
		logger:        logger,
	}, nil
}

// Register attaches every route to mux. Routes use Go 1.22 path patterns.
func (h *Handler) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /tenant/custom-domains", h.serveList)
	mux.HandleFunc("GET /tenant/custom-domains/new", h.serveWizardStep1)
	mux.HandleFunc("POST /tenant/custom-domains", h.serveEnroll)
	mux.HandleFunc("GET /tenant/custom-domains/{id}/instructions", h.serveInstructions)
	mux.HandleFunc("GET /tenant/custom-domains/{id}/status", h.serveStatusRow)
	mux.HandleFunc("GET /tenant/custom-domains/{id}/delete", h.serveDeleteModal)
	mux.HandleFunc("POST /api/customdomains/{id}/verify", h.serveVerify)
	mux.HandleFunc("PATCH /api/customdomains/{id}", h.serveSetPaused)
	mux.HandleFunc("DELETE /api/customdomains/{id}", h.serveDelete)
}

// pageData is what list.html renders against.
type pageData struct {
	Title         string
	CSRFToken     string
	PrimaryDomain string
	Domains       []domainView
	// Nonce is the per-request CSP nonce. base.html stamps it on every
	// <script> tag it owns; csp middleware (SIN-62237) is the source. An
	// empty value means the request was not routed through the CSP
	// middleware — the resulting `nonce=""` attribute is harmless because
	// every script in base.html also has a `src=` that matches the policy
	// 'self' source.
	Nonce string
}

type listPartialData struct {
	CSRFToken string
	Domains   []domainView
}

// domainView is a row in the table.
type domainView struct {
	ID                 string
	Host               string
	StatusName         string
	StatusLabel        string
	StatusTooltip      string
	BadgeColor         string
	VerifiedAtFmt      string
	CreatedAtFmt       string
	VerifiedWithDNSSEC bool
	CSRFToken          string
}

func (h *Handler) viewFor(d management.Domain, lastErr error, csrf string) domainView {
	st := management.StatusOf(d, lastErr)
	tooltip := ""
	if st == management.StatusError && lastErr != nil {
		tooltip = lastErr.Error()
	}
	verified := ""
	if d.VerifiedAt != nil {
		verified = d.VerifiedAt.Format("02/01/2006 15:04")
	}
	return domainView{
		ID:                 d.ID.String(),
		Host:               d.Host,
		StatusName:         st.String(),
		StatusLabel:        management.StatusLabelPTBR(st),
		StatusTooltip:      tooltip,
		BadgeColor:         management.StatusBadgeColor(st),
		VerifiedAtFmt:      verified,
		CreatedAtFmt:       d.CreatedAt.Format("02/01/2006 15:04"),
		VerifiedWithDNSSEC: d.VerifiedWithDNSSEC,
		CSRFToken:          csrf,
	}
}

// serveList renders the full page.
func (h *Handler) serveList(w http.ResponseWriter, r *http.Request) {
	tenant := h.requireTenant(w, r)
	if tenant == uuid.Nil {
		return
	}
	csrf, err := IssueCSRFToken(w, r, h.csrf)
	if err != nil {
		h.serverError(w, r, err)
		return
	}
	domains, err := h.uc.List(r.Context(), tenant)
	if err != nil {
		h.serverError(w, r, err)
		return
	}
	views := make([]domainView, 0, len(domains))
	for _, d := range domains {
		views = append(views, h.viewFor(d, nil, csrf))
	}
	data := pageData{
		Title:         "Domínios personalizados",
		CSRFToken:     csrf,
		PrimaryDomain: h.primaryDomain,
		Domains:       views,
		Nonce:         csp.Nonce(r.Context()),
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := renderTemplate(w, "base", data); err != nil {
		h.serverError(w, r, err)
	}
}

// serveWizardStep1 renders the hostname-input form.
func (h *Handler) serveWizardStep1(w http.ResponseWriter, r *http.Request) {
	if h.requireTenant(w, r) == uuid.Nil {
		return
	}
	csrf, err := IssueCSRFToken(w, r, h.csrf)
	if err != nil {
		h.serverError(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = renderTemplate(w, "wizard_step1", map[string]any{"CSRFToken": csrf})
}

// step2Data is the wizard step 2 view.
type step2Data struct {
	TXTRecord string
	TXTValue  string
	DomainID  string
	CSRFToken string
}

// serveEnroll handles step1 form submission.
func (h *Handler) serveEnroll(w http.ResponseWriter, r *http.Request) {
	tenant := h.requireTenant(w, r)
	if tenant == uuid.Nil {
		return
	}
	if err := VerifyCSRF(r, h.csrf); err != nil {
		h.forbidden(w, r, "CSRF token inválido. Recarregue a página.")
		return
	}
	if err := r.ParseForm(); err != nil {
		h.renderWizardError(w, r, "Formulário inválido.")
		return
	}
	res, err := h.uc.Enroll(r.Context(), tenant, r.FormValue("host"))
	if err != nil {
		switch {
		case errors.Is(err, management.ErrInvalidHost):
			h.renderWizardError(w, r, management.CopyPTBR(management.ReasonInvalidHost, 0, nil))
			return
		case errors.Is(err, management.ErrPrivateIP):
			h.renderWizardError(w, r, management.CopyPTBR(management.ReasonPrivateIP, 0, nil))
			return
		case errors.Is(err, management.ErrSlugReserved):
			// SIN-62244 reservation lock: validator rejects the host because
			// the underlying slug is still reserved within the 12-month
			// window. Surface the PT-BR copy through the wizard-error
			// template so the user sees the same UX as other deny cases.
			h.renderWizardError(w, r, management.CopyPTBR(management.ReasonSlugReserved, 0, res.ReservedUntil))
			return
		default:
			h.serverError(w, r, err)
			return
		}
	}
	if res.Reason != management.ReasonNone {
		h.renderWizardError(w, r, management.CopyPTBR(res.Reason, res.RetryAfter, res.ReservedUntil))
		return
	}
	csrf, _ := IssueCSRFToken(w, r, h.csrf)
	data := step2Data{
		TXTRecord: res.TXTRecord,
		TXTValue:  res.TXTValue,
		DomainID:  res.Domain.ID.String(),
		CSRFToken: csrf,
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = renderTemplate(w, "wizard_step2", data)
}

// serveInstructions renders the wizard step 2 partial for an existing
// pending domain.
func (h *Handler) serveInstructions(w http.ResponseWriter, r *http.Request) {
	tenant := h.requireTenant(w, r)
	if tenant == uuid.Nil {
		return
	}
	id, err := parseID(r)
	if err != nil {
		http.Error(w, "ID do domínio inválido.", http.StatusBadRequest)
		return
	}
	d, err := h.uc.Get(r.Context(), tenant, id)
	if err != nil {
		h.notFound(w, r)
		return
	}
	csrf, _ := IssueCSRFToken(w, r, h.csrf)
	data := step2Data{
		TXTRecord: management.TXTRecordFor(d.Host),
		TXTValue:  management.TXTValueFor(d.VerificationToken),
		DomainID:  d.ID.String(),
		CSRFToken: csrf,
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = renderTemplate(w, "wizard_step2", data)
}

// serveStatusRow returns one <tr> for the polling target.
func (h *Handler) serveStatusRow(w http.ResponseWriter, r *http.Request) {
	tenant := h.requireTenant(w, r)
	if tenant == uuid.Nil {
		return
	}
	id, err := parseID(r)
	if err != nil {
		http.Error(w, "ID do domínio inválido.", http.StatusBadRequest)
		return
	}
	d, err := h.uc.Get(r.Context(), tenant, id)
	if err != nil {
		h.notFound(w, r)
		return
	}
	csrf, _ := IssueCSRFToken(w, r, h.csrf)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = renderTemplate(w, "row", h.viewFor(d, nil, csrf))
}

// deleteModalData populates the confirmation modal.
type deleteModalData struct {
	Host        string
	DomainID    string
	HostPattern string
	CSRFToken   string
}

func (h *Handler) serveDeleteModal(w http.ResponseWriter, r *http.Request) {
	tenant := h.requireTenant(w, r)
	if tenant == uuid.Nil {
		return
	}
	id, err := parseID(r)
	if err != nil {
		http.Error(w, "ID do domínio inválido.", http.StatusBadRequest)
		return
	}
	d, err := h.uc.Get(r.Context(), tenant, id)
	if err != nil {
		h.notFound(w, r)
		return
	}
	csrf, _ := IssueCSRFToken(w, r, h.csrf)
	data := deleteModalData{
		Host:        d.Host,
		DomainID:    d.ID.String(),
		HostPattern: regexp.QuoteMeta(d.Host),
		CSRFToken:   csrf,
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = renderTemplate(w, "delete_modal", data)
}

// serveVerify is the "Verificar agora" target.
func (h *Handler) serveVerify(w http.ResponseWriter, r *http.Request) {
	tenant := h.requireTenant(w, r)
	if tenant == uuid.Nil {
		return
	}
	if err := VerifyCSRF(r, h.csrf); err != nil {
		h.forbidden(w, r, "CSRF token inválido. Recarregue a página.")
		return
	}
	id, err := parseID(r)
	if err != nil {
		http.Error(w, "ID do domínio inválido.", http.StatusBadRequest)
		return
	}
	out, err := h.uc.Verify(r.Context(), tenant, id)
	csrf, _ := IssueCSRFToken(w, r, h.csrf)
	if err != nil {
		// Render the row with an error tooltip so HTMX swaps it back in.
		h.renderRow(w, r, out.Domain, err, csrf)
		return
	}
	h.renderRow(w, r, out.Domain, nil, csrf)
}

// serveSetPaused handles PATCH ?paused=true|false.
func (h *Handler) serveSetPaused(w http.ResponseWriter, r *http.Request) {
	tenant := h.requireTenant(w, r)
	if tenant == uuid.Nil {
		return
	}
	if err := VerifyCSRF(r, h.csrf); err != nil {
		h.forbidden(w, r, "CSRF token inválido. Recarregue a página.")
		return
	}
	id, err := parseID(r)
	if err != nil {
		http.Error(w, "ID do domínio inválido.", http.StatusBadRequest)
		return
	}
	pausedStr := r.URL.Query().Get("paused")
	paused, err := strconv.ParseBool(pausedStr)
	if err != nil {
		http.Error(w, "Parâmetro paused obrigatório (true|false).", http.StatusBadRequest)
		return
	}
	d, err := h.uc.SetPaused(r.Context(), tenant, id, paused)
	csrf, _ := IssueCSRFToken(w, r, h.csrf)
	if err != nil {
		if errors.Is(err, management.ErrStoreNotFound) || errors.Is(err, management.ErrTenantMismatch) {
			h.notFound(w, r)
			return
		}
		h.serverError(w, r, err)
		return
	}
	h.renderRow(w, r, d, nil, csrf)
}

// serveDelete is the DELETE handler. It re-renders the entire list
// because the deleted row leaves the table.
func (h *Handler) serveDelete(w http.ResponseWriter, r *http.Request) {
	tenant := h.requireTenant(w, r)
	if tenant == uuid.Nil {
		return
	}
	if err := VerifyCSRF(r, h.csrf); err != nil {
		h.forbidden(w, r, "CSRF token inválido. Recarregue a página.")
		return
	}
	id, err := parseID(r)
	if err != nil {
		http.Error(w, "ID do domínio inválido.", http.StatusBadRequest)
		return
	}
	if err := h.uc.Delete(r.Context(), tenant, id); err != nil {
		if errors.Is(err, management.ErrStoreNotFound) || errors.Is(err, management.ErrTenantMismatch) {
			h.notFound(w, r)
			return
		}
		h.serverError(w, r, err)
		return
	}
	csrf, _ := IssueCSRFToken(w, r, h.csrf)
	domains, err := h.uc.List(r.Context(), tenant)
	if err != nil {
		h.serverError(w, r, err)
		return
	}
	views := make([]domainView, 0, len(domains))
	for _, d := range domains {
		views = append(views, h.viewFor(d, nil, csrf))
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = renderTemplate(w, "list_partial", listPartialData{CSRFToken: csrf, Domains: views})
}

func (h *Handler) renderRow(w http.ResponseWriter, _ *http.Request, d management.Domain, lastErr error, csrf string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = renderTemplate(w, "row", h.viewFor(d, lastErr, csrf))
}

func (h *Handler) renderWizardError(w http.ResponseWriter, r *http.Request, msg string) {
	// Reuse the existing CSRF cookie when valid so the rendered form can
	// be resubmitted without forcing a page reload. IssueCSRFToken
	// rotates the cookie only when there is none or the existing one is
	// invalid; either way the returned token matches the cookie that
	// arrives on the next POST.
	csrf, err := IssueCSRFToken(w, r, h.csrf)
	if err != nil {
		h.serverError(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusUnprocessableEntity)
	_ = renderTemplate(w, "wizard_step1", map[string]any{
		"CSRFToken": csrf,
		"Error":     msg,
	})
}

func (h *Handler) requireTenant(w http.ResponseWriter, r *http.Request) uuid.UUID {
	tenant := TenantIDFromContext(r.Context())
	if tenant == uuid.Nil {
		http.Error(w, "Sessão de tenant não encontrada.", http.StatusUnauthorized)
		return uuid.Nil
	}
	return tenant
}

func (h *Handler) serverError(w http.ResponseWriter, r *http.Request, err error) {
	h.logger.LogAttrs(r.Context(), slog.LevelError, "customdomain.error",
		slog.String("path", r.URL.Path),
		slog.String("method", r.Method),
		slog.String("err", err.Error()),
	)
	http.Error(w, "Erro interno. Tente novamente em alguns minutos.", http.StatusInternalServerError)
}

func (h *Handler) forbidden(w http.ResponseWriter, _ *http.Request, msg string) {
	http.Error(w, msg, http.StatusForbidden)
}

func (h *Handler) notFound(w http.ResponseWriter, _ *http.Request) {
	http.Error(w, "Domínio não encontrado.", http.StatusNotFound)
}

// parseID reads the {id} path value and parses it as a UUID.
func parseID(r *http.Request) (uuid.UUID, error) {
	raw := r.PathValue("id")
	id, err := uuid.Parse(raw)
	if err != nil {
		return uuid.Nil, err
	}
	return id, nil
}
