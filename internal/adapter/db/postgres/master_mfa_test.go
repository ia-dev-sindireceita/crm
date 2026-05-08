package postgres_test

// SIN-62342 integration tests for the master_mfa and
// master_recovery_code adapters. Uses the testpg harness to bring up a
// real Postgres and apply migrations 0004 + 0005 + 0009 against each
// per-test DB. Master operations connect through MasterOpsPool() so
// the master_ops_audit trigger from migration 0002 fires on every
// statement.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/adapter/db/postgres"
	"github.com/pericles-luz/crm/internal/iam/mfa"
)

// ---------------------------------------------------------------------------
// Argument validation — does not need DB.
// ---------------------------------------------------------------------------

func TestNewMasterMFA_RejectsBadInputs(t *testing.T) {
	t.Parallel()
	if _, err := postgres.NewMasterMFA(nil, uuid.New()); !errors.Is(err, postgres.ErrNilPool) {
		t.Fatalf("nil pool err = %v, want ErrNilPool", err)
	}
}

func TestNewMasterRecoveryCodes_RejectsBadInputs(t *testing.T) {
	t.Parallel()
	if _, err := postgres.NewMasterRecoveryCodes(nil, uuid.New()); !errors.Is(err, postgres.ErrNilPool) {
		t.Fatalf("nil pool err = %v, want ErrNilPool", err)
	}
}

// ---------------------------------------------------------------------------
// MasterMFA — happy path consolidated to one DB to keep the suite fast.
// ---------------------------------------------------------------------------

func TestMasterMFA_StoreLoadVerifyReenroll(t *testing.T) {
	db := freshDBWithMasterMFA(t)
	actor := seedMasterUser(t, db, "ops-actor@example.test")
	target := seedMasterUser(t, db, "ops-target@example.test")
	ctx := context.Background()

	t.Run("rejects uuid.Nil actor", func(t *testing.T) {
		if _, err := postgres.NewMasterMFA(db.MasterOpsPool(), uuid.Nil); !errors.Is(err, postgres.ErrZeroActor) {
			t.Fatalf("err = %v, want ErrZeroActor", err)
		}
	})

	a, err := postgres.NewMasterMFA(db.MasterOpsPool(), actor)
	if err != nil {
		t.Fatalf("NewMasterMFA: %v", err)
	}

	t.Run("StoreSeed rejects bad inputs", func(t *testing.T) {
		if err := a.StoreSeed(ctx, uuid.Nil, []byte("ct")); err == nil {
			t.Fatal("StoreSeed(nil userID) returned nil error")
		}
		if err := a.StoreSeed(ctx, target, nil); err == nil {
			t.Fatal("StoreSeed(empty ciphertext) returned nil error")
		}
	})

	t.Run("LoadSeed on missing row returns mfa.ErrNotEnrolled", func(t *testing.T) {
		_, err := a.LoadSeed(ctx, target)
		if !errors.Is(err, mfa.ErrNotEnrolled) {
			t.Fatalf("err = %v, want mfa.ErrNotEnrolled", err)
		}
	})

	want := []byte("opaque-ciphertext-from-aes-gcm")
	t.Run("StoreSeed inserts new row", func(t *testing.T) {
		if err := a.StoreSeed(ctx, target, want); err != nil {
			t.Fatalf("StoreSeed: %v", err)
		}
		got, err := a.LoadSeed(ctx, target)
		if err != nil {
			t.Fatalf("LoadSeed: %v", err)
		}
		if string(got) != string(want) {
			t.Fatalf("ciphertext: got %q want %q", got, want)
		}
	})

	t.Run("StoreSeed upsert clears reenroll_required and last_verified_at", func(t *testing.T) {
		if err := a.MarkReenrollRequired(ctx, target); err != nil {
			t.Fatalf("MarkReenrollRequired: %v", err)
		}
		if err := a.MarkVerified(ctx, target); err != nil {
			t.Fatalf("MarkVerified: %v", err)
		}
		// Re-store with a fresh ciphertext (a regenerate scenario).
		fresh := []byte("regenerated-ciphertext")
		if err := a.StoreSeed(ctx, target, fresh); err != nil {
			t.Fatalf("StoreSeed regenerate: %v", err)
		}
		// Verify both columns reset.
		var reenroll bool
		var lastVerified *time.Time
		err := db.SuperuserPool().QueryRow(ctx,
			`SELECT reenroll_required, last_verified_at
			   FROM master_mfa WHERE user_id = $1`, target).Scan(&reenroll, &lastVerified)
		if err != nil {
			t.Fatalf("read row: %v", err)
		}
		if reenroll {
			t.Errorf("reenroll_required: got true want false (cleared by upsert)")
		}
		if lastVerified != nil {
			t.Errorf("last_verified_at: got %v want NULL (cleared by upsert)", lastVerified)
		}
	})

	t.Run("MarkVerified updates timestamp", func(t *testing.T) {
		if err := a.MarkVerified(ctx, target); err != nil {
			t.Fatalf("MarkVerified: %v", err)
		}
		var lastVerified *time.Time
		err := db.SuperuserPool().QueryRow(ctx,
			`SELECT last_verified_at FROM master_mfa WHERE user_id = $1`, target).Scan(&lastVerified)
		if err != nil {
			t.Fatalf("read row: %v", err)
		}
		if lastVerified == nil {
			t.Fatal("last_verified_at: got NULL want timestamp")
		}
	})

	t.Run("MarkReenrollRequired sets the flag", func(t *testing.T) {
		if err := a.MarkReenrollRequired(ctx, target); err != nil {
			t.Fatalf("MarkReenrollRequired: %v", err)
		}
		var reenroll bool
		err := db.SuperuserPool().QueryRow(ctx,
			`SELECT reenroll_required FROM master_mfa WHERE user_id = $1`, target).Scan(&reenroll)
		if err != nil {
			t.Fatalf("read row: %v", err)
		}
		if !reenroll {
			t.Fatal("reenroll_required: got false want true")
		}
	})

	t.Run("MasterOps audit trail fires on store", func(t *testing.T) {
		// Every WithMasterOps transaction writes a session_open row plus
		// row-level entries for INSERT/UPDATE. We just assert the actor
		// is named in at least one master_mfa-targeted audit row, which
		// is the load-bearing security property.
		var count int
		err := db.SuperuserPool().QueryRow(ctx,
			`SELECT count(*) FROM master_ops_audit
			  WHERE target_table = 'master_mfa' AND actor_user_id = $1`, actor).Scan(&count)
		if err != nil {
			t.Fatalf("audit probe: %v", err)
		}
		if count == 0 {
			t.Fatal("expected master_ops_audit rows for master_mfa, got 0")
		}
	})
}

// ---------------------------------------------------------------------------
// MasterRecoveryCodes — covers AC #2, #3 (recovery codes only appear
// once, regenerate invalidates set in bulk, single-use semantics).
// ---------------------------------------------------------------------------

func TestMasterRecoveryCodes_FullLifecycle(t *testing.T) {
	db := freshDBWithMasterMFA(t)
	actor := seedMasterUser(t, db, "rc-actor@example.test")
	target := seedMasterUser(t, db, "rc-target@example.test")
	ctx := context.Background()

	a, err := postgres.NewMasterRecoveryCodes(db.MasterOpsPool(), actor)
	if err != nil {
		t.Fatalf("NewMasterRecoveryCodes: %v", err)
	}

	hashes := func(prefix string, n int) []string {
		out := make([]string, n)
		for i := 0; i < n; i++ {
			// Real callers feed the Argon2id encoding; this synthetic value is
			// only ever read back by the SELECT below.
			out[i] = "argon2id$v=19$" + prefix + "-" + uuid.New().String()
		}
		return out
	}

	t.Run("InsertHashes rejects bad inputs", func(t *testing.T) {
		if err := a.InsertHashes(ctx, uuid.Nil, []string{"x"}); err == nil {
			t.Fatal("InsertHashes(nil userID) returned nil error")
		}
		if err := a.InsertHashes(ctx, target, nil); err == nil {
			t.Fatal("InsertHashes(empty hashes) returned nil error")
		}
	})

	t.Run("ListActive on empty returns nothing", func(t *testing.T) {
		rows, err := a.ListActive(ctx, target)
		if err != nil {
			t.Fatalf("ListActive: %v", err)
		}
		if len(rows) != 0 {
			t.Fatalf("len = %d want 0", len(rows))
		}
	})

	t.Run("InsertHashes + ListActive returns all 10 active rows", func(t *testing.T) {
		if err := a.InsertHashes(ctx, target, hashes("h1", 10)); err != nil {
			t.Fatalf("InsertHashes: %v", err)
		}
		rows, err := a.ListActive(ctx, target)
		if err != nil {
			t.Fatalf("ListActive: %v", err)
		}
		if len(rows) != 10 {
			t.Fatalf("len = %d want 10", len(rows))
		}
		for i, r := range rows {
			if r.ID == uuid.Nil {
				t.Errorf("row %d: ID is nil", i)
			}
			if r.Hash == "" {
				t.Errorf("row %d: hash is empty", i)
			}
		}
	})

	var firstRow mfa.RecoveryCodeRecord
	t.Run("MarkConsumed removes that single row from active set", func(t *testing.T) {
		rows, err := a.ListActive(ctx, target)
		if err != nil {
			t.Fatalf("ListActive: %v", err)
		}
		firstRow = rows[0]
		if err := a.MarkConsumed(ctx, firstRow.ID); err != nil {
			t.Fatalf("MarkConsumed: %v", err)
		}
		rows2, err := a.ListActive(ctx, target)
		if err != nil {
			t.Fatalf("ListActive after consume: %v", err)
		}
		if len(rows2) != 9 {
			t.Fatalf("len = %d want 9 (one consumed)", len(rows2))
		}
		for _, r := range rows2 {
			if r.ID == firstRow.ID {
				t.Fatalf("consumed row %s still in active list", firstRow.ID)
			}
		}
	})

	t.Run("MarkConsumed is idempotent (preserves first consumed_at)", func(t *testing.T) {
		// Read the timestamp.
		var first time.Time
		if err := db.SuperuserPool().QueryRow(ctx,
			`SELECT consumed_at FROM master_recovery_code WHERE id = $1`,
			firstRow.ID).Scan(&first); err != nil {
			t.Fatalf("read consumed_at: %v", err)
		}
		// Sleep for a measurable interval so a clobbering UPDATE would
		// produce a different now() value.
		time.Sleep(10 * time.Millisecond)
		if err := a.MarkConsumed(ctx, firstRow.ID); err != nil {
			t.Fatalf("MarkConsumed second call: %v", err)
		}
		var second time.Time
		if err := db.SuperuserPool().QueryRow(ctx,
			`SELECT consumed_at FROM master_recovery_code WHERE id = $1`,
			firstRow.ID).Scan(&second); err != nil {
			t.Fatalf("read consumed_at after second call: %v", err)
		}
		if !first.Equal(second) {
			t.Fatalf("consumed_at clobbered: first=%v second=%v", first, second)
		}
	})

	t.Run("MarkConsumed rejects nil id", func(t *testing.T) {
		if err := a.MarkConsumed(ctx, uuid.Nil); err == nil {
			t.Fatal("MarkConsumed(uuid.Nil) returned nil error")
		}
	})

	t.Run("InvalidateAll mass-clears the remaining 9 active codes", func(t *testing.T) {
		n, err := a.InvalidateAll(ctx, target)
		if err != nil {
			t.Fatalf("InvalidateAll: %v", err)
		}
		if n != 9 {
			t.Fatalf("affected = %d want 9", n)
		}
		rows, err := a.ListActive(ctx, target)
		if err != nil {
			t.Fatalf("ListActive after invalidate: %v", err)
		}
		if len(rows) != 0 {
			t.Fatalf("len = %d want 0", len(rows))
		}
	})

	t.Run("InvalidateAll on already-empty active set returns 0", func(t *testing.T) {
		n, err := a.InvalidateAll(ctx, target)
		if err != nil {
			t.Fatalf("InvalidateAll empty: %v", err)
		}
		if n != 0 {
			t.Fatalf("affected = %d want 0", n)
		}
	})

	t.Run("InvalidateAll rejects nil userID", func(t *testing.T) {
		if _, err := a.InvalidateAll(ctx, uuid.Nil); err == nil {
			t.Fatal("InvalidateAll(uuid.Nil) returned nil error")
		}
	})

	t.Run("Regenerate path: insert fresh 10 codes after invalidate", func(t *testing.T) {
		if err := a.InsertHashes(ctx, target, hashes("h2", 10)); err != nil {
			t.Fatalf("InsertHashes regenerate: %v", err)
		}
		rows, err := a.ListActive(ctx, target)
		if err != nil {
			t.Fatalf("ListActive regenerate: %v", err)
		}
		if len(rows) != 10 {
			t.Fatalf("len = %d want 10", len(rows))
		}
		// The old set's IDs MUST NOT appear in the new active list — old
		// rows are still in the table but consumed_at IS NOT NULL.
		var oldActiveCount int
		if err := db.SuperuserPool().QueryRow(ctx,
			`SELECT count(*) FROM master_recovery_code
			  WHERE user_id = $1 AND consumed_at IS NULL`, target).Scan(&oldActiveCount); err != nil {
			t.Fatalf("count active: %v", err)
		}
		if oldActiveCount != 10 {
			t.Fatalf("active rows = %d want 10 (regen set only)", oldActiveCount)
		}
	})
}
