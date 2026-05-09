package iam

import (
	"context"
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/iam/password"
)

// fakePasswordWriter records UpdatePasswordHash calls. The optional err
// is returned to the caller; tests use it to exercise the non-fatal
// rehash failure path.
type fakePasswordWriter struct {
	mu    sync.Mutex
	rows  map[uuid.UUID]string // userID -> encoded
	calls int
	err   error
}

func newFakeWriter() *fakePasswordWriter {
	return &fakePasswordWriter{rows: map[uuid.UUID]string{}}
}

func (f *fakePasswordWriter) UpdatePasswordHash(_ context.Context, _ uuid.UUID, userID uuid.UUID, encoded string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.err != nil {
		return f.err
	}
	f.rows[userID] = encoded
	return nil
}

func (f *fakePasswordWriter) Get(userID uuid.UUID) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.rows[userID]
}

// fakePolicy implements password.PolicyChecker; returns errStub on the
// passwords listed in fail, nil otherwise.
type fakePolicy struct {
	fail map[string]error
}

func (f *fakePolicy) PolicyCheck(_ context.Context, plain string, _ password.PolicyContext) error {
	if err, ok := f.fail[plain]; ok {
		return err
	}
	return nil
}

func TestSetPassword_PolicyRejectsBeforeHashing(t *testing.T) {
	t.Parallel()
	policyErr := &password.PolicyError{Reason: password.ReasonTooShort, Detail: "min 12 chars"}
	hasher := password.Default()
	writer := newFakeWriter()
	svc := &Service{
		PasswordPolicy: &fakePolicy{fail: map[string]error{"short-pwd": policyErr}},
		PasswordHasher: hasher,
		PasswordWriter: writer,
	}
	tenantID := uuid.New()
	userID := uuid.New()
	err := svc.SetPassword(context.Background(), tenantID, userID, "short-pwd", password.PolicyContext{Email: "u@x.test"})
	var pe *password.PolicyError
	if !errors.As(err, &pe) || pe.Reason != password.ReasonTooShort {
		t.Fatalf("err=%v want *PolicyError(too_short)", err)
	}
	if writer.calls != 0 {
		t.Fatalf("writer was called %d times — must not write when policy fails", writer.calls)
	}
}

func TestSetPassword_HashesAndWrites(t *testing.T) {
	t.Parallel()
	hasher := password.Default()
	writer := newFakeWriter()
	svc := &Service{
		PasswordPolicy: &fakePolicy{},
		PasswordHasher: hasher,
		PasswordWriter: writer,
		Logger:         silentLogger(),
	}
	tenantID := uuid.New()
	userID := uuid.New()
	plain := "valid-strong-12c"
	if err := svc.SetPassword(context.Background(), tenantID, userID, plain, password.PolicyContext{Email: "u@x.test"}); err != nil {
		t.Fatalf("SetPassword: %v", err)
	}
	stored := writer.Get(userID)
	if stored == "" {
		t.Fatalf("writer did not record an encoded hash")
	}
	ok, _, err := hasher.Verify(stored, plain)
	if err != nil || !ok {
		t.Fatalf("Verify on stored hash: ok=%v err=%v", ok, err)
	}
}

func TestSetPassword_MissingDependencies(t *testing.T) {
	t.Parallel()
	svc := &Service{}
	err := svc.SetPassword(context.Background(), uuid.New(), uuid.New(), "doesntmatter", password.PolicyContext{})
	if err == nil {
		t.Fatalf("expected error when PasswordPolicy/Hasher are nil")
	}
	svc = &Service{PasswordPolicy: &fakePolicy{}, PasswordHasher: password.Default()}
	err = svc.SetPassword(context.Background(), uuid.New(), uuid.New(), "valid-strong-12", password.PolicyContext{})
	if !errors.Is(err, ErrPasswordWriteUnavailable) {
		t.Fatalf("err=%v want ErrPasswordWriteUnavailable", err)
	}
}

func TestSetPassword_WriteError_Propagates(t *testing.T) {
	t.Parallel()
	writer := newFakeWriter()
	writer.err = errors.New("postgres: timeout")
	svc := &Service{
		PasswordPolicy: &fakePolicy{},
		PasswordHasher: password.Default(),
		PasswordWriter: writer,
	}
	err := svc.SetPassword(context.Background(), uuid.New(), uuid.New(), "valid-strong-12", password.PolicyContext{})
	if err == nil {
		t.Fatalf("expected write error to propagate")
	}
}

// TestLogin_RehashOnNeedsRehash exercises the §3 quiet-upgrade path. A
// stored hash with old params still verifies but Login schedules a
// re-hash + write that lands the new encoded form in the writer. The
// rehash runs in a goroutine, so we wait briefly for it to complete.
func TestLogin_RehashOnNeedsRehash(t *testing.T) {
	t.Parallel()
	tenantID := uuid.MustParse("11111111-1111-4111-8111-111111111111")
	userID := uuid.MustParse("22222222-2222-4222-8222-222222222222")

	// Hash under deliberately-old params.
	oldHasher := &password.Argon2idHasher{
		MemoryKiB: 8 * 1024, Iterations: 1, Parallelism: 1, SaltLen: 16, KeyLen: 32,
	}
	encoded, err := oldHasher.Hash("correct-horse-battery-staple")
	if err != nil {
		t.Fatalf("seed Hash: %v", err)
	}

	writer := newFakeWriter()
	svc := &Service{
		Tenants: fakeResolver{hosts: map[string]uuid.UUID{
			"acme.crm.local": tenantID,
		}},
		Users: fakeUsers{rows: map[string]struct {
			userID uuid.UUID
			hash   string
		}{
			tenantID.String() + "|alice@acme.test": {userID, encoded},
		}},
		Sessions:         newFakeStore(),
		TTL:              time.Hour,
		Logger:           silentLogger(),
		PasswordVerifier: password.Default(),
		PasswordHasher:   password.Default(),
		PasswordWriter:   writer,
	}
	_, err = svc.Login(context.Background(), "acme.crm.local", "alice@acme.test", "correct-horse-battery-staple", net.IPv4(127, 0, 0, 1), "ua", "")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}

	// Wait for the async rehash to land. Bound the wait so the test is
	// deterministic on slow runners but never hangs.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if writer.Get(userID) != "" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	stored := writer.Get(userID)
	if stored == "" {
		t.Fatalf("rehash never wrote — needsRehash signal lost")
	}
	// New encoding MUST verify under the current Hasher with
	// needsRehash=false (it now matches Default()).
	ok, needsRehash, err := password.Default().Verify(stored, "correct-horse-battery-staple")
	if err != nil || !ok {
		t.Fatalf("Verify rehashed: ok=%v err=%v", ok, err)
	}
	if needsRehash {
		t.Fatalf("needsRehash=true on a freshly-rehashed value — params drift")
	}
}

// TestLogin_NoRehashWhenParamsCurrent — when the stored value already
// uses the current params, Login MUST NOT call the writer.
func TestLogin_NoRehashWhenParamsCurrent(t *testing.T) {
	t.Parallel()
	tenantID := uuid.MustParse("11111111-1111-4111-8111-111111111111")
	userID := uuid.MustParse("22222222-2222-4222-8222-222222222222")

	hasher := password.Default()
	encoded, err := hasher.Hash("correct-horse-battery-staple")
	if err != nil {
		t.Fatalf("seed Hash: %v", err)
	}
	writer := newFakeWriter()
	svc := &Service{
		Tenants: fakeResolver{hosts: map[string]uuid.UUID{
			"acme.crm.local": tenantID,
		}},
		Users: fakeUsers{rows: map[string]struct {
			userID uuid.UUID
			hash   string
		}{
			tenantID.String() + "|alice@acme.test": {userID, encoded},
		}},
		Sessions:         newFakeStore(),
		TTL:              time.Hour,
		Logger:           silentLogger(),
		PasswordVerifier: hasher,
		PasswordHasher:   hasher,
		PasswordWriter:   writer,
	}
	if _, err := svc.Login(context.Background(), "acme.crm.local", "alice@acme.test", "correct-horse-battery-staple", nil, "", ""); err != nil {
		t.Fatalf("Login: %v", err)
	}
	// Give the goroutine the same window as the rehash test — but expect
	// nothing.
	time.Sleep(150 * time.Millisecond)
	if writer.calls != 0 {
		t.Fatalf("writer was called %d times — current-param login must not rehash", writer.calls)
	}
}

// TestLogin_RehashFailureIsNonFatal — a writer failure during rehash
// must NOT fail the login; the session is already issued.
func TestLogin_RehashFailureIsNonFatal(t *testing.T) {
	t.Parallel()
	tenantID := uuid.MustParse("11111111-1111-4111-8111-111111111111")
	userID := uuid.MustParse("22222222-2222-4222-8222-222222222222")

	oldHasher := &password.Argon2idHasher{
		MemoryKiB: 8 * 1024, Iterations: 1, Parallelism: 1, SaltLen: 16, KeyLen: 32,
	}
	encoded, _ := oldHasher.Hash("correct-horse-battery-staple")

	writer := newFakeWriter()
	writer.err = errors.New("postgres: deadlock detected")
	calls := atomic.Int32{}
	loggedWriter := &countingWriter{inner: writer, calls: &calls}

	svc := &Service{
		Tenants: fakeResolver{hosts: map[string]uuid.UUID{
			"acme.crm.local": tenantID,
		}},
		Users: fakeUsers{rows: map[string]struct {
			userID uuid.UUID
			hash   string
		}{
			tenantID.String() + "|alice@acme.test": {userID, encoded},
		}},
		Sessions:         newFakeStore(),
		TTL:              time.Hour,
		Logger:           silentLogger(),
		PasswordVerifier: password.Default(),
		PasswordHasher:   password.Default(),
		PasswordWriter:   loggedWriter,
	}
	sess, err := svc.Login(context.Background(), "acme.crm.local", "alice@acme.test", "correct-horse-battery-staple", nil, "", "")
	if err != nil {
		t.Fatalf("Login: rehash-write failure must be non-fatal, got %v", err)
	}
	if sess.UserID != userID {
		t.Fatalf("session UserID wrong")
	}
	// Wait briefly so the goroutine has a chance to call the writer
	// (the goroutine still ran even though it returned an error).
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if calls.Load() > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if calls.Load() == 0 {
		t.Fatalf("rehash goroutine did not call writer — non-fatal path needs the call to happen")
	}
}

type countingWriter struct {
	inner *fakePasswordWriter
	calls *atomic.Int32
}

func (c *countingWriter) UpdatePasswordHash(ctx context.Context, t, u uuid.UUID, e string) error {
	c.calls.Add(1)
	return c.inner.UpdatePasswordHash(ctx, t, u, e)
}
