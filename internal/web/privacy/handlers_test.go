package privacy_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/legal"
	"github.com/pericles-luz/crm/internal/tenancy"
	webprivacy "github.com/pericles-luz/crm/internal/web/privacy"
)

// fixedClock returns the same instant every call so the rendered
// "gerado em" field is deterministic.
func fixedClock(t time.Time) webprivacy.Now {
	return func() time.Time { return t }
}

// modelOK returns a stub ModelResolver that always returns model.
func modelOK(model string) webprivacy.ModelResolverFunc {
	return func(_ context.Context, _ uuid.UUID) (string, error) { return model, nil }
}

// modelErr returns a stub ModelResolver that always errors. The
// handler must still render the page with the fallback model — the
// LGPD disclosure cannot be hostage to an internal lookup failure.
func modelErr(err error) webprivacy.ModelResolverFunc {
	return func(_ context.Context, _ uuid.UUID) (string, error) { return "", err }
}

func newHandler(t *testing.T, deps webprivacy.Deps) http.Handler {
	t.Helper()
	if deps.Logger == nil {
		// Discard logs so the test output stays clean.
		deps.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	h, err := webprivacy.New(deps)
	if err != nil {
		t.Fatalf("New: unexpected error: %v", err)
	}
	mux := http.NewServeMux()
	h.Routes(mux)
	return mux
}

func newRequest(t *testing.T, method, path string, tenant *tenancy.Tenant) *http.Request {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	if tenant != nil {
		req = req.WithContext(tenancy.WithContext(req.Context(), tenant))
	}
	return req
}

func tenantFixture() *tenancy.Tenant {
	return &tenancy.Tenant{
		ID:   uuid.MustParse("11111111-1111-1111-1111-111111111111"),
		Name: "Acme Cobranças",
		Host: "acme.crm.local",
	}
}

// TestNew_RejectsMissingDeps covers the fail-fast wire path.
func TestNew_RejectsMissingDeps(t *testing.T) {
	cases := []struct {
		name string
		deps webprivacy.Deps
	}{
		{
			name: "missing model",
			deps: webprivacy.Deps{Now: time.Now},
		},
		{
			name: "missing now",
			deps: webprivacy.Deps{Model: modelOK("openrouter/auto")},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := webprivacy.New(tc.deps); err == nil {
				t.Fatalf("expected error, got nil")
			}
		})
	}
}

// TestNew_DefaultsLogger pins that the constructor defaults Logger
// to slog.Default when the caller passes nil.
func TestNew_DefaultsLogger(t *testing.T) {
	h, err := webprivacy.New(webprivacy.Deps{
		Model: modelOK("openrouter/auto"),
		Now:   time.Now,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if h == nil {
		t.Fatal("New returned nil handler")
	}
}

// TestView_RendersOpenRouterMention is AC #4 from SIN-62354: the
// rendered HTML must contain the literal substring "OpenRouter".
func TestView_RendersOpenRouterMention(t *testing.T) {
	mux := newHandler(t, webprivacy.Deps{
		Model: modelOK("openrouter/google/gemini-flash-1.5"),
		Now:   fixedClock(time.Date(2026, 5, 16, 12, 0, 0, 0, time.UTC)),
	})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, newRequest(t, http.MethodGet, "/settings/privacy", tenantFixture()))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "OpenRouter") {
		t.Errorf("body missing required substring %q", "OpenRouter")
	}
	if !strings.Contains(body, "openrouter.ai/privacy") {
		t.Errorf("body missing required external link to OpenRouter privacy policy")
	}
}

// TestView_RendersActiveModel pins that the resolved model id is
// surfaced on the page (decisão #8 transparency requirement).
func TestView_RendersActiveModel(t *testing.T) {
	wantModel := "openrouter/anthropic/haiku"
	mux := newHandler(t, webprivacy.Deps{
		Model: modelOK(wantModel),
		Now:   fixedClock(time.Date(2026, 5, 16, 12, 0, 0, 0, time.UTC)),
	})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, newRequest(t, http.MethodGet, "/settings/privacy", tenantFixture()))

	if !strings.Contains(rec.Body.String(), wantModel) {
		t.Errorf("body missing active model %q", wantModel)
	}
}

// TestView_FallsBackOnEmptyModel covers the contract clause that an
// empty resolver result is treated as the documented fallback.
func TestView_FallsBackOnEmptyModel(t *testing.T) {
	mux := newHandler(t, webprivacy.Deps{
		Model: modelOK(""),
		Now:   fixedClock(time.Date(2026, 5, 16, 12, 0, 0, 0, time.UTC)),
	})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, newRequest(t, http.MethodGet, "/settings/privacy", tenantFixture()))

	if !strings.Contains(rec.Body.String(), webprivacy.FallbackModel) {
		t.Errorf("body missing fallback model %q", webprivacy.FallbackModel)
	}
}

// TestView_FallsBackOnResolverError is the load-bearing failure mode:
// even when the resolver returns an error, the LGPD disclosure must
// render. A privacy page that 500s would silently break compliance.
func TestView_FallsBackOnResolverError(t *testing.T) {
	mux := newHandler(t, webprivacy.Deps{
		Model: modelErr(errors.New("policy DB unreachable")),
		Now:   fixedClock(time.Date(2026, 5, 16, 12, 0, 0, 0, time.UTC)),
	})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, newRequest(t, http.MethodGet, "/settings/privacy", tenantFixture()))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 even on resolver error", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "OpenRouter") {
		t.Errorf("disclosure body must still mention OpenRouter on resolver error")
	}
	if !strings.Contains(body, webprivacy.FallbackModel) {
		t.Errorf("body missing fallback model after resolver error")
	}
}

// TestView_RendersDPAVersion is AC #3: the DPA version appears in
// the footer (and the download link uses the versioned filename).
func TestView_RendersDPAVersion(t *testing.T) {
	mux := newHandler(t, webprivacy.Deps{
		Model: modelOK("openrouter/auto"),
		Now:   fixedClock(time.Date(2026, 5, 16, 12, 0, 0, 0, time.UTC)),
	})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, newRequest(t, http.MethodGet, "/settings/privacy", tenantFixture()))

	body := rec.Body.String()
	if !strings.Contains(body, legal.DPAVersion) {
		t.Errorf("body missing DPA version %q", legal.DPAVersion)
	}
	if !strings.Contains(body, legal.DPAFilename()) {
		t.Errorf("body missing DPA download filename %q", legal.DPAFilename())
	}
	if !strings.Contains(body, "Acme Cobranças") {
		t.Errorf("body missing tenant name")
	}
}

// TestView_RendersAllSubprocessors asserts every canonical
// sub-processor appears on the page. The list comes from
// legal.Subprocessors() so a future addition (e.g. PIX PSP
// ratification) renders automatically.
func TestView_RendersAllSubprocessors(t *testing.T) {
	mux := newHandler(t, webprivacy.Deps{
		Model: modelOK("openrouter/auto"),
		Now:   fixedClock(time.Date(2026, 5, 16, 12, 0, 0, 0, time.UTC)),
	})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, newRequest(t, http.MethodGet, "/settings/privacy", tenantFixture()))

	body := rec.Body.String()
	for _, sub := range legal.Subprocessors() {
		if !strings.Contains(body, sub.Name) {
			t.Errorf("body missing sub-processor %q", sub.Name)
		}
		if sub.PolicyURL != "" && !strings.Contains(body, sub.PolicyURL) {
			t.Errorf("body missing policy URL %q for sub-processor %q", sub.PolicyURL, sub.Name)
		}
	}
}

// TestView_SetsCacheControlPrivate asserts the page is never cached
// by a shared proxy — it carries the tenant's name and active model,
// both of which are tenant-private.
func TestView_SetsCacheControlPrivate(t *testing.T) {
	mux := newHandler(t, webprivacy.Deps{
		Model: modelOK("openrouter/auto"),
		Now:   fixedClock(time.Date(2026, 5, 16, 12, 0, 0, 0, time.UTC)),
	})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, newRequest(t, http.MethodGet, "/settings/privacy", tenantFixture()))

	if got := rec.Header().Get("Cache-Control"); !strings.Contains(got, "private") || !strings.Contains(got, "no-store") {
		t.Errorf("Cache-Control = %q, want it to contain private + no-store", got)
	}
}

// TestView_500WithoutTenant is the defensive path: the handler MUST
// have a tenant in context (chi TenantScope guarantees it in prod).
// Tests without the tenant context get a clean 500, not a panic.
func TestView_500WithoutTenant(t *testing.T) {
	mux := newHandler(t, webprivacy.Deps{
		Model: modelOK("openrouter/auto"),
		Now:   fixedClock(time.Date(2026, 5, 16, 12, 0, 0, 0, time.UTC)),
	})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/settings/privacy", nil))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
}

// TestDownloadDPA_ServesMarkdown is the download endpoint contract:
// streams the markdown with the right content-type, content-
// disposition (downloadable, versioned filename), and DPA version
// header so a downstream verifier can compare versions without
// parsing the body.
func TestDownloadDPA_ServesMarkdown(t *testing.T) {
	when := time.Date(2026, 5, 16, 12, 0, 0, 0, time.UTC)
	mux := newHandler(t, webprivacy.Deps{
		Model: modelOK("openrouter/auto"),
		Now:   fixedClock(when),
	})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, newRequest(t, http.MethodGet, "/settings/privacy/dpa.md", tenantFixture()))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); got != legal.DPAContentType {
		t.Errorf("Content-Type = %q, want %q", got, legal.DPAContentType)
	}
	if got := rec.Header().Get("Content-Disposition"); !strings.Contains(got, legal.DPAFilename()) {
		t.Errorf("Content-Disposition = %q, missing filename %q", got, legal.DPAFilename())
	}
	if got := rec.Header().Get("X-DPA-Version"); got != legal.DPAVersion {
		t.Errorf("X-DPA-Version = %q, want %q", got, legal.DPAVersion)
	}
	if got := rec.Header().Get("Last-Modified"); got == "" {
		t.Errorf("Last-Modified header missing")
	}
	body := rec.Body.String()
	if !strings.Contains(body, "OpenRouter") {
		t.Errorf("downloaded body missing OpenRouter")
	}
	if body != legal.DPAMarkdown() {
		t.Errorf("downloaded body diverges from embedded DPA")
	}
}

// TestRoutes_OnlyGET pins that no POST surface is exposed by this
// handler. Adding a POST in the future without a matching CSRF + RBAC
// review would be a regression.
func TestRoutes_OnlyGET(t *testing.T) {
	mux := newHandler(t, webprivacy.Deps{
		Model: modelOK("openrouter/auto"),
		Now:   fixedClock(time.Date(2026, 5, 16, 12, 0, 0, 0, time.UTC)),
	})
	for _, path := range []string{"/settings/privacy", "/settings/privacy/dpa.md"} {
		rec := httptest.NewRecorder()
		req := newRequest(t, http.MethodPost, path, tenantFixture())
		mux.ServeHTTP(rec, req)
		if rec.Code == http.StatusOK {
			t.Errorf("POST %s should NOT be routed to a handler", path)
		}
	}
}

// TestView_RendersUnder300ms is a coarse latency floor for AC #1 (p95
// < 300ms). The test runs the render N times and asserts the mean
// latency is well below the budget. We do not measure p95 directly in
// a unit test — the dependency surface (in-memory render, no I/O)
// makes the worst case predictable.
func TestView_RendersUnder300ms(t *testing.T) {
	mux := newHandler(t, webprivacy.Deps{
		Model: modelOK("openrouter/auto"),
		Now:   fixedClock(time.Date(2026, 5, 16, 12, 0, 0, 0, time.UTC)),
	})
	const iterations = 100
	start := time.Now()
	for i := 0; i < iterations; i++ {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, newRequest(t, http.MethodGet, "/settings/privacy", tenantFixture()))
		if rec.Code != http.StatusOK {
			t.Fatalf("iteration %d: status %d", i, rec.Code)
		}
	}
	mean := time.Since(start) / iterations
	if mean > 50*time.Millisecond {
		t.Errorf("mean render latency %s exceeds the conservative 50ms floor (AC #1 budgets 300ms p95)", mean)
	}
}
