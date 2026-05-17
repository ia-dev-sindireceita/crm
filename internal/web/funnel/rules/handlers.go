package rules

import (
	"context"
	"errors"
	"html/template"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/adapter/httpapi/csrf"
	domain "github.com/pericles-luz/crm/internal/funnel/rules"
	"github.com/pericles-luz/crm/internal/tenancy"
)

// MaxNameLen caps the marketer-supplied rule name. The Postgres column
// is text (no length cap); this is defense in depth at the UI boundary.
const MaxNameLen = 200

// MaxConfigLen caps each free-text trigger/action config field. Long
// values are typically broken inputs (HTML pasted in by mistake); we
// reject them early.
const MaxConfigLen = 512

// AdminRepo is the read-write port. The composition root passes a
// concrete implementation; tests pass [domain.InMemoryRepository].
type AdminRepo interface {
	ListAll(ctx context.Context, tenantID uuid.UUID) ([]domain.Rule, error)
	Get(ctx context.Context, tenantID, id uuid.UUID) (domain.Rule, error)
	Create(ctx context.Context, r domain.Rule) error
	Update(ctx context.Context, r domain.Rule) error
	SetEnabled(ctx context.Context, tenantID, id uuid.UUID, enabled bool) error
	Delete(ctx context.Context, tenantID, id uuid.UUID) error
}

// Resolver is the cascade-preview port. The handler invokes Resolve
// for the chosen scope and renders the winning rules so the gerente
// can confirm the rule they just authored beats the cascade as
// expected (AC #2).
type Resolver interface {
	Resolve(ctx context.Context, in domain.ResolveInput) ([]domain.ResolvedRule, error)
}

// CSRFTokenFn returns the request's CSRF token sourced by the auth
// middleware.
type CSRFTokenFn func(*http.Request) string

// UserIDFn returns the authenticated principal user id. uuid.Nil
// collapses to 401 — the action is gated, so we shouldn't be running
// without one, but defense in depth.
type UserIDFn func(*http.Request) uuid.UUID

// IDFn generates a new uuid for each created rule. Injectable so
// tests can pin the value; production wires uuid.New.
type IDFn func() uuid.UUID

// NowFn returns the current time. Injectable so tests can pin it;
// production wires time.Now().UTC.
type NowFn func() time.Time

// Deps bundles the handler collaborators. The repo + resolver are
// required; ID, Now, Logger default to safe values.
type Deps struct {
	Repo      AdminRepo
	Resolver  Resolver
	CSRFToken CSRFTokenFn
	UserID    UserIDFn
	ID        IDFn
	Now       NowFn
	Logger    *slog.Logger
}

// Handler is the HTMX rules editor front controller.
type Handler struct {
	deps Deps
}

// New constructs a Handler. Missing required deps are rejected at wire
// time so cmd/server fails fast.
func New(deps Deps) (*Handler, error) {
	if deps.Repo == nil {
		return nil, errors.New("web/funnel/rules: Repo is required")
	}
	if deps.Resolver == nil {
		return nil, errors.New("web/funnel/rules: Resolver is required")
	}
	if deps.CSRFToken == nil {
		return nil, errors.New("web/funnel/rules: CSRFToken is required")
	}
	if deps.UserID == nil {
		return nil, errors.New("web/funnel/rules: UserID is required")
	}
	if deps.ID == nil {
		deps.ID = uuid.New
	}
	if deps.Now == nil {
		deps.Now = func() time.Time { return time.Now().UTC() }
	}
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	return &Handler{deps: deps}, nil
}

// Routes mounts the editor endpoints on mux. The full list mirrors the
// AC #1/#2 surfaces: CRUD on a rule plus partials for the dynamic
// trigger/action field set and the cascade preview.
func (h *Handler) Routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /funnel/rules", h.list)
	mux.HandleFunc("GET /funnel/rules/new", h.newForm)
	mux.HandleFunc("POST /funnel/rules", h.create)
	mux.HandleFunc("GET /funnel/rules/trigger-fields", h.triggerFields)
	mux.HandleFunc("GET /funnel/rules/action-fields", h.actionFields)
	mux.HandleFunc("GET /funnel/rules/preview", h.preview)
	mux.HandleFunc("GET /funnel/rules/{id}/edit", h.editForm)
	mux.HandleFunc("PATCH /funnel/rules/{id}", h.update)
	mux.HandleFunc("PATCH /funnel/rules/{id}/toggle", h.toggle)
	mux.HandleFunc("DELETE /funnel/rules/{id}", h.delete)
}

// ---------------------------------------------------------------------------
// Handlers
// ---------------------------------------------------------------------------

// list renders the dashboard shell with the rules table.
func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	tenant, err := tenancy.FromContext(r.Context())
	if err != nil {
		h.fail(w, http.StatusInternalServerError, "tenant required", err)
		return
	}
	rows, err := h.deps.Repo.ListAll(r.Context(), tenant.ID)
	if err != nil {
		h.fail(w, http.StatusInternalServerError, "list rules", err)
		return
	}
	token := h.deps.CSRFToken(r)
	if token == "" {
		h.fail(w, http.StatusInternalServerError, "csrf token missing", errors.New("empty csrf token"))
		return
	}
	scopeFilter := strings.TrimSpace(r.URL.Query().Get("scope"))
	rowViews := rowsFrom(rows, scopeFilter)
	view := listView{
		Rows:         rowViews,
		ScopeFilter:  scopeFilter,
		PreviewInput: previewInput{},
		Generated:    h.deps.Now().UTC().Format(time.RFC3339),
		CSRFMeta:     csrf.MetaTag(token),
		HXHeaders:    csrf.HXHeadersAttr(token),
	}
	h.writeHTML(w, http.StatusOK, listLayoutTmpl, view)
}

// newForm renders the create-rule form.
func (h *Handler) newForm(w http.ResponseWriter, r *http.Request) {
	token := h.deps.CSRFToken(r)
	if token == "" {
		h.fail(w, http.StatusInternalServerError, "csrf token missing", errors.New("empty csrf token"))
		return
	}
	h.writeHTML(w, http.StatusOK, formLayoutTmpl, formView{
		Mode:           formModeCreate,
		Input:          defaultFormInput(),
		CSRFMeta:       csrf.MetaTag(token),
		HXHeaders:      csrf.HXHeadersAttr(token),
		TriggerOptions: triggerOptions(),
		ActionOptions:  actionOptions(),
	})
}

// create validates the submitted form, constructs the Rule via the
// domain constructor, persists it, and re-renders the list partial so
// HTMX swaps it inline.
func (h *Handler) create(w http.ResponseWriter, r *http.Request) {
	tenant, err := tenancy.FromContext(r.Context())
	if err != nil {
		h.fail(w, http.StatusInternalServerError, "tenant required", err)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	in, verr := parseForm(r)
	if !verr.IsZero() {
		h.renderFormError(w, r, formModeCreate, "", in, verr)
		return
	}
	actor := h.deps.UserID(r)
	if actor == uuid.Nil {
		h.fail(w, http.StatusUnauthorized, "missing actor", errors.New("nil user id"))
		return
	}
	rule, derr := buildRule(h.deps.ID(), tenant.ID, in, h.deps.Now())
	if derr != nil {
		h.renderFormError(w, r, formModeCreate, "", in, domainErrorMessage(derr))
		return
	}
	if err := h.deps.Repo.Create(r.Context(), rule); err != nil {
		h.fail(w, http.StatusInternalServerError, "create rule", err)
		return
	}
	h.renderListPartial(w, r, tenant, http.StatusCreated)
}

// editForm renders the edit form for an existing rule.
func (h *Handler) editForm(w http.ResponseWriter, r *http.Request) {
	tenant, err := tenancy.FromContext(r.Context())
	if err != nil {
		h.fail(w, http.StatusInternalServerError, "tenant required", err)
		return
	}
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}
	rule, err := h.deps.Repo.Get(r.Context(), tenant.ID, id)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			http.Error(w, http.StatusText(http.StatusNotFound), http.StatusNotFound)
			return
		}
		h.fail(w, http.StatusInternalServerError, "get rule", err)
		return
	}
	token := h.deps.CSRFToken(r)
	if token == "" {
		h.fail(w, http.StatusInternalServerError, "csrf token missing", errors.New("empty csrf token"))
		return
	}
	h.writeHTML(w, http.StatusOK, formLayoutTmpl, formView{
		Mode:           formModeEdit,
		ID:             rule.ID.String(),
		Input:          inputFromRule(rule),
		CSRFMeta:       csrf.MetaTag(token),
		HXHeaders:      csrf.HXHeadersAttr(token),
		TriggerOptions: triggerOptions(),
		ActionOptions:  actionOptions(),
	})
}

// update overwrites a rule's editable fields via the form submission
// payload. PATCH semantics: every field is replaced (the form always
// carries the complete shape).
func (h *Handler) update(w http.ResponseWriter, r *http.Request) {
	tenant, err := tenancy.FromContext(r.Context())
	if err != nil {
		h.fail(w, http.StatusInternalServerError, "tenant required", err)
		return
	}
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}
	existing, err := h.deps.Repo.Get(r.Context(), tenant.ID, id)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			http.Error(w, http.StatusText(http.StatusNotFound), http.StatusNotFound)
			return
		}
		h.fail(w, http.StatusInternalServerError, "get rule", err)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	in, verr := parseForm(r)
	if !verr.IsZero() {
		h.renderFormError(w, r, formModeEdit, id.String(), in, verr)
		return
	}
	updated, derr := buildRule(id, tenant.ID, in, h.deps.Now())
	if derr != nil {
		h.renderFormError(w, r, formModeEdit, id.String(), in, domainErrorMessage(derr))
		return
	}
	// Preserve CreatedAt; UpdatedAt is stamped by the adapter.
	updated.CreatedAt = existing.CreatedAt
	if err := h.deps.Repo.Update(r.Context(), updated); err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			http.Error(w, http.StatusText(http.StatusNotFound), http.StatusNotFound)
			return
		}
		h.fail(w, http.StatusInternalServerError, "update rule", err)
		return
	}
	h.renderListPartial(w, r, tenant, http.StatusOK)
}

// toggle flips a rule's enabled flag and returns the freshly-rendered
// row so HTMX swaps it in place. The button itself targets `closest tr`
// with outerHTML swap.
func (h *Handler) toggle(w http.ResponseWriter, r *http.Request) {
	tenant, err := tenancy.FromContext(r.Context())
	if err != nil {
		h.fail(w, http.StatusInternalServerError, "tenant required", err)
		return
	}
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}
	existing, err := h.deps.Repo.Get(r.Context(), tenant.ID, id)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			http.Error(w, http.StatusText(http.StatusNotFound), http.StatusNotFound)
			return
		}
		h.fail(w, http.StatusInternalServerError, "get rule", err)
		return
	}
	if err := h.deps.Repo.SetEnabled(r.Context(), tenant.ID, id, !existing.Enabled); err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			http.Error(w, http.StatusText(http.StatusNotFound), http.StatusNotFound)
			return
		}
		h.fail(w, http.StatusInternalServerError, "toggle rule", err)
		return
	}
	updated, err := h.deps.Repo.Get(r.Context(), tenant.ID, id)
	if err != nil {
		h.fail(w, http.StatusInternalServerError, "re-read rule", err)
		return
	}
	h.writeHTML(w, http.StatusOK, rowTmpl, rowFromRule(updated, ""))
}

// delete removes the rule and returns an empty body so HTMX swaps the
// row out of the DOM.
func (h *Handler) delete(w http.ResponseWriter, r *http.Request) {
	tenant, err := tenancy.FromContext(r.Context())
	if err != nil {
		h.fail(w, http.StatusInternalServerError, "tenant required", err)
		return
	}
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}
	if err := h.deps.Repo.Delete(r.Context(), tenant.ID, id); err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			http.Error(w, http.StatusText(http.StatusNotFound), http.StatusNotFound)
			return
		}
		h.fail(w, http.StatusInternalServerError, "delete rule", err)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
}

// triggerFields serves the partial input set for the chosen
// trigger_type. The form's <select name="trigger_type"> fires hx-get
// on this endpoint as the user changes the value.
func (h *Handler) triggerFields(w http.ResponseWriter, r *http.Request) {
	t := domain.TriggerType(strings.TrimSpace(r.URL.Query().Get("type")))
	in := inputFromQuery(r)
	h.writeHTML(w, http.StatusOK, triggerFieldsTmpl, triggerFieldsView{
		Type:  string(t),
		Known: t.Known(),
		Input: in,
	})
}

// actionFields serves the partial input set for the chosen action_type.
func (h *Handler) actionFields(w http.ResponseWriter, r *http.Request) {
	a := domain.ActionType(strings.TrimSpace(r.URL.Query().Get("type")))
	in := inputFromQuery(r)
	h.writeHTML(w, http.StatusOK, actionFieldsTmpl, actionFieldsView{
		Type:  string(a),
		Known: a.Known(),
		Input: in,
	})
}

// preview runs the cascade resolver for the requested (channel, team)
// scope and returns the winning rules. The result table tells the
// gerente exactly which rule fires on a hypothetical event matching
// the chosen scope (AC #2).
func (h *Handler) preview(w http.ResponseWriter, r *http.Request) {
	tenant, err := tenancy.FromContext(r.Context())
	if err != nil {
		h.fail(w, http.StatusInternalServerError, "tenant required", err)
		return
	}
	in := previewInput{
		Channel:    strings.TrimSpace(r.URL.Query().Get("channel")),
		TeamIDText: strings.TrimSpace(r.URL.Query().Get("team_id")),
	}
	resolveIn := domain.ResolveInput{TenantID: tenant.ID, Channel: in.Channel}
	var parseErr error
	if in.TeamIDText != "" {
		teamID, err := uuid.Parse(in.TeamIDText)
		if err != nil {
			parseErr = err
		} else {
			resolveIn.TeamID = teamID
			in.TeamID = teamID
		}
	}
	view := previewView{Input: in}
	if parseErr != nil {
		view.Error = "team_id deve ser um uuid válido"
		h.writeHTML(w, http.StatusOK, previewTmpl, view)
		return
	}
	resolved, err := h.deps.Resolver.Resolve(r.Context(), resolveIn)
	if err != nil {
		h.fail(w, http.StatusInternalServerError, "resolve cascade", err)
		return
	}
	for _, rr := range resolved {
		view.Resolved = append(view.Resolved, resolvedRow{
			ID:          rr.Rule.ID.String(),
			Name:        rr.Rule.Name,
			Scope:       string(rr.SourceScope),
			Trigger:     string(rr.Rule.TriggerType),
			Action:      string(rr.Rule.ActionType),
			TriggerInfo: triggerSummary(rr.Rule),
			ActionInfo:  actionSummary(rr.Rule),
		})
	}
	h.writeHTML(w, http.StatusOK, previewTmpl, view)
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// renderListPartial re-fetches the rules and writes the list-rows
// partial. Used by create/update responses.
func (h *Handler) renderListPartial(w http.ResponseWriter, r *http.Request, tenant *tenancy.Tenant, status int) {
	rows, err := h.deps.Repo.ListAll(r.Context(), tenant.ID)
	if err != nil {
		h.fail(w, http.StatusInternalServerError, "list rules", err)
		return
	}
	scopeFilter := strings.TrimSpace(r.URL.Query().Get("scope"))
	h.writeHTML(w, status, listRowsTmpl, listView{
		Rows:        rowsFrom(rows, scopeFilter),
		ScopeFilter: scopeFilter,
	})
}

func (h *Handler) renderFormError(w http.ResponseWriter, r *http.Request, mode formMode, id string, in formInput, ferr formError) {
	token := h.deps.CSRFToken(r)
	h.writeHTML(w, http.StatusUnprocessableEntity, formLayoutTmpl, formView{
		Mode:           mode,
		ID:             id,
		Input:          in,
		Error:          ferr,
		CSRFMeta:       csrf.MetaTag(token),
		HXHeaders:      csrf.HXHeadersAttr(token),
		TriggerOptions: triggerOptions(),
		ActionOptions:  actionOptions(),
	})
}

func (h *Handler) writeHTML(w http.ResponseWriter, status int, tmpl *template.Template, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	if err := tmpl.Execute(w, data); err != nil {
		h.deps.Logger.Error("web/funnel/rules: render", "template", tmpl.Name(), "err", err)
	}
}

func (h *Handler) fail(w http.ResponseWriter, status int, msg string, err error) {
	h.deps.Logger.Error("web/funnel/rules: "+msg, "err", err)
	http.Error(w, http.StatusText(status), status)
}

// ---------------------------------------------------------------------------
// form parsing + view shaping
// ---------------------------------------------------------------------------

// formMode discriminates the create vs edit posture of the form
// template so the action URL and the button label render correctly.
type formMode string

const (
	formModeCreate formMode = "create"
	formModeEdit   formMode = "edit"
)

// formInput is the raw, structurally-validated form payload. The
// domain constructor enforces the deeper invariants.
type formInput struct {
	Name        string
	Scope       string // "channel" | "team" | "tenant"
	Channel     string
	TeamID      string
	TriggerType string
	TriggerCfg  triggerConfigInput
	ActionType  string
	ActionCfg   actionConfigInput
	Enabled     bool
}

type triggerConfigInput struct {
	Phrase     string
	CampaignID string
	Slug       string
	Regex      string
}

type actionConfigInput struct {
	StageKey string
}

// formError carries the field/message pair the template renders
// inline next to the offending input.
type formError struct {
	Field   string
	Message string
}

// IsZero reports whether ferr carries no error (template skips alert).
func (e formError) IsZero() bool { return e.Field == "" && e.Message == "" }

func parseForm(r *http.Request) (formInput, formError) {
	get := func(k string) string { return strings.TrimSpace(r.PostFormValue(k)) }
	in := formInput{
		Name:        get("name"),
		Scope:       get("scope"),
		Channel:     get("channel"),
		TeamID:      get("team_id"),
		TriggerType: get("trigger_type"),
		ActionType:  get("action_type"),
		TriggerCfg: triggerConfigInput{
			Phrase:     get("trigger_phrase"),
			CampaignID: get("trigger_campaign_id"),
			Slug:       get("trigger_slug"),
			Regex:      get("trigger_regex"),
		},
		ActionCfg: actionConfigInput{
			StageKey: get("action_stage_key"),
		},
		Enabled: r.PostFormValue("enabled") == "on",
	}
	switch {
	case in.Name == "":
		return in, formError{Field: "name", Message: "nome é obrigatório"}
	case len(in.Name) > MaxNameLen:
		return in, formError{Field: "name", Message: "nome excede o tamanho máximo"}
	case in.Scope != "channel" && in.Scope != "team" && in.Scope != "tenant":
		return in, formError{Field: "scope", Message: "escopo deve ser canal, equipe ou tenant"}
	}
	if in.Scope == "channel" && in.Channel == "" {
		return in, formError{Field: "channel", Message: "canal é obrigatório quando escopo = canal"}
	}
	if in.Scope == "team" {
		if in.TeamID == "" {
			return in, formError{Field: "team_id", Message: "team_id é obrigatório quando escopo = equipe"}
		}
		if _, err := uuid.Parse(in.TeamID); err != nil {
			return in, formError{Field: "team_id", Message: "team_id deve ser um uuid válido"}
		}
	}
	if in.TriggerType == "" {
		return in, formError{Field: "trigger_type", Message: "tipo de gatilho é obrigatório"}
	}
	if in.ActionType == "" {
		return in, formError{Field: "action_type", Message: "tipo de ação é obrigatório"}
	}
	for _, f := range []struct{ field, val string }{
		{"trigger_phrase", in.TriggerCfg.Phrase},
		{"trigger_campaign_id", in.TriggerCfg.CampaignID},
		{"trigger_slug", in.TriggerCfg.Slug},
		{"trigger_regex", in.TriggerCfg.Regex},
		{"action_stage_key", in.ActionCfg.StageKey},
	} {
		if len(f.val) > MaxConfigLen {
			return in, formError{Field: f.field, Message: "valor excede o tamanho máximo"}
		}
	}
	return in, formError{}
}

func buildRule(id, tenantID uuid.UUID, in formInput, now time.Time) (domain.Rule, error) {
	var channel string
	var teamPtr *uuid.UUID
	switch in.Scope {
	case "channel":
		channel = in.Channel
	case "team":
		t, err := uuid.Parse(in.TeamID)
		if err != nil {
			return domain.Rule{}, domain.ErrInvalidRule
		}
		teamPtr = &t
	}
	triggerCfg := map[string]any{}
	switch domain.TriggerType(in.TriggerType) {
	case domain.TriggerTypeMessageContains:
		triggerCfg["phrase"] = in.TriggerCfg.Phrase
	case domain.TriggerTypeCampaignClick:
		if in.TriggerCfg.CampaignID != "" {
			triggerCfg["campaign_id"] = in.TriggerCfg.CampaignID
		}
		if in.TriggerCfg.Slug != "" {
			triggerCfg["slug"] = in.TriggerCfg.Slug
		}
	case domain.TriggerTypeMessageKeywordRegex:
		triggerCfg["regex"] = in.TriggerCfg.Regex
	}
	actionCfg := map[string]any{}
	if domain.ActionType(in.ActionType) == domain.ActionTypeMoveToStage {
		actionCfg["stage_key"] = in.ActionCfg.StageKey
	}
	return domain.NewRule(
		id, tenantID,
		channel, teamPtr,
		in.Name,
		domain.TriggerType(in.TriggerType), triggerCfg,
		domain.ActionType(in.ActionType), actionCfg,
		in.Enabled,
		now,
	)
}

// inputFromRule rebuilds a formInput from a stored rule for the edit
// form's initial render.
func inputFromRule(r domain.Rule) formInput {
	in := formInput{
		Name:        r.Name,
		TriggerType: string(r.TriggerType),
		ActionType:  string(r.ActionType),
		Enabled:     r.Enabled,
	}
	switch r.Scope() {
	case domain.ScopeChannel:
		in.Scope = "channel"
		in.Channel = r.Channel
	case domain.ScopeTeam:
		in.Scope = "team"
		if r.TeamID != nil {
			in.TeamID = r.TeamID.String()
		}
	default:
		in.Scope = "tenant"
	}
	if v, ok := r.TriggerConfig["phrase"].(string); ok {
		in.TriggerCfg.Phrase = v
	}
	if v, ok := r.TriggerConfig["campaign_id"].(string); ok {
		in.TriggerCfg.CampaignID = v
	}
	if v, ok := r.TriggerConfig["slug"].(string); ok {
		in.TriggerCfg.Slug = v
	}
	if v, ok := r.TriggerConfig["regex"].(string); ok {
		in.TriggerCfg.Regex = v
	}
	if v, ok := r.ActionConfig["stage_key"].(string); ok {
		in.ActionCfg.StageKey = v
	}
	return in
}

// inputFromQuery extracts previously-entered values from the query
// string so the trigger/action-fields HTMX partials can preserve the
// user's typing across re-renders.
func inputFromQuery(r *http.Request) formInput {
	q := r.URL.Query()
	get := func(k string) string { return strings.TrimSpace(q.Get(k)) }
	return formInput{
		TriggerCfg: triggerConfigInput{
			Phrase:     get("trigger_phrase"),
			CampaignID: get("trigger_campaign_id"),
			Slug:       get("trigger_slug"),
			Regex:      get("trigger_regex"),
		},
		ActionCfg: actionConfigInput{
			StageKey: get("action_stage_key"),
		},
	}
}

func defaultFormInput() formInput {
	return formInput{
		Scope:       "tenant",
		TriggerType: string(domain.TriggerTypeMessageContains),
		ActionType:  string(domain.ActionTypeMoveToStage),
		Enabled:     true,
	}
}

// domainErrorMessage translates a [NewRule] error into a user-visible
// form message.
func domainErrorMessage(err error) formError {
	switch {
	case errors.Is(err, domain.ErrInvalidTenant):
		return formError{Field: "", Message: "tenant inválido"}
	case errors.Is(err, domain.ErrUnknownTriggerType):
		return formError{Field: "trigger_type", Message: "tipo de gatilho desconhecido"}
	case errors.Is(err, domain.ErrUnknownActionType):
		return formError{Field: "action_type", Message: "tipo de ação desconhecido"}
	case errors.Is(err, domain.ErrInvalidRule):
		return formError{Field: "", Message: "regra inválida — verifique campos obrigatórios"}
	default:
		return formError{Field: "", Message: "não foi possível salvar a regra"}
	}
}

// ---------------------------------------------------------------------------
// view shaping
// ---------------------------------------------------------------------------

type listView struct {
	Rows         []rowView
	ScopeFilter  string
	PreviewInput previewInput
	Generated    string
	CSRFMeta     template.HTML
	HXHeaders    template.HTMLAttr
}

type rowView struct {
	ID           string
	Name         string
	Scope        string
	ScopeLabel   string
	Channel      string
	TeamID       string
	TriggerType  string
	ActionType   string
	TriggerInfo  string
	ActionInfo   string
	Enabled      bool
	EnabledLabel string
	UpdatedAt    string
}

type formView struct {
	Mode           formMode
	ID             string
	Input          formInput
	Error          formError
	CSRFMeta       template.HTML
	HXHeaders      template.HTMLAttr
	TriggerOptions []option
	ActionOptions  []option
}

type triggerFieldsView struct {
	Type  string
	Known bool
	Input formInput
}

type actionFieldsView struct {
	Type  string
	Known bool
	Input formInput
}

type previewInput struct {
	Channel    string
	TeamIDText string
	TeamID     uuid.UUID
}

type previewView struct {
	Input    previewInput
	Resolved []resolvedRow
	Error    string
}

type resolvedRow struct {
	ID          string
	Name        string
	Scope       string
	Trigger     string
	Action      string
	TriggerInfo string
	ActionInfo  string
}

type option struct {
	Value string
	Label string
}

func triggerOptions() []option {
	return []option{
		{Value: string(domain.TriggerTypeMessageContains), Label: "Mensagem contém frase"},
		{Value: string(domain.TriggerTypeMessageKeywordRegex), Label: "Mensagem casa regex"},
		{Value: string(domain.TriggerTypeCampaignClick), Label: "Clique em campanha"},
	}
}

func actionOptions() []option {
	return []option{
		{Value: string(domain.ActionTypeMoveToStage), Label: "Mover para estágio"},
	}
}

// rowsFrom flattens the rule slice into row-view records, optionally
// filtered by scope. An empty filter passes everything through. The
// rules slice is already in cascade order coming from the repo, so the
// resulting rows render in the same order.
func rowsFrom(list []domain.Rule, scopeFilter string) []rowView {
	out := make([]rowView, 0, len(list))
	for _, r := range list {
		scope := string(r.Scope())
		if scopeFilter != "" && scope != scopeFilter {
			continue
		}
		out = append(out, rowFromRule(r, scopeFilter))
	}
	// Defensive re-sort by scope rank so a caller that hands an
	// unordered slice still gets cascade-ordered rows.
	sort.SliceStable(out, func(i, j int) bool {
		return scopeRank(out[i].Scope) < scopeRank(out[j].Scope)
	})
	return out
}

func rowFromRule(r domain.Rule, _ string) rowView {
	row := rowView{
		ID:          r.ID.String(),
		Name:        r.Name,
		Scope:       string(r.Scope()),
		Channel:     r.Channel,
		TriggerType: string(r.TriggerType),
		ActionType:  string(r.ActionType),
		TriggerInfo: triggerSummary(r),
		ActionInfo:  actionSummary(r),
		Enabled:     r.Enabled,
		UpdatedAt:   r.UpdatedAt.UTC().Format(time.RFC3339),
	}
	if r.TeamID != nil {
		row.TeamID = r.TeamID.String()
	}
	switch r.Scope() {
	case domain.ScopeChannel:
		row.ScopeLabel = "canal · " + r.Channel
	case domain.ScopeTeam:
		row.ScopeLabel = "equipe"
	default:
		row.ScopeLabel = "tenant"
	}
	if r.Enabled {
		row.EnabledLabel = "ativa"
	} else {
		row.EnabledLabel = "desativada"
	}
	return row
}

func triggerSummary(r domain.Rule) string {
	switch r.TriggerType {
	case domain.TriggerTypeMessageContains:
		if v, ok := r.TriggerConfig["phrase"].(string); ok {
			return "frase: " + v
		}
	case domain.TriggerTypeCampaignClick:
		if v, ok := r.TriggerConfig["campaign_id"].(string); ok && v != "" {
			return "campanha: " + v
		}
		if v, ok := r.TriggerConfig["slug"].(string); ok && v != "" {
			return "slug: " + v
		}
	case domain.TriggerTypeMessageKeywordRegex:
		if v, ok := r.TriggerConfig["regex"].(string); ok {
			return "regex: " + v
		}
	}
	return ""
}

func actionSummary(r domain.Rule) string {
	if r.ActionType == domain.ActionTypeMoveToStage {
		if v, ok := r.ActionConfig["stage_key"].(string); ok {
			return "estágio: " + v
		}
	}
	return ""
}

// scopeRank mirrors the domain's internal ranking — duplicated here so
// the rendering layer can sort views without re-importing the package's
// unexported helper.
func scopeRank(scope string) int {
	switch scope {
	case string(domain.ScopeChannel):
		return 0
	case string(domain.ScopeTeam):
		return 1
	case string(domain.ScopeTenant):
		return 2
	}
	return 99
}
