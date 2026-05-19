package main

// SIN-63101 — wire tests pin three guarantees of buildBrandingStack:
//
//   1. The handler and the theme middleware share the same in-memory
//      PaletteStore. A SIN-63084 save through the admin handler MUST
//      be visible to the next theme middleware lookup for the same
//      tenant — without TTL wait — because Invalidate is fired off the
//      writer path inside webbranding.Handler.
//   2. The stack tolerates a nil logger and a nil metrics
//      implementation so cmd/server boot paths that don't wire either
//      still produce a working bundle.
//   3. The cleanup is a no-op (matches the in-memory store's lack of
//      external clients) — the slot stays for orthogonality with the
//      other web/* wires.

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/adapter/httpapi/middleware"
	"github.com/pericles-luz/crm/internal/branding"
	"github.com/pericles-luz/crm/internal/iam"
	"github.com/pericles-luz/crm/internal/tenancy"
)

func quietLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(&bytes.Buffer{}, nil))
}

func TestBuildBrandingStack_ReturnsNonNilHandlerAndTheme(t *testing.T) {
	t.Parallel()
	s := buildBrandingStack(quietLogger(), nil)
	if s.Handler == nil {
		t.Fatal("Handler is nil — expected the SIN-63084 admin mux")
	}
	if s.Theme == nil {
		t.Fatal("Theme is nil — expected the SIN-63085 middleware bundled with the handler")
	}
	if s.Cleanup == nil {
		t.Fatal("Cleanup is nil — expected a callable no-op for orthogonality")
	}
	s.Cleanup()
}

func TestBuildBrandingStack_NilLoggerIsSafe(t *testing.T) {
	t.Parallel()
	s := buildBrandingStack(nil, nil)
	if s.Handler == nil || s.Theme == nil {
		t.Fatal("nil logger must still produce a working stack")
	}
}

// TestBuildBrandingStack_HandlerAndThemeShareStore drives the end-to-end
// AC #1 + AC #4 contract through the wire output: a palette persisted
// via the admin handler's POST /branding/palette/save is visible on the
// next theme-middleware lookup for the same tenant, and the new style
// reflects the saved colours (because Invalidate fires off the writer
// path inside webbranding.Handler).
func TestBuildBrandingStack_HandlerAndThemeShareStore(t *testing.T) {
	t.Parallel()
	s := buildBrandingStack(quietLogger(), nil)
	t.Cleanup(s.Cleanup)

	tenantID := uuid.New()
	tenantCtx := tenancy.WithContext(context.Background(), &tenancy.Tenant{
		ID:   tenantID,
		Name: "acme",
		Host: "acme.crm.local",
	})

	// Baseline: lookup with no persisted palette → DefaultThemeStyle.
	baseline := readThemeStyle(t, s.Theme, tenantCtx)
	if string(baseline) != string(branding.DefaultThemeStyle) {
		t.Fatalf("baseline style = %q, want DefaultThemeStyle", baseline)
	}

	// Save a palette via the admin handler's POST /branding/palette/save.
	// The handler is mounted on a stdlib mux returned by Routes, so the
	// httptest call exercises the exact production code path.
	form := strings.NewReader(
		"primary=%23112233&secondary=%23445566&accent=%23778899" +
			"&foreground=%23000000&background=%23ffffff&text_on_primary=%23ffffff",
	)
	req := httptest.NewRequest(http.MethodPost, "/branding/palette/save", form)
	req = req.WithContext(saveContext(tenantCtx))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.Handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /branding/palette/save status = %d, body=%q", rec.Code, rec.Body.String())
	}

	// After save, the theme middleware MUST observe the new palette on
	// the next lookup. The handler's Invalidate call evicts any cached
	// entry, so the next lookup re-reads the shared store.
	got := strings.ToLower(string(readThemeStyle(t, s.Theme, tenantCtx)))
	if !strings.Contains(got, "#112233") {
		t.Fatalf("post-save style = %q, want primary #112233 (handler + middleware must share store)", got)
	}
}

// readThemeStyle drives one request through the theme middleware's
// Handler and returns the branding.ThemeStyleFromContext value seen by
// the inner handler. Exposes the per-request inline style without
// re-implementing the lookup branches the middleware test already
// covers.
func readThemeStyle(t *testing.T, mw *middleware.ThemeMiddleware, ctx context.Context) string {
	t.Helper()
	var captured string
	inner := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		captured = string(branding.ThemeStyleFromContext(r.Context()))
	})
	handler := mw.Handler(inner)
	req := httptest.NewRequest(http.MethodGet, "/branding", nil).WithContext(ctx)
	handler.ServeHTTP(httptest.NewRecorder(), req)
	return captured
}

// saveContext layers the CSRF token + session shape webbranding.save
// requires onto the tenant context. The handler reads the token via
// the wire's csrfTokenFromSessionContext closure; the helper here mints
// a matching session payload.
func saveContext(ctx context.Context) context.Context {
	sess := iam.Session{
		ID:        uuid.New(),
		UserID:    uuid.New(),
		TenantID:  uuid.New(),
		ExpiresAt: time.Now().Add(time.Hour),
		CSRFToken: "csrf-token-test",
	}
	return middleware.WithSession(ctx, sess)
}
