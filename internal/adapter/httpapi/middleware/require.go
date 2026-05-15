package middleware

import (
	"net/http"
	"time"

	"github.com/pericles-luz/crm/internal/iam"
)

// RequireAuthDeps bundles the inputs RequireAuth needs to build a
// Principal from the validated session. MasterImpersonatingFn and
// MFAVerifiedAtFn are optional; nil returns "not impersonating" and
// "no recent step-up" respectively, which is the safe default for
// non-master flows. The Authorizer is constructed once at wireup; the
// middleware does not call it directly — RequireAction does.
type RequireAuthDeps struct {
	MasterImpersonatingFn func(*http.Request) bool
	MFAVerifiedAtFn       func(*http.Request) *time.Time
}

// RequireAuth is the deny-by-default authentication gate. It MUST be
// composed after middleware.Auth — Auth puts iam.Session in context;
// RequireAuth lifts it into an iam.Principal so the rest of the chain
// (RequireAction, audit) consumes the same canonical shape.
//
// A request that reaches RequireAuth without a session is the
// programmer-error case — Auth should have already redirected to
// /login. We collapse it to 401 here so the lint can rely on the
// "RequireAuth was on the path" invariant even if the wireup is wrong.
// The closed response avoids leaking information about whether the
// session existed but was expired vs never existed.
func RequireAuth(deps RequireAuthDeps) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			sess, ok := SessionFromContext(r.Context())
			if !ok {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			masterImpersonating := false
			if deps.MasterImpersonatingFn != nil {
				masterImpersonating = deps.MasterImpersonatingFn(r)
			}
			var mfaAt *time.Time
			if deps.MFAVerifiedAtFn != nil {
				mfaAt = deps.MFAVerifiedAtFn(r)
			}
			p := iam.PrincipalFromSession(sess, masterImpersonating, mfaAt)
			next.ServeHTTP(w, r.WithContext(iam.WithPrincipal(r.Context(), p)))
		})
	}
}

// ResourceResolver derives the iam.Resource for a request. Handlers
// that authorize a per-row action (e.g. read contact :id) supply a
// resolver that pulls the id from chi.URLParam; handlers that
// authorize a collection action (e.g. list contacts) return a
// Resource with an empty ID. A nil resolver yields the zero Resource —
// the per-action policy decides whether that is acceptable.
type ResourceResolver func(*http.Request) iam.Resource

// RequireAction returns a middleware that consults the Authorizer for
// the given Action. RequireAuth MUST precede it — RequireAction reads
// the Principal from context and fails closed when absent.
//
// On allow: forwards. On deny: 403 Forbidden with a generic
// "forbidden" body. The ReasonCode is intentionally NOT echoed to the
// client because policy names (denied_master_pii_step_up, denied_rbac,
// …) leak the existence and shape of internal authorization gates to
// external tenants — see [SIN-62756]. The full Decision (including
// ReasonCode, TargetKind, TargetID) is written to the request context
// so downstream audit middleware ([SIN-62254]) preserves the reason
// for internal troubleshooting without exposing it on the wire.
func RequireAction(authz iam.Authorizer, action iam.Action, resolve ResourceResolver) func(http.Handler) http.Handler {
	if authz == nil {
		panic("middleware: RequireAction Authorizer is nil")
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			p, ok := iam.PrincipalFromContext(r.Context())
			if !ok {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			var res iam.Resource
			if resolve != nil {
				res = resolve(r)
			}
			d := authz.Can(r.Context(), p, action, res)
			ctx := WithDecision(r.Context(), d)
			if !d.Allow {
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}
