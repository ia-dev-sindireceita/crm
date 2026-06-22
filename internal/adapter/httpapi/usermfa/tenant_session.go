package usermfa

import (
	"context"
	"net/http"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/adapter/httpapi/sessioncookie"
	"github.com/pericles-luz/crm/internal/iam"
	"github.com/pericles-luz/crm/internal/tenancy"
)

// TenantSessionValidator is the narrow slice of iam.Service the default
// TenantSessionResolver needs to validate a __Host-sess-tenant cookie.
// *iam.Service (and cmd/server's iamAdapter) satisfy it; it mirrors
// middleware.SessionValidator so the resolver shares the exact same
// validation semantics (tenant match, expiry, idle window) as the authed
// route group — there is one definition of "a valid tenant session".
type TenantSessionValidator interface {
	ValidateSession(ctx context.Context, tenantID, sessionID uuid.UUID) (iam.Session, error)
}

// tenantSessionResolver is the default TenantSessionResolver. It reads
// the __Host-sess-tenant cookie, resolves the request-scoped tenant
// (placed on context by middleware.TenantScope), validates the session
// row, and returns the server-derived (userID, tenantID). Any failure —
// missing tenant, missing/malformed cookie, invalid/expired session —
// collapses to ok=false so the Setup handler falls back to the pending
// predicate or the styled login redirect. It never trusts request input
// for identity (OWASP A01: the actor is derived from the validated
// session, never from a form/query/header).
type tenantSessionResolver struct {
	validator TenantSessionValidator
}

// NewTenantSessionResolver wires the default resolver around an
// iam-session validator. Returns a nil TenantSessionResolver when
// validator is nil so a caller can assign the result to
// HandlerConfig.TenantSession unconditionally and still get the
// pending-only fallback when the dependency is absent.
func NewTenantSessionResolver(validator TenantSessionValidator) TenantSessionResolver {
	if validator == nil {
		return nil
	}
	return &tenantSessionResolver{validator: validator}
}

// ResolveTenantSession satisfies TenantSessionResolver.
func (a *tenantSessionResolver) ResolveTenantSession(r *http.Request) (TenantActor, bool) {
	if r == nil {
		return TenantActor{}, false
	}
	tenant, err := tenancy.FromContext(r.Context())
	if err != nil || tenant == nil || tenant.ID == uuid.Nil {
		return TenantActor{}, false
	}
	raw, err := sessioncookie.Read(r, sessioncookie.NameTenant)
	if err != nil {
		return TenantActor{}, false
	}
	sessionID, err := uuid.Parse(raw)
	if err != nil {
		return TenantActor{}, false
	}
	sess, err := a.validator.ValidateSession(r.Context(), tenant.ID, sessionID)
	if err != nil {
		return TenantActor{}, false
	}
	return TenantActor{UserID: sess.UserID, TenantID: tenant.ID}, true
}

var _ TenantSessionResolver = (*tenantSessionResolver)(nil)
