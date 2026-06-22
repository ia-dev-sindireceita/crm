package usermfa

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/adapter/httpapi/sessioncookie"
	"github.com/pericles-luz/crm/internal/iam"
	"github.com/pericles-luz/crm/internal/iam/mfa"
	"github.com/pericles-luz/crm/internal/tenancy"
)

// stubTenantSession is a fake TenantSessionResolver for the Setup
// dual-predicate tests.
type stubTenantSession struct {
	actor TenantActor
	ok    bool
}

func (s stubTenantSession) ResolveTenantSession(_ *http.Request) (TenantActor, bool) {
	return s.actor, s.ok
}

// countingEnroller records how many times Enroll was invoked so the
// SIN-65587 anti-rotation guard can be proven: an already-enrolled
// full-session user MUST NOT trigger Enroll (which rotates the seed).
type countingEnroller struct {
	result mfa.EnrollResult
	calls  int
}

func (c *countingEnroller) Enroll(_ context.Context, _ uuid.UUID, _ string) (mfa.EnrollResult, error) {
	c.calls++
	return c.result, nil
}

// TestSetupDualPredicate is the table-driven matrix from SIN-65587: the
// four Setup access branches plus the already-enrolled anti-rotation
// guard.
func TestSetupDualPredicate(t *testing.T) {
	t.Parallel()

	actor := TenantActor{UserID: uuid.New(), TenantID: uuid.New()}
	sampleResult := mfa.EnrollResult{
		OTPAuthURI:    "otpauth://totp/Sindireceita:agent@acme.test?secret=ABC",
		SecretEncoded: "ABCDEFGHJKLMNPQRSTUVWXYZ234567",
		RecoveryCodes: []string{"AAAAAAAAAA", "BBBBBBBBBB"},
	}

	tests := []struct {
		name string
		// setup mutates deps + cfg and returns the request to run.
		build func(t *testing.T, deps *testDeps, cfg *HandlerConfig, enr *countingEnroller) *http.Request
		// assertions
		wantStatus    int
		wantBodyHas   []string
		wantEnrolls   int
		wantAudits    int
		wantLocation  string
		wantNoSetCook bool
	}{
		{
			name: "full session, NOT enrolled -> enrolment UI (AC1)",
			build: func(t *testing.T, deps *testDeps, cfg *HandlerConfig, enr *countingEnroller) *http.Request {
				deps.enrollment.mark(actor.UserID, false)
				deps.labels.set(actor.UserID, "agent@acme.test")
				enr.result = sampleResult
				cfg.TenantSession = stubTenantSession{actor: actor, ok: true}
				return httptest.NewRequest(http.MethodGet, "/admin/2fa/setup", nil)
			},
			wantStatus:  http.StatusOK,
			wantBodyHas: []string{"ABCDEFGHJKLMNPQRSTUVWXYZ234567", "otpauth://totp", "AAAAA-AAAAA"},
			wantEnrolls: 1,
			wantAudits:  0,
		},
		{
			name: "full session, ALREADY enrolled -> no rotation (AC2)",
			build: func(t *testing.T, deps *testDeps, cfg *HandlerConfig, enr *countingEnroller) *http.Request {
				deps.enrollment.mark(actor.UserID, true)
				cfg.TenantSession = stubTenantSession{actor: actor, ok: true}
				return httptest.NewRequest(http.MethodGet, "/admin/2fa/setup", nil)
			},
			wantStatus:  http.StatusOK,
			wantBodyHas: []string{"já está ativo"},
			wantEnrolls: 0, // CRITICAL: Enroll must NOT be called
			wantAudits:  0,
		},
		{
			name: "no session resolver + valid pending cookie -> enrolment (AC3 fallback)",
			build: func(t *testing.T, deps *testDeps, cfg *HandlerConfig, enr *countingEnroller) *http.Request {
				id := uuid.New()
				deps.pendings.add(Pending{ID: id, UserID: uuid.New(), TenantID: uuid.New(), ExpiresAt: deps.clock.Now().Add(5 * time.Minute), NextPath: "/x"})
				enr.result = sampleResult
				// Resolver present but reports no full session -> fall back.
				cfg.TenantSession = stubTenantSession{ok: false}
				r := httptest.NewRequest(http.MethodPost, "/admin/2fa/setup", nil)
				r.AddCookie(&http.Cookie{Name: sessioncookie.NameTenantPending, Value: id.String()})
				return r
			},
			wantStatus:  http.StatusOK,
			wantBodyHas: []string{"otpauth://totp", "AAAAA-AAAAA"},
			wantEnrolls: 1,
			wantAudits:  0,
		},
		{
			name: "no session, no pending -> styled 303 to /login, no false audit (AC4)",
			build: func(t *testing.T, deps *testDeps, cfg *HandlerConfig, enr *countingEnroller) *http.Request {
				cfg.TenantSession = stubTenantSession{ok: false}
				return httptest.NewRequest(http.MethodGet, "/admin/2fa/setup", nil)
			},
			wantStatus:   http.StatusSeeOther,
			wantEnrolls:  0,
			wantAudits:   0,
			wantLocation: "/login?next=%2Fadmin%2F2fa%2Fsetup",
		},
		{
			name: "no session, EXPIRED pending -> 303 to /login, no false audit (AC4)",
			build: func(t *testing.T, deps *testDeps, cfg *HandlerConfig, enr *countingEnroller) *http.Request {
				id := uuid.New()
				deps.pendings.add(Pending{ID: id, UserID: uuid.New(), TenantID: uuid.New(), ExpiresAt: deps.clock.Now().Add(-time.Second), NextPath: "/x"})
				cfg.TenantSession = stubTenantSession{ok: false}
				r := httptest.NewRequest(http.MethodGet, "/admin/2fa/setup", nil)
				r.AddCookie(&http.Cookie{Name: sessioncookie.NameTenantPending, Value: id.String()})
				return r
			},
			wantStatus:   http.StatusSeeOther,
			wantEnrolls:  0,
			wantAudits:   0,
			wantLocation: "/login?next=%2Fadmin%2F2fa%2Fsetup",
		},
		{
			name: "nil resolver, no pending -> 303 to /login (legacy-safe)",
			build: func(t *testing.T, deps *testDeps, cfg *HandlerConfig, enr *countingEnroller) *http.Request {
				cfg.TenantSession = nil
				return httptest.NewRequest(http.MethodGet, "/admin/2fa/setup", nil)
			},
			wantStatus:   http.StatusSeeOther,
			wantEnrolls:  0,
			wantAudits:   0,
			wantLocation: "/login?next=%2Fadmin%2F2fa%2Fsetup",
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			deps := newTestDeps()
			enr := &countingEnroller{}
			cfg := deps.config()
			cfg.Enroller = enr
			r := tc.build(t, deps, &cfg, enr)
			h, err := NewHandler(cfg)
			if err != nil {
				t.Fatalf("NewHandler: %v", err)
			}
			w := httptest.NewRecorder()
			h.Setup(w, r)

			if w.Code != tc.wantStatus {
				t.Fatalf("status: want %d got %d (body=%q)", tc.wantStatus, w.Code, w.Body.String())
			}
			if enr.calls != tc.wantEnrolls {
				t.Fatalf("Enroll calls: want %d got %d", tc.wantEnrolls, enr.calls)
			}
			if got := deps.audit.events; got != tc.wantAudits {
				t.Fatalf("audit events: want %d got %d (last=%q)", tc.wantAudits, got, deps.audit.lastReason())
			}
			if tc.wantLocation != "" {
				if loc := w.Header().Get("Location"); loc != tc.wantLocation {
					t.Fatalf("Location: want %q got %q", tc.wantLocation, loc)
				}
			}
			body := w.Body.String()
			for _, want := range tc.wantBodyHas {
				if !strings.Contains(body, want) {
					t.Fatalf("body missing %q:\n%s", want, body)
				}
			}
		})
	}
}

// TestSetupFullSessionEnrollmentCheckError covers the 500 branch when the
// IsEnrolled read fails for a full-session caller.
func TestSetupFullSessionEnrollmentCheckError(t *testing.T) {
	t.Parallel()
	deps := newTestDeps()
	actor := TenantActor{UserID: uuid.New(), TenantID: uuid.New()}
	cfg := deps.config()
	cfg.Enrollment = &errEnrollment{err: errors.New("db down")}
	cfg.TenantSession = stubTenantSession{actor: actor, ok: true}
	h, err := NewHandler(cfg)
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	r := httptest.NewRequest(http.MethodGet, "/admin/2fa/setup", nil)
	w := httptest.NewRecorder()
	h.Setup(w, r)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status: want 500 got %d", w.Code)
	}
}

// TestSetupFullSessionTakesPrecedenceOverPending proves the full-session
// predicate wins even when a pending cookie is also present: an
// already-enrolled user must NOT be re-enrolled via the pending path.
func TestSetupFullSessionTakesPrecedenceOverPending(t *testing.T) {
	t.Parallel()
	deps := newTestDeps()
	actor := TenantActor{UserID: uuid.New(), TenantID: uuid.New()}
	deps.enrollment.mark(actor.UserID, true)
	enr := &countingEnroller{}
	cfg := deps.config()
	cfg.Enroller = enr
	cfg.TenantSession = stubTenantSession{actor: actor, ok: true}
	h, err := NewHandler(cfg)
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	id := uuid.New()
	deps.pendings.add(Pending{ID: id, UserID: uuid.New(), TenantID: uuid.New(), ExpiresAt: deps.clock.Now().Add(5 * time.Minute)})
	r := httptest.NewRequest(http.MethodGet, "/admin/2fa/setup", nil)
	r.AddCookie(&http.Cookie{Name: sessioncookie.NameTenantPending, Value: id.String()})
	w := httptest.NewRecorder()
	h.Setup(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status: want 200 got %d", w.Code)
	}
	if enr.calls != 0 {
		t.Fatalf("Enroll calls: want 0 (already enrolled, no rotation) got %d", enr.calls)
	}
	if !strings.Contains(w.Body.String(), "já está ativo") {
		t.Fatalf("expected already-active page, got:\n%s", w.Body.String())
	}
}

// ---- TenantSessionResolver adapter ----

type stubSessionValidator struct {
	sess iam.Session
	err  error
}

func (s stubSessionValidator) ValidateSession(_ context.Context, _, _ uuid.UUID) (iam.Session, error) {
	return s.sess, s.err
}

func TestNewTenantSessionResolverNilValidator(t *testing.T) {
	t.Parallel()
	if r := NewTenantSessionResolver(nil); r != nil {
		t.Fatalf("want nil resolver for nil validator, got %v", r)
	}
}

func TestTenantSessionResolverResolve(t *testing.T) {
	t.Parallel()
	tenantID := uuid.New()
	userID := uuid.New()
	sessionID := uuid.New()

	withTenant := func(r *http.Request) *http.Request {
		return r.WithContext(tenancy.WithContext(r.Context(), &tenancy.Tenant{ID: tenantID}))
	}
	withCookie := func(r *http.Request, v string) *http.Request {
		r.AddCookie(&http.Cookie{Name: sessioncookie.NameTenant, Value: v})
		return r
	}

	t.Run("valid full session", func(t *testing.T) {
		t.Parallel()
		res := NewTenantSessionResolver(stubSessionValidator{sess: iam.Session{ID: sessionID, UserID: userID, TenantID: tenantID}})
		r := withCookie(withTenant(httptest.NewRequest(http.MethodGet, "/admin/2fa/setup", nil)), sessionID.String())
		actor, ok := res.ResolveTenantSession(r)
		if !ok {
			t.Fatalf("want ok=true")
		}
		if actor.UserID != userID || actor.TenantID != tenantID {
			t.Fatalf("actor mismatch: got %+v", actor)
		}
	})

	t.Run("nil request", func(t *testing.T) {
		t.Parallel()
		res := NewTenantSessionResolver(stubSessionValidator{})
		if _, ok := res.ResolveTenantSession(nil); ok {
			t.Fatalf("want ok=false for nil request")
		}
	})

	t.Run("missing tenant on context", func(t *testing.T) {
		t.Parallel()
		res := NewTenantSessionResolver(stubSessionValidator{sess: iam.Session{UserID: userID}})
		r := withCookie(httptest.NewRequest(http.MethodGet, "/admin/2fa/setup", nil), sessionID.String())
		if _, ok := res.ResolveTenantSession(r); ok {
			t.Fatalf("want ok=false when tenant missing")
		}
	})

	t.Run("missing cookie", func(t *testing.T) {
		t.Parallel()
		res := NewTenantSessionResolver(stubSessionValidator{sess: iam.Session{UserID: userID}})
		r := withTenant(httptest.NewRequest(http.MethodGet, "/admin/2fa/setup", nil))
		if _, ok := res.ResolveTenantSession(r); ok {
			t.Fatalf("want ok=false when cookie missing")
		}
	})

	t.Run("malformed cookie uuid", func(t *testing.T) {
		t.Parallel()
		res := NewTenantSessionResolver(stubSessionValidator{sess: iam.Session{UserID: userID}})
		r := withCookie(withTenant(httptest.NewRequest(http.MethodGet, "/admin/2fa/setup", nil)), "not-a-uuid")
		if _, ok := res.ResolveTenantSession(r); ok {
			t.Fatalf("want ok=false for malformed cookie")
		}
	})

	t.Run("validator rejects session", func(t *testing.T) {
		t.Parallel()
		res := NewTenantSessionResolver(stubSessionValidator{err: iam.ErrSessionExpired})
		r := withCookie(withTenant(httptest.NewRequest(http.MethodGet, "/admin/2fa/setup", nil)), sessionID.String())
		if _, ok := res.ResolveTenantSession(r); ok {
			t.Fatalf("want ok=false when validator errors")
		}
	})
}
