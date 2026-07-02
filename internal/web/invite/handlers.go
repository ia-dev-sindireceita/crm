package invite

import (
	"context"
	"errors"
	"html/template"
	"log/slog"
	"net/http"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/branding"
	"github.com/pericles-luz/crm/internal/http/middleware/csp"
	"github.com/pericles-luz/crm/internal/iam/password"
	"github.com/pericles-luz/crm/internal/tenancy"
	"github.com/pericles-luz/crm/internal/tenantusers"
)

// minPasswordLen is echoed into the form's minlength hint. It MUST match the
// policy floor (ADR 0070 §5 min 12) — the server-side policy is the real
// gate; this only improves the client-side UX.
const minPasswordLen = 12

// Service is the subset of tenantusers.CredentialService the surface needs.
// Kept as an interface so handler tests inject a fake without a DB.
type Service interface {
	Resolve(ctx context.Context, tenantID uuid.UUID, plaintextToken string) (tenantusers.Invite, error)
	SetPassword(ctx context.Context, tenantID uuid.UUID, plaintextToken, newPassword string, pctx password.PolicyContext) (tenantusers.Invite, error)
}

// Deps bundles handler collaborators. Service is required; Logger defaults.
type Deps struct {
	Service Service
	Logger  *slog.Logger
}

// Handler serves the public /invite/{token} set-password surface.
type Handler struct {
	deps Deps
}

// New constructs a Handler, rejecting a nil Service at boot.
func New(deps Deps) (*Handler, error) {
	if deps.Service == nil {
		return nil, errors.New("web/invite: Service is required")
	}
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	return &Handler{deps: deps}, nil
}

// Routes mounts the GET (render) and POST (consume) endpoints.
func (h *Handler) Routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /invite/{token}", h.show)
	mux.HandleFunc("POST /invite/{token}", h.submit)
}

// show validates the token and renders the set-password form, or the generic
// error page on any failure. The plaintext token is never logged.
func (h *Handler) show(w http.ResponseWriter, r *http.Request) {
	tenant, ok := h.tenant(r)
	if !ok {
		h.renderError(w, r, http.StatusInternalServerError)
		return
	}
	token := r.PathValue("token")
	inv, err := h.deps.Service.Resolve(r.Context(), tenant.ID, token)
	if err != nil {
		// Invalid / expired / consumed all collapse to one generic page.
		h.renderError(w, r, http.StatusNotFound)
		return
	}
	h.render(w, http.StatusOK, formTmpl, formData{
		TenantName:       tenant.Name,
		Token:            token,
		Email:            inv.Email,
		MinLen:           minPasswordLen,
		TenantThemeStyle: branding.ThemeStyleFromContext(r.Context()),
		CSPNonce:         csp.Nonce(r.Context()),
	})
}

// submit consumes the token: validates the password against the reused ADR
// 0070 §5 policy, confirms the two fields match, and atomically sets the
// password + marks the token used. Policy failures re-render the form with a
// localized message and keep the token usable; token failures render the
// generic error page.
func (h *Handler) submit(w http.ResponseWriter, r *http.Request) {
	tenant, ok := h.tenant(r)
	if !ok {
		h.renderError(w, r, http.StatusInternalServerError)
		return
	}
	token := r.PathValue("token")
	if err := r.ParseForm(); err != nil {
		h.renderFormError(w, r, tenant, token, "Formulário inválido. Tente novamente.")
		return
	}
	pw := r.PostFormValue("password")
	confirm := r.PostFormValue("password_confirm")

	if pw != confirm {
		h.renderFormError(w, r, tenant, token, "As senhas não coincidem.")
		return
	}

	pctx := password.PolicyContext{TenantName: tenant.Name}
	_, err := h.deps.Service.SetPassword(r.Context(), tenant.ID, token, pw, pctx)
	if err == nil {
		h.render(w, http.StatusOK, doneTmpl, doneData{
			TenantName:       tenant.Name,
			TenantThemeStyle: branding.ThemeStyleFromContext(r.Context()),
			CSPNonce:         csp.Nonce(r.Context()),
		})
		return
	}

	// Weak / non-compliant password: re-render the form so the invitee can
	// retry. The token is NOT consumed on a policy failure.
	var perr *password.PolicyError
	if errors.As(err, &perr) {
		h.renderFormError(w, r, tenant, token, policyMessage(perr.Reason))
		return
	}
	// Token failure (invalid / expired / consumed / lost race) → generic page.
	if errors.Is(err, tenantusers.ErrTokenInvalid) {
		h.renderError(w, r, http.StatusNotFound)
		return
	}
	// Anything else is an infra failure; do not leak detail.
	h.deps.Logger.Error("web/invite: set password", "err", err)
	h.renderError(w, r, http.StatusInternalServerError)
}

// policyMessage maps a stable PolicyReason to a localized, user-facing string.
// The reasons are an enum (never mutated for i18n) so this switch is stable.
func policyMessage(reason password.PolicyReason) string {
	switch reason {
	case password.ReasonTooShort:
		return "A senha é muito curta. Use no mínimo 12 caracteres."
	case password.ReasonTooLong:
		return "A senha é muito longa. Use no máximo 128 caracteres."
	case password.ReasonMatchesIdentity:
		return "A senha não pode ser igual ao seu e-mail ou ao nome do tenant."
	case password.ReasonBreached:
		return "Esta senha aparece em vazamentos conhecidos. Escolha outra."
	default:
		return "Senha não permitida pela política. Escolha outra."
	}
}

func (h *Handler) renderFormError(w http.ResponseWriter, r *http.Request, tenant *tenancy.Tenant, token, msg string) {
	// Re-resolve the email for the form header; if the token has since become
	// invalid, fall through to the generic error page.
	inv, err := h.deps.Service.Resolve(r.Context(), tenant.ID, token)
	if err != nil {
		h.renderError(w, r, http.StatusNotFound)
		return
	}
	h.render(w, http.StatusUnprocessableEntity, formTmpl, formData{
		TenantName:       tenant.Name,
		Token:            token,
		Email:            inv.Email,
		MinLen:           minPasswordLen,
		PasswordError:    msg,
		TenantThemeStyle: branding.ThemeStyleFromContext(r.Context()),
		CSPNonce:         csp.Nonce(r.Context()),
	})
}

func (h *Handler) renderError(w http.ResponseWriter, r *http.Request, status int) {
	name := ""
	if t, err := tenancy.FromContext(r.Context()); err == nil {
		name = t.Name
	}
	h.render(w, status, errorTmpl, errorData{
		TenantName:       name,
		TenantThemeStyle: branding.ThemeStyleFromContext(r.Context()),
		CSPNonce:         csp.Nonce(r.Context()),
	})
}

// tenant pulls the Host-resolved tenant off the context (middleware.
// TenantScope put it there). A missing tenant is a wiring error, not a client
// error — the route is only mounted inside the tenanted group.
func (h *Handler) tenant(r *http.Request) (*tenancy.Tenant, bool) {
	t, err := tenancy.FromContext(r.Context())
	if err != nil || t == nil || t.ID == uuid.Nil {
		return nil, false
	}
	return t, true
}

func (h *Handler) render(w http.ResponseWriter, status int, tmpl *template.Template, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "private, no-store")
	// The token lives in the URL; keep it out of downstream referers.
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.WriteHeader(status)
	if err := tmpl.Execute(w, data); err != nil {
		h.deps.Logger.Error("web/invite: render", "err", err)
	}
}
