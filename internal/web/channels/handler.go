package channels

import (
	"context"
	"errors"
	"html/template"
	"log/slog"
	"net/http"
	"strings"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/branding"
	"github.com/pericles-luz/crm/internal/channels"
	"github.com/pericles-luz/crm/internal/http/middleware/csp"
	"github.com/pericles-luz/crm/internal/tenancy"
	"github.com/pericles-luz/crm/internal/web/shell"
	"github.com/pericles-luz/crm/internal/web/userlabel"
)

// BasePath is the surface root. Kept as a constant so the handler and the
// router-side registration cannot drift.
const BasePath = "/settings/channels"

// MaxNameLen / MaxIdentityLen bound the free-form inputs at the boundary
// (defense in depth — the columns are unbounded text). They mirror the
// maxlength attributes the form renders.
const (
	MaxNameLen     = 120
	MaxIdentityLen = 120
)

// CSRFTokenFn / UserIDFn mirror the dashboard / wasession surfaces:
// optional app-shell chrome collaborators sourced from the session by the
// auth middleware. UserID is only used for the shell user-menu label
// today (P2 writes are gerente-gated at the route; the per-resource audit
// line is a P3 / SIN-66392 concern).
type CSRFTokenFn func(*http.Request) string

// UserIDFn returns the authenticated user id.
type UserIDFn func(*http.Request) uuid.UUID

// Deps bundles the handler collaborators. Channels + Access are required;
// the rest default (Logger → slog.Default) or degrade gracefully
// (CSRFToken / UserID / UserLabels nil → shell fallbacks).
type Deps struct {
	Channels   channels.Repository
	Access     channels.AccessRepository
	CSRFToken  CSRFTokenFn
	UserID     UserIDFn
	UserLabels userlabel.Directory
	// Audit records channel access-change privilege events (grant / revoke
	// / restricted-flip) into audit_log_security (SIN-66405). Optional: a
	// nil Auditor disables emission so the surface still renders under
	// fail-soft wiring, but production always provides it.
	Audit  AccessAuditor
	Logger *slog.Logger
}

// Handler serves the SIN-66391 channel-management admin surface.
type Handler struct {
	deps Deps
}

// New validates and wires the Handler. Nil required ports fail boot so a
// misconfigured wire surfaces immediately.
func New(deps Deps) (*Handler, error) {
	if deps.Channels == nil {
		return nil, errors.New("web/channels: Channels is required")
	}
	if deps.Access == nil {
		return nil, errors.New("web/channels: Access is required")
	}
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	return &Handler{deps: deps}, nil
}

// Routes registers every endpoint on mux. Go 1.22 method+pattern syntax
// so the chi outer match and this inner mux agree on the verbs; the
// router gates every pattern behind RequireAuth +
// RequireAction(ActionTenantChannelsManage). Each POST is registered
// explicitly on the router side too (chi route-enumeration trap —
// reference_crm_inbox_chi_route_enumeration_trap).
func (h *Handler) Routes(mux *http.ServeMux) {
	mux.HandleFunc("GET "+BasePath, h.page)
	mux.HandleFunc("GET "+BasePath+"/new", h.newForm)
	mux.HandleFunc("GET "+BasePath+"/cancel", h.cancel)
	mux.HandleFunc("POST "+BasePath, h.create)
	mux.HandleFunc("GET "+BasePath+"/{id}/edit", h.editForm)
	mux.HandleFunc("POST "+BasePath+"/{id}", h.update)
	mux.HandleFunc("POST "+BasePath+"/{id}/active", h.toggle)
}

// page renders the full registry inside the app shell.
func (h *Handler) page(w http.ResponseWriter, r *http.Request) {
	tenant, ok := h.tenant(w, r)
	if !ok {
		return
	}
	rows, err := h.loadRows(r.Context(), tenant.ID)
	if err != nil {
		h.fail(w, "load channels", err)
		return
	}
	h.render(w, pageTmpl, h.newPageData(r, tenant, rows))
}

// newForm returns the create modal with an all-checked roster (new-channel
// default per spec D2/D3).
func (h *Handler) newForm(w http.ResponseWriter, r *http.Request) {
	tenant, ok := h.tenant(w, r)
	if !ok {
		return
	}
	roster, err := h.roster(r.Context(), tenant.ID, nil, true)
	if err != nil {
		h.fail(w, "load roster", err)
		return
	}
	h.render(w, modalTmpl, modalData{
		IsNew:      true,
		Action:     BasePath,
		ChannelKey: channelTypes[0].Key,
		Types:      channelTypes,
		Roster:     roster,
	})
}

// cancel clears the modal (empty 200 → #channels-modal innerHTML emptied).
func (h *Handler) cancel(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
}

// create persists a new channel + its initial access roster.
func (h *Handler) create(w http.ResponseWriter, r *http.Request) {
	tenant, ok := h.tenant(w, r)
	if !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		h.renderCreateModal(w, r, tenant.ID, "", "", "", nil, false, "", "Não foi possível processar o formulário.")
		return
	}
	name := strings.TrimSpace(r.PostFormValue("name"))
	key := strings.TrimSpace(r.PostFormValue("channel_key"))
	identity := strings.TrimSpace(r.PostFormValue("identity"))
	userIDs := parseUserIDs(r.PostForm["user_ids"])
	restricted := parseRestricted(r)

	if name == "" || len(name) > MaxNameLen {
		h.renderCreateModal(w, r, tenant.ID, name, key, identity, userIDs, restricted, "name", "Informe um nome de exibição (até 120 caracteres).")
		return
	}
	if !validType(key) {
		h.renderCreateModal(w, r, tenant.ID, name, key, identity, userIDs, restricted, "type", "Selecione um tipo de canal válido.")
		return
	}
	if len(identity) > MaxIdentityLen {
		h.renderCreateModal(w, r, tenant.ID, name, key, identity, userIDs, restricted, "identity", "Identidade muito longa (até 120 caracteres).")
		return
	}
	ch, err := channels.New(tenant.ID, key, identity, name)
	if err != nil {
		h.renderCreateModal(w, r, tenant.ID, name, key, identity, userIDs, restricted, "identity", "Não foi possível criar o canal. Verifique os dados.")
		return
	}
	ch.SetRestricted(restricted)
	if err := h.deps.Channels.Create(r.Context(), ch); err != nil {
		if errors.Is(err, channels.ErrChannelConflict) {
			h.renderCreateModal(w, r, tenant.ID, name, key, identity, userIDs, restricted, "identity", "Já existe um canal com esse tipo e identidade.")
			return
		}
		h.fail(w, "create channel", err)
		return
	}
	if err := h.deps.Access.ReplaceAccess(r.Context(), tenant.ID, ch.ID, userIDs); err != nil {
		h.fail(w, "grant channel access", err)
		return
	}
	// Audit the initial roster + a restricted-on creation as privilege
	// events (SIN-66405). A fresh channel starts open with no grants, so
	// before is empty and from=false.
	actor := h.actorID(r)
	h.auditAccessReplace(r.Context(), actor, tenant.ID, ch.ID, nil, userIDs)
	h.auditRestrictedChange(r.Context(), actor, tenant.ID, ch.ID, false, restricted)
	h.renderRefresh(w, r, tenant.ID, "Canal criado.")
}

// editForm returns the edit modal pre-filled with the channel + its
// current roster membership (Recognition over Recall, spec §3 edit).
func (h *Handler) editForm(w http.ResponseWriter, r *http.Request) {
	tenant, ok := h.tenant(w, r)
	if !ok {
		return
	}
	ch, ok := h.getChannel(w, r, tenant.ID)
	if !ok {
		return
	}
	granted, err := h.grantedSet(r.Context(), tenant.ID, ch.ID)
	if err != nil {
		h.fail(w, "load channel access", err)
		return
	}
	roster, err := h.roster(r.Context(), tenant.ID, granted, false)
	if err != nil {
		h.fail(w, "load roster", err)
		return
	}
	h.render(w, modalTmpl, modalData{
		IsNew:      false,
		Action:     BasePath + "/" + ch.ID.String(),
		ID:         ch.ID.String(),
		Name:       ch.DisplayName,
		ChannelKey: ch.ChannelKey,
		Identity:   maskIdentity(ch.ExternalID),
		Types:      channelTypes,
		Roster:     roster,
		Restricted: ch.Restricted,
	})
}

// update renames the channel, toggles its restricted flag, and replaces
// its access roster. Type +
// identity are immutable on edit (changing a live channel's addressing
// would orphan its conversations) so they are ignored here.
func (h *Handler) update(w http.ResponseWriter, r *http.Request) {
	tenant, ok := h.tenant(w, r)
	if !ok {
		return
	}
	ch, ok := h.getChannel(w, r, tenant.ID)
	if !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		h.renderEditError(w, r, tenant.ID, ch, "name", "Não foi possível processar o formulário.")
		return
	}
	name := strings.TrimSpace(r.PostFormValue("name"))
	userIDs := parseUserIDs(r.PostForm["user_ids"])
	restricted := parseRestricted(r)
	if name == "" || len(name) > MaxNameLen {
		h.renderEditError(w, r, tenant.ID, ch, "name", "Informe um nome de exibição (até 120 caracteres).")
		return
	}
	if err := h.deps.Channels.Rename(r.Context(), tenant.ID, ch.ID, name); err != nil {
		if errors.Is(err, channels.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		if errors.Is(err, channels.ErrEmptyDisplayName) {
			h.renderEditError(w, r, tenant.ID, ch, "name", "Informe um nome de exibição.")
			return
		}
		h.fail(w, "rename channel", err)
		return
	}
	// Capture the pre-mutation roster + restricted flag so the audit diff
	// (SIN-66405) reports true grant/revoke deltas and an actual flip.
	// ch was loaded fresh above, so ch.Restricted is the stored value.
	beforeRestricted := ch.Restricted
	beforeGrants, beforeOK := h.beforeGrants(r.Context(), tenant.ID, ch.ID)
	if err := h.deps.Channels.SetRestricted(r.Context(), tenant.ID, ch.ID, restricted); err != nil {
		if errors.Is(err, channels.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		h.fail(w, "set channel restricted", err)
		return
	}
	// SIN-66411 (A09): emit the restricted-flip line immediately after
	// SetRestricted commits, BEFORE the non-atomic ReplaceAccess write. An
	// error on ReplaceAccess must not leave a committed privilege change
	// without a trail. auditRestrictedChange is a no-op when from==to, so a
	// no-op save still records nothing.
	actor := h.actorID(r)
	h.auditRestrictedChange(r.Context(), actor, tenant.ID, ch.ID, beforeRestricted, restricted)
	if err := h.deps.Access.ReplaceAccess(r.Context(), tenant.ID, ch.ID, userIDs); err != nil {
		h.fail(w, "grant channel access", err)
		return
	}
	// Roster diff emits only after ReplaceAccess commits (nothing to diff
	// until then), and is skipped when the before-read failed so a transient
	// read error never mislabels the whole roster as freshly granted.
	if beforeOK {
		h.auditAccessReplace(r.Context(), actor, tenant.ID, ch.ID, beforeGrants, userIDs)
	}
	h.renderRefresh(w, r, tenant.ID, "Canal atualizado.")
}

// toggle flips the channel's active flag and swaps the single row.
func (h *Handler) toggle(w http.ResponseWriter, r *http.Request) {
	tenant, ok := h.tenant(w, r)
	if !ok {
		return
	}
	ch, ok := h.getChannel(w, r, tenant.ID)
	if !ok {
		return
	}
	newActive := !ch.IsActive
	if err := h.deps.Channels.SetActive(r.Context(), tenant.ID, ch.ID, newActive); err != nil {
		if errors.Is(err, channels.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		h.fail(w, "toggle channel", err)
		return
	}
	ch.IsActive = newActive
	row, err := h.rowFor(r.Context(), tenant.ID, ch)
	if err != nil {
		h.fail(w, "render row", err)
		return
	}
	msg := "Canal ativado."
	if !newActive {
		msg = "Canal desativado. As conversas existentes permanecem."
	}
	h.render(w, rowTmpl, rowRefresh{Row: row, Toast: toastData{Message: msg}})
}

// ------------------------------------------------------------------ data

// loadRows builds the registry rows: every channel plus its access
// summary derived from the grant count vs the roster total.
func (h *Handler) loadRows(ctx context.Context, tenantID uuid.UUID) ([]channelRow, error) {
	total, err := h.rosterTotal(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	list, err := h.deps.Channels.List(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	rows := make([]channelRow, 0, len(list))
	for _, ch := range list {
		if ch == nil {
			continue
		}
		grantIDs, err := h.deps.Access.ChannelUserIDs(ctx, tenantID, ch.ID)
		if err != nil {
			return nil, err
		}
		rows = append(rows, rowFromChannel(ch, len(grantIDs), total))
	}
	return rows, nil
}

// rowFor builds a single registry row for the toggle response.
func (h *Handler) rowFor(ctx context.Context, tenantID uuid.UUID, ch *channels.Channel) (channelRow, error) {
	total, err := h.rosterTotal(ctx, tenantID)
	if err != nil {
		return channelRow{}, err
	}
	grantIDs, err := h.deps.Access.ChannelUserIDs(ctx, tenantID, ch.ID)
	if err != nil {
		return channelRow{}, err
	}
	return rowFromChannel(ch, len(grantIDs), total), nil
}

func rowFromChannel(ch *channels.Channel, grantCount, rosterTotal int) channelRow {
	summary, all := accessSummary(grantCount, rosterTotal)
	return channelRow{
		ID:             ch.ID.String(),
		Name:           ch.DisplayName,
		TypeLabel:      typeLabel(ch.ChannelKey),
		MaskedIdentity: maskIdentity(ch.ExternalID),
		Active:         ch.IsActive,
		AccessSummary:  summary,
		AccessAll:      all,
		Restricted:     ch.Restricted,
	}
}

func (h *Handler) rosterTotal(ctx context.Context, tenantID uuid.UUID) (int, error) {
	users, err := h.deps.Access.ListRosterUsers(ctx, tenantID)
	if err != nil {
		return 0, err
	}
	return len(users), nil
}

// roster loads the tenant roster and maps it into the checkbox view. When
// granted is nil and allChecked is true every user is pre-checked (new
// channel); otherwise only ids in granted are checked (edit / error
// re-render).
func (h *Handler) roster(ctx context.Context, tenantID uuid.UUID, granted map[uuid.UUID]struct{}, allChecked bool) (rosterView, error) {
	users, err := h.deps.Access.ListRosterUsers(ctx, tenantID)
	if err != nil {
		return rosterView{}, err
	}
	sortRosterByLabel(users)
	return buildRoster(users, granted, allChecked), nil
}

func (h *Handler) grantedSet(ctx context.Context, tenantID, channelID uuid.UUID) (map[uuid.UUID]struct{}, error) {
	ids, err := h.deps.Access.ChannelUserIDs(ctx, tenantID, channelID)
	if err != nil {
		return nil, err
	}
	set := make(map[uuid.UUID]struct{}, len(ids))
	for _, id := range ids {
		set[id] = struct{}{}
	}
	return set, nil
}

// getChannel resolves the {id} path value to a channel under the tenant
// scope, writing 404 for a bad id or an unknown/RLS-hidden channel.
func (h *Handler) getChannel(w http.ResponseWriter, r *http.Request, tenantID uuid.UUID) (*channels.Channel, bool) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.NotFound(w, r)
		return nil, false
	}
	ch, err := h.deps.Channels.Get(r.Context(), tenantID, id)
	if err != nil {
		if errors.Is(err, channels.ErrNotFound) {
			http.NotFound(w, r)
			return nil, false
		}
		h.fail(w, "get channel", err)
		return nil, false
	}
	return ch, true
}

// ---------------------------------------------------------------- render

func (h *Handler) renderRefresh(w http.ResponseWriter, r *http.Request, tenantID uuid.UUID, msg string) {
	rows, err := h.loadRows(r.Context(), tenantID)
	if err != nil {
		h.fail(w, "reload channels", err)
		return
	}
	h.render(w, refreshTmpl, listRefresh{Rows: rows, Toast: toastData{Message: msg}})
}

// renderCreateModal re-renders the create modal with the submitted values
// and an inline error. The roster re-checks whatever the operator had
// selected so a validation bounce never silently loses their edits. key
// falls back to the first offered type when the submitted one is invalid
// so the <select> always has a valid selection.
func (h *Handler) renderCreateModal(w http.ResponseWriter, r *http.Request, tenantID uuid.UUID, name, key, identity string, userIDs []uuid.UUID, restricted bool, field, msg string) {
	granted := make(map[uuid.UUID]struct{}, len(userIDs))
	for _, id := range userIDs {
		granted[id] = struct{}{}
	}
	roster, err := h.roster(r.Context(), tenantID, granted, false)
	if err != nil {
		h.fail(w, "load roster", err)
		return
	}
	if !validType(key) {
		key = channelTypes[0].Key
	}
	h.render(w, modalTmpl, modalData{
		IsNew:        true,
		Action:       BasePath,
		Name:         name,
		ChannelKey:   key,
		Identity:     identity,
		Types:        channelTypes,
		Roster:       roster,
		Restricted:   restricted,
		FieldError:   field,
		ErrorMessage: msg,
	})
}

func (h *Handler) renderEditError(w http.ResponseWriter, r *http.Request, tenantID uuid.UUID, ch *channels.Channel, field, msg string) {
	userIDs := parseUserIDs(r.PostForm["user_ids"])
	granted := make(map[uuid.UUID]struct{}, len(userIDs))
	for _, id := range userIDs {
		granted[id] = struct{}{}
	}
	roster, err := h.roster(r.Context(), tenantID, granted, false)
	if err != nil {
		h.fail(w, "load roster", err)
		return
	}
	name := strings.TrimSpace(r.PostFormValue("name"))
	h.render(w, modalTmpl, modalData{
		IsNew:        false,
		Action:       BasePath + "/" + ch.ID.String(),
		ID:           ch.ID.String(),
		Name:         name,
		ChannelKey:   ch.ChannelKey,
		Identity:     maskIdentity(ch.ExternalID),
		Types:        channelTypes,
		Roster:       roster,
		Restricted:   parseRestricted(r),
		FieldError:   field,
		ErrorMessage: msg,
	})
}

func (h *Handler) tenant(w http.ResponseWriter, r *http.Request) (*tenancy.Tenant, bool) {
	t, err := tenancy.FromContext(r.Context())
	if err != nil {
		h.deps.Logger.Error("web/channels: tenant required", "err", err)
		http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		return nil, false
	}
	return t, true
}

func (h *Handler) render(w http.ResponseWriter, tmpl *template.Template, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "private, no-store")
	if err := tmpl.Execute(w, data); err != nil {
		h.deps.Logger.Error("web/channels: render", "err", err)
	}
}

func (h *Handler) fail(w http.ResponseWriter, op string, err error) {
	h.deps.Logger.Error("web/channels: "+op, "err", err)
	http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
}

func (h *Handler) newPageData(r *http.Request, tenant *tenancy.Tenant, rows []channelRow) pageData {
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

// parseUserIDs maps the raw form values into a de-duplicated uuid slice,
// dropping anything that is not a valid uuid (input validation at the
// boundary — a forged roster entry that is not a real user is also
// rejected downstream by the channel_access foreign key).
func parseUserIDs(raw []string) []uuid.UUID {
	out := make([]uuid.UUID, 0, len(raw))
	seen := make(map[uuid.UUID]struct{}, len(raw))
	for _, s := range raw {
		id, err := uuid.Parse(strings.TrimSpace(s))
		if err != nil || id == uuid.Nil {
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out
}

// parseRestricted reads the "restricted" checkbox from the submitted
// form. An HTML checkbox only submits its value when checked, so any
// non-empty value means the gerente asked for restricted (membership
// enforced); an absent field means open (every atendente sees the
// channel). r.ParseForm must have been called by the caller.
func parseRestricted(r *http.Request) bool {
	return strings.TrimSpace(r.PostFormValue("restricted")) != ""
}

// buildNavItems / buildUserMenu mirror the wasession / dashboard chrome so
// the surface renders inside the shared SidebarNav app-shell.
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
