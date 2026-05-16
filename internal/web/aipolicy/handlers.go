package aipolicy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/aipolicy"
	"github.com/pericles-luz/crm/internal/tenancy"
)

// AllowedModels is the closed allowlist the admin form accepts.
// Other column values (legacy "openrouter/auto", future models)
// remain accepted by the database but cannot be selected via this
// UI — the form rejects them server-side. Per SIN-62906 description.
var AllowedModels = []string{"gemini-flash", "claude-haiku"}

// AllowedTones / AllowedLanguages are the enums the form constrains
// to. The DB has no CHECK on these columns (migration 0098 stores
// text) so the gate lives here.
var (
	AllowedTones     = []string{"neutro", "formal", "casual"}
	AllowedLanguages = []string{"pt-BR", "en-US", "es-ES"}
)

// MaxScopeIDLen caps free-form scope_id input so a runaway client
// cannot stuff arbitrarily large strings into the ai_policy.scope_id
// column. The column itself is text without a length cap; this gate
// is defense in depth at the UI boundary. Matches typical channel /
// team identifier shapes (uuid + small prefix).
const MaxScopeIDLen = 128

// Resolver is the read-side port the cascade preview consumes. The
// concrete *aipolicy.Resolver satisfies it; tests substitute an
// in-memory stub.
type Resolver interface {
	Resolve(ctx context.Context, in aipolicy.ResolveInput) (aipolicy.Policy, aipolicy.ResolveSource, error)
}

// Deps bundles the handler collaborators. All ports are required;
// Now and Logger default to time.Now (UTC) and slog.Default.
type Deps struct {
	Repo     aipolicy.Repository
	Resolver Resolver
	Now      func() time.Time
	Logger   *slog.Logger
}

// Handler serves the HTMX admin pages.
type Handler struct {
	deps Deps
}

// New constructs a Handler. Nil required dependencies are rejected
// at boot so the wire fails fast.
func New(deps Deps) (*Handler, error) {
	if deps.Repo == nil {
		return nil, errors.New("web/aipolicy: Repo is required")
	}
	if deps.Resolver == nil {
		return nil, errors.New("web/aipolicy: Resolver is required")
	}
	if deps.Now == nil {
		deps.Now = func() time.Time { return time.Now().UTC() }
	}
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	return &Handler{deps: deps}, nil
}

// Routes mounts every endpoint on mux. Go 1.22 method+pattern syntax
// gives r.PathValue resolution for the scope-keyed routes.
func (h *Handler) Routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /settings/ai-policy", h.list)
	mux.HandleFunc("GET /settings/ai-policy/new", h.newForm)
	mux.HandleFunc("GET /settings/ai-policy/preview", h.preview)
	mux.HandleFunc("GET /settings/ai-policy/{scope_type}/{scope_id}/edit", h.editForm)
	mux.HandleFunc("POST /settings/ai-policy", h.create)
	mux.HandleFunc("PATCH /settings/ai-policy/{scope_type}/{scope_id}", h.update)
	mux.HandleFunc("DELETE /settings/ai-policy/{scope_type}/{scope_id}", h.delete)
}

// list renders the full page: header + new-policy CTA + table of
// existing policies + cascade preview widget.
func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	tenant, err := tenancy.FromContext(r.Context())
	if err != nil {
		h.fail(w, http.StatusInternalServerError, "tenant required", err)
		return
	}
	policies, err := h.deps.Repo.List(r.Context(), tenant.ID)
	if err != nil {
		h.fail(w, http.StatusInternalServerError, "list policies", err)
		return
	}
	preview, source := h.resolveForPreview(r.Context(), tenant.ID, "", "")
	data := pageData{
		TenantName:  tenant.Name,
		GeneratedAt: h.deps.Now().Format(time.RFC3339),
		Rows:        rowsFromPolicies(policies),
		Preview:     previewData{Policy: preview, Source: string(source)},
		FormDefaults: formDefaults{
			AllowedModels:    AllowedModels,
			AllowedTones:     AllowedTones,
			AllowedLanguages: AllowedLanguages,
			Anonymize:        true,
		},
	}
	h.writeHTML(w, http.StatusOK, pageTmpl, data)
}

// newForm renders the empty form. Query params seed the scope.
func (h *Handler) newForm(w http.ResponseWriter, r *http.Request) {
	scope := strings.TrimSpace(r.URL.Query().Get("scope"))
	scopeID := strings.TrimSpace(r.URL.Query().Get("scope_id"))
	form := formData{
		Action:           "/settings/ai-policy",
		Method:           "post",
		ScopeType:        scope,
		ScopeID:          scopeID,
		Model:            "",
		Tone:             "neutro",
		Language:         "pt-BR",
		AIEnabled:        false,
		Anonymize:        true,
		OptIn:            false,
		IsNew:            true,
		AllowedModels:    AllowedModels,
		AllowedTones:     AllowedTones,
		AllowedLanguages: AllowedLanguages,
	}
	h.writeHTML(w, http.StatusOK, formTmpl, form)
}

// editForm renders the form pre-populated with an existing row.
func (h *Handler) editForm(w http.ResponseWriter, r *http.Request) {
	tenant, err := tenancy.FromContext(r.Context())
	if err != nil {
		h.fail(w, http.StatusInternalServerError, "tenant required", err)
		return
	}
	scopeType, scopeID, ok := parseScopeURL(r)
	if !ok {
		http.Error(w, "invalid scope", http.StatusBadRequest)
		return
	}
	got, found, err := h.deps.Repo.Get(r.Context(), tenant.ID, scopeType, scopeID)
	if err != nil {
		h.fail(w, http.StatusInternalServerError, "get policy", err)
		return
	}
	if !found {
		http.Error(w, "policy not found", http.StatusNotFound)
		return
	}
	form := formData{
		Action:           "/settings/ai-policy/" + string(scopeType) + "/" + scopeID,
		Method:           "patch",
		ScopeType:        string(scopeType),
		ScopeID:          scopeID,
		Model:            got.Model,
		Tone:             got.Tone,
		Language:         got.Language,
		AIEnabled:        got.AIEnabled,
		Anonymize:        got.Anonymize,
		OptIn:            got.OptIn,
		IsNew:            false,
		AllowedModels:    AllowedModels,
		AllowedTones:     AllowedTones,
		AllowedLanguages: AllowedLanguages,
	}
	h.writeHTML(w, http.StatusOK, formTmpl, form)
}

// create handles POST /settings/ai-policy.
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
	policy, verr := parsePolicyForm(r, tenant.ID)
	if verr != nil {
		var ferr *FormError
		if errors.As(verr, &ferr) {
			h.writeFormError(w, http.StatusUnprocessableEntity, ferr)
			return
		}
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	if err := h.deps.Repo.Upsert(r.Context(), policy); err != nil {
		h.fail(w, http.StatusInternalServerError, "upsert policy", err)
		return
	}
	h.renderListPartial(w, r, tenant.ID, http.StatusCreated)
}

// update handles PATCH /settings/ai-policy/{scope_type}/{scope_id}.
// The URL pins the scope; the form's scope_type/scope_id fields are
// ignored even if present (defense in depth — a request that tries to
// rename a scope is silently corrected to the URL identity).
func (h *Handler) update(w http.ResponseWriter, r *http.Request) {
	tenant, err := tenancy.FromContext(r.Context())
	if err != nil {
		h.fail(w, http.StatusInternalServerError, "tenant required", err)
		return
	}
	scopeType, scopeID, ok := parseScopeURL(r)
	if !ok {
		http.Error(w, "invalid scope", http.StatusBadRequest)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	// Pin the scope to the URL — the form fields are accepted but
	// the URL wins so a renamed scope_type / scope_id cannot escape.
	r.Form.Set("scope_type", string(scopeType))
	r.Form.Set("scope_id", scopeID)
	policy, verr := parsePolicyForm(r, tenant.ID)
	if verr != nil {
		var ferr *FormError
		if errors.As(verr, &ferr) {
			h.writeFormError(w, http.StatusUnprocessableEntity, ferr)
			return
		}
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	if err := h.deps.Repo.Upsert(r.Context(), policy); err != nil {
		h.fail(w, http.StatusInternalServerError, "upsert policy", err)
		return
	}
	h.renderListPartial(w, r, tenant.ID, http.StatusOK)
}

// delete handles DELETE /settings/ai-policy/{scope_type}/{scope_id}.
func (h *Handler) delete(w http.ResponseWriter, r *http.Request) {
	tenant, err := tenancy.FromContext(r.Context())
	if err != nil {
		h.fail(w, http.StatusInternalServerError, "tenant required", err)
		return
	}
	scopeType, scopeID, ok := parseScopeURL(r)
	if !ok {
		http.Error(w, "invalid scope", http.StatusBadRequest)
		return
	}
	if _, err := h.deps.Repo.Delete(r.Context(), tenant.ID, scopeType, scopeID); err != nil {
		h.fail(w, http.StatusInternalServerError, "delete policy", err)
		return
	}
	h.renderListPartial(w, r, tenant.ID, http.StatusOK)
}

// preview handles GET /settings/ai-policy/preview. The resolver
// runs against the current tenant; query params optionally pin a
// channel or team.
func (h *Handler) preview(w http.ResponseWriter, r *http.Request) {
	tenant, err := tenancy.FromContext(r.Context())
	if err != nil {
		h.fail(w, http.StatusInternalServerError, "tenant required", err)
		return
	}
	channelID := strings.TrimSpace(r.URL.Query().Get("channel_id"))
	teamID := strings.TrimSpace(r.URL.Query().Get("team_id"))
	policy, source := h.resolveForPreview(r.Context(), tenant.ID, channelID, teamID)
	h.writeHTML(w, http.StatusOK, previewTmpl, previewData{
		Policy:    policy,
		Source:    string(source),
		ChannelID: channelID,
		TeamID:    teamID,
	})
}

// resolveForPreview wraps Resolver with the channel/team string-to-
// pointer dance and swallows resolver errors into a SourceDefault
// fallback so the preview widget never 500s.
func (h *Handler) resolveForPreview(ctx context.Context, tenantID uuid.UUID, channelID, teamID string) (aipolicy.Policy, aipolicy.ResolveSource) {
	in := aipolicy.ResolveInput{TenantID: tenantID}
	if channelID != "" {
		in.ChannelID = &channelID
	}
	if teamID != "" {
		in.TeamID = &teamID
	}
	pol, src, err := h.deps.Resolver.Resolve(ctx, in)
	if err != nil {
		h.deps.Logger.Warn("web/aipolicy: preview resolve failed", "tenant_id", tenantID, "err", err)
		return aipolicy.DefaultPolicy(), aipolicy.SourceDefault
	}
	return pol, src
}

// renderListPartial re-fetches the tenant's policies and renders the
// full list shell so HTMX swap targets stay consistent across
// create/update/delete. The new preview reflects the post-mutation
// resolver state.
func (h *Handler) renderListPartial(w http.ResponseWriter, r *http.Request, tenantID uuid.UUID, status int) {
	policies, err := h.deps.Repo.List(r.Context(), tenantID)
	if err != nil {
		h.fail(w, http.StatusInternalServerError, "list after mutation", err)
		return
	}
	preview, source := h.resolveForPreview(r.Context(), tenantID, "", "")
	data := listPartialData{
		Rows:    rowsFromPolicies(policies),
		Preview: previewData{Policy: preview, Source: string(source)},
		Now:     h.deps.Now().Format(time.RFC3339),
	}
	h.writeHTML(w, status, listPartialTmpl, data)
}

// parseScopeURL pulls and validates the {scope_type}/{scope_id}
// path values. Invalid scope_type or blank scope_id collapse to ok=false.
func parseScopeURL(r *http.Request) (aipolicy.ScopeType, string, bool) {
	st := aipolicy.ScopeType(strings.TrimSpace(r.PathValue("scope_type")))
	if !st.IsValid() {
		return "", "", false
	}
	sid := strings.TrimSpace(r.PathValue("scope_id"))
	if sid == "" || len(sid) > MaxScopeIDLen {
		return "", "", false
	}
	return st, sid, true
}

// parsePolicyForm validates and shapes the form body into a Policy
// ready for Upsert. The returned error is a *FormError carrying the
// field-level message the form re-render uses.
func parsePolicyForm(r *http.Request, tenantID uuid.UUID) (aipolicy.Policy, error) {
	scopeRaw := strings.TrimSpace(r.Form.Get("scope_type"))
	scope := aipolicy.ScopeType(scopeRaw)
	if !scope.IsValid() {
		return aipolicy.Policy{}, formError("scope_type", "escolha um escopo válido (tenant, team ou channel)")
	}
	scopeID := strings.TrimSpace(r.Form.Get("scope_id"))
	if scopeID == "" {
		return aipolicy.Policy{}, formError("scope_id", "informe o identificador do escopo")
	}
	if len(scopeID) > MaxScopeIDLen {
		return aipolicy.Policy{}, formError("scope_id", fmt.Sprintf("máximo %d caracteres", MaxScopeIDLen))
	}
	model := strings.TrimSpace(r.Form.Get("model"))
	if !contains(AllowedModels, model) {
		return aipolicy.Policy{}, formError("model", "modelo fora da allowlist (gemini-flash, claude-haiku)")
	}
	tone := strings.TrimSpace(r.Form.Get("tone"))
	if !contains(AllowedTones, tone) {
		return aipolicy.Policy{}, formError("tone", "tom inválido (neutro, formal, casual)")
	}
	language := strings.TrimSpace(r.Form.Get("language"))
	if !contains(AllowedLanguages, language) {
		return aipolicy.Policy{}, formError("language", "idioma inválido (pt-BR, en-US, es-ES)")
	}
	promptVersion := strings.TrimSpace(r.Form.Get("prompt_version"))
	if promptVersion == "" {
		promptVersion = aipolicy.DefaultPolicy().PromptVersion
	}
	return aipolicy.Policy{
		TenantID:      tenantID,
		ScopeType:     scope,
		ScopeID:       scopeID,
		Model:         model,
		PromptVersion: promptVersion,
		Tone:          tone,
		Language:      language,
		AIEnabled:     formBool(r, "ai_enabled"),
		Anonymize:     formBoolDefault(r, "anonymize", true),
		OptIn:         formBool(r, "opt_in"),
	}, nil
}

// FormError is the typed validation error the handler returns to the
// re-render path. Field names match the form input `name` attribute
// so the template can highlight the offending control.
type FormError struct {
	Field   string
	Message string
}

func (e *FormError) Error() string { return e.Field + ": " + e.Message }

func formError(field, message string) *FormError { return &FormError{Field: field, Message: message} }

// formBool returns true when the form value is one of the truthy
// shapes a browser checkbox can produce ("on", "true", "1"). An
// absent or empty value is false.
func formBool(r *http.Request, key string) bool {
	switch strings.ToLower(strings.TrimSpace(r.Form.Get(key))) {
	case "on", "true", "1", "yes":
		return true
	default:
		return false
	}
}

// formBoolDefault is the same but the absent-key case falls back to
// def. Used for anonymize so a UI that does not render the toggle at
// all defaults to ADR-0041's anonymize=true posture.
func formBoolDefault(r *http.Request, key string, def bool) bool {
	if _, ok := r.Form[key]; !ok {
		return def
	}
	return formBool(r, key)
}

// contains is a tiny generic-string helper to keep the allowlist
// checks readable.
func contains(haystack []string, needle string) bool {
	for _, v := range haystack {
		if v == needle {
			return true
		}
	}
	return false
}

// rowsFromPolicies maps the domain slice into the view-model. The
// table renders deterministically (List already orders); we sort
// again defensively in case a future change relaxes the SQL ORDER BY.
func rowsFromPolicies(in []aipolicy.Policy) []rowData {
	out := make([]rowData, 0, len(in))
	for _, p := range in {
		out = append(out, rowData{
			ScopeType: string(p.ScopeType),
			ScopeID:   p.ScopeID,
			Model:     p.Model,
			Tone:      p.Tone,
			Language:  p.Language,
			AIEnabled: p.AIEnabled,
			Anonymize: p.Anonymize,
			OptIn:     p.OptIn,
			UpdatedAt: p.UpdatedAt.UTC().Format(time.RFC3339),
		})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].ScopeType != out[j].ScopeType {
			return out[i].ScopeType < out[j].ScopeType
		}
		return out[i].ScopeID < out[j].ScopeID
	})
	return out
}

// writeHTML is the single render path so the security headers stay
// consistent across every route.
func (h *Handler) writeHTML(w http.ResponseWriter, status int, t htmlTemplate, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "private, no-store")
	w.WriteHeader(status)
	if err := t.Execute(w, data); err != nil {
		h.deps.Logger.Error("web/aipolicy: render", "err", err)
	}
}

// writeFormError re-renders the form with the field-level message
// inlined. HTMX clients consume the partial and replace the form
// shell; the status is 422 so non-HTMX clients still see the error.
func (h *Handler) writeFormError(w http.ResponseWriter, status int, ferr *FormError) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "private, no-store")
	w.WriteHeader(status)
	if err := errorPartialTmpl.Execute(w, ferr); err != nil {
		h.deps.Logger.Error("web/aipolicy: render error", "err", err)
	}
}

// htmlTemplate is the minimal interface every render template
// satisfies — *template.Template implements Execute(w, data).
type htmlTemplate interface {
	Execute(w io.Writer, data any) error
}

func (h *Handler) fail(w http.ResponseWriter, status int, msg string, err error) {
	h.deps.Logger.Error("web/aipolicy: "+msg, "err", err)
	http.Error(w, http.StatusText(status), status)
}
