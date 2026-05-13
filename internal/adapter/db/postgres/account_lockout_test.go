package postgres_test

// SIN-62341 integration tests for the TenantLockouts and MasterLockouts
// adapters. Uses the testpg harness (SIN-62212) to bring up a real
// Postgres and apply migrations 0004-0006 + 0008 against each per-test
// DB. Tests are consolidated where they can share a single fresh DB —
// each freshDBWithLockout call costs ~5–20s of migration time on
// constrained hosts, so reusing one DB across related sub-cases keeps
// the package suite under the package-level timeout.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/adapter/db/postgres"
	"github.com/pericles-luz/crm/internal/adapter/db/postgres/testpg"
)

// freshDBWithLockout builds a per-test DB and applies the IAM
// migrations on top of the harness's default 0001-0003 sequence. The
// 0008 migration depends on users + tenants from 0004/0005 and on the
// master_ops_audit trigger from 0002.
func freshDBWithLockout(t *testing.T) *testpg.DB {
	t.Helper()
	db := harness.DB(t)
	// 60s is generous on purpose — applying four migrations against a
	// freshly-created DB on a constrained test host can stretch past
	// 30s when the cluster is also serving the parallel sibling
	// test's setup. Keeps the suite from flaking on slow CI runners.
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	for _, name := range []string{
		"0004_create_tenant.up.sql",
		"0005_create_users.up.sql",
		"0006_create_sessions.up.sql",
		"0008_account_lockout.up.sql",
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

// seedTenantUser inserts (tenant, user) and returns their ids. Caller
// supplies the email so cross-tenant isolation tests can keep the
// emails distinct.
func seedTenantUser(t *testing.T, db *testpg.DB, host, email string) (tenantID, userID uuid.UUID) {
	t.Helper()
	tenantID = uuid.New()
	userID = uuid.New()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := db.AdminPool().Exec(ctx,
		`INSERT INTO tenants (id, name, host) VALUES ($1, $2, $3)`,
		tenantID, host, host); err != nil {
		t.Fatalf("insert tenant: %v", err)
	}
	if _, err := db.AdminPool().Exec(ctx,
		`INSERT INTO users (id, tenant_id, email, password_hash, role) VALUES ($1, $2, $3, 'x', 'agent')`,
		userID, tenantID, email); err != nil {
		t.Fatalf("insert user: %v", err)
	}
	return tenantID, userID
}

// seedMasterUser inserts a master user and returns its id.
func seedMasterUser(t *testing.T, db *testpg.DB, email string) uuid.UUID {
	t.Helper()
	userID := uuid.New()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := db.AdminPool().Exec(ctx,
		`INSERT INTO users (id, tenant_id, email, password_hash, role, is_master) VALUES ($1, NULL, $2, 'x', 'master', true)`,
		userID, email); err != nil {
		t.Fatalf("insert master user: %v", err)
	}
	return userID
}

// ---------------------------------------------------------------------------
// Argument validation — does not need DB.
// ---------------------------------------------------------------------------

func TestNewTenantLockouts_RejectsBadInputs(t *testing.T) {
	t.Parallel()
	if _, err := postgres.NewTenantLockouts(nil, uuid.New()); !errors.Is(err, postgres.ErrNilPool) {
		t.Fatalf("nil pool err = %v, want ErrNilPool", err)
	}
}

func TestNewMasterLockouts_RejectsBadInputs(t *testing.T) {
	t.Parallel()
	if _, err := postgres.NewMasterLockouts(nil, uuid.New()); !errors.Is(err, postgres.ErrNilPool) {
		t.Fatalf("nil pool err = %v, want ErrNilPool", err)
	}
}

// ---------------------------------------------------------------------------
// Tenant scope — happy path consolidated to a single DB.
// ---------------------------------------------------------------------------

func TestTenantLockouts(t *testing.T) {
	db := freshDBWithLockout(t)
	tenantID, userID := seedTenantUser(t, db, "acme.crm.local", "alice@acme.test")
	now := time.Now().UTC().Truncate(time.Microsecond)
	clock := func() time.Time { return now }

	t.Run("rejects uuid.Nil tenant", func(t *testing.T) {
		l, err := postgres.NewTenantLockouts(db.RuntimePool(), uuid.Nil)
		if !errors.Is(err, postgres.ErrZeroTenant) {
			t.Fatalf("NewTenantLockouts(uuid.Nil) err = %v, want ErrZeroTenant", err)
		}
		if l != nil {
			t.Fatal("expected nil adapter on error")
		}
	})

	base, err := postgres.NewTenantLockouts(db.RuntimePool(), tenantID)
	if err != nil {
		t.Fatalf("NewTenantLockouts: %v", err)
	}
	l := base.WithClock(clock)
	ctx := context.Background()

	t.Run("Lock rejects bad inputs", func(t *testing.T) {
		if err := l.Lock(ctx, uuid.Nil, now.Add(time.Minute), "x"); err == nil {
			t.Fatal("Lock(uuid.Nil) returned nil error")
		}
		if err := l.Lock(ctx, uuid.New(), time.Time{}, "x"); err == nil {
			t.Fatal("Lock(zero time) returned nil error")
		}
	})

	t.Run("clear on missing row is noop", func(t *testing.T) {
		if err := l.Clear(ctx, userID); err != nil {
			t.Fatalf("Clear on missing row: %v", err)
		}
	})

	t.Run("not locked initially", func(t *testing.T) {
		locked, until, err := l.IsLocked(ctx, userID)
		if err != nil {
			t.Fatalf("IsLocked: %v", err)
		}
		if locked || !until.IsZero() {
			t.Fatalf("initial: locked=%v until=%v, want false / zero", locked, until)
		}
	})

	t.Run("Lock then IsLocked", func(t *testing.T) {
		want := now.Add(15 * time.Minute)
		if err := l.Lock(ctx, userID, want, "10 failed login attempts"); err != nil {
			t.Fatalf("Lock: %v", err)
		}
		locked, until, err := l.IsLocked(ctx, userID)
		if err != nil {
			t.Fatalf("IsLocked: %v", err)
		}
		if !locked {
			t.Fatal("locked = false, want true")
		}
		if !until.Equal(want) {
			t.Fatalf("until = %v, want %v", until, want)
		}
	})

	t.Run("re-lock extends locked_until via upsert", func(t *testing.T) {
		extended := now.Add(time.Hour)
		if err := l.Lock(ctx, userID, extended, "extended"); err != nil {
			t.Fatalf("Lock extend: %v", err)
		}
		locked, until, err := l.IsLocked(ctx, userID)
		if err != nil {
			t.Fatalf("IsLocked: %v", err)
		}
		if !locked || !until.Equal(extended) {
			t.Fatalf("after extend: locked=%v until=%v, want true %v", locked, until, extended)
		}
	})

	t.Run("Clear then IsLocked", func(t *testing.T) {
		if err := l.Clear(ctx, userID); err != nil {
			t.Fatalf("Clear: %v", err)
		}
		locked, _, err := l.IsLocked(ctx, userID)
		if err != nil {
			t.Fatalf("IsLocked: %v", err)
		}
		if locked {
			t.Fatal("locked = true after Clear")
		}
	})

	t.Run("expired row reads as unlocked", func(t *testing.T) {
		if err := l.Lock(ctx, userID, now.Add(-time.Minute), "stale"); err != nil {
			t.Fatalf("Lock: %v", err)
		}
		locked, until, err := l.IsLocked(ctx, userID)
		if err != nil {
			t.Fatalf("IsLocked: %v", err)
		}
		if locked || !until.IsZero() {
			t.Fatalf("expired: locked=%v until=%v", locked, until)
		}
		// Clean up so the next assertion starts from a clean slate.
		if err := l.Clear(ctx, userID); err != nil {
			t.Fatalf("Clear cleanup: %v", err)
		}
	})
}

func TestTenantLockouts_RLSIsolatesAcrossTenants(t *testing.T) {
	db := freshDBWithLockout(t)
	tenantA, userA := seedTenantUser(t, db, "a.crm.local", "alice@a.test")
	tenantB, userB := seedTenantUser(t, db, "b.crm.local", "bob@b.test")

	now := time.Now().UTC().Truncate(time.Microsecond)
	until := now.Add(time.Hour)

	la, err := postgres.NewTenantLockouts(db.RuntimePool(), tenantA)
	if err != nil {
		t.Fatalf("NewTenantLockouts A: %v", err)
	}
	lb, err := postgres.NewTenantLockouts(db.RuntimePool(), tenantB)
	if err != nil {
		t.Fatalf("NewTenantLockouts B: %v", err)
	}
	ctx := context.Background()

	if err := la.Lock(ctx, userA, until, "A locks A"); err != nil {
		t.Fatalf("la.Lock: %v", err)
	}

	// la sees the row.
	locked, _, err := la.IsLocked(ctx, userA)
	if err != nil || !locked {
		t.Fatalf("la.IsLocked(userA) = (%v, %v), want true", locked, err)
	}

	// lb cannot see userA's row (RLS).
	locked, _, err = lb.IsLocked(ctx, userA)
	if err != nil {
		t.Fatalf("lb.IsLocked(userA): %v", err)
	}
	if locked {
		t.Fatal("lb saw userA's lockout — RLS leak")
	}

	// lb cannot lock userA — INSERT WITH CHECK rejects cross-tenant
	// writes (the adapter writes tenant_id = tenantB but the
	// users.tenant_id FK to userA is tenantA → the WITH CHECK clause
	// refuses).
	if err := lb.Lock(ctx, userA, until, "B locks A"); err == nil {
		t.Fatal("lb.Lock(userA) succeeded — RLS write leak")
	}

	// Symmetric: lb writes for userB succeed and la doesn't see them.
	if err := lb.Lock(ctx, userB, until, "B locks B"); err != nil {
		t.Fatalf("lb.Lock(userB): %v", err)
	}
	locked, _, err = la.IsLocked(ctx, userB)
	if err != nil {
		t.Fatalf("la.IsLocked(userB): %v", err)
	}
	if locked {
		t.Fatal("la saw userB's lockout — RLS leak")
	}
}

// ---------------------------------------------------------------------------
// Master scope — happy path + cross-scope isolation + audit row.
// ---------------------------------------------------------------------------

func TestMasterLockouts(t *testing.T) {
	db := freshDBWithLockout(t)
	masterUser := seedMasterUser(t, db, "ops@master.test")
	actor := seedMasterUser(t, db, "actor@master.test")

	now := time.Now().UTC().Truncate(time.Microsecond)
	base, err := postgres.NewMasterLockouts(db.MasterOpsPool(), actor)
	if err != nil {
		t.Fatalf("NewMasterLockouts: %v", err)
	}
	l := base.WithClock(func() time.Time { return now })
	ctx := context.Background()

	t.Run("rejects uuid.Nil actor", func(t *testing.T) {
		if _, err := postgres.NewMasterLockouts(db.MasterOpsPool(), uuid.Nil); !errors.Is(err, postgres.ErrZeroActor) {
			t.Fatalf("uuid.Nil actor err = %v, want ErrZeroActor", err)
		}
	})

	t.Run("Lock rejects bad inputs", func(t *testing.T) {
		if err := l.Lock(ctx, uuid.Nil, now.Add(time.Minute), "x"); err == nil {
			t.Fatal("Lock(uuid.Nil) returned nil error")
		}
		if err := l.Lock(ctx, uuid.New(), time.Time{}, "x"); err == nil {
			t.Fatal("Lock(zero time) returned nil error")
		}
	})

	t.Run("Clear on missing row is noop", func(t *testing.T) {
		if err := l.Clear(ctx, masterUser); err != nil {
			t.Fatalf("Clear on missing row: %v", err)
		}
	})

	t.Run("Lock then IsLocked then Clear", func(t *testing.T) {
		want := now.Add(30 * time.Minute)
		if err := l.Lock(ctx, masterUser, want, "5 master login failures"); err != nil {
			t.Fatalf("Lock: %v", err)
		}
		locked, until, err := l.IsLocked(ctx, masterUser)
		if err != nil {
			t.Fatalf("IsLocked: %v", err)
		}
		if !locked || !until.Equal(want) {
			t.Fatalf("locked=%v until=%v, want true %v", locked, until, want)
		}
		if err := l.Clear(ctx, masterUser); err != nil {
			t.Fatalf("Clear: %v", err)
		}
		locked, _, err = l.IsLocked(ctx, masterUser)
		if err != nil {
			t.Fatalf("IsLocked after clear: %v", err)
		}
		if locked {
			t.Fatal("locked = true after Clear")
		}
	})
}

func TestMasterLockouts_DoesNotSeeTenantRows(t *testing.T) {
	db := freshDBWithLockout(t)
	tenantID, tenantUser := seedTenantUser(t, db, "acme.crm.local", "alice@acme.test")
	actor := seedMasterUser(t, db, "actor@master.test")

	tl, err := postgres.NewTenantLockouts(db.RuntimePool(), tenantID)
	if err != nil {
		t.Fatalf("NewTenantLockouts: %v", err)
	}
	if err := tl.Lock(context.Background(), tenantUser, time.Now().Add(time.Hour), "x"); err != nil {
		t.Fatalf("tenant Lock: %v", err)
	}

	ml, err := postgres.NewMasterLockouts(db.MasterOpsPool(), actor)
	if err != nil {
		t.Fatalf("NewMasterLockouts: %v", err)
	}
	// Master IsLocked is scoped to tenant_id IS NULL, so a tenant row
	// for the same user_id reads as not-locked from the master side.
	locked, _, err := ml.IsLocked(context.Background(), tenantUser)
	if err != nil {
		t.Fatalf("master IsLocked: %v", err)
	}
	if locked {
		t.Fatal("master saw a tenant lockout row through the master scope")
	}
}

func TestMasterLockouts_AuditRowsWritten(t *testing.T) {
	db := freshDBWithLockout(t)
	masterUser := seedMasterUser(t, db, "victim@master.test")
	actor := seedMasterUser(t, db, "actor@master.test")

	l, err := postgres.NewMasterLockouts(db.MasterOpsPool(), actor)
	if err != nil {
		t.Fatalf("NewMasterLockouts: %v", err)
	}
	if err := l.Lock(context.Background(), masterUser, time.Now().Add(time.Hour), "audit me"); err != nil {
		t.Fatalf("Lock: %v", err)
	}

	// Inspect the audit log via the superuser pool — RLS does not
	// apply, but the actor id MUST appear.
	var n int
	row := db.SuperuserPool().QueryRow(context.Background(),
		`SELECT count(*) FROM master_ops_audit WHERE actor_user_id = $1 AND target_table = 'account_lockout'`,
		actor)
	if err := row.Scan(&n); err != nil {
		t.Fatalf("audit count: %v", err)
	}
	if n == 0 {
		t.Fatal("expected at least one master_ops_audit row for the lock write")
	}
}
