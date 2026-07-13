package users

import (
	"context"
	"errors"
	"html/template"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/adapter/httpapi/csrf"
	"github.com/pericles-luz/crm/internal/branding"
	"github.com/pericles-luz/crm/internal/http/middleware/csp"
	"github.com/pericles-luz/crm/internal/iam"
	"github.com/pericles-luz/crm/internal/tenancy"
	"github.com/pericles-luz/crm/internal/tenantusers"
)

// Service is the subset of tenantusers use cases the surface needs. Kept as an
// interface so handler tests inject a fake without a DB.
type Service interface {
	List(ctx context.Context, actor tenantusers.Actor) ([]tenantusers.User, error)
	Get(ctx context.Context, actor tenantusers.Actor, id uuid.UUID) (tenantusers.User, error)
	Create(ctx context.Context, actor tenantusers.Actor, email string, role iam.Role) (tenantusers.User, tenantusers.Token, error)
	UpdateRole(ctx context.Context, actor tenantusers.Actor, id uuid.UUID, newRole iam.Role) (tenantusers.User, error)
	Deactivate(ctx context.Context, actor tenantusers.Actor, id uuid.UUID) (tenantusers.User, error)
	Reactivate(ctx context.Context, actor tenantusers.Actor, id uuid.UUID) (tenantusers.User, error)
}

// CSRFTokenFn returns the request's CSRF token, resolved from the session
// context populated by the authed group's CSRF middleware (double-submit,
// ADR 0073). It is transported to HTMX via hx-headers on <body> so every
// state-changing request on this surface carries X-CSRF-Token; without it
// the middleware rejects the mutation with Forbidden before role/2FA is
// even evaluated (SIN-67232).
type CSRFTokenFn func(*http.Request) string

// Deps bundles handler collaborators. Service is required; CSRFToken, Now
// and Logger default.
type Deps struct {
	Service   Service
	CSRFToken CSRFTokenFn
	Now       func() time.Time
	Logger    *slog.Logger
}

// Handler serves the /settings/users HTMX surface.
type Handler struct {
	deps Deps
}

// New constructs a Handler, rejecting a nil Service at boot.
func New(deps Deps) (*Handler, error) {
	if deps.Service == nil {
		return nil, errors.New("web/users: Service is required")
	}
	if deps.CSRFToken == nil {
		// Default keeps existing callers/tests working; production wires the
		// real session-backed resolver (users_wire.go). An empty token still
		// renders the hx-headers attribute — the token value simply resolves
		// per request.
		deps.CSRFToken = func(*http.Request) string { return "" }
	}
	if deps.Now == nil {
		deps.Now = func() time.Time { return time.Now().UTC() }
	}
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	return &Handler{deps: deps}, nil
}

// Routes mounts every endpoint on mux (Go 1.22 method+pattern syntax).
func (h *Handler) Routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /settings/users", h.list)
	mux.HandleFunc("GET /settings/users/new", h.newForm)
	mux.HandleFunc("GET /settings/users/{id}/edit", h.editForm)
	mux.HandleFunc("POST /settings/users", h.create)
	mux.HandleFunc("PATCH /settings/users/{id}", h.updateRole)
	mux.HandleFunc("POST /settings/users/{id}/deactivate", h.deactivate)
	mux.HandleFunc("POST /settings/users/{id}/reactivate", h.reactivate)
}

// actor pulls the tenant + user id from the authenticated Principal. TenantID
// ALWAYS comes from the session (SIN-66494 G2) — never from the request body.
func (h *Handler) actor(r *http.Request) (tenantusers.Actor, bool) {
	p, ok := iam.PrincipalFromContext(r.Context())
	if !ok || p.TenantID == uuid.Nil {
		return tenantusers.Actor{}, false
	}
	return tenantusers.Actor{TenantID: p.TenantID, UserID: p.UserID}, true
}

func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	actor, ok := h.actor(r)
	if !ok {
		h.fail(w, http.StatusInternalServerError, "principal required", nil)
		return
	}
	rows, err := h.loadRows(r.Context(), actor)
	if err != nil {
		h.fail(w, http.StatusInternalServerError, "list users", err)
		return
	}
	data := h.pageData(r, rows)
	h.render(w, http.StatusOK, pageTmpl, data)
}

func (h *Handler) newForm(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.actor(r); !ok {
		h.fail(w, http.StatusInternalServerError, "principal required", nil)
		return
	}
	h.render(w, http.StatusOK, formTmpl, formData{
		Mode:       "create",
		Role:       string(iam.RoleTenantAtendente), // least-privilege default
		CSPNonce:   csp.Nonce(r.Context()),
		CanGerente: true,
	})
}

func (h *Handler) editForm(w http.ResponseWriter, r *http.Request) {
	actor, ok := h.actor(r)
	if !ok {
		h.fail(w, http.StatusInternalServerError, "principal required", nil)
		return
	}
	id, ok := parseID(r)
	if !ok {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}
	u, err := h.deps.Service.Get(r.Context(), actor, id)
	if errors.Is(err, tenantusers.ErrUserNotFound) {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if err != nil {
		h.fail(w, http.StatusInternalServerError, "get user", err)
		return
	}
	// Last-gerente guard for demoting self: disable the Atendente option when
	// this user is the last active gerente. Server re-checks regardless.
	lastGerente, err := h.isLastActiveGerente(r.Context(), actor, u)
	if err != nil {
		h.fail(w, http.StatusInternalServerError, "guard check", err)
		return
	}
	h.render(w, http.StatusOK, formTmpl, formData{
		Mode:       "edit",
		ID:         u.ID.String(),
		Email:      u.Email,
		Role:       string(u.Role),
		LockDemote: lastGerente,
		CSPNonce:   csp.Nonce(r.Context()),
		CanGerente: true,
	})
}

func (h *Handler) create(w http.ResponseWriter, r *http.Request) {
	actor, ok := h.actor(r)
	if !ok {
		h.fail(w, http.StatusInternalServerError, "principal required", nil)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	email := strings.TrimSpace(r.PostFormValue("email"))
	role := iam.Role(strings.TrimSpace(r.PostFormValue("role")))

	created, _, err := h.deps.Service.Create(r.Context(), actor, email, role)
	if err != nil {
		h.writeCreateError(w, r, email, role, err)
		return
	}
	// Success: re-render the list with a post-creation invite panel (Option A —
	// no secret shown on screen).
	rows, lerr := h.loadRows(r.Context(), actor)
	if lerr != nil {
		h.fail(w, http.StatusInternalServerError, "list users", lerr)
		return
	}
	data := h.pageData(r, rows)
	data.Flash = flashData{Kind: "success", Message: "Usuário " + created.Email + " criado. Um convite para definir a senha foi gerado; a pessoa poderá acessar após defini-la."}
	h.render(w, http.StatusCreated, listPartialTmpl, data)
}

func (h *Handler) updateRole(w http.ResponseWriter, r *http.Request) {
	actor, ok := h.actor(r)
	if !ok {
		h.fail(w, http.StatusInternalServerError, "principal required", nil)
		return
	}
	id, ok := parseID(r)
	if !ok {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	role := iam.Role(strings.TrimSpace(r.PostFormValue("role")))
	if _, err := h.deps.Service.UpdateRole(r.Context(), actor, id, role); err != nil {
		h.writeMutationError(w, r, actor, err)
		return
	}
	h.renderList(w, r, actor, http.StatusOK, flashData{})
}

func (h *Handler) deactivate(w http.ResponseWriter, r *http.Request) {
	actor, ok := h.actor(r)
	if !ok {
		h.fail(w, http.StatusInternalServerError, "principal required", nil)
		return
	}
	id, ok := parseID(r)
	if !ok {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}
	if _, err := h.deps.Service.Deactivate(r.Context(), actor, id); err != nil {
		h.writeMutationError(w, r, actor, err)
		return
	}
	h.renderList(w, r, actor, http.StatusOK, flashData{})
}

func (h *Handler) reactivate(w http.ResponseWriter, r *http.Request) {
	actor, ok := h.actor(r)
	if !ok {
		h.fail(w, http.StatusInternalServerError, "principal required", nil)
		return
	}
	id, ok := parseID(r)
	if !ok {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}
	if _, err := h.deps.Service.Reactivate(r.Context(), actor, id); err != nil {
		h.writeMutationError(w, r, actor, err)
		return
	}
	h.renderList(w, r, actor, http.StatusOK, flashData{})
}

// writeCreateError re-renders the create form with the field/global error and
// a 422 (validation) or 409 (anti-lockout) status.
func (h *Handler) writeCreateError(w http.ResponseWriter, r *http.Request, email string, role iam.Role, err error) {
	fd := formData{
		Mode:       "create",
		Email:      email,
		Role:       string(role),
		CSPNonce:   csp.Nonce(r.Context()),
		CanGerente: true,
	}
	// The create form's default hx-target is #users-list-inner; on a
	// validation error redirect the swap back into the form container so the
	// re-rendered form (with the error) lands in place (HTMX HX-Retarget).
	retargetForm := func() {
		w.Header().Set("HX-Retarget", "#users-form")
		w.Header().Set("HX-Reswap", "innerHTML")
	}
	switch {
	case errors.Is(err, tenantusers.ErrInvalidEmail):
		fd.EmailError = "Informe um e-mail válido, ex.: pessoa@dominio.com."
		retargetForm()
		h.render(w, http.StatusUnprocessableEntity, formTmpl, fd)
	case errors.Is(err, tenantusers.ErrEmailTaken):
		fd.EmailError = "Já existe um usuário com este e-mail neste tenant. Edite o usuário existente ou use outro e-mail."
		retargetForm()
		h.render(w, http.StatusUnprocessableEntity, formTmpl, fd)
	case errors.Is(err, tenantusers.ErrInvalidRole):
		fd.GlobalError = "Papel inválido. Escolha Atendente ou Gerente."
		retargetForm()
		h.render(w, http.StatusUnprocessableEntity, formTmpl, fd)
	default:
		h.fail(w, http.StatusInternalServerError, "create user", err)
	}
}

// writeMutationError maps role-change / deactivate errors. Anti-lockout and
// not-found re-render the list with an alert so the operator sees why nothing
// changed.
func (h *Handler) writeMutationError(w http.ResponseWriter, r *http.Request, actor tenantusers.Actor, err error) {
	switch {
	case errors.Is(err, tenantusers.ErrLastGerente):
		h.renderList(w, r, actor, http.StatusConflict, flashData{Kind: "danger", Message: "Não é possível rebaixar ou desativar o último gerente ativo do tenant. Promova outro usuário a Gerente antes."})
	case errors.Is(err, tenantusers.ErrInvalidRole):
		h.renderList(w, r, actor, http.StatusUnprocessableEntity, flashData{Kind: "danger", Message: "Papel inválido. Escolha Atendente ou Gerente."})
	case errors.Is(err, tenantusers.ErrUserNotFound):
		http.Error(w, "not found", http.StatusNotFound)
	default:
		h.fail(w, http.StatusInternalServerError, "user mutation", err)
	}
}

// renderList re-fetches and renders the list partial with an optional flash.
func (h *Handler) renderList(w http.ResponseWriter, r *http.Request, actor tenantusers.Actor, status int, flash flashData) {
	rows, err := h.loadRows(r.Context(), actor)
	if err != nil {
		h.fail(w, http.StatusInternalServerError, "list users", err)
		return
	}
	data := h.pageData(r, rows)
	data.Flash = flash
	h.render(w, status, listPartialTmpl, data)
}

// loadRows fetches the tenant users and decorates each with view flags
// (IsSelf, CanDeactivate, CanDemote) derived from the active-gerente count.
func (h *Handler) loadRows(ctx context.Context, actor tenantusers.Actor) ([]userRow, error) {
	users, err := h.deps.Service.List(ctx, actor)
	if err != nil {
		return nil, err
	}
	activeGerentes := 0
	for _, u := range users {
		if u.Active() && u.Role == iam.RoleTenantGerente {
			activeGerentes++
		}
	}
	rows := make([]userRow, 0, len(users))
	for _, u := range users {
		lastGerente := u.Active() && u.Role == iam.RoleTenantGerente && activeGerentes <= 1
		rows = append(rows, userRow{
			ID:            u.ID.String(),
			Email:         u.Email,
			Role:          string(u.Role),
			Active:        u.Active(),
			IsSelf:        u.ID == actor.UserID,
			CanDeactivate: u.Active() && !lastGerente,
		})
	}
	return rows, nil
}

// isLastActiveGerente reports whether u is the tenant's only active gerente.
func (h *Handler) isLastActiveGerente(ctx context.Context, actor tenantusers.Actor, u tenantusers.User) (bool, error) {
	if u.Role != iam.RoleTenantGerente || !u.Active() {
		return false, nil
	}
	users, err := h.deps.Service.List(ctx, actor)
	if err != nil {
		return false, err
	}
	count := 0
	for _, x := range users {
		if x.Active() && x.Role == iam.RoleTenantGerente {
			count++
		}
	}
	return count <= 1, nil
}

func (h *Handler) pageData(r *http.Request, rows []userRow) pageData {
	name := ""
	if t, err := tenancy.FromContext(r.Context()); err == nil {
		name = t.Name
	}
	token := h.deps.CSRFToken(r)
	return pageData{
		TenantName:       name,
		Users:            rows,
		Count:            len(rows),
		CSRFMeta:         csrf.MetaTag(token),
		HXHeaders:        csrf.HXHeadersAttr(token),
		TenantThemeStyle: branding.ThemeStyleFromContext(r.Context()),
		CSPNonce:         csp.Nonce(r.Context()),
	}
}

func (h *Handler) render(w http.ResponseWriter, status int, tmpl *template.Template, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "private, no-store")
	w.WriteHeader(status)
	if err := tmpl.Execute(w, data); err != nil {
		h.deps.Logger.Error("web/users: render", "err", err)
	}
}

func (h *Handler) fail(w http.ResponseWriter, status int, msg string, err error) {
	h.deps.Logger.Error("web/users: "+msg, "err", err)
	http.Error(w, http.StatusText(status), status)
}

func parseID(r *http.Request) (uuid.UUID, bool) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		return uuid.Nil, false
	}
	return id, true
}
