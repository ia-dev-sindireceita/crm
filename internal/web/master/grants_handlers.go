package master

// SIN-62884 / Fase 2.5 C10 — three HTMX handlers for the master grants
// surface. The wire layer (internal/adapter/httpapi/router.go) wraps
// each handler with RequireAuth, RequireAction, and (POSTs only)
// mastermfa.RequireRecentMFA. The handler itself trusts the resolved
// iam.Principal and limits itself to validation, port orchestration,
// and partial rendering.

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/adapter/httpapi/csrf"
	"github.com/pericles-luz/crm/internal/iam"
)

// ShowGrantsForm renders the "conceder cortesia" form for the tenant.
// The form posts to POST /master/tenants/{id}/grants. The grants list
// for the tenant is rendered alongside so the operator sees the
// historical context (and so HTMX swaps land in the same region after
// a successful POST).
func (h *Handler) ShowGrantsForm(w http.ResponseWriter, r *http.Request) {
	if !h.requireGrants(w) {
		return
	}
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
	grants, err := h.deps.Grants.ListGrants(r.Context(), tenantID)
	if err != nil {
		h.fail(w, http.StatusInternalServerError, "list grants", err)
		return
	}
	token := h.deps.CSRFToken(r)
	if token == "" {
		h.fail(w, http.StatusInternalServerError, "csrf token missing", errors.New("empty csrf token"))
		return
	}
	data := h.buildGrantsPageData(tenantID, p.UserID, grants, "", "", string(GrantKindFreeSubscriptionPeriod), token)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "private, no-store")
	tmpl := grantsLayoutTmpl
	if r.Header.Get("HX-Request") == "true" {
		tmpl = grantsPanelTmpl
	}
	if err := tmpl.Execute(w, data); err != nil {
		h.deps.Logger.Error("web/master: render grants form", "err", err)
	}
}

// IssueGrant handles POST /master/tenants/{id}/grants. On success it
// re-renders the grants panel partial (form reset + new row at top).
// On validation / cap failure it re-renders with an inline error so
// the operator can correct without losing context.
func (h *Handler) IssueGrant(w http.ResponseWriter, r *http.Request) {
	if !h.requireGrants(w) {
		return
	}
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
	in, kind, validationErr := parseIssueGrantForm(r, p.UserID, tenantID)
	if validationErr != "" {
		h.renderGrantsError(w, r, tenantID, p.UserID, validationErr, kind, http.StatusUnprocessableEntity)
		return
	}
	res, err := h.deps.Grants.IssueGrant(r.Context(), in)
	switch {
	case errors.Is(err, ErrPerGrantCapExceeded):
		h.renderGrantsError(w, r, tenantID, p.UserID,
			"Valor acima do limite por grant. Requer aprovação 4-eyes (em construção).",
			kind, http.StatusUnprocessableEntity)
		return
	case errors.Is(err, ErrPerTenantWindowCapExceeded):
		h.renderGrantsError(w, r, tenantID, p.UserID,
			"Tenant excedeu o limite acumulado de 365 dias. Requer aprovação 4-eyes (em construção).",
			kind, http.StatusUnprocessableEntity)
		return
	case err != nil:
		h.fail(w, http.StatusInternalServerError, "issue grant", err)
		return
	}
	if eq := CapEquivalence(in.Kind, in.Amount, in.PeriodDays); eq >= AlertThresholdTokens {
		h.deps.Logger.Warn("web/master: high-value grant issued",
			"tenant_id", tenantID.String(),
			"actor_user_id", p.UserID.String(),
			"grant_id", res.Grant.ID.String(),
			"kind", string(in.Kind),
			"equivalent_tokens", eq,
		)
	}
	grants, err := h.deps.Grants.ListGrants(r.Context(), tenantID)
	if err != nil {
		h.fail(w, http.StatusInternalServerError, "list grants after issue", err)
		return
	}
	grants = ensureGrantPresent(grants, res.Grant)
	token := h.deps.CSRFToken(r)
	if token == "" {
		h.fail(w, http.StatusInternalServerError, "csrf token missing", errors.New("empty csrf token"))
		return
	}
	data := h.buildGrantsPageData(tenantID, p.UserID, grants,
		"Cortesia concedida com sucesso.", "",
		string(GrantKindFreeSubscriptionPeriod), token)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "private, no-store")
	if err := grantsPanelTmpl.Execute(w, data); err != nil {
		h.deps.Logger.Error("web/master: render grants panel after issue", "err", err)
	}
}

// RevokeGrant handles POST /master/grants/{id}/revoke. Validates the
// revoke reason, calls the GrantRevoker port, and re-renders the
// grants panel for the grant's tenant. ErrGrantAlreadyConsumed
// surfaces an inline error that points the operator to the
// compensating-grant flow (ADR-0098 §D4).
func (h *Handler) RevokeGrant(w http.ResponseWriter, r *http.Request) {
	if !h.requireGrants(w) {
		return
	}
	p, ok := iam.PrincipalFromContext(r.Context())
	if !ok {
		h.fail(w, http.StatusUnauthorized, "principal missing", errors.New("no principal in context"))
		return
	}
	grantID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "invalid grant id", http.StatusBadRequest)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	reason := strings.TrimSpace(r.PostFormValue("reason"))
	tenantIDStr := strings.TrimSpace(r.PostFormValue("tenant_id"))
	tenantID, terr := uuid.Parse(tenantIDStr)
	if terr != nil {
		http.Error(w, "invalid tenant id", http.StatusBadRequest)
		return
	}
	if len(reason) < 10 {
		h.renderGrantsError(w, r, tenantID, p.UserID,
			"Motivo da revogação deve ter pelo menos 10 caracteres.",
			string(GrantKindFreeSubscriptionPeriod), http.StatusUnprocessableEntity)
		return
	}
	rerr := h.deps.Grants.RevokeGrant(r.Context(), RevokeGrantInput{
		ActorUserID: p.UserID,
		GrantID:     grantID,
		Reason:      reason,
	})
	switch {
	case errors.Is(rerr, ErrGrantNotFound):
		http.Error(w, http.StatusText(http.StatusNotFound), http.StatusNotFound)
		return
	case errors.Is(rerr, ErrGrantAlreadyConsumed):
		h.renderGrantsError(w, r, tenantID, p.UserID,
			"Grant já consumido. Não é possível revogar — emita uma cortesia compensatória.",
			string(GrantKindFreeSubscriptionPeriod), http.StatusUnprocessableEntity)
		return
	case errors.Is(rerr, ErrGrantAlreadyRevoked):
		http.Error(w, "grant already revoked", http.StatusConflict)
		return
	case rerr != nil:
		h.fail(w, http.StatusInternalServerError, "revoke grant", rerr)
		return
	}
	grants, err := h.deps.Grants.ListGrants(r.Context(), tenantID)
	if err != nil {
		h.fail(w, http.StatusInternalServerError, "list grants after revoke", err)
		return
	}
	token := h.deps.CSRFToken(r)
	if token == "" {
		h.fail(w, http.StatusInternalServerError, "csrf token missing", errors.New("empty csrf token"))
		return
	}
	data := h.buildGrantsPageData(tenantID, p.UserID, grants,
		"Grant revogado com sucesso.", "",
		string(GrantKindFreeSubscriptionPeriod), token)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "private, no-store")
	if err := grantsPanelTmpl.Execute(w, data); err != nil {
		h.deps.Logger.Error("web/master: render grants panel after revoke", "err", err)
	}
}

// renderGrantsError re-renders the grants panel with the inline error
// banner; status defaults to 422 so HTMX callers can react to it.
func (h *Handler) renderGrantsError(w http.ResponseWriter, r *http.Request, tenantID, actor uuid.UUID, msg, kind string, status int) {
	grants, _ := h.deps.Grants.ListGrants(r.Context(), tenantID)
	token := h.deps.CSRFToken(r)
	if token == "" {
		h.fail(w, http.StatusInternalServerError, "csrf token missing", errors.New("empty csrf token"))
		return
	}
	data := h.buildGrantsPageData(tenantID, actor, grants, "", msg, kind, token)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	if err := grantsPanelTmpl.Execute(w, data); err != nil {
		h.deps.Logger.Error("web/master: render grants error", "err", err)
	}
}

// buildGrantsPageData stitches the view model used by the layout and
// the panel partial. Keeping a single shape across both means an HTMX
// partial swap is byte-identical to a full page re-render.
func (h *Handler) buildGrantsPageData(tenantID, actor uuid.UUID, grants []GrantRow, flash, formError, kind, token string) grantsPageData {
	return grantsPageData{
		TenantID:  tenantID,
		ActorID:   actor,
		Grants:    grants,
		Flash:     flash,
		FormError: formError,
		Kind:      kind,
		CSRFInput: csrf.FormHidden(token),
		HXHeaders: csrf.HXHeadersAttr(token),
		CSRFMeta:  csrf.MetaTag(token),
	}
}

// parseIssueGrantForm validates the create-grant POST body. The
// returned IssueGrantInput is only meaningful when the third return
// value (validation error) is empty. kind is returned even on error
// so renderGrantsError can preserve the kind switch's UI state.
func parseIssueGrantForm(r *http.Request, actor, tenantID uuid.UUID) (IssueGrantInput, string, string) {
	rawKind := strings.TrimSpace(r.PostFormValue("kind"))
	reason := strings.TrimSpace(r.PostFormValue("reason"))
	var (
		kind  GrantKind
		input IssueGrantInput
	)
	switch rawKind {
	case string(GrantKindFreeSubscriptionPeriod):
		kind = GrantKindFreeSubscriptionPeriod
	case string(GrantKindExtraTokens):
		kind = GrantKindExtraTokens
	default:
		return IssueGrantInput{}, string(GrantKindFreeSubscriptionPeriod), "Tipo de cortesia inválido."
	}
	if len(reason) < 10 {
		return IssueGrantInput{}, rawKind, "Motivo deve ter pelo menos 10 caracteres."
	}
	input = IssueGrantInput{
		ActorUserID: actor,
		TenantID:    tenantID,
		Kind:        kind,
		Reason:      reason,
	}
	switch kind {
	case GrantKindFreeSubscriptionPeriod:
		v := strings.TrimSpace(r.PostFormValue("period_days"))
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			return IssueGrantInput{}, rawKind, "Período em dias deve ser um inteiro positivo."
		}
		if n > 366 {
			return IssueGrantInput{}, rawKind, "Período em dias não pode exceder 366."
		}
		input.PeriodDays = n
	case GrantKindExtraTokens:
		v := strings.TrimSpace(r.PostFormValue("amount"))
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n <= 0 {
			return IssueGrantInput{}, rawKind, "Quantidade de tokens deve ser um inteiro positivo."
		}
		input.Amount = n
	}
	return input, rawKind, ""
}

// requireGrants short-circuits the grants handlers when the Grants
// port is not wired (Deps.Grants == nil). Returns 503 with a
// human-readable note so deploys that haven't yet wired the wallet
// adapter degrade gracefully instead of panicking.
func (h *Handler) requireGrants(w http.ResponseWriter) bool {
	if h.deps.Grants == nil {
		http.Error(w, "grants surface is not configured on this deploy", http.StatusServiceUnavailable)
		return false
	}
	return true
}

// ensureGrantPresent prepends grant when the slice does not already
// contain a row with grant.ID.
func ensureGrantPresent(grants []GrantRow, grant GrantRow) []GrantRow {
	for _, g := range grants {
		if g.ID == grant.ID {
			return grants
		}
	}
	return append([]GrantRow{grant}, grants...)
}
