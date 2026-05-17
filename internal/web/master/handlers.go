package master

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/adapter/httpapi/csrf"
	"github.com/pericles-luz/crm/internal/billing"
	"github.com/pericles-luz/crm/internal/iam"
)

// ListTenants renders the paginated tenant table. The route is master-
// only via the RequireAction(ActionMasterTenantRead) gate the wire
// layer wraps around this handler; the handler itself reads the
// resolved iam.Principal to guard against accidental un-gated mounts.
//
// Query params:
//   - page      (1-indexed, defaults to 1 when missing / malformed)
//   - page_size (defaults to Handler.defaultPageSize, clamped to MaxPageSize)
//   - plan      (slug filter; empty string == no filter)
//
// HTMX targeting: when the request carries the HX-Request header the
// response renders only the table partial (#tenants-table) so the page
// shell stays untouched on partial-refresh.
func (h *Handler) ListTenants(w http.ResponseWriter, r *http.Request) {
	if _, ok := iam.PrincipalFromContext(r.Context()); !ok {
		h.fail(w, http.StatusUnauthorized, "principal missing", errors.New("no principal in context"))
		return
	}
	opts := h.parseListOptions(r)
	res, err := h.deps.Tenants.List(r.Context(), opts)
	if err != nil && !errors.Is(err, ErrNotFound) {
		h.fail(w, http.StatusInternalServerError, "list tenants", err)
		return
	}
	plans, err := h.deps.Plans.List(r.Context())
	if err != nil {
		h.fail(w, http.StatusInternalServerError, "list plans", err)
		return
	}
	token := h.deps.CSRFToken(r)
	if token == "" {
		h.fail(w, http.StatusInternalServerError, "csrf token missing", errors.New("empty csrf token"))
		return
	}
	data := h.buildPageData(res, plans, token, "", "")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "private, no-store")
	tmpl := masterLayoutTmpl
	if r.Header.Get("HX-Request") == "true" {
		tmpl = tenantsTableTmpl
	}
	if err := tmpl.Execute(w, data); err != nil {
		h.deps.Logger.Error("web/master: render list", "err", err)
	}
}

// CreateTenant handles POST /master/tenants. The form carries name,
// host, optional plan slug, optional courtesy tokens. On success the
// response is the re-rendered table partial (the new row + remaining
// page-1 rows) so the HTMX swap on #tenants-table picks up the change.
// Validation failures (ErrInvalidInput / ErrHostTaken / ErrUnknownPlan)
// re-render the create form with the inline error so the operator can
// correct without losing context.
func (h *Handler) CreateTenant(w http.ResponseWriter, r *http.Request) {
	p, ok := iam.PrincipalFromContext(r.Context())
	if !ok {
		h.fail(w, http.StatusUnauthorized, "principal missing", errors.New("no principal in context"))
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	in, validationErr := parseCreateForm(r, p.UserID)
	if validationErr != "" {
		h.renderCreateError(w, r, validationErr, http.StatusUnprocessableEntity)
		return
	}
	created, err := h.deps.Creator.Create(r.Context(), in)
	switch {
	case errors.Is(err, ErrHostTaken):
		h.renderCreateError(w, r, "Esse host já está em uso por outro tenant.", http.StatusUnprocessableEntity)
		return
	case errors.Is(err, ErrUnknownPlan):
		h.renderCreateError(w, r, "Plano desconhecido.", http.StatusUnprocessableEntity)
		return
	case errors.Is(err, ErrInvalidInput):
		h.renderCreateError(w, r, "Dados de tenant inválidos.", http.StatusUnprocessableEntity)
		return
	case err != nil:
		h.fail(w, http.StatusInternalServerError, "create tenant", err)
		return
	}
	res, err := h.deps.Tenants.List(r.Context(), ListOptions{
		Page:     1,
		PageSize: h.defaultPageSize,
	})
	if err != nil && !errors.Is(err, ErrNotFound) {
		h.fail(w, http.StatusInternalServerError, "list tenants after create", err)
		return
	}
	// Ensure the freshly-created row is visible even if the list
	// adapter is eventually-consistent. Prepend on absence.
	res = ensureRowPresent(res, created.Tenant)
	plans, err := h.deps.Plans.List(r.Context())
	if err != nil {
		h.fail(w, http.StatusInternalServerError, "list plans after create", err)
		return
	}
	token := h.deps.CSRFToken(r)
	if token == "" {
		h.fail(w, http.StatusInternalServerError, "csrf token missing", errors.New("empty csrf token"))
		return
	}
	data := h.buildPageData(res, plans, token, "Tenant criado com sucesso.", "")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "private, no-store")
	if err := tenantsTableTmpl.Execute(w, data); err != nil {
		h.deps.Logger.Error("web/master: render after create", "err", err)
	}
}

// AssignPlan handles PATCH /master/tenants/{id}/plan. Form body carries
// plan slug. On success it renders the updated row partial so the
// existing <tr> swaps in place via hx-swap=outerHTML.
func (h *Handler) AssignPlan(w http.ResponseWriter, r *http.Request) {
	p, ok := iam.PrincipalFromContext(r.Context())
	if !ok {
		h.fail(w, http.StatusUnauthorized, "principal missing", errors.New("no principal in context"))
		return
	}
	tenantID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "invalid tenant id", http.StatusBadRequest)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	planSlug := strings.TrimSpace(r.PostFormValue("plan_slug"))
	if planSlug == "" {
		http.Error(w, "plan_slug is required", http.StatusUnprocessableEntity)
		return
	}
	result, err := h.deps.Assigner.Assign(r.Context(), AssignPlanInput{
		ActorUserID: p.UserID,
		TenantID:    tenantID,
		PlanSlug:    planSlug,
	})
	switch {
	case errors.Is(err, ErrNotFound):
		http.Error(w, http.StatusText(http.StatusNotFound), http.StatusNotFound)
		return
	case errors.Is(err, ErrUnknownPlan):
		http.Error(w, "plano desconhecido", http.StatusUnprocessableEntity)
		return
	case err != nil:
		h.fail(w, http.StatusInternalServerError, "assign plan", err)
		return
	}
	token := h.deps.CSRFToken(r)
	if token == "" {
		h.fail(w, http.StatusInternalServerError, "csrf token missing", errors.New("empty csrf token"))
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "private, no-store")
	if err := tenantRowTmpl.Execute(w, rowData{
		Row:       result.Tenant,
		CSRFInput: csrf.FormHidden(token),
		// Plan options are needed for the inline plan-selector in
		// the row partial — re-listing here is cheap (PlanCatalog is
		// a small, cached table) and keeps the swap self-contained.
		Plans: h.plansForRow(r),
	}); err != nil {
		h.deps.Logger.Error("web/master: render row after assign", "err", err)
	}
}

// parseListOptions extracts the three query params with safe defaults.
// page < 1 collapses to 1; page_size outside (0, MaxPageSize] falls
// back to DefaultPageSize; an unknown plan filter is passed through
// (the adapter returns an empty list, which is the right rendering).
func (h *Handler) parseListOptions(r *http.Request) ListOptions {
	page := 1
	if v := r.URL.Query().Get("page"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			page = n
		}
	}
	pageSize := h.defaultPageSize
	if v := r.URL.Query().Get("page_size"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= h.maxPageSize {
			pageSize = n
		}
	}
	return ListOptions{
		Page:           page,
		PageSize:       pageSize,
		FilterPlanSlug: strings.TrimSpace(r.URL.Query().Get("plan")),
	}
}

// buildPageData stitches together the view-model that drives both the
// full page and the table partial. Keeping the data shape identical
// across templates means a partial swap renders byte-identical output
// to a full re-render of the same region.
func (h *Handler) buildPageData(res ListResult, plans []billing.Plan, token, flash, formError string) pageData {
	if res.PageSize <= 0 {
		res.PageSize = h.defaultPageSize
	}
	if res.Page <= 0 {
		res.Page = 1
	}
	totalPages := 1
	if res.PageSize > 0 && res.TotalCount > 0 {
		totalPages = (res.TotalCount + res.PageSize - 1) / res.PageSize
	}
	return pageData{
		Tenants:    res.Tenants,
		Page:       res.Page,
		PageSize:   res.PageSize,
		TotalCount: res.TotalCount,
		TotalPages: totalPages,
		Plans:      plans,
		Flash:      flash,
		FormError:  formError,
		CSRFMeta:   csrf.MetaTag(token),
		HXHeaders:  csrf.HXHeadersAttr(token),
		CSRFInput:  csrf.FormHidden(token),
	}
}

// plansForRow re-lists plans for the row-level partial. A list-plans
// error here is non-fatal — the row still renders, just without an
// inline plan selector. The handler logs and returns an empty slice.
func (h *Handler) plansForRow(r *http.Request) []billing.Plan {
	plans, err := h.deps.Plans.List(r.Context())
	if err != nil {
		h.deps.Logger.Warn("web/master: row partial plan list", "err", err)
		return nil
	}
	return plans
}

// renderCreateError re-renders the table-and-form view with an inline
// error banner attached to the create form. Status defaults to 422 for
// validation problems so the HTMX caller can react to the status code
// when needed.
func (h *Handler) renderCreateError(w http.ResponseWriter, r *http.Request, msg string, status int) {
	res, _ := h.deps.Tenants.List(r.Context(), ListOptions{
		Page:     1,
		PageSize: h.defaultPageSize,
	})
	plans, _ := h.deps.Plans.List(r.Context())
	token := h.deps.CSRFToken(r)
	if token == "" {
		h.fail(w, http.StatusInternalServerError, "csrf token missing", errors.New("empty csrf token"))
		return
	}
	data := h.buildPageData(res, plans, token, "", msg)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	if err := tenantsTableTmpl.Execute(w, data); err != nil {
		h.deps.Logger.Error("web/master: render create error", "err", err)
	}
}

func (h *Handler) fail(w http.ResponseWriter, status int, msg string, err error) {
	h.deps.Logger.Error("web/master: "+msg, "err", err)
	http.Error(w, http.StatusText(status), status)
}

// parseCreateForm validates the create-tenant POST body and returns a
// non-empty user-facing error message when the inputs do not satisfy
// the minimum-viable constraints. The returned CreateTenantInput is
// only meaningful when the second return value is empty.
func parseCreateForm(r *http.Request, actor uuid.UUID) (CreateTenantInput, string) {
	name := strings.TrimSpace(r.PostFormValue("name"))
	host := strings.TrimSpace(r.PostFormValue("host"))
	planSlug := strings.TrimSpace(r.PostFormValue("plan_slug"))
	if name == "" {
		return CreateTenantInput{}, "Nome do tenant é obrigatório."
	}
	if host == "" {
		return CreateTenantInput{}, "Host do tenant é obrigatório."
	}
	if strings.ContainsAny(host, " \t\r\n/") {
		return CreateTenantInput{}, "Host inválido."
	}
	var courtesy int64
	if v := strings.TrimSpace(r.PostFormValue("courtesy_tokens")); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 0 {
			return CreateTenantInput{}, "Tokens de cortesia devem ser um inteiro não-negativo."
		}
		courtesy = n
	}
	return CreateTenantInput{
		ActorUserID:           actor,
		Name:                  name,
		Host:                  host,
		PlanSlug:              planSlug,
		InitialCourtesyTokens: courtesy,
	}, ""
}

// ensureRowPresent prepends row when result.Tenants does not already
// contain a row with row.ID. Some list adapters are eventually-
// consistent (e.g. RLS + the SaveSubscription trigger sequence) so the
// freshly-created tenant may not appear in the immediate re-read; the
// UI must show the operator's action regardless.
func ensureRowPresent(result ListResult, row TenantRow) ListResult {
	for _, r := range result.Tenants {
		if r.ID == row.ID {
			return result
		}
	}
	result.Tenants = append([]TenantRow{row}, result.Tenants...)
	if result.TotalCount < len(result.Tenants) {
		result.TotalCount = len(result.Tenants)
	}
	return result
}
