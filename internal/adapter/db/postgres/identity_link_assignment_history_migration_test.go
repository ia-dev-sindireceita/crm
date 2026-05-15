package postgres_test

// SIN-62790 (Fase 2 F2-03): 0092_identity_link_assignment_history.
// Mirrors the SIN-62724 0088_inbox_contacts pattern: applies 0004 → 0005
// → 0088 → 0092 on top of the harness default 0001-0003. Covers up/down
// round-trip, backfill, indexes named in the AC, and RLS posture.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	postgresadapter "github.com/pericles-luz/crm/internal/adapter/db/postgres"
	"github.com/pericles-luz/crm/internal/adapter/db/postgres/testpg"
)

var identityF2TableNames = []string{
	"identity",
	"contact_identity_link",
	"assignment_history",
}

// applyChain runs the named migrations as app_admin in order.
func applyChain(t *testing.T, ctx context.Context, db *testpg.DB, files ...string) {
	t.Helper()
	for _, name := range files {
		body, err := os.ReadFile(filepath.Join(harness.MigrationsDir(), name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if _, err := db.AdminPool().Exec(ctx, string(body)); err != nil {
			t.Fatalf("apply %s: %v", name, err)
		}
	}
}

func freshDBWithIdentityF2(t *testing.T) (*testpg.DB, context.Context) {
	t.Helper()
	db := harness.DB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	applyChain(t, ctx, db,
		"0004_create_tenant.up.sql",
		"0005_create_users.up.sql",
		"0088_inbox_contacts.up.sql",
		"0092_identity_link_assignment_history.up.sql",
	)
	return db, ctx
}

func identityF2TablesPresent(t *testing.T, ctx context.Context, db *testpg.DB) int {
	t.Helper()
	var count int
	if err := db.SuperuserPool().QueryRow(ctx,
		`SELECT count(*) FROM pg_class c
		   JOIN pg_namespace n ON n.oid = c.relnamespace
		  WHERE c.relname = ANY($1) AND n.nspname = 'public'`,
		identityF2TableNames).Scan(&count); err != nil {
		t.Fatalf("table-exists probe: %v", err)
	}
	return count
}

// TestIdentityF2_UpDownUp round-trips 0092 — up, down, up, down again —
// asserting the three tables come and go cleanly and that down is
// idempotent.
func TestIdentityF2_UpDownUp(t *testing.T) {
	db, ctx := freshDBWithIdentityF2(t)

	if got := identityF2TablesPresent(t, ctx, db); got != len(identityF2TableNames) {
		t.Fatalf("after initial up: got %d/%d tables", got, len(identityF2TableNames))
	}
	downBody, err := os.ReadFile(filepath.Join(harness.MigrationsDir(), "0092_identity_link_assignment_history.down.sql"))
	if err != nil {
		t.Fatalf("read down: %v", err)
	}
	upBody, err := os.ReadFile(filepath.Join(harness.MigrationsDir(), "0092_identity_link_assignment_history.up.sql"))
	if err != nil {
		t.Fatalf("read up: %v", err)
	}
	if _, err := db.AdminPool().Exec(ctx, string(downBody)); err != nil {
		t.Fatalf("apply down: %v", err)
	}
	if got := identityF2TablesPresent(t, ctx, db); got != 0 {
		t.Fatalf("after down: %d tables still present", got)
	}
	if _, err := db.AdminPool().Exec(ctx, string(upBody)); err != nil {
		t.Fatalf("re-apply up: %v", err)
	}
	if got := identityF2TablesPresent(t, ctx, db); got != len(identityF2TableNames) {
		t.Fatalf("after re-up: got %d tables", got)
	}
	// Down idempotency.
	if _, err := db.AdminPool().Exec(ctx, string(downBody)); err != nil {
		t.Fatalf("apply down #2: %v", err)
	}
	if _, err := db.AdminPool().Exec(ctx, string(downBody)); err != nil {
		t.Fatalf("apply down #3: %v", err)
	}
}

// TestIdentityF2_BackfillCreatesOnePerContact seeds contacts BEFORE
// running 0092, then verifies each contact has exactly one identity and
// one contact_identity_link with link_reason='manual'. Re-applying the
// migration MUST NOT create duplicate rows.
func TestIdentityF2_BackfillCreatesOnePerContact(t *testing.T) {
	db := harness.DB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	applyChain(t, ctx, db,
		"0004_create_tenant.up.sql",
		"0005_create_users.up.sql",
		"0088_inbox_contacts.up.sql",
	)
	tenantA, _ := seedTenantUserMaster(t, db)
	tenantB := uuid.New()
	if _, err := db.AdminPool().Exec(ctx,
		`INSERT INTO tenants (id, name, host) VALUES ($1, $2, $3)`,
		tenantB, "tenantB", fmt.Sprintf("b-%s.crm.local", tenantB)); err != nil {
		t.Fatalf("seed tenant B: %v", err)
	}
	for _, tid := range []uuid.UUID{tenantA, tenantA, tenantA, tenantB, tenantB} {
		if _, err := db.AdminPool().Exec(ctx,
			`INSERT INTO contact (id, tenant_id, display_name) VALUES (gen_random_uuid(), $1, 'X')`,
			tid); err != nil {
			t.Fatalf("seed contact: %v", err)
		}
	}

	upBody, err := os.ReadFile(filepath.Join(harness.MigrationsDir(), "0092_identity_link_assignment_history.up.sql"))
	if err != nil {
		t.Fatalf("read 0092 up: %v", err)
	}
	if _, err := db.AdminPool().Exec(ctx, string(upBody)); err != nil {
		t.Fatalf("apply 0092: %v", err)
	}

	assertCount := func(label, sql string, want int) {
		t.Helper()
		var n int
		if err := db.AdminPool().QueryRow(ctx, sql).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", label, err)
		}
		if n != want {
			t.Errorf("%s: got %d, want %d", label, n, want)
		}
	}
	assertCount("identity rows", `SELECT count(*) FROM identity`, 5)
	assertCount("link rows", `SELECT count(*) FROM contact_identity_link`, 5)
	assertCount("manual links", `SELECT count(*) FROM contact_identity_link WHERE link_reason='manual'`, 5)
	assertCount("tenant-mismatched links",
		`SELECT count(*) FROM contact_identity_link l JOIN contact c ON c.id=l.contact_id WHERE l.tenant_id<>c.tenant_id`,
		0)

	// Idempotency: re-applying must not seed more rows.
	if _, err := db.AdminPool().Exec(ctx, string(upBody)); err != nil {
		t.Fatalf("re-apply 0092: %v", err)
	}
	assertCount("identity rows after re-apply", `SELECT count(*) FROM identity`, 5)
	assertCount("link rows after re-apply", `SELECT count(*) FROM contact_identity_link`, 5)
}

// TestIdentityF2_ContactLinkUnique enforces that a contact belongs to
// exactly one identity. Two link rows for the same contact_id must
// violate UNIQUE(contact_id).
func TestIdentityF2_ContactLinkUnique(t *testing.T) {
	db, ctx := freshDBWithIdentityF2(t)

	tenantA, _ := seedTenantUserMaster(t, db)
	contact, id1, id2 := uuid.New(), uuid.New(), uuid.New()
	if _, err := db.AdminPool().Exec(ctx,
		`INSERT INTO contact (id, tenant_id, display_name) VALUES ($1, $2, 'A')`,
		contact, tenantA); err != nil {
		t.Fatalf("seed contact: %v", err)
	}
	if _, err := db.AdminPool().Exec(ctx,
		`INSERT INTO identity (id, tenant_id) VALUES ($1, $2), ($3, $2)`,
		id1, tenantA, id2); err != nil {
		t.Fatalf("seed identities: %v", err)
	}
	if _, err := db.AdminPool().Exec(ctx,
		`INSERT INTO contact_identity_link (tenant_id, identity_id, contact_id, link_reason) VALUES ($1, $2, $3, 'manual')`,
		tenantA, id1, contact); err != nil {
		t.Fatalf("first link: %v", err)
	}
	_, err := db.AdminPool().Exec(ctx,
		`INSERT INTO contact_identity_link (tenant_id, identity_id, contact_id, link_reason) VALUES ($1, $2, $3, 'manual')`,
		tenantA, id2, contact)
	if err == nil {
		t.Fatal("expected UNIQUE(contact_id) violation, got nil")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "duplicate key value") {
		t.Errorf("expected duplicate-key error, got: %v", err)
	}
}

// TestIdentityF2_EnumChecks proves the CHECK constraints on link_reason
// and assignment_history.reason reject anything outside the documented
// enum. Cheaper than three separate "rejects 'bogus'" tests.
func TestIdentityF2_EnumChecks(t *testing.T) {
	db, ctx := freshDBWithIdentityF2(t)
	tenantA, masterID := seedTenantUserMaster(t, db)
	id := uuid.New()
	contact := uuid.New()
	conv := uuid.New()
	if _, err := db.AdminPool().Exec(ctx,
		`INSERT INTO identity (id, tenant_id) VALUES ($1, $2)`, id, tenantA); err != nil {
		t.Fatalf("seed identity: %v", err)
	}
	if _, err := db.AdminPool().Exec(ctx,
		`INSERT INTO contact (id, tenant_id, display_name) VALUES ($1, $2, 'X')`, contact, tenantA); err != nil {
		t.Fatalf("seed contact: %v", err)
	}
	if _, err := db.AdminPool().Exec(ctx,
		`INSERT INTO conversation (id, tenant_id, contact_id, channel) VALUES ($1, $2, $3, 'whatsapp')`,
		conv, tenantA, contact); err != nil {
		t.Fatalf("seed conversation: %v", err)
	}

	cases := []struct {
		name string
		sql  string
		args []any
	}{
		{
			name: "link_reason rejects bogus",
			sql: `INSERT INTO contact_identity_link (tenant_id, identity_id, contact_id, link_reason)
			      VALUES ($1, $2, gen_random_uuid(), 'bogus')`,
			args: []any{tenantA, id},
		},
		{
			name: "assignment_history.reason rejects bogus",
			sql: `INSERT INTO assignment_history (tenant_id, conversation_id, user_id, reason)
			      VALUES ($1, $2, $3, 'bogus')`,
			args: []any{tenantA, conv, masterID},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := db.AdminPool().Exec(ctx, tc.sql, tc.args...)
			if err == nil {
				t.Fatal("expected CHECK violation, got nil")
			}
			if !strings.Contains(strings.ToLower(err.Error()), "check constraint") {
				t.Errorf("expected CHECK constraint error, got: %v", err)
			}
		})
	}

	// Accept every documented link_reason at least once. Reuse `contact`
	// and DELETE+INSERT to bypass the UNIQUE(contact_id) constraint.
	for _, r := range []string{"phone", "email", "external_id", "manual"} {
		if _, err := db.AdminPool().Exec(ctx,
			`DELETE FROM contact_identity_link WHERE contact_id = $1`, contact); err != nil {
			t.Fatalf("clear before %s: %v", r, err)
		}
		if _, err := db.AdminPool().Exec(ctx,
			`INSERT INTO contact_identity_link (tenant_id, identity_id, contact_id, link_reason) VALUES ($1, $2, $3, $4)`,
			tenantA, id, contact, r); err != nil {
			t.Errorf("link_reason %q rejected: %v", r, err)
		}
	}
	// Accept every documented assignment_history.reason at least once.
	for _, r := range []string{"lead", "manual", "reassign"} {
		if _, err := db.AdminPool().Exec(ctx,
			`INSERT INTO assignment_history (tenant_id, conversation_id, user_id, reason) VALUES ($1, $2, $3, $4)`,
			tenantA, conv, masterID, r); err != nil {
			t.Errorf("reason %q rejected: %v", r, err)
		}
	}
}

// TestIdentityF2_IndexesPresent asserts the three indexes listed in the
// SIN-62790 AC exist on the right columns with the right ordering.
func TestIdentityF2_IndexesPresent(t *testing.T) {
	db, ctx := freshDBWithIdentityF2(t)
	want := map[string]string{
		"contact_identity_link_tenant_identity_idx":   "contact_identity_link",
		"contact_identity_link_tenant_contact_idx":    "contact_identity_link",
		"assignment_history_tenant_conv_assigned_idx": "assignment_history",
	}
	for idx, tbl := range want {
		var def string
		if err := db.SuperuserPool().QueryRow(ctx,
			`SELECT indexdef FROM pg_indexes WHERE indexname = $1 AND tablename = $2`,
			idx, tbl).Scan(&def); err != nil {
			t.Errorf("missing index %s on %s: %v", idx, tbl, err)
			continue
		}
		if !strings.Contains(def, "tenant_id") {
			t.Errorf("index %s does not include tenant_id: %s", idx, def)
		}
	}
	var def string
	if err := db.SuperuserPool().QueryRow(ctx,
		`SELECT indexdef FROM pg_indexes WHERE indexname = 'assignment_history_tenant_conv_assigned_idx'`).Scan(&def); err != nil {
		t.Fatalf("read assignment_history index def: %v", err)
	}
	if !strings.Contains(def, "assigned_at DESC") {
		t.Errorf("assignment_history index missing assigned_at DESC: %s", def)
	}
}

// TestIdentityF2_RLS covers the four canonical regressions from ADR
// 0072: zero-rows without WithTenant, isolation between tenants, WITH
// CHECK on cross-tenant INSERT, and FORCE ROW LEVEL SECURITY on owner.
func TestIdentityF2_RLS(t *testing.T) {
	db, ctx := freshDBWithIdentityF2(t)
	tenantA, masterID := seedTenantUserMaster(t, db)
	tenantB := uuid.New()
	if _, err := db.AdminPool().Exec(ctx,
		`INSERT INTO tenants (id, name, host) VALUES ($1, $2, $3)`,
		tenantB, "tenantB", fmt.Sprintf("b-%s.crm.local", tenantB)); err != nil {
		t.Fatalf("seed tenant B: %v", err)
	}
	// Seed one row per tenanted table per tenant via app_admin.
	contactA, contactB := uuid.New(), uuid.New()
	convA := uuid.New()
	if _, err := db.AdminPool().Exec(ctx,
		`INSERT INTO contact (id, tenant_id, display_name) VALUES ($1, $2, 'A'), ($3, $4, 'B')`,
		contactA, tenantA, contactB, tenantB); err != nil {
		t.Fatalf("seed contacts: %v", err)
	}
	if _, err := db.AdminPool().Exec(ctx,
		`INSERT INTO conversation (id, tenant_id, contact_id, channel) VALUES ($1, $2, $3, 'whatsapp')`,
		convA, tenantA, contactA); err != nil {
		t.Fatalf("seed conversation: %v", err)
	}
	if _, err := db.AdminPool().Exec(ctx,
		`INSERT INTO assignment_history (tenant_id, conversation_id, user_id, reason) VALUES ($1, $2, $3, 'lead')`,
		tenantA, convA, masterID); err != nil {
		t.Fatalf("seed assignment_history: %v", err)
	}

	t.Run("no_tenant_set_returns_zero", func(t *testing.T) {
		for _, table := range identityF2TableNames {
			var n int
			if err := db.RuntimePool().QueryRow(ctx, fmt.Sprintf(`SELECT count(*) FROM %s`, table)).Scan(&n); err != nil {
				t.Errorf("count %s: %v", table, err)
				continue
			}
			if n != 0 {
				t.Errorf("runtime sees %d %s without WithTenant; want 0", n, table)
			}
		}
	})

	t.Run("tenant_isolation_on_identity", func(t *testing.T) {
		// Backfill from 0092 created an identity per contact, so tenant A has
		// >=1 identity and tenant B has >=1; with WithTenant(A) the runtime
		// must only see A's rows.
		if err := postgresadapter.WithTenant(ctx, db.RuntimePool(), tenantA, func(tx pgx.Tx) error {
			var leak int
			if err := tx.QueryRow(ctx, `SELECT count(*) FROM identity WHERE tenant_id = $1`, tenantB).Scan(&leak); err != nil {
				return err
			}
			if leak != 0 {
				t.Errorf("tenant A leaked B by literal: %d, want 0", leak)
			}
			return nil
		}); err != nil {
			t.Fatalf("WithTenant(A): %v", err)
		}
	})

	t.Run("insert_wrong_tenant_fails", func(t *testing.T) {
		err := postgresadapter.WithTenant(ctx, db.RuntimePool(), tenantA, func(tx pgx.Tx) error {
			_, e := tx.Exec(ctx, `INSERT INTO identity (tenant_id) VALUES ($1)`, tenantB)
			return e
		})
		if err == nil || !strings.Contains(err.Error(), "row-level security") {
			t.Errorf("expected row-level-security violation, got: %v", err)
		}
	})

	t.Run("force_rls_on_owner", func(t *testing.T) {
		for _, table := range identityF2TableNames {
			var force bool
			if err := db.SuperuserPool().QueryRow(ctx,
				`SELECT relforcerowsecurity FROM pg_class WHERE relname = $1`, table).Scan(&force); err != nil {
				t.Errorf("read relforcerowsecurity(%s): %v", table, err)
				continue
			}
			if !force {
				t.Errorf("%s: FORCE ROW LEVEL SECURITY = false (ADR 0072 violation)", table)
			}
		}
	})
}
