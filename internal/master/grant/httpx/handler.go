// Package httpx exposes the courtesy-grant service over HTTP. It is the
// only place in this feature that imports net/http; the domain stays pure.
package httpx

import (
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"strings"

	"github.com/pericles-luz/crm/internal/master/grant"
)

// PrincipalResolver extracts the authenticated master from the request.
// Production wiring uses the platform's authn middleware; tests inject a
// fake. Returning an error short-circuits as 401.
type PrincipalResolver interface {
	Resolve(r *http.Request) (grant.Principal, error)
}

// PrincipalResolverFunc adapts a function to PrincipalResolver.
type PrincipalResolverFunc func(r *http.Request) (grant.Principal, error)

// Resolve implements PrincipalResolver.
func (f PrincipalResolverFunc) Resolve(r *http.Request) (grant.Principal, error) {
	return f(r)
}

// Handler exposes POST /master/grants and POST /master/grants/{id}/ratify.
type Handler struct {
	svc      *grant.Service
	resolver PrincipalResolver
}

// NewHandler wires the HTTP layer.
func NewHandler(svc *grant.Service, resolver PrincipalResolver) *Handler {
	return &Handler{svc: svc, resolver: resolver}
}

// Register attaches the handler to mux. Endpoints are deny-by-default
// authenticated via the injected resolver.
func (h *Handler) Register(mux *http.ServeMux) {
	mux.HandleFunc("POST /master/grants", h.create)
	mux.HandleFunc("POST /master/grants/{id}/ratify", h.ratify)
}

type createRequest struct {
	TenantID       string `json:"tenant_id"`
	SubscriptionID string `json:"subscription_id"`
	Amount         int64  `json:"amount"`
	Reason         string `json:"reason"`
}

type createResponse struct {
	GrantID string `json:"grant_id"`
	Status  string `json:"status"`
}

type errorResponse struct {
	Error string `json:"error"`
}

func (h *Handler) create(w http.ResponseWriter, r *http.Request) {
	principal, err := h.resolver.Resolve(r)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "unauthenticated")
		return
	}
	// IP is resolved here, not in the domain — that's a transport concern.
	if principal.IPAddress == "" {
		principal.IPAddress = clientIP(r)
	}

	defer func() { _ = r.Body.Close() }()
	var body createRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json")
		return
	}

	req := grant.Request{
		MasterID:       principal.MasterID,
		TenantID:       body.TenantID,
		SubscriptionID: body.SubscriptionID,
		Amount:         body.Amount,
		Reason:         body.Reason,
	}

	g, err := h.svc.GrantCourtesy(r.Context(), principal, req)
	switch {
	case err == nil:
		writeJSON(w, http.StatusOK, createResponse{GrantID: g.ID, Status: string(g.Status)})
	case errors.Is(err, grant.ErrRequiresApproval):
		writeError(w, http.StatusForbidden, "requires approval")
	case isValidationErr(err):
		writeError(w, http.StatusBadRequest, err.Error())
	default:
		writeError(w, http.StatusInternalServerError, "internal error")
	}
}

type ratifyRequest struct {
	Approve bool   `json:"approve"`
	Note    string `json:"note"`
}

func (h *Handler) ratify(w http.ResponseWriter, r *http.Request) {
	principal, err := h.resolver.Resolve(r)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "unauthenticated")
		return
	}
	if principal.IPAddress == "" {
		principal.IPAddress = clientIP(r)
	}

	id := r.PathValue("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "missing id")
		return
	}

	defer func() { _ = r.Body.Close() }()
	var body ratifyRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json")
		return
	}

	g, err := h.svc.Ratify(r.Context(), principal, id, body.Approve, body.Note)
	switch {
	case err == nil:
		writeJSON(w, http.StatusOK, createResponse{GrantID: g.ID, Status: string(g.Status)})
	case errors.Is(err, grant.ErrNotFound):
		writeError(w, http.StatusNotFound, "not found")
	case errors.Is(err, grant.ErrNotPending),
		errors.Is(err, grant.ErrApprovalDisabled):
		writeError(w, http.StatusConflict, err.Error())
	case errors.Is(err, grant.ErrSelfApproval):
		writeError(w, http.StatusForbidden, err.Error())
	default:
		writeError(w, http.StatusInternalServerError, "internal error")
	}
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, errorResponse{Error: msg})
}

// clientIP resolves the caller IP. It prefers the leftmost entry in
// X-Forwarded-For when present, else falls back to RemoteAddr. The
// trust chain for XFF must be enforced by the platform's reverse proxy
// (see SIN-62227 hardening notes).
func clientIP(r *http.Request) string {
	if v := r.Header.Get("X-Forwarded-For"); v != "" {
		if i := strings.Index(v, ","); i >= 0 {
			return strings.TrimSpace(v[:i])
		}
		return strings.TrimSpace(v)
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func isValidationErr(err error) bool {
	return errors.Is(err, grant.ErrInvalidMaster) ||
		errors.Is(err, grant.ErrInvalidTenant) ||
		errors.Is(err, grant.ErrInvalidSubscription) ||
		errors.Is(err, grant.ErrInvalidAmount) ||
		errors.Is(err, grant.ErrInvalidReason)
}

var _ PrincipalResolver = PrincipalResolverFunc(nil)
