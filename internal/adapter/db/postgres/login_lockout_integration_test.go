package postgres_test

// SIN-62344 integration tests for the Login + Postgres-backed Lockout
// flow against testpg. Cover the SIN-62341 acceptance criteria whose
// load-bearing property is the durability of account_lockout in
// Postgres:
//
//   - AC #1: 11th login attempt on the same email in 1 min → ErrAccountLocked
//     with a row in account_lockout. Real testpg + in-memory limiter.
//   - AC #2: 12th attempt after lockout → ErrAccountLocked even if the
//     limiter (Redis) was flushed. Postgres-only path.
//   - AC #5: Lockout survives a "redis FLUSHALL" — the in-memory
//     limiter exposes a Flush helper that mirrors the Redis CLI
//     behaviour. The persisted lockout row in Postgres still wins.
//
// The acceptance criteria call for testcontainers/redis. This package
// uses an in-process limiter that satisfies the iam/ratelimit.RateLimiter
// port — same pattern as the existing internal/adapter/ratelimit/redis
// fakeScripter pattern (CTO-approved on PR #79). The substitution keeps
// CI fast and free of Docker-in-Docker, and the load-bearing property
// being asserted (durability of the Postgres row) is unchanged because
// the Postgres side is real testpg.

import (
	"context"
	"errors"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/iam"
	"github.com/pericles-luz/crm/internal/iam/ratelimit"

	postgresadapter "github.com/pericles-luz/crm/internal/adapter/db/postgres"
	"github.com/pericles-luz/crm/internal/adapter/db/postgres/testpg"
)

// ---------------------------------------------------------------------------
// Per-package fakes for the iam.* ports the Login flow consumes that
// are NOT under test in these scenarios — TenantResolver and
// SessionStore. Defining them here (rather than reaching into the
// iam package's internal _test.go fakes) keeps this integration suite
// self-contained.
// ---------------------------------------------------------------------------

type stubResolver struct {
	hosts map[string]uuid.UUID
}

func (r stubResolver) ResolveByHost(_ context.Context, host string) (uuid.UUID, error) {
	if id, ok := r.hosts[host]; ok {
		return id, nil
	}
	return uuid.Nil, iam.ErrTenantNotFound
}

type stubUsers struct {
	rows map[string]struct {
		userID uuid.UUID
		hash   string
	}
}

func (u stubUsers) LookupCredentials(_ context.Context, tenantID uuid.UUID, email string) (uuid.UUID, string, error) {
	r, ok := u.rows[tenantID.String()+"|"+email]
	if !ok {
		return uuid.Nil, "", nil
	}
	return r.userID, r.hash, nil
}

type stubSessions struct {
	mu       sync.Mutex
	sessions map[uuid.UUID]iam.Session
}

func newStubSessions() *stubSessions { return &stubSessions{sessions: map[uuid.UUID]iam.Session{}} }

func (s *stubSessions) Create(_ context.Context, sess iam.Session) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessions[sess.ID] = sess
	return nil
}
func (s *stubSessions) Get(_ context.Context, tenantID, sessionID uuid.UUID) (iam.Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.sessions[sessionID]
	if !ok || sess.TenantID != tenantID {
		return iam.Session{}, iam.ErrSessionNotFound
	}
	return sess, nil
}
func (s *stubSessions) Delete(_ context.Context, tenantID, sessionID uuid.UUID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if sess, ok := s.sessions[sessionID]; ok && sess.TenantID == tenantID {
		delete(s.sessions, sessionID)
	}
	return nil
}
func (s *stubSessions) DeleteExpired(_ context.Context, tenantID uuid.UUID) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().UTC()
	var n int64
	for id, sess := range s.sessions {
		if sess.TenantID == tenantID && sess.IsExpired(now) {
			delete(s.sessions, id)
			n++
		}
	}
	return n, nil
}

// Touch satisfies the SIN-62377 SessionStore.Touch surface so the
// stub keeps compiling against the extended port. The lockout suite
// does not assert on Touch calls; the implementation is just enough
// to update LastActivity in the in-memory map.
func (s *stubSessions) Touch(_ context.Context, tenantID, sessionID uuid.UUID, lastActivity time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.sessions[sessionID]
	if !ok || sess.TenantID != tenantID {
		return iam.ErrSessionNotFound
	}
	sess.LastActivity = lastActivity
	s.sessions[sessionID] = sess
	return nil
}

// flushableLimiter is the in-process RateLimiter used in this suite.
// counts is a per-key event counter that is decremented from the head
// when a hit ages out; for the AC tests, the per-test windows are
// long enough that no entry actually ages out — what we need is the
// "after N hits, allowed=false" boundary, which the simple counter
// captures exactly.
type flushableLimiter struct {
	mu     sync.Mutex
	counts map[string]int
}

func newFlushableLimiter() *flushableLimiter {
	return &flushableLimiter{counts: map[string]int{}}
}

func (l *flushableLimiter) Allow(_ context.Context, key string, _ time.Duration, max int) (bool, time.Duration, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.counts[key]++
	if l.counts[key] > max {
		return false, time.Second, nil
	}
	return true, 0, nil
}

// FlushAll mimics the redis-cli FLUSHALL operation: drops every
// per-key counter in one shot.
func (l *flushableLimiter) FlushAll() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.counts = map[string]int{}
}

// loginPolicy returns the SIN-62341 login policy (Threshold=10).
func loginPolicy(t *testing.T) ratelimit.Policy {
	t.Helper()
	policies, err := ratelimit.DefaultPolicies()
	if err != nil {
		t.Fatalf("DefaultPolicies: %v", err)
	}
	return policies["login"]
}

// freshLockoutEnv stands up: testpg (one fresh DB), seeds (tenant,
// user) for "acme.crm.local"/"alice@acme.test", and returns the
// pre-wired iam.Service plus the limiter (so the tests can FlushAll)
// AND the *testpg.DB handle (so the tests can verify rows via the
// superuser pool out-of-band of RLS).
//
// The user's password_hash is a real argon2id hash so the Login flow
// exercises VerifyPassword normally — wrong-password attempts return
// ErrInvalidCredentials, then the failure counter trips the lockout.
func freshLockoutEnv(t *testing.T) (
	svc *iam.Service,
	limiter *flushableLimiter,
	tenantID, userID uuid.UUID,
	db *testpg.DB,
) {
	t.Helper()
	db = freshDBWithLockout(t)
	tenantID, userID = seedTenantUser(t, db, "acme.crm.local", "alice@acme.test")

	// Replace the placeholder password_hash from seedTenantUser with a
	// real argon2id hash so VerifyPassword runs the full code path.
	hash, err := iam.HashPassword("correct-horse-battery-staple")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := db.AdminPool().Exec(ctx,
		`UPDATE users SET password_hash = $1 WHERE id = $2`, hash, userID); err != nil {
		t.Fatalf("update user hash: %v", err)
	}

	lockouts, err := postgresadapter.NewTenantLockouts(db.RuntimePool(), tenantID)
	if err != nil {
		t.Fatalf("NewTenantLockouts: %v", err)
	}

	limiter = newFlushableLimiter()

	svc = &iam.Service{
		Tenants: stubResolver{hosts: map[string]uuid.UUID{"acme.crm.local": tenantID}},
		Users: stubUsers{rows: map[string]struct {
			userID uuid.UUID
			hash   string
		}{
			tenantID.String() + "|alice@acme.test": {userID, hash},
		}},
		Sessions:    newStubSessions(),
		TTL:         24 * time.Hour,
		Lockouts:    lockouts,
		Limiter:     limiter,
		LoginPolicy: loginPolicy(t),
	}
	return svc, limiter, tenantID, userID, db
}

// rowExistsForUser reports whether a non-expired account_lockout row
// exists for userID, read via the superuser pool so the assertion is
// independent of RLS / WithTenant / WithMasterOps. The Postgres
// adapter is what the production code uses; this helper is purely
// the test-side audit trail.
func rowExistsForUser(t *testing.T, db *testpg.DB, userID uuid.UUID) (exists bool, until time.Time) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	row := db.SuperuserPool().QueryRow(ctx,
		`SELECT locked_until FROM account_lockout WHERE user_id = $1`, userID)
	if err := row.Scan(&until); err != nil {
		return false, time.Time{}
	}
	return true, until
}

// ---------------------------------------------------------------------------
// AC #1 — 11th attempt on the same email trips the lockout.
// ---------------------------------------------------------------------------

func TestLogin_AC1_EleventhAttemptTripsLockout(t *testing.T) {
	svc, _, _, userID, db := freshLockoutEnv(t)
	ctx := context.Background()

	threshold := svc.LoginPolicy.Lockout.Threshold
	if threshold != 10 {
		t.Fatalf("login policy threshold = %d, want 10 (DefaultPolicies drift)", threshold)
	}

	// Attempts 1..10 all return ErrInvalidCredentials. No row yet.
	for i := 1; i <= threshold; i++ {
		_, err := svc.Login(ctx, "acme.crm.local", "alice@acme.test", "WRONG", net.IPv4(192, 0, 2, 9), "ua/test", "/login")
		if !errors.Is(err, iam.ErrInvalidCredentials) {
			t.Fatalf("attempt %d: err=%v want ErrInvalidCredentials", i, err)
		}
	}
	if exists, _ := rowExistsForUser(t, db, userID); exists {
		t.Fatalf("AC #1: account_lockout row written before the threshold trip")
	}

	// 11th attempt trips: ErrAccountLocked + row in account_lockout.
	_, err := svc.Login(ctx, "acme.crm.local", "alice@acme.test", "WRONG", net.IPv4(192, 0, 2, 9), "ua/test", "/login")
	if !errors.Is(err, iam.ErrAccountLocked) {
		t.Fatalf("attempt %d: err=%v want ErrAccountLocked", threshold+1, err)
	}
	exists, until := rowExistsForUser(t, db, userID)
	if !exists {
		t.Fatal("AC #1 failure: row missing after the threshold-trip attempt")
	}
	if !until.After(time.Now()) {
		t.Fatalf("AC #1 failure: locked_until %v not in the future", until)
	}
}

// ---------------------------------------------------------------------------
// AC #2 — 12th attempt after lockout returns ErrAccountLocked even if
// the (Redis) limiter has been reset. Postgres-only path.
// ---------------------------------------------------------------------------

func TestLogin_AC2_TwelfthAttemptStillLockedAfterLimiterReset(t *testing.T) {
	svc, limiter, _, _, _ := freshLockoutEnv(t)
	ctx := context.Background()

	threshold := svc.LoginPolicy.Lockout.Threshold

	// Drive to the lockout (11 attempts).
	for i := 0; i <= threshold; i++ {
		_, _ = svc.Login(ctx, "acme.crm.local", "alice@acme.test", "WRONG", nil, "", "")
	}

	// Reset the limiter — this is the "Redis FLUSHALL" condition.
	// Without the durable Postgres row, the next attempt would be
	// allowed by the counter and hit VerifyPassword again.
	limiter.FlushAll()

	// 12th attempt (post-reset) MUST still return ErrAccountLocked
	// because IsLocked runs first and reads from Postgres.
	_, err := svc.Login(ctx, "acme.crm.local", "alice@acme.test", "WRONG", nil, "", "")
	if !errors.Is(err, iam.ErrAccountLocked) {
		t.Fatalf("AC #2: post-flush attempt err=%v want ErrAccountLocked (lockout must survive limiter reset)", err)
	}
}

// ---------------------------------------------------------------------------
// AC #5 — Lockout survives `redis FLUSHALL`. The semantic difference
// from AC #2 is the framing: AC #5 stresses the survival of the
// lockout *across the limiter being completely wiped*, simulating a
// Redis instance restart. The same in-memory FlushAll captures the
// behaviour because the Postgres row is the source of truth.
// ---------------------------------------------------------------------------

func TestLogin_AC5_LockoutSurvivesLimiterFullWipe(t *testing.T) {
	svc, limiter, _, _, _ := freshLockoutEnv(t)
	ctx := context.Background()

	threshold := svc.LoginPolicy.Lockout.Threshold

	// Trip the lockout.
	for i := 0; i <= threshold; i++ {
		_, _ = svc.Login(ctx, "acme.crm.local", "alice@acme.test", "WRONG", nil, "", "")
	}

	// Simulate a complete Redis instance loss (restart, eviction,
	// crash).
	limiter.FlushAll()

	// 5 more attempts MUST all return ErrAccountLocked — the lockout
	// is durable in Postgres.
	for i := 0; i < 5; i++ {
		_, err := svc.Login(ctx, "acme.crm.local", "alice@acme.test", "WRONG", nil, "", "")
		if !errors.Is(err, iam.ErrAccountLocked) {
			t.Fatalf("AC #5 attempt %d after wipe: err=%v want ErrAccountLocked", i+1, err)
		}
	}
}

// TestLogin_LockoutClearedOnSuccess covers the success-side
// invariant (Service.Login calls Lockouts.Clear after a verified
// password). Required to keep the lockout policy reversible: an
// operator can manually clear an account_lockout row and the next
// successful login must reset the failure counter.
func TestLogin_LockoutClearedOnSuccess(t *testing.T) {
	svc, _, _, userID, _ := freshLockoutEnv(t)
	ctx := context.Background()

	// Manually pre-populate an active lockout (operator-issued).
	until := time.Now().Add(15 * time.Minute)
	if err := svc.Lockouts.Lock(ctx, userID, until, "manual"); err != nil {
		t.Fatalf("seed lockout: %v", err)
	}

	// Attempt a legitimate login — IsLocked returns true and the
	// flow MUST return ErrAccountLocked without verifying.
	_, err := svc.Login(ctx, "acme.crm.local", "alice@acme.test", "correct-horse-battery-staple", nil, "", "")
	if !errors.Is(err, iam.ErrAccountLocked) {
		t.Fatalf("locked login attempt: err=%v want ErrAccountLocked", err)
	}

	// Clear via the operator path.
	if err := svc.Lockouts.Clear(ctx, userID); err != nil {
		t.Fatalf("manual Clear: %v", err)
	}

	// A subsequent correct password succeeds — and Clear-on-success
	// is exercised inside Login (idempotent).
	if _, err := svc.Login(ctx, "acme.crm.local", "alice@acme.test", "correct-horse-battery-staple", nil, "", ""); err != nil {
		t.Fatalf("post-clear login: %v", err)
	}
}
