package iam

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

// fakeStore is an in-memory iam.SessionStore for unit tests. It is NOT a
// drop-in for production: there is no RLS, no transaction, no cross-tenant
// hardening. The postgres adapter test (session_store_test.go) covers the
// real persistence + RLS surface against a live Postgres.
type fakeStore struct {
	mu       sync.Mutex
	sessions map[uuid.UUID]Session
}

func newFakeStore() *fakeStore { return &fakeStore{sessions: map[uuid.UUID]Session{}} }

func (f *fakeStore) Create(_ context.Context, s Session) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sessions[s.ID] = s
	return nil
}

func (f *fakeStore) Get(_ context.Context, tenantID, sessionID uuid.UUID) (Session, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.sessions[sessionID]
	if !ok || s.TenantID != tenantID {
		return Session{}, ErrSessionNotFound
	}
	return s, nil
}

func (f *fakeStore) Delete(_ context.Context, tenantID, sessionID uuid.UUID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if s, ok := f.sessions[sessionID]; ok && s.TenantID == tenantID {
		delete(f.sessions, sessionID)
	}
	return nil
}

func (f *fakeStore) DeleteExpired(_ context.Context, tenantID uuid.UUID) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	now := time.Now().UTC()
	var n int64
	for id, s := range f.sessions {
		if s.TenantID == tenantID && s.IsExpired(now) {
			delete(f.sessions, id)
			n++
		}
	}
	return n, nil
}

// fakeResolver maps host -> tenantID, returning ErrTenantNotFound for misses.
type fakeResolver struct {
	hosts map[string]uuid.UUID
	err   error // injected infra error path
}

func (r fakeResolver) ResolveByHost(_ context.Context, host string) (uuid.UUID, error) {
	if r.err != nil {
		return uuid.Nil, r.err
	}
	if id, ok := r.hosts[host]; ok {
		return id, nil
	}
	return uuid.Nil, ErrTenantNotFound
}

// fakeUsers maps (tenantID, email) -> (userID, password_hash).
type fakeUsers struct {
	rows map[string]struct {
		userID uuid.UUID
		hash   string
	}
	err error
}

func (u fakeUsers) LookupCredentials(_ context.Context, tenantID uuid.UUID, email string) (uuid.UUID, string, error) {
	if u.err != nil {
		return uuid.Nil, "", u.err
	}
	if r, ok := u.rows[tenantID.String()+"|"+email]; ok {
		return r.userID, r.hash, nil
	}
	return uuid.Nil, "", nil
}

// silentLogger discards all log records — keeps test output clean.
func silentLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

func mustHash(t *testing.T, plaintext string) string {
	t.Helper()
	h, err := HashPassword(plaintext)
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	return h
}

func newServiceForTest(t *testing.T) (*Service, uuid.UUID, uuid.UUID) {
	t.Helper()
	tenantID := uuid.MustParse("11111111-1111-4111-8111-111111111111")
	userID := uuid.MustParse("22222222-2222-4222-8222-222222222222")
	hash := mustHash(t, "correct-horse-battery-staple")

	return &Service{
		Tenants: fakeResolver{hosts: map[string]uuid.UUID{
			"acme.crm.local": tenantID,
		}},
		Users: fakeUsers{rows: map[string]struct {
			userID uuid.UUID
			hash   string
		}{
			tenantID.String() + "|alice@acme.test": {userID, hash},
		}},
		Sessions: newFakeStore(),
		TTL:      24 * time.Hour,
		Logger:   silentLogger(),
	}, tenantID, userID
}

// TestLoginLogoutCycle covers acceptance criterion #3 — login → session
// active → logout → ValidateSession returns ErrSessionNotFound.
func TestLoginLogoutCycle(t *testing.T) {
	svc, tenantID, userID := newServiceForTest(t)
	ctx := context.Background()

	sess, err := svc.Login(ctx, "acme.crm.local", "alice@acme.test", "correct-horse-battery-staple", net.IPv4(192, 0, 2, 1), "ua/test")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	if sess.UserID != userID || sess.TenantID != tenantID {
		t.Fatalf("session ids wrong: got user=%s tenant=%s", sess.UserID, sess.TenantID)
	}
	if sess.ExpiresAt.Sub(sess.CreatedAt) != 24*time.Hour {
		t.Fatalf("TTL not honoured: ExpiresAt-CreatedAt = %v", sess.ExpiresAt.Sub(sess.CreatedAt))
	}

	got, err := svc.ValidateSession(ctx, tenantID, sess.ID)
	if err != nil {
		t.Fatalf("ValidateSession: %v", err)
	}
	if got.ID != sess.ID {
		t.Fatalf("ValidateSession returned different session")
	}

	if err := svc.Logout(ctx, tenantID, sess.ID); err != nil {
		t.Fatalf("Logout: %v", err)
	}

	if _, err := svc.ValidateSession(ctx, tenantID, sess.ID); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("after Logout, ValidateSession err=%v want ErrSessionNotFound", err)
	}
}

func TestLogout_Idempotent(t *testing.T) {
	svc, tenantID, _ := newServiceForTest(t)
	ctx := context.Background()
	if err := svc.Logout(ctx, tenantID, uuid.New()); err != nil {
		t.Fatalf("Logout of unknown id should be idempotent, got: %v", err)
	}
}

// TestLogin_WrongPassword_NoEnumerate covers acceptance criterion #4 —
// unknown email and wrong password must both return ErrInvalidCredentials.
// We also assert the timing dummy-verify path does not blow up: the
// not-found path runs a full argon2 verify against dummyHash, so its
// runtime must be roughly comparable to the real verify (we assert it is
// at least non-trivial — a strict timing-equivalence assertion would be
// CI-flaky, which the SecurityEngineer review explicitly flagged).
func TestLogin_WrongPassword_NoEnumerate(t *testing.T) {
	svc, _, _ := newServiceForTest(t)
	ctx := context.Background()

	_, err := svc.Login(ctx, "acme.crm.local", "alice@acme.test", "WRONG", nil, "")
	if !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("wrong-password err=%v want ErrInvalidCredentials", err)
	}

	_, err = svc.Login(ctx, "acme.crm.local", "ghost@acme.test", "anything", nil, "")
	if !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("unknown-email err=%v want ErrInvalidCredentials", err)
	}

	// Anti-enumeration via timing: both paths perform a full argon2
	// derivation. The not-found branch should take at least the same
	// order of magnitude as the wrong-password branch. We measure each a
	// few times and assert the not-found median is not implausibly fast
	// (i.e. there is no early-return short-circuit).
	measure := func(email string) time.Duration {
		var samples []time.Duration
		for i := 0; i < 3; i++ {
			start := time.Now()
			_, _ = svc.Login(ctx, "acme.crm.local", email, "WRONG", nil, "")
			samples = append(samples, time.Since(start))
		}
		// median of 3
		if samples[0] > samples[1] {
			samples[0], samples[1] = samples[1], samples[0]
		}
		if samples[1] > samples[2] {
			samples[1], samples[2] = samples[2], samples[1]
		}
		if samples[0] > samples[1] {
			samples[0], samples[1] = samples[1], samples[0]
		}
		return samples[1]
	}
	wrongPwd := measure("alice@acme.test")
	unknown := measure("ghost@acme.test")

	// CI-stable bound: not-found must be at least 25% of wrong-pwd. A
	// short-circuit (no dummy-verify) would make the not-found branch
	// orders of magnitude faster; 25% leaves plenty of room for noise
	// and slow CI runners while still detecting a regression.
	if unknown*4 < wrongPwd {
		t.Fatalf("not-found branch too fast: unknown=%v wrong-pwd=%v — dummy-verify likely missing", unknown, wrongPwd)
	}
}

func TestLogin_HostInvalid_NoEnumerate(t *testing.T) {
	svc, _, _ := newServiceForTest(t)
	_, err := svc.Login(context.Background(), "unknown.example.com", "alice@acme.test", "anything", nil, "")
	if !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("err=%v want ErrInvalidCredentials (no ErrTenantNotFound leak)", err)
	}
	// Must NOT match the raw tenant-not-found sentinel.
	if errors.Is(err, ErrTenantNotFound) {
		t.Fatalf("ErrTenantNotFound leaked through Login — should collapse to ErrInvalidCredentials")
	}
}

// TestLogin_HostInvalid_TimingEqualized is the SIN-62305 / SIN-62518
// anti-enumeration assertion for the host-not-found branch. Mirrors the
// median-of-3 / ≥25% bound used by TestLogin_WrongPassword_NoEnumerate so
// a regression that early-returns without dummyVerify on the host-not-
// found path is caught without being CI-flaky.
//
// Without the dummyVerify call the host-not-found branch finishes in ~µs
// while the known-host wrong-password branch takes ~100 ms (one argon2id
// derivation). An on-the-wire attacker can use that gap to enumerate
// which hosts map to tenants (i.e. the SaaS customer list).
func TestLogin_HostInvalid_TimingEqualized(t *testing.T) {
	svc, _, _ := newServiceForTest(t)
	ctx := context.Background()

	measure := func(host string) time.Duration {
		var samples [3]time.Duration
		for i := 0; i < 3; i++ {
			start := time.Now()
			_, _ = svc.Login(ctx, host, "alice@acme.test", "WRONG", nil, "")
			samples[i] = time.Since(start)
		}
		if samples[0] > samples[1] {
			samples[0], samples[1] = samples[1], samples[0]
		}
		if samples[1] > samples[2] {
			samples[1], samples[2] = samples[2], samples[1]
		}
		if samples[0] > samples[1] {
			samples[0], samples[1] = samples[1], samples[0]
		}
		return samples[1]
	}
	wrongPwd := measure("acme.crm.local")
	hostInvalid := measure("unknown.example.com")

	if hostInvalid*4 < wrongPwd {
		t.Fatalf("host-not-found branch too fast: host-invalid=%v wrong-pwd=%v — dummy-verify likely missing on host-not-found path", hostInvalid, wrongPwd)
	}
}

func TestLogin_TenantResolverInfraError_Propagates(t *testing.T) {
	svc, _, _ := newServiceForTest(t)
	svc.Tenants = fakeResolver{err: errors.New("dial tcp: connection refused")}
	_, err := svc.Login(context.Background(), "any.host", "alice@acme.test", "x", nil, "")
	if err == nil || errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("infra error should propagate as 5xx-eligible, got: %v", err)
	}
}

func TestLogin_UserLookupInfraError_Propagates(t *testing.T) {
	svc, _, _ := newServiceForTest(t)
	svc.Users = fakeUsers{err: errors.New("postgres: timeout")}
	_, err := svc.Login(context.Background(), "acme.crm.local", "alice@acme.test", "x", nil, "")
	if err == nil || errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("infra error should propagate, got: %v", err)
	}
}

// TestSessionExpired covers acceptance criterion #5 — a session whose
// expires_at is before now must be rejected with ErrSessionExpired.
func TestSessionExpired(t *testing.T) {
	svc, tenantID, _ := newServiceForTest(t)
	t0 := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	svc.Now = func() time.Time { return t0 }
	ctx := context.Background()

	sess, err := svc.Login(ctx, "acme.crm.local", "alice@acme.test", "correct-horse-battery-staple", nil, "")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	if sess.ExpiresAt != t0.Add(24*time.Hour) {
		t.Fatalf("ExpiresAt not absolute against frozen Now: got %v want %v", sess.ExpiresAt, t0.Add(24*time.Hour))
	}

	// Move clock past expiry and re-validate.
	svc.Now = func() time.Time { return t0.Add(25 * time.Hour) }
	if _, err := svc.ValidateSession(ctx, tenantID, sess.ID); !errors.Is(err, ErrSessionExpired) {
		t.Fatalf("expected ErrSessionExpired, got %v", err)
	}
}

func TestValidateSession_UnknownID(t *testing.T) {
	svc, tenantID, _ := newServiceForTest(t)
	if _, err := svc.ValidateSession(context.Background(), tenantID, uuid.New()); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("err=%v want ErrSessionNotFound", err)
	}
}

func TestService_LoggerDefaultDoesNotPanic(t *testing.T) {
	svc, _, _ := newServiceForTest(t)
	svc.Logger = nil
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("nil Logger caused panic: %v", r)
		}
	}()
	_, _ = svc.Login(context.Background(), "acme.crm.local", "alice@acme.test", "correct-horse-battery-staple", nil, "")
}

func TestService_TTLDefaultsTo24h(t *testing.T) {
	svc, _, _ := newServiceForTest(t)
	svc.TTL = 0
	t0 := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	svc.Now = func() time.Time { return t0 }
	sess, err := svc.Login(context.Background(), "acme.crm.local", "alice@acme.test", "correct-horse-battery-staple", nil, "")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	if sess.ExpiresAt != t0.Add(24*time.Hour) {
		t.Fatalf("zero TTL did not fall back to 24h: got %v", sess.ExpiresAt)
	}
}
