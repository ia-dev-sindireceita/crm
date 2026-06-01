package middleware

// SIN-63958 / master-impersonation-spec §1.3: session-bound impersonation
// middleware. Replaces the per-request X-Impersonate-Tenant header path
// (legacy Impersonation middleware) with a server-authoritative envelope
// stored in master_impersonation_session.
//
// Mount AFTER RequireAuth — the middleware reads iam.Session from
// context to learn the calling user, and the master session id from
// the __Host-sess-master cookie. A missing session at this point is a
// wireup bug, not a user error: the middleware returns 503 so the
// router lint can rely on "RequireAuth was on the path" being true.
//
// Behaviour (spec §1.3 steps 1–8):
//
//   1. Session missing on ctx               → 503 + log.
//   2. ActiveForSession → ErrNoActive…      → pass through, no-op.
//   3. ActiveForSession → DB error          → 503 + audit "lookup_error"
//                                              + STOP (fail-closed,
//                                              spec §0.2).
//   4. clock() >= ExpiresAt                 → End(reason="expired") +
//                                              audit impersonation_stop +
//                                              303 → /master/tenants?expired=1
//   5. MasterChecker says not master        → End(reason="role_lost") + 403.
//   6. Resolve tenant by id → ErrTenantNotFound → End(reason="tenant_missing") + 404.
//                                Other error  → 503.
//   7. tenancy.WithContext swap + audit.ContextWithCorrelationID.
//   8. Serve next.
//
// No per-request impersonation_stop row — spec §1.3 step 8 keeps the
// audit volume bounded by emitting stop only on (a) expiry (step 4) or
// (b) explicit /master/impersonation/end.

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/adapter/httpapi/sessioncookie"
	"github.com/pericles-luz/crm/internal/iam"
	"github.com/pericles-luz/crm/internal/iam/audit"
	"github.com/pericles-luz/crm/internal/iam/impersonation"
	"github.com/pericles-luz/crm/internal/tenancy"
)

// ImpersonationFromSession returns the middleware. Constructor panics
// on a missing required dep so misconfigured wireup fails at boot,
// matching the existing Impersonation / RequireAction pattern.
func ImpersonationFromSession(
	checker MasterChecker,
	resolver tenancy.ByIDResolver,
	sessions impersonation.Repo,
	auditor audit.SplitLogger,
	clock func() time.Time,
	logger *slog.Logger,
) func(http.Handler) http.Handler {
	if checker == nil {
		panic("middleware: ImpersonationFromSession MasterChecker is nil")
	}
	if resolver == nil {
		panic("middleware: ImpersonationFromSession ByIDResolver is nil")
	}
	if sessions == nil {
		panic("middleware: ImpersonationFromSession impersonation.Repo is nil")
	}
	if auditor == nil {
		panic("middleware: ImpersonationFromSession audit.SplitLogger is nil")
	}
	if clock == nil {
		clock = func() time.Time { return time.Now().UTC() }
	}
	if logger == nil {
		logger = slog.Default()
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			sess, ok := SessionFromContext(r.Context())
			if !ok {
				logger.ErrorContext(r.Context(), "impersonation_session: no session on context — RequireAuth wireup gap",
					slog.String("path", r.URL.Path),
				)
				http.Error(w, "impersonation requires session", http.StatusServiceUnavailable)
				return
			}

			masterSessionID, ok := readMasterSessionID(r)
			if !ok {
				// Mounted on /master/*, the master cookie should
				// always be present (mastermfa.Auth runs upstream
				// on /m/*; the /master/* surface authenticates via
				// the tenant cookie + master role). When the
				// master cookie is absent the operator is acting
				// as a normal master-role user without any active
				// impersonation envelope — pass through.
				next.ServeHTTP(w, r)
				return
			}

			active, err := sessions.ActiveForSession(r.Context(), masterSessionID)
			if err != nil {
				if errors.Is(err, impersonation.ErrNoActiveImpersonation) {
					// Step 2 — no envelope active, common case.
					next.ServeHTTP(w, r)
					return
				}
				// Step 3 — fail-closed. Best-effort audit; the
				// lookup error MUST NOT silently let the request
				// through.
				_ = auditor.WriteSecurity(r.Context(), audit.SecurityAuditEvent{
					Event:       audit.SecurityEventImpersonationStop,
					ActorUserID: sess.UserID,
					Target: map[string]any{
						"reason":            "lookup_error",
						"master_session_id": masterSessionID.String(),
						"error":             err.Error(),
					},
					OccurredAt: clock(),
				})
				logger.ErrorContext(r.Context(), "impersonation_session: lookup failed",
					slog.String("master_session_id", masterSessionID.String()),
					slog.String("error", err.Error()),
				)
				http.Error(w, "impersonation lookup failed", http.StatusServiceUnavailable)
				return
			}

			// Step 4 — expiry. The check is clock()>=ExpiresAt so
			// the boundary case (exactly at ExpiresAt) ends rather
			// than continuing.
			now := clock()
			if !now.Before(active.ExpiresAt) {
				_ = sessions.End(r.Context(), active.ID, active.MasterUserID, "expired", now)
				tenantID := active.TargetTenantID
				_ = auditor.WriteSecurity(r.Context(), audit.SecurityAuditEvent{
					Event:         audit.SecurityEventImpersonationStop,
					ActorUserID:   sess.UserID,
					TenantID:      &tenantID,
					CorrelationID: &active.ID,
					Target: map[string]any{
						"reason":      "expired",
						"duration_ms": now.Sub(active.StartedAt).Milliseconds(),
						"tenant_id":   tenantID.String(),
					},
					OccurredAt: now,
				})
				http.Redirect(w, r, "/master/tenants?expired=1", http.StatusSeeOther)
				return
			}

			// Step 5 — role gate. A master who lost the role
			// mid-envelope cannot continue impersonating; we end
			// the envelope and 403.
			isMaster, err := checker.IsMaster(r.Context(), sess.UserID)
			if err != nil {
				logger.ErrorContext(r.Context(), "impersonation_session: master check failed",
					slog.String("user_id", sess.UserID.String()),
					slog.String("error", err.Error()),
				)
				http.Error(w, "impersonation role check failed", http.StatusServiceUnavailable)
				return
			}
			if !isMaster {
				_ = sessions.End(r.Context(), active.ID, active.MasterUserID, "role_lost", now)
				tenantID := active.TargetTenantID
				_ = auditor.WriteSecurity(r.Context(), audit.SecurityAuditEvent{
					Event:         audit.SecurityEventImpersonationStop,
					ActorUserID:   sess.UserID,
					TenantID:      &tenantID,
					CorrelationID: &active.ID,
					Target: map[string]any{
						"reason":      "role_lost",
						"duration_ms": now.Sub(active.StartedAt).Milliseconds(),
						"tenant_id":   tenantID.String(),
					},
					OccurredAt: now,
				})
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}

			// Step 6 — tenant resolution. Spec §1.3 step 6 covers
			// the swap on success; the ErrTenantNotFound branch
			// here is a defensive add-on so a tenant deleted
			// mid-envelope cannot leave the operator stuck on a
			// 503 page. We end the envelope with "tenant_missing"
			// and return 404 so the operator sees a meaningful
			// error and the audit trail records why the envelope
			// closed.
			target, err := resolver.ResolveByID(r.Context(), active.TargetTenantID)
			if err != nil {
				if errors.Is(err, tenancy.ErrTenantNotFound) {
					_ = sessions.End(r.Context(), active.ID, active.MasterUserID, "tenant_missing", now)
					http.Error(w, "target tenant not found", http.StatusNotFound)
					return
				}
				logger.ErrorContext(r.Context(), "impersonation_session: tenant resolve failed",
					slog.String("tenant_id", active.TargetTenantID.String()),
					slog.String("error", err.Error()),
				)
				http.Error(w, "tenant resolve failed", http.StatusServiceUnavailable)
				return
			}

			// Steps 7 + 8 — swap context + tag audit + serve.
			ctx := tenancy.WithContext(r.Context(), target)
			ctx = audit.ContextWithCorrelationID(ctx, active.ID)
			ctx = withActiveImpersonation(ctx, active)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// readMasterSessionID lifts the master_session_id from the
// __Host-sess-master cookie. Returns (uuid.Nil, false) when the cookie
// is absent or unparseable — both cases are treated as "no master
// session", which collapses to the pass-through branch (step 2).
//
// Distinct from mastermfa.readMasterSessionID (an internal helper of
// the mastermfa package); duplicated here so the middleware does not
// pick up an import dependency on the mastermfa package's session
// adapter.
func readMasterSessionID(r *http.Request) (uuid.UUID, bool) {
	raw, err := sessioncookie.Read(r, sessioncookie.NameMaster)
	if err != nil {
		return uuid.Nil, false
	}
	id, err := uuid.Parse(raw)
	if err != nil {
		return uuid.Nil, false
	}
	return id, true
}

// ActiveImpersonation returns the envelope (if any) the middleware
// attached to the context. Handlers reading this MUST treat the bool
// as the source of truth for "is the operator currently impersonating
// a tenant on this request" — a non-nil session implies the audit
// correlation_id is already populated for downstream writes.
func ActiveImpersonation(ctx context.Context) (*impersonation.Session, bool) {
	s, ok := ctx.Value(activeImpersonationCtxKey{}).(*impersonation.Session)
	if !ok || s == nil {
		return nil, false
	}
	return s, true
}

type activeImpersonationCtxKey struct{}

func withActiveImpersonation(ctx context.Context, s *impersonation.Session) context.Context {
	return context.WithValue(ctx, activeImpersonationCtxKey{}, s)
}

// RequireRoleMaster gates a route on the principal carrying RoleMaster.
// Returned 403 on miss; the caller is expected to compose this after
// RequireAuth (which installs the Principal). Used for End + Feed
// (spec §1.4) where the impersonation envelope is optional but the
// surface is master-only.
func RequireRoleMaster() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			p, ok := iam.PrincipalFromContext(r.Context())
			if !ok {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			if !p.IsMaster() {
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
