package usermfa

import (
	"context"
	"errors"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	pg "github.com/pericles-luz/crm/internal/adapter/db/postgres"
	"github.com/pericles-luz/crm/internal/iam"
)

func TestPendingsBridgeRoundTrip(t *testing.T) {
	t.Parallel()
	inner := &fakePendingsInner{}
	b := NewPendingsBridge(inner)
	now := time.Date(2030, 1, 1, 12, 0, 0, 0, time.UTC)
	row := pg.PendingMFASession{
		ID:        uuid.New(),
		UserID:    uuid.New(),
		TenantID:  uuid.New(),
		ExpiresAt: now,
		NextPath:  "/x",
	}
	inner.created = row

	gotCreate, err := b.Create(context.Background(), row.UserID, time.Minute, "/x")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if gotCreate.ID != row.ID || gotCreate.UserID != row.UserID || gotCreate.NextPath != "/x" {
		t.Fatalf("Create mismatch: %#v", gotCreate)
	}

	inner.fetched = map[uuid.UUID]pg.PendingMFASession{row.ID: row}
	gotGet, err := b.Get(context.Background(), row.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if gotGet.UserID != row.UserID {
		t.Fatalf("Get UserID mismatch: %s != %s", gotGet.UserID, row.UserID)
	}

	if err := b.Delete(context.Background(), row.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if !inner.deleted[row.ID] {
		t.Fatalf("expected Delete to propagate")
	}
}

func TestPendingsBridgeErrors(t *testing.T) {
	t.Parallel()
	wantErr := errors.New("boom")
	inner := &fakePendingsInner{err: wantErr}
	b := NewPendingsBridge(inner)
	if _, err := b.Create(context.Background(), uuid.New(), time.Minute, ""); !errors.Is(err, wantErr) {
		t.Fatalf("Create err: %v", err)
	}
	if _, err := b.Get(context.Background(), uuid.New()); !errors.Is(err, wantErr) {
		t.Fatalf("Get err: %v", err)
	}
	if err := b.Delete(context.Background(), uuid.New()); !errors.Is(err, wantErr) {
		t.Fatalf("Delete err: %v", err)
	}
}

func TestRequirementsBridgePropagates(t *testing.T) {
	t.Parallel()
	inner := &fakeRequirementsInner{row: pg.UserMFARequirement{Role: "admin", TOTPRequired: true, TOTPEnrolled: true}}
	b := NewRequirementsBridge(inner)
	got, err := b.Load(context.Background(), uuid.New())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !got.TOTPRequired || !got.TOTPEnrolled {
		t.Fatalf("Load mismatch: %#v", got)
	}
}

func TestRequirementsBridgeError(t *testing.T) {
	t.Parallel()
	wantErr := errors.New("db")
	inner := &fakeRequirementsInner{err: wantErr}
	b := NewRequirementsBridge(inner)
	if _, err := b.Load(context.Background(), uuid.New()); !errors.Is(err, wantErr) {
		t.Fatalf("Load err: %v", err)
	}
}

func TestNewTenantSessionMinterValidates(t *testing.T) {
	t.Parallel()
	if _, err := NewTenantSessionMinter(nil, time.Hour); err == nil {
		t.Fatalf("expected nil error for nil sessions")
	}
}

func TestTenantSessionMinterDefaults(t *testing.T) {
	t.Parallel()
	store := &fakeSessionStore{}
	m, err := NewTenantSessionMinter(store, 0)
	if err != nil {
		t.Fatalf("NewTenantSessionMinter: %v", err)
	}
	if m.ttl != DefaultSessionTTL {
		t.Fatalf("expected DefaultSessionTTL got %v", m.ttl)
	}
}

func TestTenantSessionMinterMintsRow(t *testing.T) {
	t.Parallel()
	store := &fakeSessionStore{}
	m, err := NewTenantSessionMinter(store, time.Hour)
	if err != nil {
		t.Fatalf("NewTenantSessionMinter: %v", err)
	}
	frozen := time.Date(2030, 1, 1, 12, 0, 0, 0, time.UTC)
	m.WithClock(func() time.Time { return frozen })
	tenantID := uuid.New()
	userID := uuid.New()
	sess, err := m.MintTenantSession(context.Background(), tenantID, userID, "127.0.0.1", "ua")
	if err != nil {
		t.Fatalf("MintTenantSession: %v", err)
	}
	if sess.UserID != userID || sess.TenantID != tenantID {
		t.Fatalf("mismatch: %#v", sess)
	}
	if sess.ExpiresAt.Sub(sess.CreatedAt) != time.Hour {
		t.Fatalf("ttl: want 1h got %v", sess.ExpiresAt.Sub(sess.CreatedAt))
	}
	if sess.Role != iam.RoleTenantGerente {
		t.Fatalf("role: want %s got %s", iam.RoleTenantGerente, sess.Role)
	}
	if sess.CSRFToken == "" {
		t.Fatalf("CSRFToken must be set")
	}
	if !store.called {
		t.Fatalf("expected SessionStore.Create to be called")
	}
}

func TestTenantSessionMinterCreateError(t *testing.T) {
	t.Parallel()
	store := &fakeSessionStore{err: errors.New("db")}
	m, err := NewTenantSessionMinter(store, time.Hour)
	if err != nil {
		t.Fatalf("NewTenantSessionMinter: %v", err)
	}
	if _, err := m.MintTenantSession(context.Background(), uuid.New(), uuid.New(), "", ""); err == nil {
		t.Fatalf("expected Create error to propagate")
	}
}

func TestNewPostgresUserLabelValidates(t *testing.T) {
	t.Parallel()
	if _, err := NewPostgresUserLabel(nil); err == nil {
		t.Fatalf("expected error for nil pool")
	}
}

func TestParseIPCases(t *testing.T) {
	t.Parallel()
	if got := parseIP(""); got != nil {
		t.Fatalf("empty: want nil got %v", got)
	}
	if got := parseIP("127.0.0.1"); got == nil {
		t.Fatalf("valid IP returned nil")
	}
	if got := parseIP("not-an-ip"); got != nil {
		t.Fatalf("invalid IP returned %v", got)
	}
}

// ---- fakes ----

type fakePendingsInner struct {
	mu      sync.Mutex
	created pg.PendingMFASession
	fetched map[uuid.UUID]pg.PendingMFASession
	deleted map[uuid.UUID]bool
	err     error
}

func (f *fakePendingsInner) Create(_ context.Context, _ uuid.UUID, _ time.Duration, _ string) (pg.PendingMFASession, error) {
	if f.err != nil {
		return pg.PendingMFASession{}, f.err
	}
	return f.created, nil
}
func (f *fakePendingsInner) Get(_ context.Context, id uuid.UUID) (pg.PendingMFASession, error) {
	if f.err != nil {
		return pg.PendingMFASession{}, f.err
	}
	if row, ok := f.fetched[id]; ok {
		return row, nil
	}
	return pg.PendingMFASession{}, errors.New("not found")
}
func (f *fakePendingsInner) Delete(_ context.Context, id uuid.UUID) error {
	if f.err != nil {
		return f.err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.deleted == nil {
		f.deleted = map[uuid.UUID]bool{}
	}
	f.deleted[id] = true
	return nil
}

type fakeRequirementsInner struct {
	row pg.UserMFARequirement
	err error
}

func (f *fakeRequirementsInner) Load(_ context.Context, _ uuid.UUID) (pg.UserMFARequirement, error) {
	if f.err != nil {
		return pg.UserMFARequirement{}, f.err
	}
	return f.row, nil
}

type fakeSessionStore struct {
	called bool
	err    error
}

func (f *fakeSessionStore) Create(_ context.Context, _ iam.Session) error {
	f.called = true
	return f.err
}
func (f *fakeSessionStore) Get(_ context.Context, _, _ uuid.UUID) (iam.Session, error) {
	return iam.Session{}, nil
}
func (f *fakeSessionStore) Delete(_ context.Context, _, _ uuid.UUID) error { return nil }
func (f *fakeSessionStore) DeleteExpired(_ context.Context, _ uuid.UUID) (int64, error) {
	return 0, nil
}
func (f *fakeSessionStore) Touch(_ context.Context, _, _ uuid.UUID, _ time.Time) error { return nil }

// Suppress unused-import warning for net.IP usage in the helper.
var _ = net.IP(nil)
