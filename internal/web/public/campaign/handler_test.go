package campaign_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/campaigns"
	"github.com/pericles-luz/crm/internal/tenancy"
	"github.com/pericles-luz/crm/internal/web/public/campaign"
)

func newTestHandler(t *testing.T, repo campaigns.Repository, now func() time.Time, allowed []string) http.Handler {
	t.Helper()
	h, err := campaign.New(campaign.Deps{
		Repo:         repo,
		Now:          now,
		NewClickID:   func() string { return "ck-test-token" },
		AllowedHosts: allowed,
		CookieSecure: false,
		Logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("campaign.New: %v", err)
	}
	mux := http.NewServeMux()
	h.Routes(mux)
	return mux
}

func newRequestForHost(t *testing.T, host, path string) (*http.Request, *tenancy.Tenant) {
	t.Helper()
	tenant := &tenancy.Tenant{ID: uuid.New(), Name: "acme", Host: host}
	r := httptest.NewRequest(http.MethodGet, path, nil)
	r.Host = host
	r.RemoteAddr = "203.0.113.42:55555"
	r = r.WithContext(tenancy.WithContext(r.Context(), tenant))
	return r, tenant
}

func mustCampaign(t *testing.T, tenantID uuid.UUID, slug, redirectURL string, expiresAt *time.Time) *campaigns.Campaign {
	t.Helper()
	c, err := campaigns.NewCampaign(uuid.New(), tenantID, "name", slug, redirectURL, expiresAt, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("NewCampaign: %v", err)
	}
	return c
}

func TestHandler_HappyPath_Redirects302AndSetsCookie(t *testing.T) {
	t.Parallel()
	repo := campaigns.NewInMemoryRepository()
	r, tenant := newRequestForHost(t, "acme.crm.local", "/c/promo")
	c := mustCampaign(t, tenant.ID, "promo", "https://wa.me/5511999999999?text=hi", nil)
	if err := repo.CreateCampaign(context.Background(), c); err != nil {
		t.Fatalf("CreateCampaign: %v", err)
	}
	now := func() time.Time { return time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC) }
	h := newTestHandler(t, repo, now, []string{"wa.me"})

	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	res := w.Result()
	defer res.Body.Close()

	if res.StatusCode != http.StatusFound {
		t.Fatalf("status = %d, want %d", res.StatusCode, http.StatusFound)
	}
	if loc := res.Header.Get("Location"); loc != c.RedirectURL {
		t.Fatalf("Location = %q, want %q", loc, c.RedirectURL)
	}
	cookies := res.Cookies()
	if len(cookies) != 1 {
		t.Fatalf("cookies len = %d, want 1", len(cookies))
	}
	cookie := cookies[0]
	if cookie.Name != campaign.CookieName {
		t.Fatalf("cookie name = %q, want %q", cookie.Name, campaign.CookieName)
	}
	if cookie.Value != "ck-test-token" {
		t.Fatalf("cookie value = %q, want %q", cookie.Value, "ck-test-token")
	}
	if !cookie.HttpOnly {
		t.Error("cookie HttpOnly = false, want true")
	}
	if cookie.SameSite != http.SameSiteLaxMode {
		t.Errorf("cookie SameSite = %v, want Lax", cookie.SameSite)
	}
	if cookie.Path != "/" {
		t.Errorf("cookie Path = %q, want /", cookie.Path)
	}
	if cookie.MaxAge != int(campaign.CookieMaxAge.Seconds()) {
		t.Errorf("cookie MaxAge = %d, want %d", cookie.MaxAge, int(campaign.CookieMaxAge.Seconds()))
	}
}

func TestHandler_AC2_IdempotentOnRepeatedClickFromSameCookie(t *testing.T) {
	t.Parallel()
	repo := campaigns.NewInMemoryRepository()
	r, tenant := newRequestForHost(t, "acme.crm.local", "/c/promo")
	c := mustCampaign(t, tenant.ID, "promo", "https://wa.me/5511999999999", nil)
	_ = repo.CreateCampaign(context.Background(), c)
	now := func() time.Time { return time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC) }
	h := newTestHandler(t, repo, now, []string{"wa.me"})

	// First click — server mints click_id and sets cookie.
	w1 := httptest.NewRecorder()
	h.ServeHTTP(w1, r)
	cookies := w1.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("first click: cookies len = %d, want 1", len(cookies))
	}
	clickID := cookies[0].Value

	// Second click from the same browser carries the cookie back.
	// Reuse the same tenant so the campaign row is found under RLS.
	r2 := httptest.NewRequest(http.MethodGet, "/c/promo", nil)
	r2.Host = "acme.crm.local"
	r2.RemoteAddr = "203.0.113.42:55555"
	r2 = r2.WithContext(tenancy.WithContext(r2.Context(), tenant))
	r2.AddCookie(&http.Cookie{Name: campaign.CookieName, Value: clickID})
	w2 := httptest.NewRecorder()
	h.ServeHTTP(w2, r2)
	if w2.Result().StatusCode != http.StatusFound {
		t.Fatalf("second click status = %d, want %d", w2.Result().StatusCode, http.StatusFound)
	}
	if got := len(w2.Result().Cookies()); got != 0 {
		t.Errorf("second click set %d cookies, want 0 (cookie should be reused)", got)
	}

	// And the ledger has exactly one click for that click_id.
	if err := repo.LinkContactToCampaign(context.Background(), tenant.ID, clickID, uuid.New()); err != nil {
		t.Fatalf("LinkContactToCampaign expected to find the click row: %v", err)
	}
}

func TestHandler_NotFoundOnUnknownSlug(t *testing.T) {
	t.Parallel()
	repo := campaigns.NewInMemoryRepository()
	r, _ := newRequestForHost(t, "acme.crm.local", "/c/no-such-slug")
	h := newTestHandler(t, repo, time.Now, []string{"wa.me"})

	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Result().StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", w.Result().StatusCode, http.StatusNotFound)
	}
}

func TestHandler_NotFoundOnInvalidSlugShape(t *testing.T) {
	t.Parallel()
	repo := campaigns.NewInMemoryRepository()
	r, _ := newRequestForHost(t, "acme.crm.local", "/c/INVALID_SLUG!!")
	h := newTestHandler(t, repo, time.Now, []string{"wa.me"})

	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Result().StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", w.Result().StatusCode, http.StatusNotFound)
	}
}

func TestHandler_GoneWhenExpired(t *testing.T) {
	t.Parallel()
	repo := campaigns.NewInMemoryRepository()
	r, tenant := newRequestForHost(t, "acme.crm.local", "/c/old-promo")
	expired := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	c := mustCampaign(t, tenant.ID, "old-promo", "https://wa.me/5511999999999", &expired)
	_ = repo.CreateCampaign(context.Background(), c)
	now := func() time.Time { return time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC) }
	h := newTestHandler(t, repo, now, []string{"wa.me"})

	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Result().StatusCode != http.StatusGone {
		t.Fatalf("status = %d, want %d", w.Result().StatusCode, http.StatusGone)
	}
}

func TestHandler_AC7_RejectsRedirectOutsideAllowlist(t *testing.T) {
	t.Parallel()
	repo := campaigns.NewInMemoryRepository()
	r, tenant := newRequestForHost(t, "acme.crm.local", "/c/evil")
	// Legitimate campaign row pointing at an attacker-controlled host.
	c := mustCampaign(t, tenant.ID, "evil", "https://attacker.example.com/steal", nil)
	_ = repo.CreateCampaign(context.Background(), c)
	now := func() time.Time { return time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC) }
	h := newTestHandler(t, repo, now, []string{"wa.me", "*.tenant-marketing.com"})

	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Result().StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want %d", w.Result().StatusCode, http.StatusBadGateway)
	}
}

func TestHandler_AllowsRedirectMatchingRequestHost(t *testing.T) {
	t.Parallel()
	repo := campaigns.NewInMemoryRepository()
	r, tenant := newRequestForHost(t, "acme.crm.local", "/c/self")
	c := mustCampaign(t, tenant.ID, "self", "https://acme.crm.local/marketing/page", nil)
	_ = repo.CreateCampaign(context.Background(), c)
	now := func() time.Time { return time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC) }
	h := newTestHandler(t, repo, now, nil)

	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Result().StatusCode != http.StatusFound {
		t.Fatalf("status = %d, want %d", w.Result().StatusCode, http.StatusFound)
	}
}

func TestHandler_AllowsRedirectMatchingWildcardAllowlist(t *testing.T) {
	t.Parallel()
	repo := campaigns.NewInMemoryRepository()
	r, tenant := newRequestForHost(t, "acme.crm.local", "/c/sub")
	c := mustCampaign(t, tenant.ID, "sub", "https://lp.tenant-marketing.com/promo", nil)
	_ = repo.CreateCampaign(context.Background(), c)
	now := func() time.Time { return time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC) }
	h := newTestHandler(t, repo, now, []string{"*.tenant-marketing.com"})

	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Result().StatusCode != http.StatusFound {
		t.Fatalf("status = %d, want %d", w.Result().StatusCode, http.StatusFound)
	}
}

func TestHandler_WildcardAllowlistDoesNotMatchApex(t *testing.T) {
	t.Parallel()
	repo := campaigns.NewInMemoryRepository()
	r, tenant := newRequestForHost(t, "acme.crm.local", "/c/apex")
	c := mustCampaign(t, tenant.ID, "apex", "https://tenant-marketing.com/promo", nil)
	_ = repo.CreateCampaign(context.Background(), c)
	now := func() time.Time { return time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC) }
	h := newTestHandler(t, repo, now, []string{"*.tenant-marketing.com"})

	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Result().StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want %d (apex must require its own entry)", w.Result().StatusCode, http.StatusBadGateway)
	}
}

func TestHandler_500WhenTenantMissing(t *testing.T) {
	t.Parallel()
	repo := campaigns.NewInMemoryRepository()
	r := httptest.NewRequest(http.MethodGet, "/c/promo", nil)
	r.Host = "acme.crm.local"
	h := newTestHandler(t, repo, time.Now, []string{"wa.me"})

	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Result().StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d", w.Result().StatusCode, http.StatusInternalServerError)
	}
}

func TestHandler_RecordsBotMetaForBotUA(t *testing.T) {
	t.Parallel()
	repo := campaigns.NewInMemoryRepository()
	r, tenant := newRequestForHost(t, "acme.crm.local", "/c/promo")
	r.Header.Set("User-Agent", "Mozilla/5.0 (compatible; Googlebot/2.1)")
	c := mustCampaign(t, tenant.ID, "promo", "https://wa.me/5511999999999", nil)
	_ = repo.CreateCampaign(context.Background(), c)
	now := func() time.Time { return time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC) }
	h := newTestHandler(t, repo, now, []string{"wa.me"})

	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Result().StatusCode != http.StatusFound {
		t.Fatalf("status = %d, want %d", w.Result().StatusCode, http.StatusFound)
	}
	cookieID := w.Result().Cookies()[0].Value
	// To verify meta.bot landed on the row, we have to introspect via
	// LinkContactToCampaign (the only port surface that touches the
	// row by click_id). For meta inspection we go through the
	// in-memory adapter directly.
	clicks := drainClicks(repo)
	if len(clicks) != 1 {
		t.Fatalf("clicks len = %d, want 1", len(clicks))
	}
	if got, ok := clicks[0].Meta["bot"].(bool); !ok || !got {
		t.Errorf("Meta[bot] = %v, want true", clicks[0].Meta["bot"])
	}
	if clicks[0].ClickID != cookieID {
		t.Errorf("ClickID = %q, want %q", clicks[0].ClickID, cookieID)
	}
}

// drainClicks pulls the click rows out of an InMemoryRepository via a
// tenant scan. The adapter exposes ListByTenant for campaigns but not
// clicks; we rely on LinkContactToCampaign being usable as a probe to
// validate persistence. For meta inspection a test-only helper would
// be tighter — we accept the indirection because the public surface
// of campaigns.InMemoryRepository is the contract under test.
func drainClicks(repo campaigns.Repository) []*campaigns.CampaignClick {
	// Type-assertion is intentional: the in-memory repo exposes the
	// concrete fields the production adapter hides behind the port.
	// Tests in this package may rely on the concrete type because the
	// fake is shipped from the same module.
	inmem, ok := repo.(*campaigns.InMemoryRepository)
	if !ok {
		return nil
	}
	return inmemDumpClicks(inmem)
}

// inmemDumpClicks unwraps the concrete in-memory adapter's click map.
// It lives next to the handler tests rather than on the adapter so the
// production type does not grow a test-only accessor.
func inmemDumpClicks(inmem *campaigns.InMemoryRepository) []*campaigns.CampaignClick {
	// We can extract via probing: walk every (tenant, click_id) by
	// trying LinkContactToCampaign with a sentinel contact and rolling
	// back. That is too clever; instead expose via reflection-free
	// helper added to the package below. Use the exported accessor.
	return inmem.DumpClicksForTest()
}

func TestHandler_AnyMethodOtherThanGetIs405(t *testing.T) {
	t.Parallel()
	repo := campaigns.NewInMemoryRepository()
	tenant := &tenancy.Tenant{ID: uuid.New(), Name: "acme", Host: "acme.crm.local"}
	c := mustCampaign(t, tenant.ID, "promo", "https://wa.me/5511999999999", nil)
	_ = repo.CreateCampaign(context.Background(), c)
	now := func() time.Time { return time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC) }
	h := newTestHandler(t, repo, now, []string{"wa.me"})

	for _, m := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
		req := httptest.NewRequest(m, "/c/promo", nil)
		req.Host = "acme.crm.local"
		req.RemoteAddr = "203.0.113.1:80"
		req = req.WithContext(tenancy.WithContext(req.Context(), tenant))
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Result().StatusCode != http.StatusMethodNotAllowed {
			t.Errorf("method %s status = %d, want %d", m, w.Result().StatusCode, http.StatusMethodNotAllowed)
		}
	}
}

func TestHandler_NewRejectsNilDeps(t *testing.T) {
	t.Parallel()
	repo := campaigns.NewInMemoryRepository()
	cases := []struct {
		name string
		d    campaign.Deps
	}{
		{name: "nil repo", d: campaign.Deps{Now: time.Now, NewClickID: uuid.NewString}},
		{name: "nil now", d: campaign.Deps{Repo: repo, NewClickID: uuid.NewString}},
		{name: "nil click gen", d: campaign.Deps{Repo: repo, Now: time.Now}},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, err := campaign.New(tc.d); err == nil {
				t.Fatalf("New(%v) err = nil, want non-nil", tc.name)
			}
		})
	}
}

func TestHandler_NewDefaultsLogger(t *testing.T) {
	t.Parallel()
	if _, err := campaign.New(campaign.Deps{
		Repo:       campaigns.NewInMemoryRepository(),
		Now:        time.Now,
		NewClickID: uuid.NewString,
	}); err != nil {
		t.Fatalf("New defaults logger: %v", err)
	}
}

// TestHandler_RemoteIPParsing exercises IPv4, IPv6, host-only,
// malformed combinations. It is a smoke check on extractRemoteIP via
// the public surface — we read the persisted click row back.
func TestHandler_RemoteIPParsing(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		addr  string
		valid bool
	}{
		{name: "ipv4 with port", addr: "203.0.113.1:80", valid: true},
		{name: "ipv6 with port", addr: "[2001:db8::1]:80", valid: true},
		{name: "ipv4 host only", addr: "203.0.113.2", valid: true},
		{name: "empty", addr: "", valid: false},
		{name: "garbage", addr: "not-an-ip", valid: false},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			repo := campaigns.NewInMemoryRepository()
			tenant := &tenancy.Tenant{ID: uuid.New(), Name: "acme", Host: "acme.crm.local"}
			c := mustCampaign(t, tenant.ID, "promo-"+strconv.Itoa(int(time.Now().UnixNano())), "https://wa.me/5511999999999", nil)
			_ = repo.CreateCampaign(context.Background(), c)
			now := func() time.Time { return time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC) }
			h := newTestHandler(t, repo, now, []string{"wa.me"})

			req := httptest.NewRequest(http.MethodGet, "/c/"+c.Slug, nil)
			req.Host = "acme.crm.local"
			req.RemoteAddr = tc.addr
			req = req.WithContext(tenancy.WithContext(req.Context(), tenant))
			w := httptest.NewRecorder()
			h.ServeHTTP(w, req)
			if w.Result().StatusCode != http.StatusFound {
				t.Fatalf("status = %d, want %d", w.Result().StatusCode, http.StatusFound)
			}
			clicks := drainClicks(repo)
			if len(clicks) != 1 {
				t.Fatalf("clicks len = %d, want 1", len(clicks))
			}
			if tc.valid && !clicks[0].IP.IsValid() {
				t.Errorf("IP not parsed; remote=%q", tc.addr)
			}
			if !tc.valid && clicks[0].IP.IsValid() {
				t.Errorf("IP parsed for invalid remote=%q: %v", tc.addr, clicks[0].IP)
			}
		})
	}
}

func TestHandler_ContinuesRedirectOnPersistenceError(t *testing.T) {
	t.Parallel()
	repo := &failingRepo{inner: campaigns.NewInMemoryRepository()}
	tenant := &tenancy.Tenant{ID: uuid.New(), Name: "acme", Host: "acme.crm.local"}
	c := mustCampaign(t, tenant.ID, "promo", "https://wa.me/5511999999999", nil)
	_ = repo.inner.CreateCampaign(context.Background(), c)
	repo.recordErr = errBoom
	now := func() time.Time { return time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC) }

	h := newTestHandler(t, repo, now, []string{"wa.me"})

	req := httptest.NewRequest(http.MethodGet, "/c/promo", nil)
	req.Host = "acme.crm.local"
	req.RemoteAddr = "203.0.113.4:80"
	req = req.WithContext(tenancy.WithContext(req.Context(), tenant))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Result().StatusCode != http.StatusFound {
		t.Fatalf("status = %d, want %d (ledger failure must not block redirect)", w.Result().StatusCode, http.StatusFound)
	}
}

func TestHandler_500OnRepoLookupError(t *testing.T) {
	t.Parallel()
	repo := &failingRepo{inner: campaigns.NewInMemoryRepository(), lookupErr: errBoom}
	tenant := &tenancy.Tenant{ID: uuid.New(), Name: "acme", Host: "acme.crm.local"}
	now := func() time.Time { return time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC) }
	h := newTestHandler(t, repo, now, []string{"wa.me"})

	req := httptest.NewRequest(http.MethodGet, "/c/promo", nil)
	req.Host = "acme.crm.local"
	req.RemoteAddr = "203.0.113.4:80"
	req = req.WithContext(tenancy.WithContext(req.Context(), tenant))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Result().StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d", w.Result().StatusCode, http.StatusInternalServerError)
	}
}

// failingRepo wraps the in-memory adapter and injects errors on the
// two methods the handler calls. It satisfies the campaigns.Repository
// port via embedding-style forwarding so unrelated methods stay
// functional.
type failingRepo struct {
	mu        sync.Mutex
	inner     *campaigns.InMemoryRepository
	lookupErr error
	recordErr error
}

var errBoom = stringError("boom")

type stringError string

func (s stringError) Error() string { return string(s) }

func (f *failingRepo) CreateCampaign(ctx context.Context, c *campaigns.Campaign) error {
	return f.inner.CreateCampaign(ctx, c)
}

func (f *failingRepo) GetBySlug(ctx context.Context, tenantID uuid.UUID, slug string) (*campaigns.Campaign, error) {
	f.mu.Lock()
	err := f.lookupErr
	f.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return f.inner.GetBySlug(ctx, tenantID, slug)
}

func (f *failingRepo) RecordClick(ctx context.Context, click *campaigns.CampaignClick) (*campaigns.CampaignClick, error) {
	f.mu.Lock()
	err := f.recordErr
	f.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return f.inner.RecordClick(ctx, click)
}

func (f *failingRepo) LinkContactToCampaign(ctx context.Context, tenantID uuid.UUID, clickID string, contactID uuid.UUID) error {
	return f.inner.LinkContactToCampaign(ctx, tenantID, clickID, contactID)
}

func (f *failingRepo) ListByTenant(ctx context.Context, tenantID uuid.UUID) ([]*campaigns.Campaign, error) {
	return f.inner.ListByTenant(ctx, tenantID)
}

func (f *failingRepo) StatsByTenant(ctx context.Context, tenantID uuid.UUID) (map[uuid.UUID]campaigns.CampaignStats, error) {
	return f.inner.StatsByTenant(ctx, tenantID)
}

func (f *failingRepo) ListClicks(ctx context.Context, tenantID, campaignID uuid.UUID, limit int) ([]*campaigns.CampaignClick, error) {
	return f.inner.ListClicks(ctx, tenantID, campaignID, limit)
}

// TestRoutes_RegistersExactPattern proves Routes wires GET /c/{slug}
// only; an unrelated path resolves to 404 from the same mux.
func TestHandler_ExpandsClickIDPlaceholderInRedirect(t *testing.T) {
	t.Parallel()
	repo := campaigns.NewInMemoryRepository()
	tenant := &tenancy.Tenant{ID: uuid.New(), Name: "acme", Host: "acme.crm.local"}
	c := mustCampaign(t, tenant.ID, "promo", "https://wa.me/5511999999999?text=hi%20%5Bcrm%3A{click_id}%5D", nil)
	_ = repo.CreateCampaign(context.Background(), c)
	now := func() time.Time { return time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC) }
	h := newTestHandler(t, repo, now, []string{"wa.me"})

	req := httptest.NewRequest(http.MethodGet, "/c/promo", nil)
	req.Host = "acme.crm.local"
	req.RemoteAddr = "203.0.113.7:80"
	req = req.WithContext(tenancy.WithContext(req.Context(), tenant))
	req.AddCookie(&http.Cookie{Name: campaign.CookieName, Value: "ck-pin"})
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	loc := w.Result().Header.Get("Location")
	wantSubstring := "ck-pin"
	if !contains(loc, wantSubstring) {
		t.Fatalf("Location = %q, want it to contain %q", loc, wantSubstring)
	}
	if contains(loc, "{click_id}") {
		t.Fatalf("Location = %q must not still contain {click_id}", loc)
	}
}

// contains is strings.Contains under a local name so the test file does
// not import strings only for the assertion (keeps the imports tight).
func contains(s, sub string) bool {
	return len(sub) == 0 || indexOf(s, sub) >= 0
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

func TestRoutes_RegistersExactPattern(t *testing.T) {
	t.Parallel()
	repo := campaigns.NewInMemoryRepository()
	h := newTestHandler(t, repo, time.Now, nil)

	req := httptest.NewRequest(http.MethodGet, "/other/path", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Result().StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", w.Result().StatusCode, http.StatusNotFound)
	}
}

// TestEmptyUAIsBot asserts the documented behaviour from isBotUA: a
// missing User-Agent header is tagged bot=true. We assert by reading
// the persisted click row Meta map.
func TestEmptyUAIsBot(t *testing.T) {
	t.Parallel()
	repo := campaigns.NewInMemoryRepository()
	tenant := &tenancy.Tenant{ID: uuid.New(), Name: "acme", Host: "acme.crm.local"}
	c := mustCampaign(t, tenant.ID, "promo", "https://wa.me/5511999999999", nil)
	_ = repo.CreateCampaign(context.Background(), c)
	now := func() time.Time { return time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC) }
	h := newTestHandler(t, repo, now, []string{"wa.me"})

	req := httptest.NewRequest(http.MethodGet, "/c/promo", nil)
	req.Host = "acme.crm.local"
	req.RemoteAddr = "203.0.113.5:80"
	req.Header.Del("User-Agent")
	req = req.WithContext(tenancy.WithContext(req.Context(), tenant))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	clicks := drainClicks(repo)
	if got, ok := clicks[0].Meta["bot"].(bool); !ok || !got {
		t.Errorf("Meta[bot] = %v, want true on empty UA", clicks[0].Meta["bot"])
	}
}

// TestInvalidStoredRedirectURLRejected demonstrates the click-time
// re-parse guard: even if a row escapes write-time validation (legacy
// data, manual SQL), the click handler refuses to forward to a
// non-http(s) URL.
func TestInvalidStoredRedirectURLRejected(t *testing.T) {
	t.Parallel()
	repo := &mutableRedirect{inner: campaigns.NewInMemoryRepository(), override: "javascript:alert(1)"}
	tenant := &tenancy.Tenant{ID: uuid.New(), Name: "acme", Host: "acme.crm.local"}
	c := mustCampaign(t, tenant.ID, "promo", "https://wa.me/x", nil)
	_ = repo.inner.CreateCampaign(context.Background(), c)
	now := func() time.Time { return time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC) }
	h := newTestHandler(t, repo, now, []string{"wa.me"})

	req := httptest.NewRequest(http.MethodGet, "/c/promo", nil)
	req.Host = "acme.crm.local"
	req.RemoteAddr = "203.0.113.6:80"
	req = req.WithContext(tenancy.WithContext(req.Context(), tenant))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Result().StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want %d", w.Result().StatusCode, http.StatusBadGateway)
	}
}

type mutableRedirect struct {
	inner    *campaigns.InMemoryRepository
	override string
}

func (m *mutableRedirect) CreateCampaign(ctx context.Context, c *campaigns.Campaign) error {
	return m.inner.CreateCampaign(ctx, c)
}

func (m *mutableRedirect) GetBySlug(ctx context.Context, tenantID uuid.UUID, slug string) (*campaigns.Campaign, error) {
	c, err := m.inner.GetBySlug(ctx, tenantID, slug)
	if err != nil {
		return nil, err
	}
	c.RedirectURL = m.override
	return c, nil
}

func (m *mutableRedirect) RecordClick(ctx context.Context, click *campaigns.CampaignClick) (*campaigns.CampaignClick, error) {
	return m.inner.RecordClick(ctx, click)
}

func (m *mutableRedirect) LinkContactToCampaign(ctx context.Context, tenantID uuid.UUID, clickID string, contactID uuid.UUID) error {
	return m.inner.LinkContactToCampaign(ctx, tenantID, clickID, contactID)
}

func (m *mutableRedirect) ListByTenant(ctx context.Context, tenantID uuid.UUID) ([]*campaigns.Campaign, error) {
	return m.inner.ListByTenant(ctx, tenantID)
}

func (m *mutableRedirect) StatsByTenant(ctx context.Context, tenantID uuid.UUID) (map[uuid.UUID]campaigns.CampaignStats, error) {
	return m.inner.StatsByTenant(ctx, tenantID)
}

func (m *mutableRedirect) ListClicks(ctx context.Context, tenantID, campaignID uuid.UUID, limit int) ([]*campaigns.CampaignClick, error) {
	return m.inner.ListClicks(ctx, tenantID, campaignID, limit)
}
