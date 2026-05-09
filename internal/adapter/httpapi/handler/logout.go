package handler

import (
	"context"
	"net/http"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/adapter/httpapi/sessioncookie"
	"github.com/pericles-luz/crm/internal/tenancy"
)

// LogoutDeleter is the slice of iam.Service the logout handler needs.
// *iam.Service satisfies it.
type LogoutDeleter interface {
	Logout(ctx context.Context, tenantID, sessionID uuid.UUID) error
}

// Logout deletes the server-side session row (best-effort — a missing
// session is treated as already-logged-out), expires the client cookie,
// and redirects to /login. Mounted on the tenant-scoped chain but
// OUTSIDE the auth chain so users with a stale cookie can still hit it
// without a redirect loop.
func Logout(svc LogoutDeleter) http.HandlerFunc {
	if svc == nil {
		panic("handler: Logout deleter is nil")
	}
	return func(w http.ResponseWriter, r *http.Request) {
		tenant, err := tenancy.FromContext(r.Context())
		if err != nil {
			http.Error(w, "tenant scope missing", http.StatusInternalServerError)
			return
		}
		if value, err := sessioncookie.Read(r, sessioncookie.NameTenant); err == nil {
			if sessionID, err := uuid.Parse(value); err == nil {
				_ = svc.Logout(r.Context(), tenant.ID, sessionID)
			}
		}
		sessioncookie.ClearTenant(w)
		// Drop the CSRF cookie alongside the session when one was
		// presented — the next authenticated request must mint a
		// fresh token via Login, matching the per-session rotation
		// rule in ADR 0073 §D1/D3. Conditional emit keeps the
		// response bytes minimal when the client never carried a
		// __Host-csrf cookie (legacy session, programmatic client).
		if _, err := sessioncookie.Read(r, sessioncookie.NameCSRF); err == nil {
			sessioncookie.ClearCSRF(w)
		}
		http.Redirect(w, r, "/login", http.StatusFound)
	}
}
