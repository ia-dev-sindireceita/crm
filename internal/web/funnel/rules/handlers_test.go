package rules_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	domain "github.com/pericles-luz/crm/internal/funnel/rules"
	"github.com/pericles-luz/crm/internal/tenancy"
	webrules "github.com/pericles-luz/crm/internal/web/funnel/rules"
)

var fixedNow = time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)

// fullDeps builds Deps wired with the in-memory repo, a fixed clock,
// and a fixed id generator.
func fullDeps(repo *domain.InMemoryRepository, actor, fixedID uuid.UUID) webrules.Deps {
	return webrules.Deps{
		Repo:      repo,
		Resolver:  &resolverFromRepo{repo: repo},
		CSRFToken: func(*http.Request) string { return "tok" },
		UserID:    func(*http.Request) uuid.UUID { return actor },
		ID:        func() uuid.UUID { return fixedID },
		Now:       func() time.Time { return fixedNow },
	}
}

// resolverFromRepo wires the real domain.Resolver to a fresh
// InMemoryRepository so the preview test exercises the actual cascade
// algorithm.
type resolverFromRepo struct {
	repo *domain.InMemoryRepository
}

func (r *resolverFromRepo) Resolve(ctx context.Context, in domain.ResolveInput) ([]domain.ResolvedRule, error) {
	rv, _ := domain.NewResolver(r.repo)
	return rv.Resolve(ctx, in)
}

func reqWithTenant(method, target string, body string, tenant *tenancy.Tenant) *http.Request {
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, target, nil)
	} else {
		r = httptest.NewRequest(method, target, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	return r.WithContext(tenancy.WithContext(r.Context(), tenant))
}

func newTenant() *tenancy.Tenant {
	return &tenancy.Tenant{ID: uuid.New(), Host: "acme.crm.test", Name: "Acme"}
}

func buildHandler(t *testing.T, deps webrules.Deps) *webrules.Handler {
	t.Helper()
	h, err := webrules.New(deps)
	if err != nil {
		t.Fatalf("webrules.New: %v", err)
	}
	return h
}

func mux(h *webrules.Handler) *http.ServeMux {
	m := http.NewServeMux()
	h.Routes(m)
	return m
}

// makeRule helper seeds a rule via NewRule + Create.
func makeRule(t *testing.T, repo *domain.InMemoryRepository, tenantID uuid.UUID, channel string, teamID *uuid.UUID, name string, enabled bool) domain.Rule {
	t.Helper()
	r, err := domain.NewRule(uuid.New(), tenantID, channel, teamID, name,
		domain.TriggerTypeMessageContains, map[string]any{"phrase": name + "-phrase"},
		domain.ActionTypeMoveToStage, map[string]any{"stage_key": "novo"},
		enabled, fixedNow)
	if err != nil {
		t.Fatalf("NewRule(%s): %v", name, err)
	}
	if err := repo.Create(context.Background(), r); err != nil {
		t.Fatalf("Create(%s): %v", name, err)
	}
	return r
}

// ---------------------------------------------------------------------------
// New
// ---------------------------------------------------------------------------

func TestNew_RejectsMissingDeps(t *testing.T) {
	t.Parallel()
	repo := domain.NewInMemoryRepository()
	base := fullDeps(repo, uuid.New(), uuid.New())
	cases := map[string]func(*webrules.Deps){
		"missing Repo":      func(d *webrules.Deps) { d.Repo = nil },
		"missing Resolver":  func(d *webrules.Deps) { d.Resolver = nil },
		"missing CSRFToken": func(d *webrules.Deps) { d.CSRFToken = nil },
		"missing UserID":    func(d *webrules.Deps) { d.UserID = nil },
	}
	for name, mut := range cases {
		name, mut := name, mut
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			d := base
			mut(&d)
			if _, err := webrules.New(d); err == nil {
				t.Fatalf("New(%s): want error, got nil", name)
			}
		})
	}
}

func TestNew_DefaultsApplied(t *testing.T) {
	t.Parallel()
	repo := domain.NewInMemoryRepository()
	deps := webrules.Deps{
		Repo:      repo,
		Resolver:  &resolverFromRepo{repo: repo},
		CSRFToken: func(*http.Request) string { return "tok" },
		UserID:    func(*http.Request) uuid.UUID { return uuid.New() },
		// ID, Now, Logger intentionally nil — New must fill them.
	}
	if _, err := webrules.New(deps); err != nil {
		t.Fatalf("New(defaults): %v", err)
	}
}

// ---------------------------------------------------------------------------
// list
// ---------------------------------------------------------------------------

func TestList_RendersSeededRules(t *testing.T) {
	t.Parallel()
	tenant := newTenant()
	repo := domain.NewInMemoryRepository()
	makeRule(t, repo, tenant.ID, "", nil, "tenant-default", true)
	makeRule(t, repo, tenant.ID, "webchat", nil, "webchat-rule", false)
	h := buildHandler(t, fullDeps(repo, uuid.New(), uuid.New()))

	w := httptest.NewRecorder()
	mux(h).ServeHTTP(w, reqWithTenant("GET", "/funnel/rules", "", tenant))
	if w.Code != http.StatusOK {
		t.Fatalf("status: want 200, got %d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{"Regras de funil", "tenant-default", "webchat-rule",
		"funnel-rules-row--disabled", "Cascade preview"} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q\n--- body ---\n%s", want, body)
		}
	}
}

func TestList_ScopeFilter(t *testing.T) {
	t.Parallel()
	tenant := newTenant()
	team := uuid.New()
	repo := domain.NewInMemoryRepository()
	makeRule(t, repo, tenant.ID, "", nil, "tenant-only", true)
	makeRule(t, repo, tenant.ID, "", &team, "team-only", true)
	makeRule(t, repo, tenant.ID, "webchat", nil, "channel-only", true)
	h := buildHandler(t, fullDeps(repo, uuid.New(), uuid.New()))

	w := httptest.NewRecorder()
	mux(h).ServeHTTP(w, reqWithTenant("GET", "/funnel/rules?scope=channel", "", tenant))
	body := w.Body.String()
	if !strings.Contains(body, "channel-only") {
		t.Errorf("scope=channel should keep channel-only:\n%s", body)
	}
	if strings.Contains(body, "tenant-only") || strings.Contains(body, "team-only") {
		t.Errorf("scope=channel should drop other scopes:\n%s", body)
	}
}

// ---------------------------------------------------------------------------
// create
// ---------------------------------------------------------------------------

func TestCreate_HappyPath_TenantScope(t *testing.T) {
	t.Parallel()
	tenant := newTenant()
	repo := domain.NewInMemoryRepository()
	h := buildHandler(t, fullDeps(repo, uuid.New(), uuid.New()))

	form := url.Values{}
	form.Set("name", "My rule")
	form.Set("scope", "tenant")
	form.Set("trigger_type", "message_contains")
	form.Set("trigger_phrase", "preço")
	form.Set("action_type", "move_to_stage")
	form.Set("action_stage_key", "qualificando")
	form.Set("enabled", "on")

	w := httptest.NewRecorder()
	mux(h).ServeHTTP(w, reqWithTenant("POST", "/funnel/rules", form.Encode(), tenant))
	if w.Code != http.StatusCreated {
		t.Fatalf("status: want 201, got %d (body=%s)", w.Code, w.Body.String())
	}

	rows, _ := repo.ListAll(context.Background(), tenant.ID)
	if len(rows) != 1 || rows[0].Name != "My rule" || rows[0].Scope() != domain.ScopeTenant {
		t.Fatalf("repo did not persist rule: %+v", rows)
	}
	// Response is the rows partial.
	if !strings.Contains(w.Body.String(), "My rule") {
		t.Errorf("response should include new row:\n%s", w.Body.String())
	}
}

func TestCreate_HappyPath_ChannelScope(t *testing.T) {
	t.Parallel()
	tenant := newTenant()
	repo := domain.NewInMemoryRepository()
	h := buildHandler(t, fullDeps(repo, uuid.New(), uuid.New()))

	form := url.Values{}
	form.Set("name", "Webchat rule")
	form.Set("scope", "channel")
	form.Set("channel", "webchat")
	form.Set("trigger_type", "message_keyword_regex")
	form.Set("trigger_regex", "NF-\\d+")
	form.Set("action_type", "move_to_stage")
	form.Set("action_stage_key", "qualificando")
	form.Set("enabled", "on")

	w := httptest.NewRecorder()
	mux(h).ServeHTTP(w, reqWithTenant("POST", "/funnel/rules", form.Encode(), tenant))
	if w.Code != http.StatusCreated {
		t.Fatalf("status: want 201, got %d (body=%s)", w.Code, w.Body.String())
	}
	rows, _ := repo.ListAll(context.Background(), tenant.ID)
	if len(rows) != 1 || rows[0].Channel != "webchat" || rows[0].Scope() != domain.ScopeChannel {
		t.Fatalf("rule not channel-scoped as expected: %+v", rows)
	}
}

func TestCreate_HappyPath_TeamScopeWithCampaignClick(t *testing.T) {
	t.Parallel()
	tenant := newTenant()
	team := uuid.New()
	repo := domain.NewInMemoryRepository()
	h := buildHandler(t, fullDeps(repo, uuid.New(), uuid.New()))

	form := url.Values{}
	form.Set("name", "Team click rule")
	form.Set("scope", "team")
	form.Set("team_id", team.String())
	form.Set("trigger_type", "campaign_click")
	form.Set("trigger_slug", "black-friday")
	form.Set("action_type", "move_to_stage")
	form.Set("action_stage_key", "ganho")
	form.Set("enabled", "on")

	w := httptest.NewRecorder()
	mux(h).ServeHTTP(w, reqWithTenant("POST", "/funnel/rules", form.Encode(), tenant))
	if w.Code != http.StatusCreated {
		t.Fatalf("status: want 201, got %d (body=%s)", w.Code, w.Body.String())
	}
	rows, _ := repo.ListAll(context.Background(), tenant.ID)
	if len(rows) != 1 || rows[0].TeamID == nil || *rows[0].TeamID != team {
		t.Fatalf("rule not team-scoped: %+v", rows[0])
	}
	if rows[0].TriggerConfig["slug"] != "black-friday" {
		t.Fatalf("trigger config not persisted: %+v", rows[0].TriggerConfig)
	}
}

func TestCreate_FormErrors(t *testing.T) {
	t.Parallel()
	tenant := newTenant()
	cases := []struct {
		name string
		set  func(url.Values)
		want string
	}{
		{"missing name", func(f url.Values) { f.Del("name") }, "nome é obrigatório"},
		{"missing scope", func(f url.Values) { f.Set("scope", "") }, "escopo deve ser canal"},
		{"channel scope without channel", func(f url.Values) {
			f.Set("scope", "channel")
			f.Del("channel")
		}, "canal é obrigatório"},
		{"team scope without uuid", func(f url.Values) {
			f.Set("scope", "team")
			f.Set("team_id", "not-a-uuid")
		}, "team_id deve ser um uuid válido"},
		{"missing trigger phrase", func(f url.Values) {
			f.Del("trigger_phrase")
		}, "regra inválida"},
		{"missing stage_key", func(f url.Values) { f.Del("action_stage_key") }, "regra inválida"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			repo := domain.NewInMemoryRepository()
			h := buildHandler(t, fullDeps(repo, uuid.New(), uuid.New()))
			form := goodCreateForm()
			tc.set(form)
			w := httptest.NewRecorder()
			mux(h).ServeHTTP(w, reqWithTenant("POST", "/funnel/rules", form.Encode(), tenant))
			if w.Code != http.StatusUnprocessableEntity {
				t.Fatalf("status: want 422, got %d", w.Code)
			}
			if !strings.Contains(w.Body.String(), tc.want) {
				t.Errorf("body should contain %q:\n%s", tc.want, w.Body.String())
			}
		})
	}
}

func goodCreateForm() url.Values {
	form := url.Values{}
	form.Set("name", "rule")
	form.Set("scope", "tenant")
	form.Set("trigger_type", "message_contains")
	form.Set("trigger_phrase", "x")
	form.Set("action_type", "move_to_stage")
	form.Set("action_stage_key", "novo")
	form.Set("enabled", "on")
	return form
}

// ---------------------------------------------------------------------------
// edit / update / toggle / delete
// ---------------------------------------------------------------------------

func TestEditForm_RendersExistingRule(t *testing.T) {
	t.Parallel()
	tenant := newTenant()
	repo := domain.NewInMemoryRepository()
	rule := makeRule(t, repo, tenant.ID, "webchat", nil, "edit-me", true)
	h := buildHandler(t, fullDeps(repo, uuid.New(), uuid.New()))

	w := httptest.NewRecorder()
	mux(h).ServeHTTP(w, reqWithTenant("GET", "/funnel/rules/"+rule.ID.String()+"/edit", "", tenant))
	if w.Code != http.StatusOK {
		t.Fatalf("status: want 200, got %d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{"Editar regra", "edit-me", "webchat"} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q\n%s", want, body)
		}
	}
}

func TestEditForm_NotFound(t *testing.T) {
	t.Parallel()
	tenant := newTenant()
	repo := domain.NewInMemoryRepository()
	h := buildHandler(t, fullDeps(repo, uuid.New(), uuid.New()))
	w := httptest.NewRecorder()
	mux(h).ServeHTTP(w, reqWithTenant("GET", "/funnel/rules/"+uuid.New().String()+"/edit", "", tenant))
	if w.Code != http.StatusNotFound {
		t.Fatalf("status: want 404, got %d", w.Code)
	}
}

func TestUpdate_HappyPath(t *testing.T) {
	t.Parallel()
	tenant := newTenant()
	repo := domain.NewInMemoryRepository()
	rule := makeRule(t, repo, tenant.ID, "", nil, "rename-me", true)
	h := buildHandler(t, fullDeps(repo, uuid.New(), uuid.New()))

	form := goodCreateForm()
	form.Set("name", "renamed")
	w := httptest.NewRecorder()
	mux(h).ServeHTTP(w, reqWithTenant("PATCH", "/funnel/rules/"+rule.ID.String(), form.Encode(), tenant))
	if w.Code != http.StatusOK {
		t.Fatalf("status: want 200, got %d (body=%s)", w.Code, w.Body.String())
	}
	got, _ := repo.Get(context.Background(), tenant.ID, rule.ID)
	if got.Name != "renamed" {
		t.Fatalf("Name: want renamed, got %q", got.Name)
	}
}

func TestUpdate_NotFound(t *testing.T) {
	t.Parallel()
	tenant := newTenant()
	repo := domain.NewInMemoryRepository()
	h := buildHandler(t, fullDeps(repo, uuid.New(), uuid.New()))
	w := httptest.NewRecorder()
	mux(h).ServeHTTP(w, reqWithTenant("PATCH", "/funnel/rules/"+uuid.New().String(), goodCreateForm().Encode(), tenant))
	if w.Code != http.StatusNotFound {
		t.Fatalf("status: want 404, got %d", w.Code)
	}
}

func TestToggle_FlipsEnabled(t *testing.T) {
	t.Parallel()
	tenant := newTenant()
	repo := domain.NewInMemoryRepository()
	rule := makeRule(t, repo, tenant.ID, "", nil, "toggle-me", true)
	h := buildHandler(t, fullDeps(repo, uuid.New(), uuid.New()))

	w := httptest.NewRecorder()
	mux(h).ServeHTTP(w, reqWithTenant("PATCH", "/funnel/rules/"+rule.ID.String()+"/toggle", "", tenant))
	if w.Code != http.StatusOK {
		t.Fatalf("status: want 200, got %d", w.Code)
	}
	got, _ := repo.Get(context.Background(), tenant.ID, rule.ID)
	if got.Enabled {
		t.Fatal("toggle did not flip from true → false")
	}
	if !strings.Contains(w.Body.String(), "ativar") {
		t.Errorf("response should show 'ativar' button after disabling:\n%s", w.Body.String())
	}
}

func TestToggle_NotFound(t *testing.T) {
	t.Parallel()
	tenant := newTenant()
	repo := domain.NewInMemoryRepository()
	h := buildHandler(t, fullDeps(repo, uuid.New(), uuid.New()))
	w := httptest.NewRecorder()
	mux(h).ServeHTTP(w, reqWithTenant("PATCH", "/funnel/rules/"+uuid.New().String()+"/toggle", "", tenant))
	if w.Code != http.StatusNotFound {
		t.Fatalf("status: want 404, got %d", w.Code)
	}
}

func TestDelete_RemovesRow(t *testing.T) {
	t.Parallel()
	tenant := newTenant()
	repo := domain.NewInMemoryRepository()
	rule := makeRule(t, repo, tenant.ID, "", nil, "delete-me", true)
	h := buildHandler(t, fullDeps(repo, uuid.New(), uuid.New()))

	w := httptest.NewRecorder()
	mux(h).ServeHTTP(w, reqWithTenant("DELETE", "/funnel/rules/"+rule.ID.String(), "", tenant))
	if w.Code != http.StatusOK {
		t.Fatalf("status: want 200, got %d", w.Code)
	}
	if _, err := repo.Get(context.Background(), tenant.ID, rule.ID); err == nil {
		t.Fatal("repo still has the rule after DELETE")
	}
}

func TestDelete_NotFound(t *testing.T) {
	t.Parallel()
	tenant := newTenant()
	repo := domain.NewInMemoryRepository()
	h := buildHandler(t, fullDeps(repo, uuid.New(), uuid.New()))
	w := httptest.NewRecorder()
	mux(h).ServeHTTP(w, reqWithTenant("DELETE", "/funnel/rules/"+uuid.New().String(), "", tenant))
	if w.Code != http.StatusNotFound {
		t.Fatalf("status: want 404, got %d", w.Code)
	}
}

// ---------------------------------------------------------------------------
// trigger-fields / action-fields partials
// ---------------------------------------------------------------------------

func TestTriggerFields_PerTypeInputSet(t *testing.T) {
	t.Parallel()
	tenant := newTenant()
	repo := domain.NewInMemoryRepository()
	h := buildHandler(t, fullDeps(repo, uuid.New(), uuid.New()))

	cases := []struct {
		typ   string
		want  string
		other string // input that must NOT appear
	}{
		{"message_contains", `name="trigger_phrase"`, `name="trigger_regex"`},
		{"campaign_click", `name="trigger_campaign_id"`, `name="trigger_phrase"`},
		{"message_keyword_regex", `name="trigger_regex"`, `name="trigger_phrase"`},
		{"unknown", "Selecione um tipo de gatilho", `name="trigger_phrase"`},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.typ, func(t *testing.T) {
			t.Parallel()
			w := httptest.NewRecorder()
			mux(h).ServeHTTP(w, reqWithTenant("GET", "/funnel/rules/trigger-fields?type="+tc.typ, "", tenant))
			body := w.Body.String()
			if !strings.Contains(body, tc.want) {
				t.Errorf("body for type=%s should contain %q\n%s", tc.typ, tc.want, body)
			}
			if tc.other != "" && strings.Contains(body, tc.other) {
				t.Errorf("body for type=%s should NOT contain %q\n%s", tc.typ, tc.other, body)
			}
		})
	}
}

func TestActionFields_PerTypeInputSet(t *testing.T) {
	t.Parallel()
	tenant := newTenant()
	repo := domain.NewInMemoryRepository()
	h := buildHandler(t, fullDeps(repo, uuid.New(), uuid.New()))
	w := httptest.NewRecorder()
	mux(h).ServeHTTP(w, reqWithTenant("GET", "/funnel/rules/action-fields?type=move_to_stage", "", tenant))
	if !strings.Contains(w.Body.String(), `name="action_stage_key"`) {
		t.Errorf("move_to_stage should render stage_key input:\n%s", w.Body.String())
	}

	w = httptest.NewRecorder()
	mux(h).ServeHTTP(w, reqWithTenant("GET", "/funnel/rules/action-fields?type=unknown", "", tenant))
	if !strings.Contains(w.Body.String(), "Selecione um tipo de ação") {
		t.Errorf("unknown action should render hint:\n%s", w.Body.String())
	}
}

// ---------------------------------------------------------------------------
// cascade preview
// ---------------------------------------------------------------------------

func TestPreview_ResolvesChannelRuleAheadOfTenantDefault(t *testing.T) {
	t.Parallel()
	tenant := newTenant()
	repo := domain.NewInMemoryRepository()
	// Both rules MUST carry the same trigger phrase so the cascade
	// dedup actually kicks in (the resolver dedups by
	// TriggerSignature; distinct phrases produce distinct signatures
	// and would both survive).
	seedSamePhrase := func(channel, name string) {
		r, err := domain.NewRule(uuid.New(), tenant.ID, channel, nil, name,
			domain.TriggerTypeMessageContains, map[string]any{"phrase": "shared-phrase"},
			domain.ActionTypeMoveToStage, map[string]any{"stage_key": "novo"},
			true, fixedNow)
		if err != nil {
			t.Fatalf("NewRule(%s): %v", name, err)
		}
		_ = repo.Create(context.Background(), r)
	}
	seedSamePhrase("", "tenant-rule")
	seedSamePhrase("webchat", "channel-rule")
	h := buildHandler(t, fullDeps(repo, uuid.New(), uuid.New()))

	w := httptest.NewRecorder()
	mux(h).ServeHTTP(w, reqWithTenant("GET", "/funnel/rules/preview?channel=webchat", "", tenant))
	if w.Code != http.StatusOK {
		t.Fatalf("status: want 200, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "channel-rule") {
		t.Errorf("preview should include channel-rule:\n%s", body)
	}
	// Both rules carry the same phrase signature → only the channel rule
	// survives the cascade dedup.
	if strings.Contains(body, "tenant-rule") {
		t.Errorf("preview should NOT include tenant-rule (deduped by channel):\n%s", body)
	}
}

func TestPreview_EmptyResult(t *testing.T) {
	t.Parallel()
	tenant := newTenant()
	repo := domain.NewInMemoryRepository()
	h := buildHandler(t, fullDeps(repo, uuid.New(), uuid.New()))
	w := httptest.NewRecorder()
	mux(h).ServeHTTP(w, reqWithTenant("GET", "/funnel/rules/preview?channel=webchat", "", tenant))
	if !strings.Contains(w.Body.String(), "Nenhuma regra ativa") {
		t.Errorf("empty result should render hint:\n%s", w.Body.String())
	}
}

func TestPreview_InvalidTeamID(t *testing.T) {
	t.Parallel()
	tenant := newTenant()
	repo := domain.NewInMemoryRepository()
	h := buildHandler(t, fullDeps(repo, uuid.New(), uuid.New()))
	w := httptest.NewRecorder()
	mux(h).ServeHTTP(w, reqWithTenant("GET", "/funnel/rules/preview?team_id=not-a-uuid", "", tenant))
	if !strings.Contains(w.Body.String(), "team_id deve ser um uuid válido") {
		t.Errorf("invalid uuid should surface error:\n%s", w.Body.String())
	}
}

// ---------------------------------------------------------------------------
// Tenant context required
// ---------------------------------------------------------------------------

func TestList_RequiresTenantInContext(t *testing.T) {
	t.Parallel()
	repo := domain.NewInMemoryRepository()
	h := buildHandler(t, fullDeps(repo, uuid.New(), uuid.New()))
	w := httptest.NewRecorder()
	mux(h).ServeHTTP(w, httptest.NewRequest("GET", "/funnel/rules", nil))
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("missing tenant: want 500, got %d", w.Code)
	}
}

// ---------------------------------------------------------------------------
// CSRF token must be present
// ---------------------------------------------------------------------------

func TestList_RequiresCSRFToken(t *testing.T) {
	t.Parallel()
	tenant := newTenant()
	repo := domain.NewInMemoryRepository()
	deps := fullDeps(repo, uuid.New(), uuid.New())
	deps.CSRFToken = func(*http.Request) string { return "" }
	h := buildHandler(t, deps)
	w := httptest.NewRecorder()
	mux(h).ServeHTTP(w, reqWithTenant("GET", "/funnel/rules", "", tenant))
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("empty csrf token: want 500, got %d", w.Code)
	}
}

// ---------------------------------------------------------------------------
// extra coverage: error branches, alt trigger types, invalid uuids
// ---------------------------------------------------------------------------

func TestNewForm_RendersFreshForm(t *testing.T) {
	t.Parallel()
	tenant := newTenant()
	repo := domain.NewInMemoryRepository()
	h := buildHandler(t, fullDeps(repo, uuid.New(), uuid.New()))
	w := httptest.NewRecorder()
	mux(h).ServeHTTP(w, reqWithTenant("GET", "/funnel/rules/new", "", tenant))
	if w.Code != http.StatusOK {
		t.Fatalf("status: want 200, got %d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{"Nova regra", `name="name"`, `value="tenant"`} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q\n%s", want, body)
		}
	}
}

func TestNewForm_RequiresCSRFToken(t *testing.T) {
	t.Parallel()
	tenant := newTenant()
	repo := domain.NewInMemoryRepository()
	deps := fullDeps(repo, uuid.New(), uuid.New())
	deps.CSRFToken = func(*http.Request) string { return "" }
	h := buildHandler(t, deps)
	w := httptest.NewRecorder()
	mux(h).ServeHTTP(w, reqWithTenant("GET", "/funnel/rules/new", "", tenant))
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("want 500 on missing csrf, got %d", w.Code)
	}
}

func TestEditForm_RendersCampaignClickAndRegexBranches(t *testing.T) {
	t.Parallel()
	tenant := newTenant()
	team := uuid.New()
	repo := domain.NewInMemoryRepository()
	// One campaign_click rule + one regex rule, each with their full
	// per-type config so inputFromRule's per-key branches all execute.
	rCC, _ := domain.NewRule(uuid.New(), tenant.ID, "", &team, "click",
		domain.TriggerTypeCampaignClick, map[string]any{"campaign_id": "camp-x", "slug": "blackfriday"},
		domain.ActionTypeMoveToStage, map[string]any{"stage_key": "ganho"},
		true, fixedNow)
	_ = repo.Create(context.Background(), rCC)
	rRegex, _ := domain.NewRule(uuid.New(), tenant.ID, "webchat", nil, "regex",
		domain.TriggerTypeMessageKeywordRegex, map[string]any{"regex": "NF-\\d+"},
		domain.ActionTypeMoveToStage, map[string]any{"stage_key": "qualificando"},
		false, fixedNow)
	_ = repo.Create(context.Background(), rRegex)
	h := buildHandler(t, fullDeps(repo, uuid.New(), uuid.New()))

	for _, id := range []uuid.UUID{rCC.ID, rRegex.ID} {
		w := httptest.NewRecorder()
		mux(h).ServeHTTP(w, reqWithTenant("GET", "/funnel/rules/"+id.String()+"/edit", "", tenant))
		if w.Code != http.StatusOK {
			t.Fatalf("status: want 200 for %s, got %d", id, w.Code)
		}
	}
}

func TestEditForm_InvalidUUID(t *testing.T) {
	t.Parallel()
	tenant := newTenant()
	repo := domain.NewInMemoryRepository()
	h := buildHandler(t, fullDeps(repo, uuid.New(), uuid.New()))
	w := httptest.NewRecorder()
	mux(h).ServeHTTP(w, reqWithTenant("GET", "/funnel/rules/not-a-uuid/edit", "", tenant))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", w.Code)
	}
}

func TestEditForm_RequiresCSRF(t *testing.T) {
	t.Parallel()
	tenant := newTenant()
	repo := domain.NewInMemoryRepository()
	r := makeRule(t, repo, tenant.ID, "", nil, "x", true)
	deps := fullDeps(repo, uuid.New(), uuid.New())
	deps.CSRFToken = func(*http.Request) string { return "" }
	h := buildHandler(t, deps)
	w := httptest.NewRecorder()
	mux(h).ServeHTTP(w, reqWithTenant("GET", "/funnel/rules/"+r.ID.String()+"/edit", "", tenant))
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("want 500, got %d", w.Code)
	}
}

func TestUpdate_InvalidUUID(t *testing.T) {
	t.Parallel()
	tenant := newTenant()
	repo := domain.NewInMemoryRepository()
	h := buildHandler(t, fullDeps(repo, uuid.New(), uuid.New()))
	w := httptest.NewRecorder()
	mux(h).ServeHTTP(w, reqWithTenant("PATCH", "/funnel/rules/not-a-uuid", goodCreateForm().Encode(), tenant))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", w.Code)
	}
}

func TestUpdate_FormError(t *testing.T) {
	t.Parallel()
	tenant := newTenant()
	repo := domain.NewInMemoryRepository()
	rule := makeRule(t, repo, tenant.ID, "", nil, "x", true)
	h := buildHandler(t, fullDeps(repo, uuid.New(), uuid.New()))
	form := goodCreateForm()
	form.Set("name", "") // trigger form-level validation error
	w := httptest.NewRecorder()
	mux(h).ServeHTTP(w, reqWithTenant("PATCH", "/funnel/rules/"+rule.ID.String(), form.Encode(), tenant))
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("want 422, got %d", w.Code)
	}
}

func TestToggle_InvalidUUID(t *testing.T) {
	t.Parallel()
	tenant := newTenant()
	repo := domain.NewInMemoryRepository()
	h := buildHandler(t, fullDeps(repo, uuid.New(), uuid.New()))
	w := httptest.NewRecorder()
	mux(h).ServeHTTP(w, reqWithTenant("PATCH", "/funnel/rules/not-a-uuid/toggle", "", tenant))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", w.Code)
	}
}

func TestDelete_InvalidUUID(t *testing.T) {
	t.Parallel()
	tenant := newTenant()
	repo := domain.NewInMemoryRepository()
	h := buildHandler(t, fullDeps(repo, uuid.New(), uuid.New()))
	w := httptest.NewRecorder()
	mux(h).ServeHTTP(w, reqWithTenant("DELETE", "/funnel/rules/not-a-uuid", "", tenant))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", w.Code)
	}
}

func TestCreate_UnknownTriggerType_HitsDomainError(t *testing.T) {
	t.Parallel()
	tenant := newTenant()
	repo := domain.NewInMemoryRepository()
	h := buildHandler(t, fullDeps(repo, uuid.New(), uuid.New()))
	form := goodCreateForm()
	form.Set("trigger_type", "future_kind") // parseForm accepts any non-empty
	w := httptest.NewRecorder()
	mux(h).ServeHTTP(w, reqWithTenant("POST", "/funnel/rules", form.Encode(), tenant))
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("want 422, got %d (body=%s)", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "tipo de gatilho desconhecido") {
		t.Errorf("body should surface domain error:\n%s", w.Body.String())
	}
}

func TestCreate_UnknownActionType_HitsDomainError(t *testing.T) {
	t.Parallel()
	tenant := newTenant()
	repo := domain.NewInMemoryRepository()
	h := buildHandler(t, fullDeps(repo, uuid.New(), uuid.New()))
	form := goodCreateForm()
	form.Set("action_type", "send_email")
	w := httptest.NewRecorder()
	mux(h).ServeHTTP(w, reqWithTenant("POST", "/funnel/rules", form.Encode(), tenant))
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("want 422, got %d (body=%s)", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "tipo de ação desconhecido") {
		t.Errorf("body should surface action-domain error:\n%s", w.Body.String())
	}
}

func TestCreate_MissingActor_Returns401(t *testing.T) {
	t.Parallel()
	tenant := newTenant()
	repo := domain.NewInMemoryRepository()
	deps := fullDeps(repo, uuid.Nil, uuid.New())
	h := buildHandler(t, deps)
	w := httptest.NewRecorder()
	mux(h).ServeHTTP(w, reqWithTenant("POST", "/funnel/rules", goodCreateForm().Encode(), tenant))
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("want 401 for nil actor, got %d", w.Code)
	}
}

// stubResolverWithError lets us drive the preview's 500 path.
type stubErrResolver struct{}

func (stubErrResolver) Resolve(_ context.Context, _ domain.ResolveInput) ([]domain.ResolvedRule, error) {
	return nil, errResolver
}

var errResolver = stubResolverErr("boom")

type stubResolverErr string

func (e stubResolverErr) Error() string { return string(e) }

func TestPreview_ResolverErrorReturns500(t *testing.T) {
	t.Parallel()
	tenant := newTenant()
	repo := domain.NewInMemoryRepository()
	deps := fullDeps(repo, uuid.New(), uuid.New())
	deps.Resolver = stubErrResolver{}
	h := buildHandler(t, deps)
	w := httptest.NewRecorder()
	mux(h).ServeHTTP(w, reqWithTenant("GET", "/funnel/rules/preview?channel=webchat", "", tenant))
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("want 500, got %d", w.Code)
	}
}

// faultyRepo wraps InMemoryRepository and lets a test inject errors on
// individual methods, so the handler's 500-on-repo-error branches can
// be driven without standing up a faulty DB.
type faultyRepo struct {
	inner       *domain.InMemoryRepository
	listAllErr  error
	getErr      error
	createErr   error
	updateErr   error
	enabledErr  error
	deleteErr   error
	getNthError int // when > 0, only the Nth call to Get errors
	getCalls    int
}

func (f *faultyRepo) ListAll(ctx context.Context, tenantID uuid.UUID) ([]domain.Rule, error) {
	if f.listAllErr != nil {
		return nil, f.listAllErr
	}
	return f.inner.ListAll(ctx, tenantID)
}
func (f *faultyRepo) Get(ctx context.Context, tenantID, id uuid.UUID) (domain.Rule, error) {
	f.getCalls++
	if f.getErr != nil && (f.getNthError == 0 || f.getNthError == f.getCalls) {
		return domain.Rule{}, f.getErr
	}
	return f.inner.Get(ctx, tenantID, id)
}
func (f *faultyRepo) Create(ctx context.Context, r domain.Rule) error {
	if f.createErr != nil {
		return f.createErr
	}
	return f.inner.Create(ctx, r)
}
func (f *faultyRepo) Update(ctx context.Context, r domain.Rule) error {
	if f.updateErr != nil {
		return f.updateErr
	}
	return f.inner.Update(ctx, r)
}
func (f *faultyRepo) SetEnabled(ctx context.Context, tenantID, id uuid.UUID, enabled bool) error {
	if f.enabledErr != nil {
		return f.enabledErr
	}
	return f.inner.SetEnabled(ctx, tenantID, id, enabled)
}
func (f *faultyRepo) Delete(ctx context.Context, tenantID, id uuid.UUID) error {
	if f.deleteErr != nil {
		return f.deleteErr
	}
	return f.inner.Delete(ctx, tenantID, id)
}

func boomErr() error { return stubResolverErr("boom") }

func TestList_RepoError(t *testing.T) {
	t.Parallel()
	tenant := newTenant()
	inner := domain.NewInMemoryRepository()
	repo := &faultyRepo{inner: inner, listAllErr: boomErr()}
	deps := fullDeps(inner, uuid.New(), uuid.New())
	deps.Repo = repo
	h := buildHandler(t, deps)
	w := httptest.NewRecorder()
	mux(h).ServeHTTP(w, reqWithTenant("GET", "/funnel/rules", "", tenant))
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("want 500, got %d", w.Code)
	}
}

func TestCreate_RepoError(t *testing.T) {
	t.Parallel()
	tenant := newTenant()
	inner := domain.NewInMemoryRepository()
	repo := &faultyRepo{inner: inner, createErr: boomErr()}
	deps := fullDeps(inner, uuid.New(), uuid.New())
	deps.Repo = repo
	h := buildHandler(t, deps)
	w := httptest.NewRecorder()
	mux(h).ServeHTTP(w, reqWithTenant("POST", "/funnel/rules", goodCreateForm().Encode(), tenant))
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("want 500, got %d", w.Code)
	}
}

func TestEditForm_GetError(t *testing.T) {
	t.Parallel()
	tenant := newTenant()
	inner := domain.NewInMemoryRepository()
	r := makeRule(t, inner, tenant.ID, "", nil, "x", true)
	repo := &faultyRepo{inner: inner, getErr: boomErr()}
	deps := fullDeps(inner, uuid.New(), uuid.New())
	deps.Repo = repo
	h := buildHandler(t, deps)
	w := httptest.NewRecorder()
	mux(h).ServeHTTP(w, reqWithTenant("GET", "/funnel/rules/"+r.ID.String()+"/edit", "", tenant))
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("want 500, got %d", w.Code)
	}
}

func TestUpdate_GetError(t *testing.T) {
	t.Parallel()
	tenant := newTenant()
	inner := domain.NewInMemoryRepository()
	r := makeRule(t, inner, tenant.ID, "", nil, "x", true)
	repo := &faultyRepo{inner: inner, getErr: boomErr()}
	deps := fullDeps(inner, uuid.New(), uuid.New())
	deps.Repo = repo
	h := buildHandler(t, deps)
	w := httptest.NewRecorder()
	mux(h).ServeHTTP(w, reqWithTenant("PATCH", "/funnel/rules/"+r.ID.String(), goodCreateForm().Encode(), tenant))
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("want 500, got %d", w.Code)
	}
}

func TestUpdate_UpdateError(t *testing.T) {
	t.Parallel()
	tenant := newTenant()
	inner := domain.NewInMemoryRepository()
	r := makeRule(t, inner, tenant.ID, "", nil, "x", true)
	repo := &faultyRepo{inner: inner, updateErr: boomErr()}
	deps := fullDeps(inner, uuid.New(), uuid.New())
	deps.Repo = repo
	h := buildHandler(t, deps)
	w := httptest.NewRecorder()
	mux(h).ServeHTTP(w, reqWithTenant("PATCH", "/funnel/rules/"+r.ID.String(), goodCreateForm().Encode(), tenant))
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("want 500, got %d", w.Code)
	}
}

func TestUpdate_UpdateNotFound(t *testing.T) {
	t.Parallel()
	tenant := newTenant()
	inner := domain.NewInMemoryRepository()
	r := makeRule(t, inner, tenant.ID, "", nil, "x", true)
	// Get succeeds, Update returns ErrNotFound (race: someone deleted
	// between read and write).
	repo := &faultyRepo{inner: inner, updateErr: domain.ErrNotFound}
	deps := fullDeps(inner, uuid.New(), uuid.New())
	deps.Repo = repo
	h := buildHandler(t, deps)
	w := httptest.NewRecorder()
	mux(h).ServeHTTP(w, reqWithTenant("PATCH", "/funnel/rules/"+r.ID.String(), goodCreateForm().Encode(), tenant))
	if w.Code != http.StatusNotFound {
		t.Fatalf("want 404, got %d", w.Code)
	}
}

func TestUpdate_BuildRuleError(t *testing.T) {
	t.Parallel()
	tenant := newTenant()
	inner := domain.NewInMemoryRepository()
	r := makeRule(t, inner, tenant.ID, "", nil, "x", true)
	h := buildHandler(t, fullDeps(inner, uuid.New(), uuid.New()))
	form := goodCreateForm()
	// Pass form-valid trigger_type but a config-invalid (no phrase)
	// payload by overriding trigger_type to one that requires a key
	// parseForm doesn't enforce.
	form.Set("trigger_type", "future_kind")
	w := httptest.NewRecorder()
	mux(h).ServeHTTP(w, reqWithTenant("PATCH", "/funnel/rules/"+r.ID.String(), form.Encode(), tenant))
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("want 422, got %d", w.Code)
	}
}

func TestToggle_GetError(t *testing.T) {
	t.Parallel()
	tenant := newTenant()
	inner := domain.NewInMemoryRepository()
	r := makeRule(t, inner, tenant.ID, "", nil, "x", true)
	repo := &faultyRepo{inner: inner, getErr: boomErr()}
	deps := fullDeps(inner, uuid.New(), uuid.New())
	deps.Repo = repo
	h := buildHandler(t, deps)
	w := httptest.NewRecorder()
	mux(h).ServeHTTP(w, reqWithTenant("PATCH", "/funnel/rules/"+r.ID.String()+"/toggle", "", tenant))
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("want 500, got %d", w.Code)
	}
}

func TestToggle_SetEnabledError(t *testing.T) {
	t.Parallel()
	tenant := newTenant()
	inner := domain.NewInMemoryRepository()
	r := makeRule(t, inner, tenant.ID, "", nil, "x", true)
	repo := &faultyRepo{inner: inner, enabledErr: boomErr()}
	deps := fullDeps(inner, uuid.New(), uuid.New())
	deps.Repo = repo
	h := buildHandler(t, deps)
	w := httptest.NewRecorder()
	mux(h).ServeHTTP(w, reqWithTenant("PATCH", "/funnel/rules/"+r.ID.String()+"/toggle", "", tenant))
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("want 500, got %d", w.Code)
	}
}

func TestToggle_SetEnabledNotFound(t *testing.T) {
	t.Parallel()
	tenant := newTenant()
	inner := domain.NewInMemoryRepository()
	r := makeRule(t, inner, tenant.ID, "", nil, "x", true)
	repo := &faultyRepo{inner: inner, enabledErr: domain.ErrNotFound}
	deps := fullDeps(inner, uuid.New(), uuid.New())
	deps.Repo = repo
	h := buildHandler(t, deps)
	w := httptest.NewRecorder()
	mux(h).ServeHTTP(w, reqWithTenant("PATCH", "/funnel/rules/"+r.ID.String()+"/toggle", "", tenant))
	if w.Code != http.StatusNotFound {
		t.Fatalf("want 404, got %d", w.Code)
	}
}

func TestToggle_ReReadGetError(t *testing.T) {
	t.Parallel()
	tenant := newTenant()
	inner := domain.NewInMemoryRepository()
	r := makeRule(t, inner, tenant.ID, "", nil, "x", true)
	// The handler calls Get TWICE — once before SetEnabled, once after.
	// Inject an error only on the SECOND call so the re-read 500 path
	// fires.
	repo := &faultyRepo{inner: inner, getErr: boomErr(), getNthError: 2}
	deps := fullDeps(inner, uuid.New(), uuid.New())
	deps.Repo = repo
	h := buildHandler(t, deps)
	w := httptest.NewRecorder()
	mux(h).ServeHTTP(w, reqWithTenant("PATCH", "/funnel/rules/"+r.ID.String()+"/toggle", "", tenant))
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("want 500, got %d", w.Code)
	}
}

func TestDelete_DeleteError(t *testing.T) {
	t.Parallel()
	tenant := newTenant()
	inner := domain.NewInMemoryRepository()
	r := makeRule(t, inner, tenant.ID, "", nil, "x", true)
	repo := &faultyRepo{inner: inner, deleteErr: boomErr()}
	deps := fullDeps(inner, uuid.New(), uuid.New())
	deps.Repo = repo
	h := buildHandler(t, deps)
	w := httptest.NewRecorder()
	mux(h).ServeHTTP(w, reqWithTenant("DELETE", "/funnel/rules/"+r.ID.String(), "", tenant))
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("want 500, got %d", w.Code)
	}
}

func TestCreate_RenderListPartial_ListError(t *testing.T) {
	t.Parallel()
	tenant := newTenant()
	inner := domain.NewInMemoryRepository()
	// Create succeeds, ListAll fails on the post-create re-render.
	repo := &faultyRepo{inner: inner, listAllErr: boomErr()}
	deps := fullDeps(inner, uuid.New(), uuid.New())
	deps.Repo = repo
	h := buildHandler(t, deps)
	w := httptest.NewRecorder()
	mux(h).ServeHTTP(w, reqWithTenant("POST", "/funnel/rules", goodCreateForm().Encode(), tenant))
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("want 500 on re-render list error, got %d", w.Code)
	}
}

func TestParseForm_LengthLimits(t *testing.T) {
	t.Parallel()
	tenant := newTenant()
	repo := domain.NewInMemoryRepository()
	h := buildHandler(t, fullDeps(repo, uuid.New(), uuid.New()))

	cases := []struct {
		field string
		val   string
	}{
		{"name", strings.Repeat("a", 201)},
		{"trigger_phrase", strings.Repeat("a", 513)},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.field, func(t *testing.T) {
			t.Parallel()
			form := goodCreateForm()
			form.Set(tc.field, tc.val)
			w := httptest.NewRecorder()
			mux(h).ServeHTTP(w, reqWithTenant("POST", "/funnel/rules", form.Encode(), tenant))
			if w.Code != http.StatusUnprocessableEntity {
				t.Fatalf("field=%s: want 422, got %d", tc.field, w.Code)
			}
		})
	}
}

func TestPreview_WithValidTeamID(t *testing.T) {
	t.Parallel()
	tenant := newTenant()
	team := uuid.New()
	repo := domain.NewInMemoryRepository()
	makeRule(t, repo, tenant.ID, "", &team, "team-rule", true)
	h := buildHandler(t, fullDeps(repo, uuid.New(), uuid.New()))
	w := httptest.NewRecorder()
	mux(h).ServeHTTP(w, reqWithTenant("GET", "/funnel/rules/preview?team_id="+team.String(), "", tenant))
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "team-rule") {
		t.Errorf("preview should include team rule:\n%s", w.Body.String())
	}
}
