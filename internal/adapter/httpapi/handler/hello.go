package handler

import (
	"net/http"

	"github.com/pericles-luz/crm/internal/adapter/httpapi/middleware"
	"github.com/pericles-luz/crm/internal/adapter/httpapi/views"
	"github.com/pericles-luz/crm/internal/tenancy"
)

// HelloTenant renders the post-login landing page proving the auth →
// tenancy → RLS stack works end-to-end. The body MUST contain the tenant
// name (Acceptance Criterion #1.c of SIN-62217) so an integration test
// can assert isolation by string-matching the response.
//
// Both the tenant and the session arrive via context (TenantScope and
// Auth respectively). A missing tenant or session here is a programmer
// error — the route is wired inside both middlewares.
func HelloTenant(w http.ResponseWriter, r *http.Request) {
	tenant, err := tenancy.FromContext(r.Context())
	if err != nil {
		http.Error(w, "tenant scope missing", http.StatusInternalServerError)
		return
	}
	sess, ok := middleware.SessionFromContext(r.Context())
	if !ok {
		http.Error(w, "session missing", http.StatusInternalServerError)
		return
	}
	data := struct {
		TenantName string
		UserID     string
		CSRFToken  string
	}{
		TenantName: tenant.Name,
		UserID:     sess.UserID.String(),
		CSRFToken:  sess.CSRFToken,
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := views.Hello.ExecuteTemplate(w, "layout", data); err != nil {
		http.Error(w, "render error", http.StatusInternalServerError)
		return
	}
}
