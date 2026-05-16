package aipolicy_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/aipolicy"
	"github.com/pericles-luz/crm/internal/tenancy"
	webaipolicy "github.com/pericles-luz/crm/internal/web/aipolicy"
)

// memRepo is the in-memory Repository the handler tests stack
// against. It is the same shape the production pgx adapter exposes
// — every method tracks calls so tests can assert mutations did or
// did not happen.
type memRepo struct {
	mu   sync.Mutex
	rows map[memKey]aipolicy.Policy
	// failures keyed by method name return the configured error so a
	// test can drive the handler's failure paths.
	getErr    error
	listErr   error
	upsertErr error
	deleteErr error
}

type memKey struct {
	tenant    uuid.UUID
	scopeType aipolicy.ScopeType
	scopeID   string
}

func newMemRepo() *memRepo { return &memRepo{rows: map[memKey]aipolicy.Policy{}} }

func (r *memRepo) Get(_ context.Context, tenantID uuid.UUID, st aipolicy.ScopeType, sid string) (aipolicy.Policy, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.getErr != nil {
		return aipolicy.Policy{}, false, r.getErr
	}
	p, ok := r.rows[memKey{tenant: tenantID, scopeType: st, scopeID: sid}]
	return p, ok, nil
}

func (r *memRepo) Upsert(_ context.Context, p aipolicy.Policy) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.upsertErr != nil {
		return r.upsertErr
	}
	p.UpdatedAt = time.Date(2026, 5, 16, 12, 0, 0, 0, time.UTC)
	r.rows[memKey{tenant: p.TenantID, scopeType: p.ScopeType, scopeID: p.ScopeID}] = p
	return nil
}

func (r *memRepo) List(_ context.Context, tenantID uuid.UUID) ([]aipolicy.Policy, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.listErr != nil {
		return nil, r.listErr
	}
	out := []aipolicy.Policy{}
	for k, v := range r.rows {
		if k.tenant == tenantID {
			out = append(out, v)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].ScopeType != out[j].ScopeType {
			return out[i].ScopeType < out[j].ScopeType
		}
		return out[i].ScopeID < out[j].ScopeID
	})
	return out, nil
}

func (r *memRepo) Delete(_ context.Context, tenantID uuid.UUID, st aipolicy.ScopeType, sid string) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.deleteErr != nil {
		return false, r.deleteErr
	}
	k := memKey{tenant: tenantID, scopeType: st, scopeID: sid}
	_, ok := r.rows[k]
	delete(r.rows, k)
	return ok, nil
}

// resolverFromRepo builds a real aipolicy.Resolver against the
// memRepo so the cascade behavior under test mirrors production.
func resolverFromRepo(t *testing.T, repo aipolicy.Repository) *aipolicy.Resolver {
	t.Helper()
	r, err := aipolicy.NewResolver(repo)
	if err != nil {
		t.Fatalf("NewResolver: %v", err)
	}
	return r
}

// failingResolver always returns an error. Used to confirm the
// handler's preview path swallows the error into a SourceDefault
// fallback instead of 5xx-ing.
type failingResolver struct{ err error }

func (f failingResolver) Resolve(_ context.Context, _ aipolicy.ResolveInput) (aipolicy.Policy, aipolicy.ResolveSource, error) {
	return aipolicy.Policy{}, "", f.err
}

func newHandler(t *testing.T, repo aipolicy.Repository, res webaipolicy.Resolver) http.Handler {
	t.Helper()
	h, err := webaipolicy.New(webaipolicy.Deps{
		Repo:     repo,
		Resolver: res,
		Now:      func() time.Time { return time.Date(2026, 5, 16, 12, 0, 0, 0, time.UTC) },
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("webaipolicy.New: %v", err)
	}
	mux := http.NewServeMux()
	h.Routes(mux)
	return mux
}

func tenantCtx(t *testing.T) (context.Context, *tenancy.Tenant) {
	t.Helper()
	tenant := &tenancy.Tenant{
		ID:   uuid.MustParse("11111111-1111-1111-1111-111111111111"),
		Name: "Acme CRM",
		Host: "acme.crm.local",
	}
	return tenancy.WithContext(context.Background(), tenant), tenant
}

func newRequest(t *testing.T, method, target string, body io.Reader) *http.Request {
	t.Helper()
	ctx, _ := tenantCtx(t)
	req, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	if method == http.MethodPost || method == http.MethodPatch {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	return req
}

// TestNew_RejectsMissingDeps documents the construction-time gates.
func TestNew_RejectsMissingDeps(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		deps webaipolicy.Deps
	}{
		{"missing Repo", webaipolicy.Deps{Resolver: failingResolver{}}},
		{"missing Resolver", webaipolicy.Deps{Repo: newMemRepo()}},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if _, err := webaipolicy.New(c.deps); err == nil {
				t.Fatalf("New(%s) err = nil, want failure", c.name)
			}
		})
	}
}

// TestList_RendersEmptyState confirms the page renders the
// "nenhuma policy configurada" copy when the tenant has no rows.
func TestList_RendersEmptyState(t *testing.T) {
	t.Parallel()
	repo := newMemRepo()
	mux := newHandler(t, repo, resolverFromRepo(t, repo))

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, newRequest(t, http.MethodGet, "/settings/ai-policy", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "Nenhuma policy configurada") {
		t.Errorf("body missing empty-state copy:\n%s", body)
	}
	if !strings.Contains(body, "Acme CRM") {
		t.Errorf("body missing tenant name:\n%s", body)
	}
	if !strings.Contains(body, "Padrão do sistema") {
		t.Errorf("body missing default-source preview label:\n%s", body)
	}
}

// TestCreatePolicy_TenantScope_PreviewShowsIt covers AC #1 from the
// issue: admin creates a policy at tenant scope → preview shows it.
func TestCreatePolicy_TenantScope_PreviewShowsIt(t *testing.T) {
	t.Parallel()
	_, tenant := tenantCtx(t)
	repo := newMemRepo()
	mux := newHandler(t, repo, resolverFromRepo(t, repo))

	form := url.Values{}
	form.Set("scope_type", "tenant")
	form.Set("scope_id", tenant.ID.String())
	form.Set("model", "claude-haiku")
	form.Set("tone", "formal")
	form.Set("language", "pt-BR")
	form.Set("ai_enabled", "on")
	form.Set("anonymize", "on")
	form.Set("opt_in", "on")

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, newRequest(t, http.MethodPost, "/settings/ai-policy", strings.NewReader(form.Encode())))
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}

	// The post-mutation partial includes the row AND the preview card.
	body := rec.Body.String()
	if !strings.Contains(body, "claude-haiku") {
		t.Errorf("partial body missing model row:\n%s", body)
	}
	if !strings.Contains(body, "Tenant (configuração padrão)") {
		t.Errorf("partial body missing tenant-source preview:\n%s", body)
	}

	// The repository persisted the row.
	got, ok, err := repo.Get(context.Background(), tenant.ID, aipolicy.ScopeTenant, tenant.ID.String())
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !ok {
		t.Fatal("policy not persisted")
	}
	if got.Model != "claude-haiku" {
		t.Errorf("Model = %q, want claude-haiku", got.Model)
	}
	if !got.AIEnabled {
		t.Errorf("AIEnabled = false, want true")
	}
}

// TestCascadeOverride_ChannelBeatsTenant covers AC #2: a channel
// override resolves to the channel row, not the tenant row.
func TestCascadeOverride_ChannelBeatsTenant(t *testing.T) {
	t.Parallel()
	_, tenant := tenantCtx(t)
	repo := newMemRepo()
	mux := newHandler(t, repo, resolverFromRepo(t, repo))

	// Seed tenant + channel rows directly through the handler so the
	// full HTTP path is exercised twice.
	for _, c := range []struct {
		scope, scopeID, model string
	}{
		{"tenant", tenant.ID.String(), "claude-haiku"},
		{"channel", "whatsapp", "gemini-flash"},
	} {
		form := url.Values{}
		form.Set("scope_type", c.scope)
		form.Set("scope_id", c.scopeID)
		form.Set("model", c.model)
		form.Set("tone", "neutro")
		form.Set("language", "pt-BR")
		form.Set("ai_enabled", "on")
		form.Set("anonymize", "on")
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, newRequest(t, http.MethodPost, "/settings/ai-policy", strings.NewReader(form.Encode())))
		if rec.Code != http.StatusCreated {
			t.Fatalf("seed %s: status = %d; body=%s", c.scope, rec.Code, rec.Body.String())
		}
	}

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, newRequest(t, http.MethodGet, "/settings/ai-policy/preview?channel_id=whatsapp", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("preview status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "Canal (override mais específico)") {
		t.Errorf("preview missing channel source label:\n%s", body)
	}
	if !strings.Contains(body, "gemini-flash") {
		t.Errorf("preview missing channel model:\n%s", body)
	}
	if strings.Contains(body, "claude-haiku") {
		t.Errorf("preview incorrectly surfaces tenant model:\n%s", body)
	}
}

// TestAIEnabledOff_PreviewSaysDisabled covers AC #3: toggling
// ai_enabled=false on a scope makes the preview confirm "IA
// desabilitada neste escopo".
func TestAIEnabledOff_PreviewSaysDisabled(t *testing.T) {
	t.Parallel()
	_, tenant := tenantCtx(t)
	repo := newMemRepo()
	mux := newHandler(t, repo, resolverFromRepo(t, repo))

	// Channel policy with ai_enabled left OFF (form does not send the
	// checkbox value when unchecked).
	form := url.Values{}
	form.Set("scope_type", "channel")
	form.Set("scope_id", "whatsapp")
	form.Set("model", "claude-haiku")
	form.Set("tone", "neutro")
	form.Set("language", "pt-BR")
	// ai_enabled deliberately absent → false
	form.Set("anonymize", "on")

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, newRequest(t, http.MethodPost, "/settings/ai-policy", strings.NewReader(form.Encode())))
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}

	// Confirm the row was persisted with AIEnabled=false.
	got, ok, err := repo.Get(context.Background(), tenant.ID, aipolicy.ScopeChannel, "whatsapp")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !ok {
		t.Fatal("policy not persisted")
	}
	if got.AIEnabled {
		t.Errorf("AIEnabled = true, want false (toggle off)")
	}

	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, newRequest(t, http.MethodGet, "/settings/ai-policy/preview?channel_id=whatsapp", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("preview status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "IA desabilitada neste escopo") {
		t.Errorf("preview missing disabled-message:\n%s", body)
	}
}

// TestUpdate_PinScopeFromURL confirms that PATCH ignores form-supplied
// scope identity and uses the URL — a request that tries to rename
// the scope is silently corrected. Defense in depth against a
// malformed form.
func TestUpdate_PinScopeFromURL(t *testing.T) {
	t.Parallel()
	_, tenant := tenantCtx(t)
	repo := newMemRepo()
	// Seed a row at (channel, whatsapp).
	repo.rows[memKey{tenant.ID, aipolicy.ScopeChannel, "whatsapp"}] = aipolicy.Policy{
		TenantID: tenant.ID, ScopeType: aipolicy.ScopeChannel, ScopeID: "whatsapp",
		Model: "claude-haiku", PromptVersion: "v1", Tone: "neutro", Language: "pt-BR",
		AIEnabled: true, Anonymize: true,
	}
	mux := newHandler(t, repo, resolverFromRepo(t, repo))

	form := url.Values{}
	// Try to relabel the row to (tenant, attacker-controlled-id).
	form.Set("scope_type", "tenant")
	form.Set("scope_id", "00000000-0000-0000-0000-000000000000")
	form.Set("model", "gemini-flash")
	form.Set("tone", "neutro")
	form.Set("language", "pt-BR")
	form.Set("ai_enabled", "on")
	form.Set("anonymize", "on")

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, newRequest(t, http.MethodPatch, "/settings/ai-policy/channel/whatsapp", strings.NewReader(form.Encode())))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	// The original (channel, whatsapp) row was updated.
	got, ok, err := repo.Get(context.Background(), tenant.ID, aipolicy.ScopeChannel, "whatsapp")
	if err != nil {
		t.Fatalf("Get channel/whatsapp: %v", err)
	}
	if !ok || got.Model != "gemini-flash" {
		t.Errorf("(channel, whatsapp) not updated; got ok=%v model=%q", ok, got.Model)
	}
	// And no new row was created at the attacker-controlled identity.
	_, ok, err = repo.Get(context.Background(), tenant.ID, aipolicy.ScopeTenant, "00000000-0000-0000-0000-000000000000")
	if err != nil {
		t.Fatalf("Get tenant/attacker: %v", err)
	}
	if ok {
		t.Errorf("PATCH wrote to attacker-controlled (tenant, 00000…) scope; URL pin failed")
	}
}

// TestDelete_RemovesRowAndReRendersTable confirms DELETE drops the row
// and the response carries the empty-state shell.
func TestDelete_RemovesRowAndReRendersTable(t *testing.T) {
	t.Parallel()
	_, tenant := tenantCtx(t)
	repo := newMemRepo()
	repo.rows[memKey{tenant.ID, aipolicy.ScopeChannel, "whatsapp"}] = aipolicy.Policy{
		TenantID: tenant.ID, ScopeType: aipolicy.ScopeChannel, ScopeID: "whatsapp",
		Model: "claude-haiku", PromptVersion: "v1", Tone: "neutro", Language: "pt-BR",
		AIEnabled: true, Anonymize: true,
	}
	mux := newHandler(t, repo, resolverFromRepo(t, repo))

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, newRequest(t, http.MethodDelete, "/settings/ai-policy/channel/whatsapp", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if _, ok := repo.rows[memKey{tenant.ID, aipolicy.ScopeChannel, "whatsapp"}]; ok {
		t.Errorf("row not removed")
	}
	if !strings.Contains(rec.Body.String(), "Nenhuma policy configurada") {
		t.Errorf("body missing empty-state after delete:\n%s", rec.Body.String())
	}
}

// TestCreate_RejectsInvalidModel asserts the allowlist gate. The
// response is a 422 with a form-error fragment naming the field.
func TestCreate_RejectsInvalidModel(t *testing.T) {
	t.Parallel()
	repo := newMemRepo()
	mux := newHandler(t, repo, resolverFromRepo(t, repo))

	form := url.Values{}
	form.Set("scope_type", "tenant")
	form.Set("scope_id", uuid.New().String())
	form.Set("model", "openrouter/auto") // not in allowlist
	form.Set("tone", "neutro")
	form.Set("language", "pt-BR")

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, newRequest(t, http.MethodPost, "/settings/ai-policy", strings.NewReader(form.Encode())))
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, `data-field="model"`) {
		t.Errorf("error fragment missing model field marker:\n%s", body)
	}
	if !strings.Contains(body, "allowlist") {
		t.Errorf("error fragment missing allowlist explanation:\n%s", body)
	}
}

// TestCreate_RejectsInvalidScope asserts scope_type enum + scope_id
// presence are gated. Covers two of the four invalid shapes (tone /
// language enum failures share the same code path).
func TestCreate_RejectsInvalidScope(t *testing.T) {
	t.Parallel()
	repo := newMemRepo()
	mux := newHandler(t, repo, resolverFromRepo(t, repo))

	cases := []struct {
		name      string
		scopeType string
		scopeID   string
		wantField string
	}{
		{"bad scope type", "global", "x", "scope_type"},
		{"blank scope id", "tenant", "", "scope_id"},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			form := url.Values{}
			form.Set("scope_type", c.scopeType)
			form.Set("scope_id", c.scopeID)
			form.Set("model", "claude-haiku")
			form.Set("tone", "neutro")
			form.Set("language", "pt-BR")
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, newRequest(t, http.MethodPost, "/settings/ai-policy", strings.NewReader(form.Encode())))
			if rec.Code != http.StatusUnprocessableEntity {
				t.Fatalf("status = %d, want 422; body=%s", rec.Code, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), `data-field="`+c.wantField+`"`) {
				t.Errorf("error fragment missing field=%q marker:\n%s", c.wantField, rec.Body.String())
			}
		})
	}
}

// TestNewForm_PrefillsScope confirms the new-form HTMX partial picks
// up ?scope= and ?scope_id= so a context-aware "create channel
// policy" link lands the admin on the right options.
func TestNewForm_PrefillsScope(t *testing.T) {
	t.Parallel()
	repo := newMemRepo()
	mux := newHandler(t, repo, resolverFromRepo(t, repo))

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, newRequest(t, http.MethodGet, "/settings/ai-policy/new?scope=channel&scope_id=whatsapp", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, `value="whatsapp"`) {
		t.Errorf("form missing scope_id prefill:\n%s", body)
	}
	if !strings.Contains(body, `value="channel" selected`) {
		t.Errorf("form missing scope_type prefill:\n%s", body)
	}
}

// TestEditForm_PrefillsPolicy confirms GET …/edit returns the policy
// loaded from the repo with each field pre-populated and the
// scope_type input disabled (a policy's scope identity is immutable).
func TestEditForm_PrefillsPolicy(t *testing.T) {
	t.Parallel()
	_, tenant := tenantCtx(t)
	repo := newMemRepo()
	repo.rows[memKey{tenant.ID, aipolicy.ScopeChannel, "whatsapp"}] = aipolicy.Policy{
		TenantID: tenant.ID, ScopeType: aipolicy.ScopeChannel, ScopeID: "whatsapp",
		Model: "gemini-flash", PromptVersion: "v1", Tone: "formal", Language: "en-US",
		AIEnabled: true, Anonymize: true, OptIn: true,
	}
	mux := newHandler(t, repo, resolverFromRepo(t, repo))

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, newRequest(t, http.MethodGet, "/settings/ai-policy/channel/whatsapp/edit", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{
		`value="whatsapp"`,
		`<option value="gemini-flash" selected>gemini-flash</option>`,
		`<option value="formal" selected>formal</option>`,
		`<option value="en-US" selected>en-US</option>`,
		`Editar policy`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("edit form missing %q:\n%s", want, body)
		}
	}
}

// TestEditForm_NotFound returns 404 when the scope has no policy.
func TestEditForm_NotFound(t *testing.T) {
	t.Parallel()
	repo := newMemRepo()
	mux := newHandler(t, repo, resolverFromRepo(t, repo))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, newRequest(t, http.MethodGet, "/settings/ai-policy/channel/whatsapp/edit", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

// TestEditForm_RejectsInvalidScope rejects a malformed scope_type
// before hitting the repo (400 vs 404).
func TestEditForm_RejectsInvalidScope(t *testing.T) {
	t.Parallel()
	repo := newMemRepo()
	mux := newHandler(t, repo, resolverFromRepo(t, repo))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, newRequest(t, http.MethodGet, "/settings/ai-policy/global/whatsapp/edit", nil))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

// TestPreview_ResolverErrorFallsBackToDefault confirms the preview
// page absorbs resolver errors into the SourceDefault display rather
// than 500-ing — the page is informational and never a hard failure.
func TestPreview_ResolverErrorFallsBackToDefault(t *testing.T) {
	t.Parallel()
	repo := newMemRepo()
	res := failingResolver{err: errors.New("downstream boom")}
	mux := newHandler(t, repo, res)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, newRequest(t, http.MethodGet, "/settings/ai-policy/preview?channel_id=whatsapp", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (fail-soft)", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "Padrão do sistema") {
		t.Errorf("preview missing default-fallback label:\n%s", body)
	}
}

// TestList_RepoErrorReturns500 confirms the page surfaces a 5xx when
// the repository fails — the list page is not a fail-soft surface
// because callers expect the rendered table to be authoritative.
func TestList_RepoErrorReturns500(t *testing.T) {
	t.Parallel()
	repo := newMemRepo()
	repo.listErr = errors.New("db down")
	mux := newHandler(t, repo, resolverFromRepo(t, repo))

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, newRequest(t, http.MethodGet, "/settings/ai-policy", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
}

// TestDelete_RejectsInvalidScope ensures the URL gate fires before
// the repo is consulted.
func TestDelete_RejectsInvalidScope(t *testing.T) {
	t.Parallel()
	repo := newMemRepo()
	mux := newHandler(t, repo, resolverFromRepo(t, repo))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, newRequest(t, http.MethodDelete, "/settings/ai-policy/global/whatsapp", nil))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

// TestList_TenantMissingFromContext yields a 500 — the handler
// refuses to render without a tenant. A misconfigured router that
// drops TenantScope is the only way to hit this branch.
func TestList_TenantMissingFromContext(t *testing.T) {
	t.Parallel()
	repo := newMemRepo()
	mux := newHandler(t, repo, resolverFromRepo(t, repo))
	req, err := http.NewRequest(http.MethodGet, "/settings/ai-policy", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
}

// TestFormError_Error exercises the FormError.Error() formatting so
// the rest of the package sees a consistent string surface.
func TestFormError_Error(t *testing.T) {
	t.Parallel()
	got := (&webaipolicy.FormError{Field: "model", Message: "allowlist"}).Error()
	want := "model: allowlist"
	if got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
}

// TestEditForm_TenantMissing and the parallel preview/create/update/
// delete tests cover the 500-from-missing-tenant branch on every
// other route so a router that drops the scope is consistently
// rejected.
func TestEditForm_TenantMissing(t *testing.T) {
	t.Parallel()
	repo := newMemRepo()
	mux := newHandler(t, repo, resolverFromRepo(t, repo))
	req, _ := http.NewRequest(http.MethodGet, "/settings/ai-policy/channel/whatsapp/edit", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
}

func TestPreview_TenantMissing(t *testing.T) {
	t.Parallel()
	repo := newMemRepo()
	mux := newHandler(t, repo, resolverFromRepo(t, repo))
	req, _ := http.NewRequest(http.MethodGet, "/settings/ai-policy/preview", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
}

func TestCreate_TenantMissing(t *testing.T) {
	t.Parallel()
	repo := newMemRepo()
	mux := newHandler(t, repo, resolverFromRepo(t, repo))
	req, _ := http.NewRequest(http.MethodPost, "/settings/ai-policy", strings.NewReader("scope_type=tenant"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
}

func TestUpdate_TenantMissing(t *testing.T) {
	t.Parallel()
	repo := newMemRepo()
	mux := newHandler(t, repo, resolverFromRepo(t, repo))
	req, _ := http.NewRequest(http.MethodPatch, "/settings/ai-policy/channel/whatsapp", strings.NewReader("model=claude-haiku"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
}

func TestDelete_TenantMissing(t *testing.T) {
	t.Parallel()
	repo := newMemRepo()
	mux := newHandler(t, repo, resolverFromRepo(t, repo))
	req, _ := http.NewRequest(http.MethodDelete, "/settings/ai-policy/channel/whatsapp", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
}

// TestCreate_UpsertErrorReturns500 covers the repo-write failure
// branch of POST.
func TestCreate_UpsertErrorReturns500(t *testing.T) {
	t.Parallel()
	repo := newMemRepo()
	repo.upsertErr = errors.New("db down")
	mux := newHandler(t, repo, resolverFromRepo(t, repo))

	form := url.Values{}
	form.Set("scope_type", "tenant")
	form.Set("scope_id", uuid.New().String())
	form.Set("model", "claude-haiku")
	form.Set("tone", "neutro")
	form.Set("language", "pt-BR")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, newRequest(t, http.MethodPost, "/settings/ai-policy", strings.NewReader(form.Encode())))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body=%s", rec.Code, rec.Body.String())
	}
}

// TestUpdate_UpsertErrorReturns500 covers the same branch on PATCH.
func TestUpdate_UpsertErrorReturns500(t *testing.T) {
	t.Parallel()
	repo := newMemRepo()
	repo.upsertErr = errors.New("db down")
	mux := newHandler(t, repo, resolverFromRepo(t, repo))

	form := url.Values{}
	form.Set("model", "claude-haiku")
	form.Set("tone", "neutro")
	form.Set("language", "pt-BR")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, newRequest(t, http.MethodPatch, "/settings/ai-policy/channel/whatsapp", strings.NewReader(form.Encode())))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body=%s", rec.Code, rec.Body.String())
	}
}

// TestUpdate_FormValidationFailsReturns422 confirms PATCH also
// surfaces validation errors as 422.
func TestUpdate_FormValidationFailsReturns422(t *testing.T) {
	t.Parallel()
	repo := newMemRepo()
	mux := newHandler(t, repo, resolverFromRepo(t, repo))

	form := url.Values{}
	form.Set("model", "openrouter/auto") // not in allowlist
	form.Set("tone", "neutro")
	form.Set("language", "pt-BR")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, newRequest(t, http.MethodPatch, "/settings/ai-policy/channel/whatsapp", strings.NewReader(form.Encode())))
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body=%s", rec.Code, rec.Body.String())
	}
}

// TestDelete_RepoErrorReturns500 covers the repo-side failure branch.
func TestDelete_RepoErrorReturns500(t *testing.T) {
	t.Parallel()
	repo := newMemRepo()
	repo.deleteErr = errors.New("db down")
	mux := newHandler(t, repo, resolverFromRepo(t, repo))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, newRequest(t, http.MethodDelete, "/settings/ai-policy/channel/whatsapp", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
}

// TestUpdate_RejectsInvalidScopeURL covers the URL-validation branch
// on PATCH (e.g. a tampered scope_type that bypasses the form).
func TestUpdate_RejectsInvalidScopeURL(t *testing.T) {
	t.Parallel()
	repo := newMemRepo()
	mux := newHandler(t, repo, resolverFromRepo(t, repo))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, newRequest(t, http.MethodPatch, "/settings/ai-policy/global/whatsapp", strings.NewReader("")))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

// TestEditForm_RepoErrorReturns500 covers the Get-error branch.
func TestEditForm_RepoErrorReturns500(t *testing.T) {
	t.Parallel()
	repo := newMemRepo()
	repo.getErr = errors.New("db down")
	mux := newHandler(t, repo, resolverFromRepo(t, repo))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, newRequest(t, http.MethodGet, "/settings/ai-policy/channel/whatsapp/edit", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
}

// TestRenderListPartial_RepoErrorReturns500 forces the post-mutation
// refresh to error so the renderListPartial fallback branch fires.
func TestRenderListPartial_RepoErrorReturns500(t *testing.T) {
	t.Parallel()
	repo := newMemRepo()
	mux := newHandler(t, repo, resolverFromRepo(t, repo))

	// Seed via direct repo write to bypass listErr-guarded paths…
	repo.rows[memKey{uuid.MustParse("11111111-1111-1111-1111-111111111111"), aipolicy.ScopeChannel, "whatsapp"}] = aipolicy.Policy{}
	// …then flip listErr so the post-DELETE refresh fails.
	repo.listErr = errors.New("db down")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, newRequest(t, http.MethodDelete, "/settings/ai-policy/channel/whatsapp", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
}
