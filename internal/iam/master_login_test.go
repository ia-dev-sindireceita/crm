package iam

// SIN-65254 — Service.MasterLogin use-case tests. The tenant-less master
// operator login was wired (SIN-65223) to the tenant-scoped iam.Service.Login,
// which resolves host→tenant and never finds the NULL-tenant master row;
// every master login returned 401. These tests pin the master-aware path:
// credential resolution by email (host-independent, case-insensitive),
// anti-enumeration, the m_login lockout + Slack alert, and the tenant-less
// Session shape. The postgres-backed credential lookup is covered against a
// real cluster in internal/adapter/db/postgres/master_credential_reader_test.go.

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// fakeMasterUsers satisfies MasterCredentialReader. Keyed by lower(email);
// a miss returns the (uuid.Nil, "", nil) sentinel per the port contract.
type fakeMasterUsers struct {
	rows map[string]struct {
		id   uuid.UUID
		hash string
	}
	err error
}

func (u fakeMasterUsers) LookupMasterCredentials(_ context.Context, email string) (uuid.UUID, string, error) {
	if u.err != nil {
		return uuid.Nil, "", u.err
	}
	if r, ok := u.rows[strings.ToLower(strings.TrimSpace(email))]; ok {
		return r.id, r.hash, nil
	}
	return uuid.Nil, "", nil
}

const (
	masterTestEmail    = "master@crm.local"
	masterTestPassword = "stg-password-correct"
)

func newMasterServiceForTest(t *testing.T) (*Service, uuid.UUID) {
	t.Helper()
	userID := uuid.MustParse("00000000-0000-0000-0000-0000000a57e7")
	hash := mustHash(t, masterTestPassword)
	return &Service{
		MasterUsers: fakeMasterUsers{rows: map[string]struct {
			id   uuid.UUID
			hash string
		}{
			masterTestEmail: {userID, hash},
		}},
		Logger: silentLogger(),
	}, userID
}

func TestMasterLogin_Success_TenantlessSession(t *testing.T) {
	t.Parallel()
	svc, userID := newMasterServiceForTest(t)

	sess, err := svc.MasterLogin(context.Background(), "ignored-host", masterTestEmail, masterTestPassword, nil, "", "/m/login")
	if err != nil {
		t.Fatalf("MasterLogin: %v", err)
	}
	if sess.UserID != userID {
		t.Errorf("UserID = %s, want %s", sess.UserID, userID)
	}
	// The master operator is tenant-less and the master_session is minted by
	// the HTTP handler, so the returned Session carries neither a tenant nor
	// a session id.
	if sess.TenantID != uuid.Nil {
		t.Errorf("TenantID = %s, want nil (master is tenant-less)", sess.TenantID)
	}
	if sess.ID != uuid.Nil {
		t.Errorf("ID = %s, want nil (no tenant session minted on master login)", sess.ID)
	}
}

// TestMasterLogin_HostIgnored_CaseInsensitiveEmail proves the resolution is
// global (host is ignored — the actual SIN-65254 bug was a host-scoped
// lookup) and email casing does not bypass it.
func TestMasterLogin_HostIgnored_CaseInsensitiveEmail(t *testing.T) {
	t.Parallel()
	svc, userID := newMasterServiceForTest(t)

	for _, host := range []string{"", "acme.crm.local", "master.example.com"} {
		sess, err := svc.MasterLogin(context.Background(), host, "MASTER@CRM.LOCAL", masterTestPassword, nil, "", "")
		if err != nil {
			t.Fatalf("host %q: MasterLogin: %v", host, err)
		}
		if sess.UserID != userID {
			t.Errorf("host %q: UserID = %s, want %s", host, sess.UserID, userID)
		}
	}
}

func TestMasterLogin_UnknownEmail_InvalidCredentials(t *testing.T) {
	t.Parallel()
	svc, _ := newMasterServiceForTest(t)

	_, err := svc.MasterLogin(context.Background(), "", "nobody@crm.local", masterTestPassword, nil, "", "")
	if !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("err = %v, want ErrInvalidCredentials", err)
	}
}

func TestMasterLogin_WrongPassword_InvalidCredentials(t *testing.T) {
	t.Parallel()
	svc, _ := newMasterServiceForTest(t)

	_, err := svc.MasterLogin(context.Background(), "", masterTestEmail, "WRONG", nil, "", "")
	if !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("err = %v, want ErrInvalidCredentials", err)
	}
}

// TestMasterLogin_ReaderError_Propagates — an infra failure must surface as a
// non-credential error so the HTTP boundary returns 5xx, not a misleading 401.
func TestMasterLogin_ReaderError_Propagates(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("boom")
	svc := &Service{
		MasterUsers: fakeMasterUsers{err: sentinel},
		Logger:      silentLogger(),
	}

	_, err := svc.MasterLogin(context.Background(), "", masterTestEmail, masterTestPassword, nil, "", "")
	if errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("infra error collapsed to ErrInvalidCredentials: %v", err)
	}
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want wrapped %v", err, sentinel)
	}
}

// TestMasterLogin_NilReader_Errors — a misconfigured Service (no master
// reader) is an operator error, surfaced as a non-credential error.
func TestMasterLogin_NilReader_Errors(t *testing.T) {
	t.Parallel()
	svc := &Service{Logger: silentLogger()}

	_, err := svc.MasterLogin(context.Background(), "", masterTestEmail, masterTestPassword, nil, "", "")
	if err == nil {
		t.Fatal("expected error for nil MasterUsers")
	}
	if errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("nil reader must not look like a credential reject: %v", err)
	}
}

// TestMasterLogin_PreLocked_ShortCircuits — an active lockout returns
// AccountLockedError before any password verification (SIN-62341 contract).
func TestMasterLogin_PreLocked_ShortCircuits(t *testing.T) {
	t.Parallel()
	svc, userID := newMasterServiceForTest(t)
	lockouts := newInMemoryLockouts()
	if err := lockouts.Lock(context.Background(), userID, lockouts.now().Add(time.Hour), "test"); err != nil {
		t.Fatalf("seed lockout: %v", err)
	}
	svc.Lockouts = lockouts

	// Even with the CORRECT password the locked account is rejected.
	_, err := svc.MasterLogin(context.Background(), "", masterTestEmail, masterTestPassword, nil, "", "")
	if !errors.Is(err, ErrAccountLocked) {
		t.Fatalf("err = %v, want ErrAccountLocked", err)
	}
}

// TestMasterLogin_LockoutTrips_FiresAlertOnce mirrors the tenant master-alert
// test for the master path: the m_login policy (Threshold 5, AlertOnLock)
// trips on the 6th failure and fires exactly one Slack alert.
func TestMasterLogin_LockoutTrips_FiresAlertOnce(t *testing.T) {
	t.Parallel()
	svc, _ := newMasterServiceForTest(t)
	svc.Lockouts = newInMemoryLockouts()
	svc.Limiter = newInMemoryLimiter()
	svc.LoginPolicy = masterPolicy(t)
	alerter := &recordingAlerter{}
	svc.Alerter = alerter
	ctx := context.Background()

	for i := 0; i < 5; i++ {
		_, err := svc.MasterLogin(ctx, "", masterTestEmail, "WRONG", nil, "", "/m/login")
		if !errors.Is(err, ErrInvalidCredentials) {
			t.Fatalf("attempt %d: err=%v want ErrInvalidCredentials", i+1, err)
		}
	}
	if alerter.count() != 0 {
		t.Fatalf("alerter fired %d times before threshold", alerter.count())
	}

	_, err := svc.MasterLogin(ctx, "", masterTestEmail, "WRONG", nil, "", "/m/login")
	if !errors.Is(err, ErrAccountLocked) {
		t.Fatalf("trip attempt: err=%v want ErrAccountLocked", err)
	}
	if got := alerter.count(); got != 1 {
		t.Fatalf("alerter.count() = %d, want 1", got)
	}
}

// TestMasterLogin_Success_ClearsLockout — a successful login clears any stale
// lockout row (best-effort, idempotent).
func TestMasterLogin_Success_ClearsLockout(t *testing.T) {
	t.Parallel()
	svc, userID := newMasterServiceForTest(t)
	lockouts := newInMemoryLockouts()
	// A stale, already-expired lockout must not block a correct login and
	// must be cleared.
	if err := lockouts.Lock(context.Background(), userID, lockouts.now().Add(-time.Minute), "stale"); err != nil {
		t.Fatalf("seed lockout: %v", err)
	}
	svc.Lockouts = lockouts

	if _, err := svc.MasterLogin(context.Background(), "", masterTestEmail, masterTestPassword, nil, "", ""); err != nil {
		t.Fatalf("MasterLogin: %v", err)
	}
	if locked, _, _ := lockouts.IsLocked(context.Background(), userID); locked {
		t.Error("lockout row should have been cleared after a successful login")
	}
}
