package middleware_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/adapter/httpapi/middleware"
	"github.com/pericles-luz/crm/internal/iam"
	"github.com/pericles-luz/crm/internal/tenancy"
)

type fakeValidator struct {
	want    uuid.UUID
	tenant  uuid.UUID
	session iam.Session
	err     error
	calls   int
}

func (f *fakeValidator) ValidateSession(_ context.Context, tenantID, sessionID uuid.UUID) (iam.Session, error) {
	f.calls++
	if f.err != nil {
		return iam.Session{}, f.err
	}
	if tenantID != f.tenant || sessionID != f.want {
		return iam.Session{}, iam.ErrSessionNotFound
	}
	return f.session, nil
}

func okHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
}

func newRequestWithTenant(t *testing.T, target string, tenantID uuid.UUID) *http.Request {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, target, nil)
	ctx := tenancy.WithContext(r.Context(), &tenancy.Tenant{
		ID:   tenantID,
		Name: "acme",
		Host: "acme.crm.local",
	})
	return r.WithContext(ctx)
}

func TestAuth_PanicsOnNilValidator(t *testing.T) {
	t.Parallel()
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic on nil validator")
		}
	}()
	middleware.Auth(nil)
}

func TestAuth_RedirectsWhenTenantMissingReturns500(t *testing.T) {
	t.Parallel()
	v := &fakeValidator{}
	h := middleware.Auth(v)(okHandler())

	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/hello-tenant", nil)
	h.ServeHTTP(rec, r)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d, want 500", rec.Code)
	}
	if v.calls != 0 {
		t.Fatalf("validator called %d times, want 0", v.calls)
	}
}

func TestAuth_RedirectsWhenCookieMissing(t *testing.T) {
	t.Parallel()
	v := &fakeValidator{}
	h := middleware.Auth(v)(okHandler())

	rec := httptest.NewRecorder()
	tenantID := uuid.New()
	h.ServeHTTP(rec, newRequestWithTenant(t, "/hello-tenant?x=1", tenantID))

	if rec.Code != http.StatusFound {
		t.Fatalf("status=%d, want 302", rec.Code)
	}
	loc := rec.Header().Get("Location")
	if !strings.HasPrefix(loc, "/login?next=") {
		t.Fatalf("Location=%q does not start with /login?next=", loc)
	}
	if !strings.Contains(loc, "%2Fhello-tenant%3Fx%3D1") {
		t.Fatalf("Location=%q does not encode the original URI", loc)
	}
	if v.calls != 0 {
		t.Fatalf("validator called %d times, want 0", v.calls)
	}
}

func TestAuth_RedirectsWhenCookieValueEmpty(t *testing.T) {
	t.Parallel()
	v := &fakeValidator{}
	h := middleware.Auth(v)(okHandler())

	rec := httptest.NewRecorder()
	r := newRequestWithTenant(t, "/hello-tenant", uuid.New())
	r.AddCookie(&http.Cookie{Name: middleware.SessionCookieName, Value: ""})
	h.ServeHTTP(rec, r)

	if rec.Code != http.StatusFound {
		t.Fatalf("status=%d, want 302", rec.Code)
	}
}

func TestAuth_RedirectsWhenCookieIsNotUUID(t *testing.T) {
	t.Parallel()
	v := &fakeValidator{}
	h := middleware.Auth(v)(okHandler())

	rec := httptest.NewRecorder()
	r := newRequestWithTenant(t, "/hello-tenant", uuid.New())
	r.AddCookie(&http.Cookie{Name: middleware.SessionCookieName, Value: "not-a-uuid"})
	h.ServeHTTP(rec, r)

	if rec.Code != http.StatusFound {
		t.Fatalf("status=%d, want 302", rec.Code)
	}
}

func TestAuth_RedirectsOnSessionNotFound(t *testing.T) {
	t.Parallel()
	v := &fakeValidator{err: iam.ErrSessionNotFound}
	h := middleware.Auth(v)(okHandler())

	rec := httptest.NewRecorder()
	r := newRequestWithTenant(t, "/hello-tenant", uuid.New())
	r.AddCookie(&http.Cookie{Name: middleware.SessionCookieName, Value: uuid.New().String()})
	h.ServeHTTP(rec, r)

	if rec.Code != http.StatusFound {
		t.Fatalf("status=%d, want 302", rec.Code)
	}
}

func TestAuth_RedirectsOnSessionExpired(t *testing.T) {
	t.Parallel()
	v := &fakeValidator{err: iam.ErrSessionExpired}
	h := middleware.Auth(v)(okHandler())

	rec := httptest.NewRecorder()
	r := newRequestWithTenant(t, "/hello-tenant", uuid.New())
	r.AddCookie(&http.Cookie{Name: middleware.SessionCookieName, Value: uuid.New().String()})
	h.ServeHTTP(rec, r)

	if rec.Code != http.StatusFound {
		t.Fatalf("status=%d, want 302", rec.Code)
	}
}

func TestAuth_500OnInfraError(t *testing.T) {
	t.Parallel()
	v := &fakeValidator{err: errors.New("db down")}
	h := middleware.Auth(v)(okHandler())

	rec := httptest.NewRecorder()
	r := newRequestWithTenant(t, "/hello-tenant", uuid.New())
	r.AddCookie(&http.Cookie{Name: middleware.SessionCookieName, Value: uuid.New().String()})
	h.ServeHTTP(rec, r)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d, want 500", rec.Code)
	}
}

func TestAuth_PassesThroughWithSessionInjected(t *testing.T) {
	t.Parallel()
	tenantID := uuid.New()
	userID := uuid.New()
	sid := uuid.New()
	v := &fakeValidator{
		want:   sid,
		tenant: tenantID,
		session: iam.Session{
			ID:        sid,
			UserID:    userID,
			TenantID:  tenantID,
			ExpiresAt: time.Now().Add(time.Hour),
		},
	}
	var observed iam.Session
	var ok bool
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		observed, ok = middleware.SessionFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	})
	h := middleware.Auth(v)(inner)

	rec := httptest.NewRecorder()
	r := newRequestWithTenant(t, "/hello-tenant", tenantID)
	r.AddCookie(&http.Cookie{Name: middleware.SessionCookieName, Value: sid.String()})
	h.ServeHTTP(rec, r)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200", rec.Code)
	}
	if !ok {
		t.Fatal("session not injected into context")
	}
	if observed.UserID != userID {
		t.Fatalf("session.UserID=%v, want %v", observed.UserID, userID)
	}
}

func TestSessionFromContext_ReturnsFalseWhenAbsent(t *testing.T) {
	t.Parallel()
	if _, ok := middleware.SessionFromContext(context.Background()); ok {
		t.Fatal("expected !ok on empty context")
	}
}
