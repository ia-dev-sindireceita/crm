package campaigns_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/campaigns"
	"github.com/pericles-luz/crm/internal/tenancy"
	webcampaigns "github.com/pericles-luz/crm/internal/web/campaigns"
)

// fakeRepo is the in-memory triple-port fake the handler tests drive.
// It implements Reader, Writer, Stats, and Clicks all from one struct
// so tests can mutate a single fixture instead of juggling four.
type fakeRepo struct {
	mu       sync.Mutex
	rows     []*campaigns.Campaign
	clicks   map[uuid.UUID][]*campaigns.CampaignClick
	stats    map[uuid.UUID]campaigns.CampaignStats
	bySlug   map[string]*campaigns.Campaign
	listErr  error
	getErr   error
	saveErr  error
	statsErr error
	clickErr error
	saved    *campaigns.Campaign
}

func newFakeRepo() *fakeRepo {
	return &fakeRepo{
		clicks: map[uuid.UUID][]*campaigns.CampaignClick{},
		stats:  map[uuid.UUID]campaigns.CampaignStats{},
		bySlug: map[string]*campaigns.Campaign{},
	}
}

func (f *fakeRepo) ListByTenant(_ context.Context, _ uuid.UUID) ([]*campaigns.Campaign, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.rows, nil
}

func (f *fakeRepo) GetBySlug(_ context.Context, _ uuid.UUID, slug string) (*campaigns.Campaign, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.getErr != nil {
		return nil, f.getErr
	}
	c, ok := f.bySlug[strings.ToLower(strings.TrimSpace(slug))]
	if !ok {
		return nil, campaigns.ErrNotFound
	}
	return c, nil
}

func (f *fakeRepo) CreateCampaign(_ context.Context, c *campaigns.Campaign) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.saveErr != nil {
		return f.saveErr
	}
	if _, exists := f.bySlug[c.Slug]; exists {
		return campaigns.ErrSlugAlreadyExists
	}
	f.rows = append([]*campaigns.Campaign{c}, f.rows...)
	f.bySlug[c.Slug] = c
	f.saved = c
	return nil
}

func (f *fakeRepo) StatsByTenant(_ context.Context, _ uuid.UUID) (map[uuid.UUID]campaigns.CampaignStats, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.statsErr != nil {
		return nil, f.statsErr
	}
	return f.stats, nil
}

func (f *fakeRepo) ListClicks(_ context.Context, _, campaignID uuid.UUID, _ int) ([]*campaigns.CampaignClick, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.clickErr != nil {
		return nil, f.clickErr
	}
	return f.clicks[campaignID], nil
}

// fullDeps builds Deps wired with the fake repo, a stable now, and
// a stable id generator so assertions can pin both.
func fullDeps(repo *fakeRepo, actor uuid.UUID, fixedID uuid.UUID, now time.Time) webcampaigns.Deps {
	return webcampaigns.Deps{
		Reader:    repo,
		Writer:    repo,
		Stats:     repo,
		Clicks:    repo,
		CSRFToken: func(*http.Request) string { return "tok" },
		UserID:    func(*http.Request) uuid.UUID { return actor },
		ID:        func() uuid.UUID { return fixedID },
		Now:       func() time.Time { return now },
	}
}

// reqWithTenant constructs a request with the tenant already in
// context, mirroring what the production TenantScope middleware does.
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

func buildHandler(t *testing.T, deps webcampaigns.Deps) *webcampaigns.Handler {
	t.Helper()
	h, err := webcampaigns.New(deps)
	if err != nil {
		t.Fatalf("webcampaigns.New: %v", err)
	}
	return h
}

func mux(h *webcampaigns.Handler) *http.ServeMux {
	m := http.NewServeMux()
	h.Routes(m)
	return m
}

// ---------------------------------------------------------------------------
// New / construction
// ---------------------------------------------------------------------------

func TestNew_RejectsMissingDeps(t *testing.T) {
	t.Parallel()
	mutators := map[string]func(*webcampaigns.Deps){
		"missing Reader":    func(d *webcampaigns.Deps) { d.Reader = nil },
		"missing Writer":    func(d *webcampaigns.Deps) { d.Writer = nil },
		"missing Stats":     func(d *webcampaigns.Deps) { d.Stats = nil },
		"missing Clicks":    func(d *webcampaigns.Deps) { d.Clicks = nil },
		"missing CSRFToken": func(d *webcampaigns.Deps) { d.CSRFToken = nil },
		"missing UserID":    func(d *webcampaigns.Deps) { d.UserID = nil },
	}
	repo := newFakeRepo()
	base := fullDeps(repo, uuid.New(), uuid.New(), time.Unix(0, 0).UTC())
	for name, mut := range mutators {
		name, mut := name, mut
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			deps := base
			mut(&deps)
			if _, err := webcampaigns.New(deps); err == nil {
				t.Fatalf("New(%s) = nil error, want failure", name)
			}
		})
	}
}

func TestNew_FullDepsConstructs(t *testing.T) {
	t.Parallel()
	repo := newFakeRepo()
	if _, err := webcampaigns.New(fullDeps(repo, uuid.New(), uuid.New(), time.Now())); err != nil {
		t.Fatalf("New(full): %v", err)
	}
}

func TestNew_DefaultsApplied(t *testing.T) {
	t.Parallel()
	repo := newFakeRepo()
	deps := webcampaigns.Deps{
		Reader:    repo,
		Writer:    repo,
		Stats:     repo,
		Clicks:    repo,
		CSRFToken: func(*http.Request) string { return "tok" },
		UserID:    func(*http.Request) uuid.UUID { return uuid.New() },
		// ID, Now, Logger intentionally left nil; New must fill them.
	}
	h, err := webcampaigns.New(deps)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// Smoke-test the handler still serves a request (covers defaults).
	tenant := newTenant()
	r := reqWithTenant(http.MethodGet, "/campaigns", "", tenant)
	w := httptest.NewRecorder()
	mux(h).ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("defaults: status = %d, body = %s", w.Code, w.Body.String())
	}
}

// ---------------------------------------------------------------------------
// list (GET /campaigns)
// ---------------------------------------------------------------------------

func TestList_RendersDashboardWithRowsAndCSRF(t *testing.T) {
	t.Parallel()
	repo := newFakeRepo()
	tenant := newTenant()
	now := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)
	camp, err := campaigns.NewCampaign(uuid.New(), tenant.ID, "Black Friday", "blackfriday",
		"https://target.test/promo", nil, now)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	camp.WithUTM("google", "cpc", "summer", "shoes", "ad1")
	repo.rows = []*campaigns.Campaign{camp}
	repo.bySlug[camp.Slug] = camp
	repo.stats[camp.ID] = campaigns.CampaignStats{Clicks: 12, Attributions: 4}

	h := buildHandler(t, fullDeps(repo, uuid.New(), uuid.New(), now))
	r := reqWithTenant(http.MethodGet, "/campaigns", "", tenant)
	w := httptest.NewRecorder()
	mux(h).ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	for _, want := range []string{
		`<title>Campanhas</title>`,
		"Black Friday",
		"https://acme.crm.test/c/blackfriday?utm_campaign=summer",
		`name="csrf-token"`,
		`hx-get="/campaigns/new"`,
		"campaign-copy",
		`href="/campaigns/blackfriday"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("list body missing %q\n--- body ---\n%s", want, body)
		}
	}
	// Counters surface in their cells.
	for _, want := range []string{">12<", ">4<"} {
		if !strings.Contains(body, want) {
			t.Errorf("counter cell missing %q", want)
		}
	}
}

func TestList_EmptyStateRendersFallbackCopy(t *testing.T) {
	t.Parallel()
	repo := newFakeRepo()
	tenant := newTenant()
	h := buildHandler(t, fullDeps(repo, uuid.New(), uuid.New(), time.Now().UTC()))
	r := reqWithTenant(http.MethodGet, "/campaigns", "", tenant)
	w := httptest.NewRecorder()
	mux(h).ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "Nenhuma campanha ainda") {
		t.Errorf("empty-state copy missing: %s", w.Body.String())
	}
}

func TestList_5xxOnRepoErrors(t *testing.T) {
	t.Parallel()
	for name, mut := range map[string]func(*fakeRepo){
		"list error":  func(r *fakeRepo) { r.listErr = errors.New("boom") },
		"stats error": func(r *fakeRepo) { r.statsErr = errors.New("boom") },
	} {
		name, mut := name, mut
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			repo := newFakeRepo()
			mut(repo)
			h := buildHandler(t, fullDeps(repo, uuid.New(), uuid.New(), time.Now().UTC()))
			r := reqWithTenant(http.MethodGet, "/campaigns", "", newTenant())
			w := httptest.NewRecorder()
			mux(h).ServeHTTP(w, r)
			if w.Code != http.StatusInternalServerError {
				t.Errorf("status = %d, want 500", w.Code)
			}
		})
	}
}

func TestList_5xxOnMissingCSRF(t *testing.T) {
	t.Parallel()
	repo := newFakeRepo()
	deps := fullDeps(repo, uuid.New(), uuid.New(), time.Now().UTC())
	deps.CSRFToken = func(*http.Request) string { return "" }
	h := buildHandler(t, deps)
	r := reqWithTenant(http.MethodGet, "/campaigns", "", newTenant())
	w := httptest.NewRecorder()
	mux(h).ServeHTTP(w, r)
	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", w.Code)
	}
}

func TestList_5xxOnMissingTenant(t *testing.T) {
	t.Parallel()
	repo := newFakeRepo()
	h := buildHandler(t, fullDeps(repo, uuid.New(), uuid.New(), time.Now().UTC()))
	r := httptest.NewRequest(http.MethodGet, "/campaigns", nil) // no tenant in ctx
	w := httptest.NewRecorder()
	mux(h).ServeHTTP(w, r)
	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", w.Code)
	}
}

// ---------------------------------------------------------------------------
// newForm (GET /campaigns/new)
// ---------------------------------------------------------------------------

func TestNewForm_RendersBlankForm(t *testing.T) {
	t.Parallel()
	repo := newFakeRepo()
	h := buildHandler(t, fullDeps(repo, uuid.New(), uuid.New(), time.Now().UTC()))
	r := reqWithTenant(http.MethodGet, "/campaigns/new", "", newTenant())
	w := httptest.NewRecorder()
	mux(h).ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	for _, want := range []string{
		`<title>Nova campanha</title>`,
		`hx-post="/campaigns"`,
		`name="slug"`,
		`name="redirect_url"`,
		`name="utm_source"`,
	} {
		if !strings.Contains(w.Body.String(), want) {
			t.Errorf("form body missing %q", want)
		}
	}
}

func TestNewForm_5xxOnMissingCSRF(t *testing.T) {
	t.Parallel()
	repo := newFakeRepo()
	deps := fullDeps(repo, uuid.New(), uuid.New(), time.Now().UTC())
	deps.CSRFToken = func(*http.Request) string { return "" }
	h := buildHandler(t, deps)
	r := reqWithTenant(http.MethodGet, "/campaigns/new", "", newTenant())
	w := httptest.NewRecorder()
	mux(h).ServeHTTP(w, r)
	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", w.Code)
	}
}

// ---------------------------------------------------------------------------
// create (POST /campaigns)
// ---------------------------------------------------------------------------

func formBody(values map[string]string) string {
	b := strings.Builder{}
	first := true
	for k, v := range values {
		if !first {
			b.WriteByte('&')
		}
		first = false
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(encode(v))
	}
	return b.String()
}

func encode(v string) string {
	// Minimal URL-encoder used by the test seeds; the production
	// path uses net/url Values.Encode under the hood. We use a thin
	// wrapper so test bodies stay readable.
	out := strings.Builder{}
	for _, r := range v {
		switch {
		case r >= 'a' && r <= 'z',
			r >= 'A' && r <= 'Z',
			r >= '0' && r <= '9',
			r == '-' || r == '.' || r == '_' || r == '~':
			out.WriteRune(r)
		case r == ' ':
			out.WriteByte('+')
		default:
			b := []byte(string(r))
			for _, c := range b {
				out.WriteByte('%')
				const hex = "0123456789ABCDEF"
				out.WriteByte(hex[c>>4])
				out.WriteByte(hex[c&0x0f])
			}
		}
	}
	return out.String()
}

func TestCreate_PersistsAndRendersListPartial(t *testing.T) {
	t.Parallel()
	repo := newFakeRepo()
	tenant := newTenant()
	actor := uuid.New()
	fixedID := uuid.New()
	now := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)
	h := buildHandler(t, fullDeps(repo, actor, fixedID, now))

	body := formBody(map[string]string{
		"name":         "Black Friday 2026",
		"slug":         "blackfriday-2026",
		"redirect_url": "https://target.test/landing",
		"utm_source":   "google",
		"utm_medium":   "cpc",
	})
	r := reqWithTenant(http.MethodPost, "/campaigns", body, tenant)
	w := httptest.NewRecorder()
	mux(h).ServeHTTP(w, r)
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d (want 201), body=%s", w.Code, w.Body.String())
	}
	if repo.saved == nil {
		t.Fatal("repo.saved is nil — CreateCampaign was not called")
	}
	if repo.saved.ID != fixedID {
		t.Errorf("saved id = %s, want %s (ID factory ignored)", repo.saved.ID, fixedID)
	}
	if repo.saved.UTMSource != "google" || repo.saved.UTMMedium != "cpc" {
		t.Errorf("UTM not applied: %+v", repo.saved)
	}
	// Response body is the list partial, so it should include the new row.
	if !strings.Contains(w.Body.String(), "Black Friday 2026") {
		t.Errorf("list partial missing new row: %s", w.Body.String())
	}
}

func TestCreate_MissingFieldsRender422WithInlineError(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		body map[string]string
		want string
	}{
		{"missing name", map[string]string{"slug": "ok", "redirect_url": "https://x.test"}, "nome é obrigatório"},
		{"missing slug", map[string]string{"name": "n", "redirect_url": "https://x.test"}, "slug é obrigatório"},
		{"missing url", map[string]string{"name": "n", "slug": "ok"}, "URL de destino é obrigatória"},
		{"bad slug chars", map[string]string{"name": "n", "slug": "BAD slug!", "redirect_url": "https://x.test"}, "slug deve usar apenas a-z, 0-9"},
		{"non-http url", map[string]string{"name": "n", "slug": "ok", "redirect_url": "javascript:alert(1)"}, "URL deve usar http ou https"},
		{"bad expires", map[string]string{"name": "n", "slug": "ok", "redirect_url": "https://x.test", "expires_at": "not-a-date"}, "data inválida"},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			repo := newFakeRepo()
			h := buildHandler(t, fullDeps(repo, uuid.New(), uuid.New(), time.Now().UTC()))
			r := reqWithTenant(http.MethodPost, "/campaigns", formBody(c.body), newTenant())
			w := httptest.NewRecorder()
			mux(h).ServeHTTP(w, r)
			if w.Code != http.StatusUnprocessableEntity {
				t.Fatalf("status = %d, want 422 (body=%s)", w.Code, w.Body.String())
			}
			if !strings.Contains(w.Body.String(), c.want) {
				t.Errorf("missing error copy %q in body=%s", c.want, w.Body.String())
			}
			if repo.saved != nil {
				t.Errorf("CreateCampaign should NOT have run for %q", c.name)
			}
		})
	}
}

func TestCreate_SlugCollisionRendersInlineError(t *testing.T) {
	t.Parallel()
	repo := newFakeRepo()
	repo.saveErr = campaigns.ErrSlugAlreadyExists
	h := buildHandler(t, fullDeps(repo, uuid.New(), uuid.New(), time.Now().UTC()))
	body := formBody(map[string]string{
		"name":         "dup",
		"slug":         "shared",
		"redirect_url": "https://x.test",
	})
	r := reqWithTenant(http.MethodPost, "/campaigns", body, newTenant())
	w := httptest.NewRecorder()
	mux(h).ServeHTTP(w, r)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", w.Code)
	}
	if !strings.Contains(w.Body.String(), "slug já está em uso") {
		t.Errorf("collision message missing: %s", w.Body.String())
	}
}

func TestCreate_5xxOnPersistError(t *testing.T) {
	t.Parallel()
	repo := newFakeRepo()
	repo.saveErr = errors.New("disk full")
	h := buildHandler(t, fullDeps(repo, uuid.New(), uuid.New(), time.Now().UTC()))
	body := formBody(map[string]string{
		"name":         "x",
		"slug":         "x",
		"redirect_url": "https://x.test",
	})
	r := reqWithTenant(http.MethodPost, "/campaigns", body, newTenant())
	w := httptest.NewRecorder()
	mux(h).ServeHTTP(w, r)
	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500 (body=%s)", w.Code, w.Body.String())
	}
}

func TestCreate_UnauthorizedWhenUserIDIsNil(t *testing.T) {
	t.Parallel()
	repo := newFakeRepo()
	deps := fullDeps(repo, uuid.Nil, uuid.New(), time.Now().UTC())
	h := buildHandler(t, deps)
	body := formBody(map[string]string{
		"name":         "x",
		"slug":         "x",
		"redirect_url": "https://x.test",
	})
	r := reqWithTenant(http.MethodPost, "/campaigns", body, newTenant())
	w := httptest.NewRecorder()
	mux(h).ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", w.Code)
	}
}

func TestCreate_5xxOnMissingTenant(t *testing.T) {
	t.Parallel()
	repo := newFakeRepo()
	h := buildHandler(t, fullDeps(repo, uuid.New(), uuid.New(), time.Now().UTC()))
	body := formBody(map[string]string{
		"name":         "x",
		"slug":         "x",
		"redirect_url": "https://x.test",
	})
	r := httptest.NewRequest(http.MethodPost, "/campaigns", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	mux(h).ServeHTTP(w, r)
	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", w.Code)
	}
}

func TestCreate_ParsesExpiresAtAsUTC(t *testing.T) {
	t.Parallel()
	repo := newFakeRepo()
	now := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)
	h := buildHandler(t, fullDeps(repo, uuid.New(), uuid.New(), now))
	body := formBody(map[string]string{
		"name":         "x",
		"slug":         "ok",
		"redirect_url": "https://x.test",
		"expires_at":   "2026-12-31T23:59",
	})
	r := reqWithTenant(http.MethodPost, "/campaigns", body, newTenant())
	w := httptest.NewRecorder()
	mux(h).ServeHTTP(w, r)
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d (body=%s)", w.Code, w.Body.String())
	}
	want := time.Date(2026, 12, 31, 23, 59, 0, 0, time.UTC)
	if repo.saved.ExpiresAt == nil || !repo.saved.ExpiresAt.Equal(want) {
		t.Errorf("ExpiresAt = %v, want %v", repo.saved.ExpiresAt, want)
	}
}

// ---------------------------------------------------------------------------
// detail (GET /campaigns/{slug})
// ---------------------------------------------------------------------------

func TestDetail_RendersCampaignAndClicks(t *testing.T) {
	t.Parallel()
	repo := newFakeRepo()
	tenant := newTenant()
	now := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)
	camp, _ := campaigns.NewCampaign(uuid.New(), tenant.ID, "Black Friday", "blackfriday",
		"https://target.test/promo", nil, now)
	repo.rows = []*campaigns.Campaign{camp}
	repo.bySlug[camp.Slug] = camp
	repo.stats[camp.ID] = campaigns.CampaignStats{Clicks: 2, Attributions: 1}
	linked := uuid.New()
	repo.clicks[camp.ID] = []*campaigns.CampaignClick{
		{ID: uuid.New(), CampaignID: camp.ID, ClickID: "ck-a", CreatedAt: now, ContactID: &linked, IP: netip.MustParseAddr("203.0.113.7")},
		{ID: uuid.New(), CampaignID: camp.ID, ClickID: "ck-b", CreatedAt: now.Add(-time.Minute), UserAgent: "Mozilla/5.0"},
	}
	h := buildHandler(t, fullDeps(repo, uuid.New(), uuid.New(), now))
	r := reqWithTenant(http.MethodGet, "/campaigns/blackfriday", "", tenant)
	w := httptest.NewRecorder()
	mux(h).ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d (body=%s)", w.Code, w.Body.String())
	}
	for _, want := range []string{
		"<title>Campanha · Black Friday</title>",
		"ck-a",
		"ck-b",
		linked.String(),
		"203.0.113.7",
		"Mozilla/5.0",
		`hx-trigger="every 10s"`,
		`hx-get="/campaigns/blackfriday/clicks"`,
		"cliques: <strong>2",
		"atribuições: <strong>1",
	} {
		if !strings.Contains(w.Body.String(), want) {
			t.Errorf("detail body missing %q", want)
		}
	}
}

func TestDetail_404WhenSlugMissing(t *testing.T) {
	t.Parallel()
	repo := newFakeRepo()
	h := buildHandler(t, fullDeps(repo, uuid.New(), uuid.New(), time.Now().UTC()))
	r := reqWithTenant(http.MethodGet, "/campaigns/missing", "", newTenant())
	w := httptest.NewRecorder()
	mux(h).ServeHTTP(w, r)
	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", w.Code)
	}
}

func TestDetail_5xxOnRepoErrors(t *testing.T) {
	t.Parallel()
	tenant := newTenant()
	now := time.Now().UTC()
	seed := func(repo *fakeRepo) {
		camp, _ := campaigns.NewCampaign(uuid.New(), tenant.ID, "X", "x",
			"https://x.test", nil, now)
		repo.bySlug[camp.Slug] = camp
	}
	for name, mut := range map[string]func(*fakeRepo){
		"get error":   func(r *fakeRepo) { seed(r); r.getErr = errors.New("boom") },
		"stats error": func(r *fakeRepo) { seed(r); r.statsErr = errors.New("boom") },
		"click error": func(r *fakeRepo) { seed(r); r.clickErr = errors.New("boom") },
	} {
		name, mut := name, mut
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			repo := newFakeRepo()
			mut(repo)
			h := buildHandler(t, fullDeps(repo, uuid.New(), uuid.New(), now))
			r := reqWithTenant(http.MethodGet, "/campaigns/x", "", tenant)
			w := httptest.NewRecorder()
			mux(h).ServeHTTP(w, r)
			if w.Code != http.StatusInternalServerError {
				t.Errorf("%s status = %d, want 500", name, w.Code)
			}
		})
	}
}

func TestDetail_5xxOnMissingCSRF(t *testing.T) {
	t.Parallel()
	repo := newFakeRepo()
	tenant := newTenant()
	now := time.Now().UTC()
	camp, _ := campaigns.NewCampaign(uuid.New(), tenant.ID, "X", "x", "https://x.test", nil, now)
	repo.bySlug[camp.Slug] = camp
	deps := fullDeps(repo, uuid.New(), uuid.New(), now)
	deps.CSRFToken = func(*http.Request) string { return "" }
	h := buildHandler(t, deps)
	r := reqWithTenant(http.MethodGet, "/campaigns/x", "", tenant)
	w := httptest.NewRecorder()
	mux(h).ServeHTTP(w, r)
	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", w.Code)
	}
}

func TestDetail_5xxOnMissingTenant(t *testing.T) {
	t.Parallel()
	repo := newFakeRepo()
	h := buildHandler(t, fullDeps(repo, uuid.New(), uuid.New(), time.Now().UTC()))
	r := httptest.NewRequest(http.MethodGet, "/campaigns/x", nil)
	w := httptest.NewRecorder()
	mux(h).ServeHTTP(w, r)
	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", w.Code)
	}
}

// ---------------------------------------------------------------------------
// clicksFragment (GET /campaigns/{slug}/clicks)
// ---------------------------------------------------------------------------

func TestClicksFragment_RendersTableAndCounters(t *testing.T) {
	t.Parallel()
	repo := newFakeRepo()
	tenant := newTenant()
	now := time.Now().UTC()
	camp, _ := campaigns.NewCampaign(uuid.New(), tenant.ID, "X", "x", "https://x.test", nil, now)
	repo.bySlug[camp.Slug] = camp
	repo.stats[camp.ID] = campaigns.CampaignStats{Clicks: 7, Attributions: 3}
	repo.clicks[camp.ID] = []*campaigns.CampaignClick{
		{ID: uuid.New(), CampaignID: camp.ID, ClickID: "tic-1", CreatedAt: now},
	}
	h := buildHandler(t, fullDeps(repo, uuid.New(), uuid.New(), now))
	r := reqWithTenant(http.MethodGet, "/campaigns/x/clicks", "", tenant)
	w := httptest.NewRecorder()
	mux(h).ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d (body=%s)", w.Code, w.Body.String())
	}
	body := w.Body.String()
	for _, want := range []string{
		"tic-1",
		"cliques: <strong>7",
		"atribuições: <strong>3",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("clicks fragment missing %q", want)
		}
	}
	// Fragment must NOT include the full page <html> shell.
	if strings.Contains(body, "<html") {
		t.Errorf("fragment leaked full page wrapper: %s", body)
	}
}

func TestClicksFragment_404WhenSlugMissing(t *testing.T) {
	t.Parallel()
	repo := newFakeRepo()
	h := buildHandler(t, fullDeps(repo, uuid.New(), uuid.New(), time.Now().UTC()))
	r := reqWithTenant(http.MethodGet, "/campaigns/missing/clicks", "", newTenant())
	w := httptest.NewRecorder()
	mux(h).ServeHTTP(w, r)
	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", w.Code)
	}
}

func TestClicksFragment_5xxOnClickError(t *testing.T) {
	t.Parallel()
	repo := newFakeRepo()
	tenant := newTenant()
	now := time.Now().UTC()
	camp, _ := campaigns.NewCampaign(uuid.New(), tenant.ID, "X", "x", "https://x.test", nil, now)
	repo.bySlug[camp.Slug] = camp
	repo.clickErr = errors.New("boom")
	h := buildHandler(t, fullDeps(repo, uuid.New(), uuid.New(), now))
	r := reqWithTenant(http.MethodGet, "/campaigns/x/clicks", "", tenant)
	w := httptest.NewRecorder()
	mux(h).ServeHTTP(w, r)
	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", w.Code)
	}
}

func TestClicksFragment_5xxOnGetError(t *testing.T) {
	t.Parallel()
	repo := newFakeRepo()
	repo.getErr = errors.New("boom")
	h := buildHandler(t, fullDeps(repo, uuid.New(), uuid.New(), time.Now().UTC()))
	r := reqWithTenant(http.MethodGet, "/campaigns/x/clicks", "", newTenant())
	w := httptest.NewRecorder()
	mux(h).ServeHTTP(w, r)
	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", w.Code)
	}
}

func TestClicksFragment_5xxOnStatsError(t *testing.T) {
	t.Parallel()
	repo := newFakeRepo()
	tenant := newTenant()
	now := time.Now().UTC()
	camp, _ := campaigns.NewCampaign(uuid.New(), tenant.ID, "X", "x", "https://x.test", nil, now)
	repo.bySlug[camp.Slug] = camp
	repo.statsErr = errors.New("boom")
	h := buildHandler(t, fullDeps(repo, uuid.New(), uuid.New(), now))
	r := reqWithTenant(http.MethodGet, "/campaigns/x/clicks", "", tenant)
	w := httptest.NewRecorder()
	mux(h).ServeHTTP(w, r)
	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", w.Code)
	}
}

func TestClicksFragment_5xxOnMissingTenant(t *testing.T) {
	t.Parallel()
	repo := newFakeRepo()
	h := buildHandler(t, fullDeps(repo, uuid.New(), uuid.New(), time.Now().UTC()))
	r := httptest.NewRequest(http.MethodGet, "/campaigns/x/clicks", nil)
	w := httptest.NewRecorder()
	mux(h).ServeHTTP(w, r)
	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", w.Code)
	}
}

// ---------------------------------------------------------------------------
// link rendering edges
// ---------------------------------------------------------------------------

func TestLink_FallsBackWhenTenantHostEmpty(t *testing.T) {
	t.Parallel()
	repo := newFakeRepo()
	tenant := &tenancy.Tenant{ID: uuid.New(), Name: "NoHost"} // Host == ""
	now := time.Now().UTC()
	camp, _ := campaigns.NewCampaign(uuid.New(), tenant.ID, "X", "no-host", "https://x.test", nil, now)
	repo.rows = []*campaigns.Campaign{camp}
	repo.bySlug[camp.Slug] = camp
	h := buildHandler(t, fullDeps(repo, uuid.New(), uuid.New(), now))
	r := reqWithTenant(http.MethodGet, "/campaigns", "", tenant)
	w := httptest.NewRecorder()
	mux(h).ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "https://crm.invalid/c/no-host") {
		t.Errorf("missing fallback link: %s", w.Body.String())
	}
}

func TestRow_ExpiredCampaignFlagsStatus(t *testing.T) {
	t.Parallel()
	repo := newFakeRepo()
	tenant := newTenant()
	now := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	past := now.Add(-time.Hour)
	camp, _ := campaigns.NewCampaign(uuid.New(), tenant.ID, "Old", "old", "https://x.test", &past, now.Add(-2*time.Hour))
	repo.rows = []*campaigns.Campaign{camp}
	repo.bySlug[camp.Slug] = camp
	h := buildHandler(t, fullDeps(repo, uuid.New(), uuid.New(), now))
	r := reqWithTenant(http.MethodGet, "/campaigns", "", tenant)
	w := httptest.NewRecorder()
	mux(h).ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "campaign-row--expired") {
		t.Errorf("expired row missing row-modifier class: %s", body)
	}
	if !strings.Contains(body, "campaign-status--expired") {
		t.Errorf("expired status pill missing: %s", body)
	}
}

func TestDetail_BadSlugReturns400(t *testing.T) {
	t.Parallel()
	repo := newFakeRepo()
	h := buildHandler(t, fullDeps(repo, uuid.New(), uuid.New(), time.Now().UTC()))
	// Path-value slug must be present; blank value would be a different
	// route, but Go's mux refuses an empty path segment — easier path
	// is to invoke detail directly with a tenant context and a blank
	// slug via the trailing /campaigns/ path. The handler treats it as
	// a 400 because the mux still produces an empty PathValue("slug").
	r := reqWithTenant(http.MethodGet, "/campaigns/%20/clicks", "", newTenant())
	w := httptest.NewRecorder()
	mux(h).ServeHTTP(w, r)
	// Whitespace-only slug trims to empty → 400.
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 (body=%s)", w.Code, w.Body.String())
	}
}
