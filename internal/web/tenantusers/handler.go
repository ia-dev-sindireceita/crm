package tenantusers

import (
	"context"
	"errors"
	"html/template"
	"log/slog"
	"net/http"
	"strings"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/branding"
	"github.com/pericles-luz/crm/internal/http/middleware/csp"
	"github.com/pericles-luz/crm/internal/iam"
	"github.com/pericles-luz/crm/internal/tenancy"
	"github.com/pericles-luz/crm/internal/tenantusers"
	"github.com/pericles-luz/crm/internal/web/shell"
	"github.com/pericles-luz/crm/internal/web/userlabel"
)

// BasePath is the surface root. Kept as a constant so the handler and the
// router-side registration cannot drift.
const BasePath = "/settings/users"

// MaxEmailLen bounds the email input at the boundary (defense in depth — the
// column is citext/unbounded). It mirrors the maxlength the form renders and
// the RFC 5322 practical maximum.
const MaxEmailLen = 254

// userService is the domain seam the handler consumes. The concrete
// implementation is *tenantusers.Service; tests inject a fake so the handler
// is exercised without a database.
type userService interface {
	ListUsers(ctx context.Context, tenantID uuid.UUID) ([]*tenantusers.User, error)
	CreateUser(ctx context.Context, tenantID, actorID uuid.UUID, email string, role iam.Role) (*tenantusers.CreateResult, error)
	UpdateUserRole(ctx context.Context, tenantID, actorID, targetID uuid.UUID, newRole iam.Role) error
	DeactivateUser(ctx context.Context, tenantID, actorID, targetID uuid.UUID) error
}

// CSRFTokenFn / UserIDFn mirror the channels / dashboard surfaces: optional
// app-shell chrome collaborators sourced from the session by the auth
// middleware. UserID additionally supplies the audit actor.
type CSRFTokenFn func(*http.Request) string

// UserIDFn returns the authenticated user id.
type UserIDFn func(*http.Request) uuid.UUID

// Deps bundles the handler collaborators. Users is required; the rest
// default (Logger → slog.Default) or degrade gracefully (CSRFToken / UserID /
// UserLabels nil → shell fallbacks).
type Deps struct {
	Users      userService
	CSRFToken  CSRFTokenFn
	UserID     UserIDFn
	UserLabels userlabel.Directory
	Logger     *slog.Logger
}

// Handler serves the tenant user-management admin surface.
type Handler struct {
	deps Deps
}

// New validates and wires the Handler. A nil Users port fails boot so a
// misconfigured wire surfaces immediately.
func New(deps Deps) (*Handler, error) {
	if deps.Users == nil {
		return nil, errors.New("web/tenantusers: Users is required")
	}
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	return &Handler{deps: deps}, nil
}

// Routes registers every endpoint on mux. Go 1.22 method+pattern syntax so
// the chi outer match and this inner mux agree on the verbs; the router
// gates every pattern behind RequireAuth + RequireAction. Each POST is
// registered explicitly on the router side too (chi route-enumeration trap —
// memory reference_crm_inbox_chi_route_enumeration_trap).
func (h *Handler) Routes(mux *http.ServeMux) {
	mux.HandleFunc("GET "+BasePath, h.page)
	mux.HandleFunc("GET "+BasePath+"/new", h.newForm)
	mux.HandleFunc("GET "+BasePath+"/cancel", h.cancel)
	mux.HandleFunc("POST "+BasePath, h.create)
	mux.HandleFunc("GET "+BasePath+"/{id}/edit", h.editForm)
	mux.HandleFunc("POST "+BasePath+"/{id}/role", h.updateRole)
	mux.HandleFunc("POST "+BasePath+"/{id}/deactivate", h.deactivate)
}

// page renders the full user list inside the app shell.
func (h *Handler) page(w http.ResponseWriter, r *http.Request) {
	tenant, ok := h.tenant(w, r)
	if !ok {
		return
	}
	rows, err := h.loadRows(r.Context(), tenant.ID)
	if err != nil {
		h.fail(w, "load users", err)
		return
	}
	h.render(w, pageTmpl, h.newPageData(r, tenant, rows))
}

// newForm returns the create modal defaulting to the Atendente role.
func (h *Handler) newForm(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.tenant(w, r); !ok {
		return
	}
	h.render(w, modalTmpl, modalData{
		IsNew:   true,
		Action:  BasePath,
		RoleKey: string(iam.RoleTenantAtendente),
		Roles:   roleOptions(),
	})
}

// cancel clears the modal (empty 200 → #tenantusers-modal innerHTML emptied).
func (h *Handler) cancel(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
}

// create validates the form and creates a user, then renders the one-time
// temporary-password card + the refreshed list.
func (h *Handler) create(w http.ResponseWriter, r *http.Request) {
	tenant, ok := h.tenant(w, r)
	if !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		h.renderCreateError(w, "", "", "", "Não foi possível processar o formulário.")
		return
	}
	email := strings.TrimSpace(r.PostFormValue("email"))
	roleKey := strings.TrimSpace(r.PostFormValue("role"))
	if email == "" || len(email) > MaxEmailLen {
		h.renderCreateError(w, email, roleKey, "email", "Informe um e-mail válido (até 254 caracteres).")
		return
	}
	role, roleOK := parseRole(roleKey)
	if !roleOK {
		h.renderCreateError(w, email, roleKey, "role", "Selecione um papel válido (Gerente ou Atendente).")
		return
	}
	res, err := h.deps.Users.CreateUser(r.Context(), tenant.ID, h.actorID(r), email, role)
	if err != nil {
		switch {
		case errors.Is(err, tenantusers.ErrEmailConflict):
			h.renderCreateError(w, email, roleKey, "email", "Já existe um usuário com esse e-mail.")
		case errors.Is(err, tenantusers.ErrInvalidEmail):
			h.renderCreateError(w, email, roleKey, "email", "E-mail inválido.")
		case errors.Is(err, tenantusers.ErrRoleNotAssignable):
			h.renderCreateError(w, email, roleKey, "role", "Selecione um papel válido (Gerente ou Atendente).")
		default:
			h.fail(w, "create user", err)
		}
		return
	}
	rows, err := h.loadRows(r.Context(), tenant.ID)
	if err != nil {
		h.fail(w, "reload users", err)
		return
	}
	h.render(w, createTmpl, createRefresh{
		Credential: credentialData{Email: res.User.Email, TempPassword: res.TempPassword},
		Rows:       rows,
		Toast:      toastData{Message: "Usuário criado."},
	})
}

// editForm returns the edit-role modal pre-filled with the user's current
// role. The email is shown read-only (Recognition over Recall).
func (h *Handler) editForm(w http.ResponseWriter, r *http.Request) {
	tenant, ok := h.tenant(w, r)
	if !ok {
		return
	}
	id, ok := h.pathID(w, r)
	if !ok {
		return
	}
	u, err := h.getUser(w, r, tenant.ID, id)
	if err != nil {
		return
	}
	h.render(w, modalTmpl, modalData{
		IsNew:   false,
		Action:  BasePath + "/" + u.ID.String() + "/role",
		ID:      u.ID.String(),
		Email:   u.Email,
		RoleKey: string(u.Role),
		Roles:   roleOptions(),
	})
}

// updateRole changes the target user's role.
func (h *Handler) updateRole(w http.ResponseWriter, r *http.Request) {
	tenant, ok := h.tenant(w, r)
	if !ok {
		return
	}
	id, ok := h.pathID(w, r)
	if !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		h.renderRoleError(w, tenant.ID, id, "", "Não foi possível processar o formulário.")
		return
	}
	roleKey := strings.TrimSpace(r.PostFormValue("role"))
	role, roleOK := parseRole(roleKey)
	if !roleOK {
		h.renderRoleError(w, tenant.ID, id, roleKey, "Selecione um papel válido (Gerente ou Atendente).")
		return
	}
	err := h.deps.Users.UpdateUserRole(r.Context(), tenant.ID, h.actorID(r), id, role)
	switch {
	case err == nil:
		h.renderRefresh(w, r, tenant.ID, "Papel atualizado.")
	case errors.Is(err, tenantusers.ErrNotFound):
		http.NotFound(w, r)
	case errors.Is(err, tenantusers.ErrLastGerente):
		h.renderRoleError(w, tenant.ID, id, roleKey, "Não é possível rebaixar o último gerente ativo do tenant.")
	case errors.Is(err, tenantusers.ErrRoleNotAssignable):
		h.renderRoleError(w, tenant.ID, id, roleKey, "Selecione um papel válido (Gerente ou Atendente).")
	default:
		h.fail(w, "update role", err)
	}
}

// deactivate soft-deletes the target user and refreshes the list.
func (h *Handler) deactivate(w http.ResponseWriter, r *http.Request) {
	tenant, ok := h.tenant(w, r)
	if !ok {
		return
	}
	id, ok := h.pathID(w, r)
	if !ok {
		return
	}
	err := h.deps.Users.DeactivateUser(r.Context(), tenant.ID, h.actorID(r), id)
	switch {
	case err == nil:
		h.renderRefresh(w, r, tenant.ID, "Usuário desativado. O histórico é preservado.")
	case errors.Is(err, tenantusers.ErrNotFound):
		http.NotFound(w, r)
	case errors.Is(err, tenantusers.ErrLastGerente):
		h.renderListWithError(w, r, tenant.ID, "Não é possível desativar o último gerente ativo do tenant.")
	default:
		h.fail(w, "deactivate user", err)
	}
}

// ------------------------------------------------------------------ data

func (h *Handler) loadRows(ctx context.Context, tenantID uuid.UUID) ([]userRow, error) {
	users, err := h.deps.Users.ListUsers(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	rows := make([]userRow, 0, len(users))
	for _, u := range users {
		if u == nil {
			continue
		}
		rows = append(rows, rowFromUser(u))
	}
	return rows, nil
}

// getUser resolves id under the tenant scope, writing 404 for an unknown /
// RLS-hidden user. It reads the whole list rather than adding a Get to the
// service seam the handler needs — the surface already has ListUsers wired,
// and a per-tenant user set is small.
func (h *Handler) getUser(w http.ResponseWriter, r *http.Request, tenantID, id uuid.UUID) (*tenantusers.User, error) {
	users, err := h.deps.Users.ListUsers(r.Context(), tenantID)
	if err != nil {
		h.fail(w, "load user", err)
		return nil, err
	}
	for _, u := range users {
		if u != nil && u.ID == id {
			return u, nil
		}
	}
	http.NotFound(w, r)
	return nil, errors.New("not found")
}

// ---------------------------------------------------------------- render

func (h *Handler) renderRefresh(w http.ResponseWriter, r *http.Request, tenantID uuid.UUID, msg string) {
	rows, err := h.loadRows(r.Context(), tenantID)
	if err != nil {
		h.fail(w, "reload users", err)
		return
	}
	h.render(w, refreshTmpl, listRefresh{Rows: rows, Toast: toastData{Message: msg}})
}

// renderListWithError re-renders the list with an error toast (used when a
// guardrail — e.g. last-gerente — rejects a row action).
func (h *Handler) renderListWithError(w http.ResponseWriter, r *http.Request, tenantID uuid.UUID, msg string) {
	rows, err := h.loadRows(r.Context(), tenantID)
	if err != nil {
		h.fail(w, "reload users", err)
		return
	}
	// Reuse the refresh template; the toast slot carries the guardrail
	// message. A success class on a refusal is acceptable here because the
	// message text itself states the refusal; a dedicated danger toast is a
	// follow-up polish.
	h.render(w, refreshTmpl, listRefresh{Rows: rows, Toast: toastData{Message: msg}})
}

func (h *Handler) renderCreateError(w http.ResponseWriter, email, roleKey, field, msg string) {
	if _, ok := parseRole(roleKey); !ok {
		roleKey = string(iam.RoleTenantAtendente)
	}
	h.render(w, modalTmpl, modalData{
		IsNew:        true,
		Action:       BasePath,
		Email:        email,
		RoleKey:      roleKey,
		Roles:        roleOptions(),
		FieldError:   field,
		ErrorMessage: msg,
	})
}

func (h *Handler) renderRoleError(w http.ResponseWriter, tenantID, id uuid.UUID, roleKey, msg string) {
	email := ""
	if u, err := h.lookup(tenantID, id); err == nil && u != nil {
		email = u.Email
		if roleKey == "" {
			roleKey = string(u.Role)
		}
	}
	if _, ok := parseRole(roleKey); !ok {
		roleKey = string(iam.RoleTenantAtendente)
	}
	h.render(w, modalTmpl, modalData{
		IsNew:        false,
		Action:       BasePath + "/" + id.String() + "/role",
		ID:           id.String(),
		Email:        email,
		RoleKey:      roleKey,
		Roles:        roleOptions(),
		FieldError:   "role",
		ErrorMessage: msg,
	})
}

// lookup is the non-writing helper the error re-render uses to recover the
// user's email/role for the bounced form. It swallows the not-found case
// (returns nil) — the error re-render degrades to an empty email rather than
// failing the response.
func (h *Handler) lookup(tenantID, id uuid.UUID) (*tenantusers.User, error) {
	users, err := h.deps.Users.ListUsers(context.Background(), tenantID)
	if err != nil {
		return nil, err
	}
	for _, u := range users {
		if u != nil && u.ID == id {
			return u, nil
		}
	}
	return nil, nil
}

func (h *Handler) tenant(w http.ResponseWriter, r *http.Request) (*tenancy.Tenant, bool) {
	t, err := tenancy.FromContext(r.Context())
	if err != nil {
		h.deps.Logger.Error("web/tenantusers: tenant required", "err", err)
		http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		return nil, false
	}
	return t, true
}

func (h *Handler) pathID(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.NotFound(w, r)
		return uuid.Nil, false
	}
	return id, true
}

func (h *Handler) render(w http.ResponseWriter, tmpl *template.Template, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "private, no-store")
	if err := tmpl.Execute(w, data); err != nil {
		h.deps.Logger.Error("web/tenantusers: render", "err", err)
	}
}

func (h *Handler) fail(w http.ResponseWriter, op string, err error) {
	h.deps.Logger.Error("web/tenantusers: "+op, "err", err)
	http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
}

func (h *Handler) newPageData(r *http.Request, tenant *tenancy.Tenant, rows []userRow) pageData {
	return pageData{
		Rows:             rows,
		TenantName:       tenant.Name,
		TenantThemeStyle: branding.ThemeStyleFromContext(r.Context()),
		CSPNonce:         csp.Nonce(r.Context()),
		UserDisplayName:  h.userDisplayName(r, tenant.ID),
		NavItems:         buildNavItems(),
		UserMenuItems:    buildUserMenu(),
		CSRFToken:        h.csrfToken(r),
	}
}

func (h *Handler) csrfToken(r *http.Request) string {
	if h.deps.CSRFToken == nil {
		return ""
	}
	return h.deps.CSRFToken(r)
}

func (h *Handler) userDisplayName(r *http.Request, tenantID uuid.UUID) string {
	if h.deps.UserID == nil {
		return ""
	}
	return userlabel.Resolve(r.Context(), h.deps.UserLabels, tenantID, h.deps.UserID(r))
}

// actorID resolves the authenticated actor for the audit trail, returning
// uuid.Nil when the UserID collaborator is unwired (the Service then skips
// audit emission).
func (h *Handler) actorID(r *http.Request) uuid.UUID {
	if h.deps.UserID == nil {
		return uuid.Nil
	}
	return h.deps.UserID(r)
}

// buildNavItems / buildUserMenu mirror the channels / dashboard chrome so the
// surface renders inside the shared SidebarNav app-shell.
func buildNavItems() []shell.NavItem {
	return []shell.NavItem{
		{Label: "Inbox", Path: "/inbox", Icon: "inbox"},
		{Label: "Funil", Path: "/funnel", Icon: "git-branch"},
		{Label: "Contatos", Path: "/contacts", Icon: "users"},
		{Label: "Painel", Path: "/dashboard", Icon: "bar-chart"},
	}
}

func buildUserMenu() []shell.UserMenuItem {
	return []shell.UserMenuItem{
		{Label: "Sair", Path: "/logout", Form: true},
	}
}
