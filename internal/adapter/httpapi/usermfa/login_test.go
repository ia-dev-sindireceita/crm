package usermfa

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/adapter/httpapi/sessioncookie"
	"github.com/pericles-luz/crm/internal/iam"
)

func TestLoginPostNonAdminWritesSessionCookie(t *testing.T) {
	t.Parallel()
	deps := newLoginDeps()
	deps.iam.session = iam.Session{ID: uuid.New(), UserID: uuid.New(), TenantID: uuid.New(), CSRFToken: "csrf-x"}
	deps.requirements.req = Requirement{TOTPRequired: false}
	h := LoginPost(deps.config())
	w := httptest.NewRecorder()
	body := url.Values{"email": []string{"member@x"}, "password": []string{"pwd"}}
	r := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(body.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	h(w, r)
	if w.Code != http.StatusFound {
		t.Fatalf("status: want 302 got %d", w.Code)
	}
	hasTenant := false
	hasPending := false
	for _, c := range w.Result().Cookies() {
		if c.Name == sessioncookie.NameTenant && c.Value != "" {
			hasTenant = true
		}
		if c.Name == sessioncookie.NameTenantPending && c.Value != "" {
			hasPending = true
		}
	}
	if !hasTenant {
		t.Fatalf("expected __Host-sess-tenant cookie")
	}
	if hasPending {
		t.Fatalf("did not expect __Host-mfa-pending cookie on non-MFA path")
	}
	if deps.sessions.deleted != uuid.Nil {
		t.Fatalf("expected session to NOT be deleted on non-MFA path")
	}
}

func TestLoginPostAdminWritesPendingCookieAndRollsBackSession(t *testing.T) {
	t.Parallel()
	deps := newLoginDeps()
	sess := iam.Session{ID: uuid.New(), UserID: uuid.New(), TenantID: uuid.New(), CSRFToken: "csrf-x"}
	deps.iam.session = sess
	deps.requirements.req = Requirement{TOTPRequired: true, TOTPEnrolled: true}
	pendingID := uuid.New()
	deps.pendings.row = Pending{ID: pendingID, UserID: sess.UserID, TenantID: sess.TenantID, ExpiresAt: time.Now().Add(5 * time.Minute), NextPath: "/inbox"}
	h := LoginPost(deps.config())
	w := httptest.NewRecorder()
	body := url.Values{"email": []string{"admin@x"}, "password": []string{"pwd"}, "next": []string{"/inbox"}}
	r := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(body.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	h(w, r)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("status: want 303 got %d", w.Code)
	}
	if loc := w.Header().Get("Location"); loc != "/admin/2fa/verify" {
		t.Fatalf("Location: want /admin/2fa/verify got %q", loc)
	}
	if deps.sessions.deleted != sess.ID {
		t.Fatalf("expected session %s to be deleted, got %s", sess.ID, deps.sessions.deleted)
	}
	hasPending := false
	hasTenant := false
	for _, c := range w.Result().Cookies() {
		if c.Name == sessioncookie.NameTenantPending && c.Value == pendingID.String() {
			hasPending = true
		}
		if c.Name == sessioncookie.NameTenant && c.Value != "" {
			hasTenant = true
		}
	}
	if !hasPending {
		t.Fatalf("expected __Host-mfa-pending cookie")
	}
	if hasTenant {
		t.Fatalf("AC #1: did not expect __Host-sess-tenant cookie on MFA-pending path")
	}
}

func TestLoginPostAdminNotEnrolledRedirectsToSetup(t *testing.T) {
	t.Parallel()
	deps := newLoginDeps()
	sess := iam.Session{ID: uuid.New(), UserID: uuid.New(), TenantID: uuid.New()}
	deps.iam.session = sess
	deps.requirements.req = Requirement{TOTPRequired: true, TOTPEnrolled: false}
	deps.pendings.row = Pending{ID: uuid.New(), UserID: sess.UserID, TenantID: sess.TenantID, ExpiresAt: time.Now().Add(5 * time.Minute)}
	h := LoginPost(deps.config())
	w := httptest.NewRecorder()
	body := url.Values{"email": []string{"admin@x"}, "password": []string{"pwd"}}
	r := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(body.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	h(w, r)
	if loc := w.Header().Get("Location"); loc != "/admin/2fa/setup" {
		t.Fatalf("Location: want /admin/2fa/setup got %q", loc)
	}
}

func TestLoginPostInvalidCredentialsDoesNotTouchMFA(t *testing.T) {
	t.Parallel()
	deps := newLoginDeps()
	deps.iam.err = iam.ErrInvalidCredentials
	h := LoginPost(deps.config())
	w := httptest.NewRecorder()
	body := url.Values{"email": []string{"x"}, "password": []string{"y"}}
	r := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(body.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	h(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status: want 401 got %d", w.Code)
	}
	if deps.requirements.called {
		t.Fatalf("Requirements.Load must not be called when password verification fails")
	}
}

// ---- login fakes ----

type loginDeps struct {
	iam          *fakeLoginIAM
	sessions     *fakeSessionDeleter
	pendings     *fakePendingCreator
	requirements *fakeRequirements
}

func newLoginDeps() *loginDeps {
	return &loginDeps{
		iam:          &fakeLoginIAM{},
		sessions:     &fakeSessionDeleter{},
		pendings:     &fakePendingCreator{},
		requirements: &fakeRequirements{},
	}
}

func (d *loginDeps) config() LoginConfig {
	return LoginConfig{
		IAM:          d.iam,
		Sessions:     d.sessions,
		Pendings:     d.pendings,
		Requirements: d.requirements,
		PendingTTL:   5 * time.Minute,
	}
}

type fakeLoginIAM struct {
	session iam.Session
	err     error
}

func (f *fakeLoginIAM) Login(_ context.Context, _, _, _ string, _ net.IP, _, _ string) (iam.Session, error) {
	if f.err != nil {
		return iam.Session{}, f.err
	}
	return f.session, nil
}

type fakeSessionDeleter struct {
	mu      sync.Mutex
	deleted uuid.UUID
}

func (f *fakeSessionDeleter) Delete(_ context.Context, _, sessionID uuid.UUID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleted = sessionID
	return nil
}

type fakePendingCreator struct {
	row Pending
	err error
}

func (f *fakePendingCreator) Create(_ context.Context, _ uuid.UUID, _ time.Duration, _ string) (Pending, error) {
	if f.err != nil {
		return Pending{}, f.err
	}
	return f.row, nil
}

type fakeRequirements struct {
	req    Requirement
	err    error
	called bool
}

func (f *fakeRequirements) Load(_ context.Context, _ uuid.UUID) (Requirement, error) {
	f.called = true
	if f.err != nil {
		return Requirement{}, f.err
	}
	return f.req, nil
}

// Suppress unused-package warnings by referencing helper symbols.
var _ = errors.Is
