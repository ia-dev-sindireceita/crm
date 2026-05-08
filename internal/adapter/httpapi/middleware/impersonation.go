package middleware

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/iam/audit"
	"github.com/pericles-luz/crm/internal/tenancy"
)

// HeaderImpersonateTenant carries the target tenant uuid the master
// user wants to act as for the duration of the request.
const HeaderImpersonateTenant = "X-Impersonate-Tenant"

// HeaderImpersonationReason is the human-readable justification for
// the impersonation. The audit row captures it verbatim; absence is a
// 400 (impersonation without a recorded motive is forbidden).
const HeaderImpersonationReason = "X-Impersonation-Reason"

// MasterChecker reports whether a user id belongs to a master user.
// Implementations consult the users table (is_master = true).
type MasterChecker interface {
	IsMaster(ctx context.Context, userID uuid.UUID) (bool, error)
}

// Impersonation is the master-only middleware that swaps the request's
// tenant scope to the id supplied via X-Impersonate-Tenant, after
// writing a non-repudiation audit row. Mount it AFTER Auth so the
// session is already on the context.
//
// Behaviour matrix:
//
//	header missing                  → pass through, no audit, no swap.
//	header present, non-master user → pass through, no audit, no swap
//	                                  (deliberately silent — a normal
//	                                  user trying the header should
//	                                  observe identical behaviour to a
//	                                  user who did not send it).
//	header present, master, no
//	  X-Impersonation-Reason         → 400 Bad Request.
//	header present, master, target
//	  tenant unknown                 → 404 Not Found.
//	header present, master, audit
//	  write fails                    → 500 Internal Server Error
//	                                  (request never proceeds, AC #5).
//	all checks ok                    → audit "impersonation_started",
//	                                  swap tenant in context, run
//	                                  next; defer "impersonation_ended"
//	                                  with duration_ms.
//
// The "_ended" event is best-effort: a panic in next or a write
// failure on the deferred call is logged-and-swallowed by the audit
// adapter. This is the documented trade-off in the issue lens
// "Idiomatic Go: defer is OK because close of transaction is
// independent of the log".
func Impersonation(checker MasterChecker, resolver tenancy.ByIDResolver, logger audit.Logger) func(http.Handler) http.Handler {
	if checker == nil {
		panic("middleware: Impersonation MasterChecker is nil")
	}
	if resolver == nil {
		panic("middleware: Impersonation ByIDResolver is nil")
	}
	if logger == nil {
		panic("middleware: Impersonation audit.Logger is nil")
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			rawTarget := r.Header.Get(HeaderImpersonateTenant)
			if rawTarget == "" {
				next.ServeHTTP(w, r)
				return
			}

			sess, ok := SessionFromContext(r.Context())
			if !ok {
				// Defense-in-depth: Impersonation MUST be mounted
				// after Auth. A missing session here is a wiring
				// bug, not a user error — fail loudly.
				http.Error(w, "impersonation requires session", http.StatusInternalServerError)
				return
			}

			isMaster, err := checker.IsMaster(r.Context(), sess.UserID)
			if err != nil {
				http.Error(w, "internal server error", http.StatusInternalServerError)
				return
			}
			if !isMaster {
				// Silent ignore: a regular agent's request must
				// look identical to one with no header at all so
				// header presence cannot be used to enumerate
				// master accounts.
				next.ServeHTTP(w, r)
				return
			}

			reason := r.Header.Get(HeaderImpersonationReason)
			if reason == "" {
				http.Error(w, "X-Impersonation-Reason header required for impersonation", http.StatusBadRequest)
				return
			}

			targetID, err := uuid.Parse(rawTarget)
			if err != nil {
				http.Error(w, "invalid X-Impersonate-Tenant uuid", http.StatusBadRequest)
				return
			}

			targetTenant, err := resolver.ResolveByID(r.Context(), targetID)
			if err != nil {
				if errors.Is(err, tenancy.ErrTenantNotFound) {
					http.Error(w, "target tenant not found", http.StatusNotFound)
					return
				}
				http.Error(w, "internal server error", http.StatusInternalServerError)
				return
			}

			startedAt := time.Now().UTC()
			tenantID := targetTenant.ID
			if err := logger.Log(r.Context(), audit.AuditEvent{
				Event:       audit.EventImpersonationStarted,
				ActorUserID: sess.UserID,
				TenantID:    &tenantID,
				Target: map[string]any{
					"tenant_id": tenantID.String(),
					"reason":    reason,
				},
				CreatedAt: startedAt,
			}); err != nil {
				// AC #5: a failed audit write blocks the request.
				// Without the trail we cannot allow impersonation
				// to proceed — non-repudiation is a precondition.
				http.Error(w, "audit write failed", http.StatusInternalServerError)
				return
			}

			defer func() {
				endedAt := time.Now().UTC()
				_ = logger.Log(r.Context(), audit.AuditEvent{
					Event:       audit.EventImpersonationEnded,
					ActorUserID: sess.UserID,
					TenantID:    &tenantID,
					Target: map[string]any{
						"tenant_id":   tenantID.String(),
						"reason":      reason,
						"duration_ms": endedAt.Sub(startedAt).Milliseconds(),
					},
					CreatedAt: endedAt,
				})
			}()

			ctx := tenancy.WithContext(r.Context(), targetTenant)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}
