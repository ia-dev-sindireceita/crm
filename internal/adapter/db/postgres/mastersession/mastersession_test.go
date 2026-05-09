package mastersession_test

// SIN-62385 integration tests for the master_session adapter. Brings
// up a real Postgres via the testpg harness (SIN-62212), applies
// migrations 0004 + 0005 + 0010 against each per-test DB, and drives
// the adapter through the app_master_ops pool so the
// master_ops_audit trigger from migration 0002 fires on every write.
//
// No DB mocking — the Coder agent quality bar (rule 5) forbids it
// for storage-touching code. The TestMain pattern follows the
// existing internal/adapter/db/postgres/withtenant_test.go harness.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pericles-luz/crm/internal/adapter/db/postgres"
	"github.com/pericles-luz/crm/internal/adapter/db/postgres/mastersession"
	"github.com/pericles-luz/crm/internal/adapter/db/postgres/testpg"
	"github.com/pericles-luz/crm/internal/adapter/httpapi/mastermfa"
)

var harness *testpg.Harness

// TestMain spins up a single Postgres cluster for the whole package
// and tears it down at the end. Each test asks for its own
// freshly-migrated DB via harness.DB(t).
func TestMain(m *testing.M) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	h, err := testpg.Start(ctx)
	if err != nil {
		panic("testpg.Start: " + err.Error())
	}
	harness = h
	code := m.Run()
	if err := h.Stop(); err != nil {
		_, _ = os.Stderr.WriteString("testpg.Stop: " + err.Error() + "\n")
	}
	os.Exit(code)
}

// freshDBWithMasterSession brings up a per-test DB with all
// migrations the adapter depends on: 0004 (tenants — required by
// 0005's FK), 0005 (users — references tenant_id) and 0010
// (master_session itself). 0002 (master_ops_audit) is already
// applied by the harness.
func freshDBWithMasterSession(t *testing.T) *testpg.DB {
	t.Helper()
	db := harness.DB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	for _, name := range []string{
		"0004_create_tenant.up.sql",
		"0005_create_users.up.sql",
		"0010_master_session.up.sql",
	} {
		path := filepath.Join(harness.MigrationsDir(), name)
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if _, err := db.AdminPool().Exec(ctx, string(body)); err != nil {
			t.Fatalf("apply %s: %v", name, err)
		}
	}
	return db
}

// seedMasterUser inserts a master user (tenant_id NULL, is_master
// true) and returns its id. Mirrors the helper in
// account_lockout_test.go but is duplicated here so this package
// stays self-contained.
func seedMasterUser(t *testing.T, db *testpg.DB, email string) uuid.UUID {
	t.Helper()
	userID := uuid.New()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := db.AdminPool().Exec(ctx,
		`INSERT INTO users (id, tenant_id, email, password_hash, role, is_master)
		 VALUES ($1, NULL, $2, 'x', 'master', true)`,
		userID, email); err != nil {
		t.Fatalf("insert master user: %v", err)
	}
	return userID
}

// frozenClock returns a func that always reports t. Pinning the
// clock makes Create / Touch / MarkVerified deterministic to the
// microsecond — Postgres timestamptz storage truncates below that.
func frozenClock(t time.Time) func() time.Time { return func() time.Time { return t } }

// ---------------------------------------------------------------------------
// Argument validation — does not need DB.
// ---------------------------------------------------------------------------

func TestNew_RejectsBadInputs(t *testing.T) {
	t.Parallel()
	if _, err := mastersession.New(nil, uuid.New()); !errors.Is(err, postgres.ErrNilPool) {
		t.Fatalf("nil pool err = %v, want ErrNilPool", err)
	}
	// uuid.Nil actor needs a real (non-nil) pool to surface the
	// actor check, so use a stub pool that never connects.
	pool := &pgxpool.Pool{}
	if _, err := mastersession.New(pool, uuid.Nil); !errors.Is(err, postgres.ErrZeroActor) {
		t.Fatalf("uuid.Nil actor err = %v, want ErrZeroActor", err)
	}
}

// ---------------------------------------------------------------------------
// Create / Get / Delete / MarkVerified / Touch — happy path consolidated
// to one DB to keep the suite fast.
// ---------------------------------------------------------------------------

func TestStore_Lifecycle(t *testing.T) {
	db := freshDBWithMasterSession(t)
	actor := seedMasterUser(t, db, "actor@master.test")
	user := seedMasterUser(t, db, "user@master.test")
	ctx := context.Background()

	now := time.Now().UTC().Truncate(time.Microsecond)
	base, err := mastersession.New(db.MasterOpsPool(), actor)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	s := base.WithClock(frozenClock(now))

	t.Run("Create rejects bad inputs", func(t *testing.T) {
		if _, err := s.Create(ctx, uuid.Nil, time.Hour); err == nil {
			t.Fatal("Create(uuid.Nil) returned nil error")
		}
		if _, err := s.Create(ctx, user, 0); err == nil {
			t.Fatal("Create(ttl=0) returned nil error")
		}
		if _, err := s.Create(ctx, user, -time.Second); err == nil {
			t.Fatal("Create(ttl<0) returned nil error")
		}
	})

	var sessionID uuid.UUID
	t.Run("Create inserts row and returns it", func(t *testing.T) {
		sess, err := s.Create(ctx, user, time.Hour)
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		if sess.ID == uuid.Nil {
			t.Fatal("Create returned uuid.Nil id")
		}
		if sess.UserID != user {
			t.Fatalf("UserID = %s, want %s", sess.UserID, user)
		}
		if !sess.CreatedAt.Equal(now) {
			t.Fatalf("CreatedAt = %v, want %v", sess.CreatedAt, now)
		}
		if !sess.ExpiresAt.Equal(now.Add(time.Hour)) {
			t.Fatalf("ExpiresAt = %v, want %v", sess.ExpiresAt, now.Add(time.Hour))
		}
		if sess.MFAVerifiedAt != nil {
			t.Fatalf("MFAVerifiedAt = %v, want nil on freshly-created row", sess.MFAVerifiedAt)
		}
		sessionID = sess.ID
	})

	t.Run("Get returns the row", func(t *testing.T) {
		got, err := s.Get(ctx, sessionID)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if got.ID != sessionID {
			t.Fatalf("ID = %s, want %s", got.ID, sessionID)
		}
		if got.UserID != user {
			t.Fatalf("UserID = %s, want %s", got.UserID, user)
		}
		if !got.ExpiresAt.Equal(now.Add(time.Hour)) {
			t.Fatalf("ExpiresAt = %v, want %v", got.ExpiresAt, now.Add(time.Hour))
		}
		if got.MFAVerifiedAt != nil {
			t.Fatalf("MFAVerifiedAt = %v, want nil pre-MarkVerified", got.MFAVerifiedAt)
		}
	})

	t.Run("Get(uuid.Nil) returns ErrSessionNotFound", func(t *testing.T) {
		if _, err := s.Get(ctx, uuid.Nil); !errors.Is(err, mastermfa.ErrSessionNotFound) {
			t.Fatalf("Get(uuid.Nil) err = %v, want ErrSessionNotFound", err)
		}
	})

	t.Run("Get on missing id returns ErrSessionNotFound", func(t *testing.T) {
		if _, err := s.Get(ctx, uuid.New()); !errors.Is(err, mastermfa.ErrSessionNotFound) {
			t.Fatalf("Get(missing) err = %v, want ErrSessionNotFound", err)
		}
	})

	t.Run("VerifiedAt(uuid.Nil) returns ErrSessionNotFound", func(t *testing.T) {
		if _, err := s.VerifiedAt(ctx, uuid.Nil); !errors.Is(err, mastermfa.ErrSessionNotFound) {
			t.Fatalf("VerifiedAt(uuid.Nil) err = %v, want ErrSessionNotFound", err)
		}
	})

	t.Run("VerifiedAt on row with NULL returns zero time", func(t *testing.T) {
		got, err := s.VerifiedAt(ctx, sessionID)
		if err != nil {
			t.Fatalf("VerifiedAt: %v", err)
		}
		if !got.IsZero() {
			t.Fatalf("VerifiedAt = %v, want zero (NULL mfa_verified_at)", got)
		}
	})

	t.Run("VerifiedAt on missing id returns ErrSessionNotFound", func(t *testing.T) {
		if _, err := s.VerifiedAt(ctx, uuid.New()); !errors.Is(err, mastermfa.ErrSessionNotFound) {
			t.Fatalf("VerifiedAt(missing) err = %v, want ErrSessionNotFound", err)
		}
	})

	t.Run("MarkVerified stamps timestamp visible to VerifiedAt and Get", func(t *testing.T) {
		// Use a slightly-advanced clock so we can observe the write.
		future := now.Add(5 * time.Minute)
		ms := base.WithClock(frozenClock(future))
		got, err := ms.MarkVerified(ctx, sessionID)
		if err != nil {
			t.Fatalf("MarkVerified: %v", err)
		}
		if !got.Equal(future) {
			t.Fatalf("MarkVerified returned %v, want %v", got, future)
		}
		// Visible through VerifiedAt
		va, err := ms.VerifiedAt(ctx, sessionID)
		if err != nil {
			t.Fatalf("VerifiedAt after MarkVerified: %v", err)
		}
		if !va.Equal(future) {
			t.Fatalf("VerifiedAt = %v, want %v", va, future)
		}
		// Visible through Get
		row, err := ms.Get(ctx, sessionID)
		if err != nil {
			t.Fatalf("Get after MarkVerified: %v", err)
		}
		if row.MFAVerifiedAt == nil || !row.MFAVerifiedAt.Equal(future) {
			t.Fatalf("Get.MFAVerifiedAt = %v, want %v", row.MFAVerifiedAt, future)
		}
	})

	t.Run("MarkVerified(uuid.Nil) returns ErrSessionNotFound", func(t *testing.T) {
		if _, err := s.MarkVerified(ctx, uuid.Nil); !errors.Is(err, mastermfa.ErrSessionNotFound) {
			t.Fatalf("MarkVerified(uuid.Nil) err = %v, want ErrSessionNotFound", err)
		}
	})

	t.Run("MarkVerified on missing id returns ErrSessionNotFound", func(t *testing.T) {
		if _, err := s.MarkVerified(ctx, uuid.New()); !errors.Is(err, mastermfa.ErrSessionNotFound) {
			t.Fatalf("MarkVerified(missing) err = %v, want ErrSessionNotFound", err)
		}
	})

	t.Run("Touch extends expires_at", func(t *testing.T) {
		future := now.Add(2 * time.Hour)
		ts := base.WithClock(frozenClock(future))
		if err := ts.Touch(ctx, sessionID, 30*time.Minute); err != nil {
			t.Fatalf("Touch: %v", err)
		}
		row, err := ts.Get(ctx, sessionID)
		if err != nil {
			t.Fatalf("Get after Touch: %v", err)
		}
		want := future.Add(30 * time.Minute)
		if !row.ExpiresAt.Equal(want) {
			t.Fatalf("ExpiresAt after Touch = %v, want %v", row.ExpiresAt, want)
		}
	})

	t.Run("Touch rejects bad inputs", func(t *testing.T) {
		if err := s.Touch(ctx, uuid.Nil, time.Minute); !errors.Is(err, mastermfa.ErrSessionNotFound) {
			t.Fatalf("Touch(uuid.Nil) err = %v, want ErrSessionNotFound", err)
		}
		if err := s.Touch(ctx, sessionID, 0); err == nil {
			t.Fatal("Touch(idleTTL=0) returned nil error")
		}
	})

	t.Run("Touch on missing id returns ErrSessionNotFound", func(t *testing.T) {
		if err := s.Touch(ctx, uuid.New(), time.Minute); !errors.Is(err, mastermfa.ErrSessionNotFound) {
			t.Fatalf("Touch(missing) err = %v, want ErrSessionNotFound", err)
		}
	})

	t.Run("Delete removes the row and is idempotent on missing id", func(t *testing.T) {
		if err := s.Delete(ctx, sessionID); err != nil {
			t.Fatalf("Delete: %v", err)
		}
		if _, err := s.Get(ctx, sessionID); !errors.Is(err, mastermfa.ErrSessionNotFound) {
			t.Fatalf("Get after Delete: %v, want ErrSessionNotFound", err)
		}
		// Idempotent — no error when the row is already gone.
		if err := s.Delete(ctx, sessionID); err != nil {
			t.Fatalf("Delete on missing row: %v", err)
		}
	})

	t.Run("Delete(uuid.Nil) returns error", func(t *testing.T) {
		if err := s.Delete(ctx, uuid.Nil); err == nil {
			t.Fatal("Delete(uuid.Nil) returned nil error")
		}
	})
}

// ---------------------------------------------------------------------------
// Get distinguishes ErrSessionExpired from ErrSessionNotFound.
// ---------------------------------------------------------------------------

func TestStore_GetReturnsErrSessionExpired(t *testing.T) {
	db := freshDBWithMasterSession(t)
	actor := seedMasterUser(t, db, "actor@master.test")
	user := seedMasterUser(t, db, "user@master.test")
	ctx := context.Background()

	// Create a row in the past so Get observes expires_at < now.
	past := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Microsecond)
	base, err := mastersession.New(db.MasterOpsPool(), actor)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	creator := base.WithClock(frozenClock(past))
	sess, err := creator.Create(ctx, user, time.Hour) // expires_at = past + 1h, still in past
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Get with a "now" anchored to the present sees expires_at as past.
	got, err := base.Get(ctx, sess.ID)
	if !errors.Is(err, mastermfa.ErrSessionExpired) {
		t.Fatalf("Get on expired row err = %v, want ErrSessionExpired", err)
	}
	if got.ID != sess.ID {
		t.Fatalf("Get returned id = %s, want %s on expired path (row should be hydrated)", got.ID, sess.ID)
	}
}

// ---------------------------------------------------------------------------
// VerifiedAt is a narrow port: an expired row still returns the
// stored mfa_verified_at (or zero), NOT ErrSessionExpired. The
// upstream session-validity gate (PR2's master-auth middleware) is
// what stops expired sessions; the re-MFA gate is downstream of it.
// ---------------------------------------------------------------------------

func TestStore_VerifiedAtIgnoresExpiry(t *testing.T) {
	db := freshDBWithMasterSession(t)
	actor := seedMasterUser(t, db, "actor@master.test")
	user := seedMasterUser(t, db, "user@master.test")
	ctx := context.Background()

	past := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Microsecond)
	base, err := mastersession.New(db.MasterOpsPool(), actor)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	creator := base.WithClock(frozenClock(past))
	sess, err := creator.Create(ctx, user, time.Hour)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	// Mark verified at past+1m so the stored timestamp is non-zero.
	verifyAt := past.Add(time.Minute)
	verifier := base.WithClock(frozenClock(verifyAt))
	if _, err := verifier.MarkVerified(ctx, sess.ID); err != nil {
		t.Fatalf("MarkVerified: %v", err)
	}

	got, err := base.VerifiedAt(ctx, sess.ID)
	if err != nil {
		t.Fatalf("VerifiedAt on expired row err = %v, want nil", err)
	}
	if !got.Equal(verifyAt) {
		t.Fatalf("VerifiedAt = %v, want %v (expiry must NOT mask the stored value)", got, verifyAt)
	}
}

// ---------------------------------------------------------------------------
// Cross-user isolation — a session created for one master MUST NOT be
// visible to another via a guessed id (the id is a uuid; the test
// proves there is no trans-row leak via the user_id index).
// ---------------------------------------------------------------------------

func TestStore_CrossUserIsolation(t *testing.T) {
	db := freshDBWithMasterSession(t)
	actor := seedMasterUser(t, db, "actor@master.test")
	userA := seedMasterUser(t, db, "a@master.test")
	userB := seedMasterUser(t, db, "b@master.test")
	ctx := context.Background()

	s, err := mastersession.New(db.MasterOpsPool(), actor)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	sessA, err := s.Create(ctx, userA, time.Hour)
	if err != nil {
		t.Fatalf("Create A: %v", err)
	}
	if _, err := s.Create(ctx, userB, time.Hour); err != nil {
		t.Fatalf("Create B: %v", err)
	}

	got, err := s.Get(ctx, sessA.ID)
	if err != nil {
		t.Fatalf("Get A: %v", err)
	}
	if got.UserID != userA {
		t.Fatalf("Get A returned UserID %s, want %s", got.UserID, userA)
	}
}

// ---------------------------------------------------------------------------
// Audit trigger fires on Create / MarkVerified / Touch / Delete.
// ---------------------------------------------------------------------------

func TestStore_AuditRowsWritten(t *testing.T) {
	db := freshDBWithMasterSession(t)
	actor := seedMasterUser(t, db, "actor@master.test")
	user := seedMasterUser(t, db, "user@master.test")
	ctx := context.Background()

	s, err := mastersession.New(db.MasterOpsPool(), actor)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	sess, err := s.Create(ctx, user, time.Hour)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := s.MarkVerified(ctx, sess.ID); err != nil {
		t.Fatalf("MarkVerified: %v", err)
	}
	if err := s.Touch(ctx, sess.ID, time.Hour); err != nil {
		t.Fatalf("Touch: %v", err)
	}
	if err := s.Delete(ctx, sess.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	// Create runs WithMasterOps under userID (the master who just
	// authenticated). The other three run under actor. Every change
	// MUST land in master_ops_audit on the master_session table.
	var n int
	row := db.SuperuserPool().QueryRow(ctx,
		`SELECT count(*) FROM master_ops_audit WHERE target_table = 'master_session'`)
	if err := row.Scan(&n); err != nil {
		t.Fatalf("audit count: %v", err)
	}
	if n < 4 {
		t.Fatalf("audit rows = %d, want >= 4 (create + mark + touch + delete)", n)
	}

	// Audit actor for the Create row MUST be userID; for the others
	// it MUST be actor.
	var createCount int
	if err := db.SuperuserPool().QueryRow(ctx,
		`SELECT count(*) FROM master_ops_audit
		   WHERE target_table = 'master_session'
		     AND actor_user_id = $1`, user).Scan(&createCount); err != nil {
		t.Fatalf("create audit count: %v", err)
	}
	if createCount < 1 {
		t.Fatal("expected at least one master_session audit row attributed to the user (Create)")
	}

	var actorCount int
	if err := db.SuperuserPool().QueryRow(ctx,
		`SELECT count(*) FROM master_ops_audit
		   WHERE target_table = 'master_session'
		     AND actor_user_id = $1`, actor).Scan(&actorCount); err != nil {
		t.Fatalf("actor audit count: %v", err)
	}
	if actorCount < 3 {
		t.Fatalf("expected at least 3 actor-attributed audit rows (mark+touch+delete), got %d", actorCount)
	}
}

// ---------------------------------------------------------------------------
// Migration up/down/up — proves both directions are idempotent and
// round-trip safe. Mirrors master_mfa_migration_test.go.
// ---------------------------------------------------------------------------

func TestMasterSessionMigration_UpDownUp(t *testing.T) {
	db := freshDBWithMasterSession(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if !tableExists(t, ctx, db, "master_session") {
		t.Fatal("master_session missing after initial up")
	}

	downBody := readMigration(t, "0010_master_session.down.sql")
	if _, err := db.AdminPool().Exec(ctx, downBody); err != nil {
		t.Fatalf("apply down: %v", err)
	}
	if tableExists(t, ctx, db, "master_session") {
		t.Fatal("master_session still present after down")
	}

	upBody := readMigration(t, "0010_master_session.up.sql")
	if _, err := db.AdminPool().Exec(ctx, upBody); err != nil {
		t.Fatalf("re-apply up: %v", err)
	}
	if !tableExists(t, ctx, db, "master_session") {
		t.Fatal("master_session missing after re-up")
	}
	// Re-applying up MUST be idempotent (the migration uses CREATE
	// TABLE IF NOT EXISTS / DROP TRIGGER IF EXISTS / etc). A second
	// up on top of itself MUST NOT raise.
	if _, err := db.AdminPool().Exec(ctx, upBody); err != nil {
		t.Fatalf("idempotent re-up: %v", err)
	}
	// Down twice in a row MUST also be idempotent.
	if _, err := db.AdminPool().Exec(ctx, downBody); err != nil {
		t.Fatalf("apply down (idempotent): %v", err)
	}
	if _, err := db.AdminPool().Exec(ctx, downBody); err != nil {
		t.Fatalf("apply down again: %v", err)
	}
}

// TestMasterSession_RuntimeRoleHasNoAccess proves the migration
// revoked all privileges from app_runtime — a tenant-scope code path
// that accidentally reaches for master_session MUST get a permission
// denial, not silent data leakage. Uses the runtime pool directly.
func TestMasterSession_RuntimeRoleHasNoAccess(t *testing.T) {
	db := freshDBWithMasterSession(t)
	_, err := db.RuntimePool().Exec(context.Background(),
		`SELECT 1 FROM master_session LIMIT 1`)
	if err == nil {
		t.Fatal("app_runtime SELECT on master_session succeeded; expected permission denial")
	}
	if !strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("app_runtime SELECT err = %v, want permission denied", err)
	}
}

func tableExists(t *testing.T, ctx context.Context, db *testpg.DB, name string) bool {
	t.Helper()
	var count int
	row := db.SuperuserPool().QueryRow(ctx,
		`SELECT count(*) FROM pg_class c
		   JOIN pg_namespace n ON n.oid = c.relnamespace
		  WHERE c.relname = $1 AND n.nspname = 'public'`, name)
	if err := row.Scan(&count); err != nil {
		t.Fatalf("table-exists probe: %v", err)
	}
	return count == 1
}

func readMigration(t *testing.T, name string) string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(harness.MigrationsDir(), name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(body)
}
