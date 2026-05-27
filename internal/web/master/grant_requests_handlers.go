package master

// SIN-63605 / Fase 2.5 follow-up — five HTMX handlers for the 4-eyes
// approval surface. The wire layer (internal/adapter/httpapi/router.go)
// wraps each handler with RequireAuth, RequireAction, and (POSTs only)
// mastermfa.RequireRecentMFA. The handler itself trusts the resolved
// iam.Principal and limits itself to validation, port orchestration,
// and partial rendering.

import (
	"errors"
	"net/http"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/adapter/httpapi/csrf"
	"github.com/pericles-luz/crm/internal/branding"
	"github.com/pericles-luz/crm/internal/http/middleware/csp"
	"github.com/pericles-luz/crm/internal/iam"
)

// CreateGrantRequest handles POST /master/tenants/{id}/grants/requests.
// The form body mirrors the regular issue-grant form so the UI can
// re-POST a denied (cap-exceeded) submission verbatim. On success the
// response is a 303 → /master/grants/requests/{id} so the operator
// lands on the detail / second-approver page; HTMX picks the redirect
// up via HX-Redirect for in-place navigation.
func (h *Handler) CreateGrantRequest(w http.ResponseWriter, r *http.Request) {
	if !h.requireGrantRequests(w) {
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
	issueIn, _, validationErr := parseIssueGrantForm(r, p.UserID, tenantID)
	if validationErr != "" {
		http.Error(w, validationErr, http.StatusUnprocessableEntity)
		return
	}
	in := CreateGrantRequestInput{
		ActorUserID: issueIn.ActorUserID,
		TenantID:    issueIn.TenantID,
		Kind:        issueIn.Kind,
		PeriodDays:  issueIn.PeriodDays,
		Amount:      issueIn.Amount,
		Reason:      issueIn.Reason,
	}
	req, err := h.deps.GrantRequests.CreateGrantRequest(r.Context(), in)
	if err != nil {
		h.fail(w, http.StatusInternalServerError, "create grant request", err)
		return
	}
	location := "/master/grants/requests/" + req.ID.String()
	if r.Header.Get("HX-Request") == "true" {
		w.Header().Set("HX-Redirect", location)
		w.WriteHeader(http.StatusOK)
		return
	}
	http.Redirect(w, r, location, http.StatusSeeOther)
}

// ListGrantRequests handles GET /master/grants/requests. Renders the
// awaiting list. HTMX requests get the panel partial; full requests
// get the page shell.
func (h *Handler) ListGrantRequests(w http.ResponseWriter, r *http.Request) {
	if !h.requireGrantRequests(w) {
		return
	}
	if _, ok := iam.PrincipalFromContext(r.Context()); !ok {
		h.fail(w, http.StatusUnauthorized, "principal missing", errors.New("no principal in context"))
		return
	}
	requests, err := h.deps.GrantRequests.ListAwaitingRequests(r.Context())
	if err != nil {
		h.fail(w, http.StatusInternalServerError, "list grant requests", err)
		return
	}
	token := h.deps.CSRFToken(r)
	if token == "" {
		h.fail(w, http.StatusInternalServerError, "csrf token missing", errors.New("empty csrf token"))
		return
	}
	data := grantRequestsListData{
		Requests:         requests,
		CSRFInput:        csrf.FormHidden(token),
		HXHeaders:        csrf.HXHeadersAttr(token),
		CSRFMeta:         csrf.MetaTag(token),
		TenantThemeStyle: branding.ThemeStyleFromContext(r.Context()),
		CSPNonce:         csp.Nonce(r.Context()),
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "private, no-store")
	tmpl := grantRequestsLayoutTmpl
	if r.Header.Get("HX-Request") == "true" {
		tmpl = grantRequestsPanelTmpl
	}
	if err := tmpl.Execute(w, data); err != nil {
		h.deps.Logger.Error("web/master: render grant requests list", "err", err)
	}
}

// ShowGrantRequest handles GET /master/grants/requests/{id}. Renders
// the detail page with approve/reject forms when the request is still
// awaiting; a "decided" line replaces the forms otherwise.
func (h *Handler) ShowGrantRequest(w http.ResponseWriter, r *http.Request) {
	if !h.requireGrantRequests(w) {
		return
	}
	if _, ok := iam.PrincipalFromContext(r.Context()); !ok {
		h.fail(w, http.StatusUnauthorized, "principal missing", errors.New("no principal in context"))
		return
	}
	requestID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "invalid grant request id", http.StatusBadRequest)
		return
	}
	req, err := h.deps.GrantRequests.GetGrantRequest(r.Context(), requestID)
	switch {
	case errors.Is(err, ErrGrantRequestNotFound):
		http.Error(w, http.StatusText(http.StatusNotFound), http.StatusNotFound)
		return
	case err != nil:
		h.fail(w, http.StatusInternalServerError, "get grant request", err)
		return
	}
	h.renderGrantRequestDetail(w, r, req, "", "", http.StatusOK)
}

// ApproveGrantRequest handles POST /master/grants/requests/{id}/approve.
// Returns 422 when the actor is the requester (4-eyes invariant) and
// 409 when the request was already decided by a concurrent master.
func (h *Handler) ApproveGrantRequest(w http.ResponseWriter, r *http.Request) {
	h.decideGrantRequest(w, r, decideApprove)
}

// RejectGrantRequest handles POST /master/grants/requests/{id}/reject.
// Same error envelope as ApproveGrantRequest.
func (h *Handler) RejectGrantRequest(w http.ResponseWriter, r *http.Request) {
	h.decideGrantRequest(w, r, decideReject)
}

type decideVerb int

const (
	decideApprove decideVerb = iota
	decideReject
)

func (h *Handler) decideGrantRequest(w http.ResponseWriter, r *http.Request, verb decideVerb) {
	if !h.requireGrantRequests(w) {
		return
	}
	p, ok := iam.PrincipalFromContext(r.Context())
	if !ok {
		h.fail(w, http.StatusUnauthorized, "principal missing", errors.New("no principal in context"))
		return
	}
	requestID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "invalid grant request id", http.StatusBadRequest)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	in := DecideGrantRequestInput{
		ActorUserID: p.UserID,
		RequestID:   requestID,
		Reason:      r.PostFormValue("reason"),
	}
	var verbErr error
	switch verb {
	case decideApprove:
		_, verbErr = h.deps.GrantRequests.ApproveGrantRequest(r.Context(), in)
	case decideReject:
		verbErr = h.deps.GrantRequests.RejectGrantRequest(r.Context(), in)
	}
	switch {
	case errors.Is(verbErr, ErrGrantRequestNotFound):
		http.Error(w, http.StatusText(http.StatusNotFound), http.StatusNotFound)
		return
	case errors.Is(verbErr, ErrGrantRequestApproverIsCreator):
		h.renderGrantRequestDetailLookup(w, r, requestID,
			"O aprovador deve ser um usuário diferente do solicitante.",
			http.StatusUnprocessableEntity)
		return
	case errors.Is(verbErr, ErrGrantRequestAlreadyDecided):
		h.renderGrantRequestDetailLookup(w, r, requestID,
			"Esta solicitação já foi decidida por outro master.",
			http.StatusConflict)
		return
	case verbErr != nil:
		h.fail(w, http.StatusInternalServerError, "decide grant request", verbErr)
		return
	}
	// Re-read so the partial reflects the post-decision state +
	// SecondApproverID / DecidedAt populated by the adapter.
	updated, err := h.deps.GrantRequests.GetGrantRequest(r.Context(), requestID)
	if err != nil {
		h.fail(w, http.StatusInternalServerError, "reload grant request", err)
		return
	}
	flash := "Solicitação aprovada e grant emitida."
	if verb == decideReject {
		flash = "Solicitação rejeitada."
	}
	h.renderGrantRequestDetail(w, r, updated, flash, "", http.StatusOK)
}

// renderGrantRequestDetail emits the detail panel partial (HTMX swap)
// or the full page when HX-Request is absent. status defaults to 200
// on the happy path; the error branches above pass 4xx codes so the
// HTMX caller can react to the status.
func (h *Handler) renderGrantRequestDetail(w http.ResponseWriter, r *http.Request, req GrantRequest, flash, formError string, status int) {
	token := h.deps.CSRFToken(r)
	if token == "" {
		h.fail(w, http.StatusInternalServerError, "csrf token missing", errors.New("empty csrf token"))
		return
	}
	data := grantRequestDetailData{
		Request:          req,
		Flash:            flash,
		FormError:        formError,
		CSRFInput:        csrf.FormHidden(token),
		HXHeaders:        csrf.HXHeadersAttr(token),
		CSRFMeta:         csrf.MetaTag(token),
		TenantThemeStyle: branding.ThemeStyleFromContext(r.Context()),
		CSPNonce:         csp.Nonce(r.Context()),
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "private, no-store")
	if status != http.StatusOK {
		w.WriteHeader(status)
	}
	tmpl := grantRequestDetailLayoutTmpl
	if r.Header.Get("HX-Request") == "true" {
		tmpl = grantRequestDetailPanelTmpl
	}
	if err := tmpl.Execute(w, data); err != nil {
		h.deps.Logger.Error("web/master: render grant request detail", "err", err)
	}
}

// renderGrantRequestDetailLookup re-reads the request before rendering
// the error panel so the post-error swap shows the latest server state
// (e.g. SecondApproverID populated by the concurrent winner). A read
// failure here degrades to a generic placeholder rather than 500-ing
// on top of the original 422/409.
func (h *Handler) renderGrantRequestDetailLookup(w http.ResponseWriter, r *http.Request, id uuid.UUID, errMsg string, status int) {
	req, err := h.deps.GrantRequests.GetGrantRequest(r.Context(), id)
	if err != nil {
		req = GrantRequest{ID: id, State: GrantRequestStateAwaiting}
	}
	h.renderGrantRequestDetail(w, r, req, "", errMsg, status)
}

// requireGrantRequests short-circuits the surface when the
// GrantRequests port is not wired (Deps.GrantRequests == nil). Same
// failure shape as requireGrants — 503 with a human note so deploys
// that haven't yet wired the adapter degrade gracefully instead of
// panicking.
func (h *Handler) requireGrantRequests(w http.ResponseWriter) bool {
	if h.deps.GrantRequests == nil {
		http.Error(w, "grant request surface is not configured on this deploy", http.StatusServiceUnavailable)
		return false
	}
	return true
}
