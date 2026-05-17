package master

// SIN-62885 / Fase 2.5 C11 — read-only HTMX handlers for the master
// billing-history and token-ledger views. The wire layer wraps each
// route with RequireAuth + RequireAction (tenant.billing.view for the
// billing panel, tenant.wallet.view_ledger for the ledger). The
// handlers themselves trust the resolved iam.Principal and limit
// themselves to query parsing + port orchestration + partial rendering.

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/iam"
)

// ShowBilling renders the GET /master/tenants/{id}/billing view: three
// panels (subscription / invoices / grants). The route is read-only —
// no destructive actions live here. The render path falls back to a
// partial when HX-Request is set so a "load this tenant" swap from an
// external page can drop straight into the panel container.
func (h *Handler) ShowBilling(w http.ResponseWriter, r *http.Request) {
	if h.deps.Billing == nil {
		http.Error(w, "billing surface is not configured on this deploy", http.StatusServiceUnavailable)
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
	if !crossTenantPermitted(p, tenantID) {
		http.Error(w, http.StatusText(http.StatusForbidden), http.StatusForbidden)
		return
	}
	view, err := h.deps.Billing.ViewBilling(r.Context(), tenantID)
	switch {
	case errors.Is(err, ErrTenantNotFound):
		http.Error(w, http.StatusText(http.StatusNotFound), http.StatusNotFound)
		return
	case err != nil:
		h.fail(w, http.StatusInternalServerError, "view billing", err)
		return
	}
	token := h.deps.CSRFToken(r)
	if token == "" {
		h.fail(w, http.StatusInternalServerError, "csrf token missing", errors.New("empty csrf token"))
		return
	}
	data := newBillingPageData(view, token)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "private, no-store")
	tmpl := billingLayoutTmpl
	if r.Header.Get("HX-Request") == "true" {
		tmpl = billingPanelTmpl
	}
	if err := tmpl.Execute(w, data); err != nil {
		h.deps.Logger.Error("web/master: render billing", "err", err)
	}
}

// ShowLedger renders the GET /master/tenants/{id}/ledger view: a
// cursor-paginated ledger table. Cursor query params:
//
//	cursor_at   — RFC3339Nano timestamp of the last-rendered row
//	cursor_id   — UUID of the last-rendered row (tie-breaker)
//	page_size   — bounded by LedgerMaxPageSize (default 50)
//
// HTMX "load more": the table partial renders the next-cursor link
// with hx-get pointing at the same route. When HX-Request is set the
// handler returns the rows partial alone so the swap appends.
func (h *Handler) ShowLedger(w http.ResponseWriter, r *http.Request) {
	if h.deps.Ledger == nil {
		http.Error(w, "ledger surface is not configured on this deploy", http.StatusServiceUnavailable)
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
	if !crossTenantPermitted(p, tenantID) {
		http.Error(w, http.StatusText(http.StatusForbidden), http.StatusForbidden)
		return
	}
	opts := h.parseLedgerOptions(r, tenantID)
	page, err := h.deps.Ledger.ViewLedger(r.Context(), opts)
	if err != nil {
		h.fail(w, http.StatusInternalServerError, "view ledger", err)
		return
	}
	token := h.deps.CSRFToken(r)
	if token == "" {
		h.fail(w, http.StatusInternalServerError, "csrf token missing", errors.New("empty csrf token"))
		return
	}
	data := newLedgerPageData(tenantID, page, opts.PageSize, token)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "private, no-store")
	tmpl := ledgerLayoutTmpl
	// Both the chrome-less "load more" swap and the in-page filter form
	// pass HX-Request. They differ in target — load-more replaces the
	// rows partial (#ledger-rows), full re-fetch replaces #ledger-panel.
	// HX-Target lets us pick: when targeting #ledger-rows we send just
	// the rows partial; for anything else we send the full panel.
	if r.Header.Get("HX-Request") == "true" {
		if r.Header.Get("HX-Target") == "ledger-rows" {
			tmpl = ledgerRowsTmpl
		} else {
			tmpl = ledgerPanelTmpl
		}
	}
	if err := tmpl.Execute(w, data); err != nil {
		h.deps.Logger.Error("web/master: render ledger", "err", err)
	}
}

// crossTenantPermitted is the handler-level defensive gate for AC #3:
// a tenant-scoped principal (e.g. RoleTenantGerente) MUST NOT see a
// different tenant's billing/ledger even if the wire layer mis-mounts
// the route. RoleMaster (with or without impersonation) is allowed
// through — the master pool wiring is the next gate, and master
// operators legitimately span tenants on this console.
func crossTenantPermitted(p iam.Principal, pathTenantID uuid.UUID) bool {
	if p.IsMaster() {
		return true
	}
	return p.TenantID == pathTenantID
}

// parseLedgerOptions extracts the cursor + page_size query params,
// applying defaults and clamps. Malformed cursors collapse to "start
// from the top" rather than 4xx-ing — the operator pasted a URL they
// got from a colleague and the worst that happens is the first page.
func (h *Handler) parseLedgerOptions(r *http.Request, tenantID uuid.UUID) LedgerOptions {
	opts := LedgerOptions{TenantID: tenantID}
	pageSize := h.ledgerDefaultPageSize
	if v := r.URL.Query().Get("page_size"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= h.ledgerMaxPageSize {
			pageSize = n
		}
	}
	opts.PageSize = pageSize
	if v := strings.TrimSpace(r.URL.Query().Get("cursor_at")); v != "" {
		if t, err := time.Parse(time.RFC3339Nano, v); err == nil {
			opts.CursorOccurredAt = t
		}
	}
	if v := strings.TrimSpace(r.URL.Query().Get("cursor_id")); v != "" {
		if id, err := uuid.Parse(v); err == nil {
			opts.CursorID = id
		}
	}
	return opts
}
