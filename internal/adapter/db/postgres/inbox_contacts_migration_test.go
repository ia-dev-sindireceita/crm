package postgres_test

// SIN-62724 acceptance criteria: 0088_inbox_contacts migrates up and down
// cleanly, RLS policies actually isolate by tenant, and the two
// UNIQUE constraints required by the spec are enforced by Postgres.
//
// Mirrors the SIN-62342 0086_master_mfa pattern: per-test database via
// the shared harness, apply the chain (4 → 5 → 88) on top of the
// default 0001-0003, then exercise.

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

// inboxTableNames are every table created by 0088_inbox_contacts.up.sql.
var inboxTableNames = []string{
	"contact",
	"contact_channel_identity",
	"conversation",
	"message",
	"assignment",
	"inbound_message_dedup",
}

// freshDBWithInboxContacts applies 0004 (tenants) → 0005 (users) → 0088
// (this migration) → 0092 (message.media jsonb, [SIN-62805] F2-05d
// integration: scanMessage now SELECTs media) on top of the harness
// default 0001-0003.
//
// 0092 is purely additive (ADD COLUMN IF NOT EXISTS media jsonb on the
// message table) — every existing test stays passing because the column
// is nullable and unreferenced by their query paths.
func freshDBWithInboxContacts(t *testing.T) *testpg.DB {
	t.Helper()
	db := harness.DB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	for _, name := range []string{
		"0004_create_tenant.up.sql",
		"0005_create_users.up.sql",
		"0088_inbox_contacts.up.sql",
		"0094_message_media_scan_status.up.sql",
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

func inboxTablesPresent(t *testing.T, ctx context.Context, db *testpg.DB) int {
	t.Helper()
	var count int
	row := db.SuperuserPool().QueryRow(ctx,
		`SELECT count(*) FROM pg_class c
		   JOIN pg_namespace n ON n.oid = c.relnamespace
		  WHERE c.relname = ANY($1)
		    AND n.nspname = 'public'`, inboxTableNames)
	if err := row.Scan(&count); err != nil {
		t.Fatalf("table-exists probe: %v", err)
	}
	return count
}

// TestInboxContactsMigration_UpDownUp proves both directions of
// 0088_inbox_contacts are idempotent and round-trip safe.
func TestInboxContactsMigration_UpDownUp(t *testing.T) {
	db := freshDBWithInboxContacts(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if got := inboxTablesPresent(t, ctx, db); got != len(inboxTableNames) {
		t.Fatalf("after initial up: got %d/%d inbox tables", got, len(inboxTableNames))
	}

	downBody, err := os.ReadFile(filepath.Join(harness.MigrationsDir(), "0088_inbox_contacts.down.sql"))
	if err != nil {
		t.Fatalf("read down: %v", err)
	}
	if _, err := db.AdminPool().Exec(ctx, string(downBody)); err != nil {
		t.Fatalf("apply down: %v", err)
	}
	if got := inboxTablesPresent(t, ctx, db); got != 0 {
		t.Fatalf("after down: %d/%d inbox tables still present", got, len(inboxTableNames))
	}

	upBody, err := os.ReadFile(filepath.Join(harness.MigrationsDir(), "0088_inbox_contacts.up.sql"))
	if err != nil {
		t.Fatalf("read up: %v", err)
	}
	if _, err := db.AdminPool().Exec(ctx, string(upBody)); err != nil {
		t.Fatalf("re-apply up: %v", err)
	}
	if got := inboxTablesPresent(t, ctx, db); got != len(inboxTableNames) {
		t.Fatalf("after re-up: got %d/%d inbox tables", got, len(inboxTableNames))
	}

	// Idempotency: down twice in a row.
	if _, err := db.AdminPool().Exec(ctx, string(downBody)); err != nil {
		t.Fatalf("apply down (idempotent): %v", err)
	}
	if _, err := db.AdminPool().Exec(ctx, string(downBody)); err != nil {
		t.Fatalf("apply down again: %v", err)
	}
}

// TestInboxContactsRLS_TenantIsolation: seed a contact under tenant A and
// a contact under tenant B (as app_admin), then connect via WithTenant(A)
// and confirm only A's row is visible. Mirrors the canonical regression
// established in ADR 0072 §process #2.
func TestInboxContactsRLS_TenantIsolation(t *testing.T) {
	db := freshDBWithInboxContacts(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	tenantA, _ := seedTenantUserMaster(t, db) // ignore master uid
	tenantB := uuid.New()
	if _, err := db.AdminPool().Exec(ctx,
		`INSERT INTO tenants (id, name, host) VALUES ($1, $2, $3)`,
		tenantB, "tenantB", fmt.Sprintf("b-%s.crm.local", tenantB)); err != nil {
		t.Fatalf("seed tenant B: %v", err)
	}

	contactA := uuid.New()
	contactB := uuid.New()
	if _, err := db.AdminPool().Exec(ctx,
		`INSERT INTO contact (id, tenant_id, display_name)
		 VALUES ($1, $2, 'Alice'), ($3, $4, 'Bob')`,
		contactA, tenantA, contactB, tenantB); err != nil {
		t.Fatalf("seed contacts: %v", err)
	}

	var seenAName string
	if err := postgresadapter.WithTenant(ctx, db.RuntimePool(), tenantA, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT display_name FROM contact ORDER BY display_name`)
		if err != nil {
			return err
		}
		defer rows.Close()
		seen := []string{}
		for rows.Next() {
			var n string
			if err := rows.Scan(&n); err != nil {
				return err
			}
			seen = append(seen, n)
		}
		if len(seen) != 1 {
			t.Fatalf("tenant A sees %d contacts, want 1: %v", len(seen), seen)
		}
		seenAName = seen[0]
		return rows.Err()
	}); err != nil {
		t.Fatalf("WithTenant(A): %v", err)
	}
	if seenAName != "Alice" {
		t.Errorf("tenant A saw %q, want Alice", seenAName)
	}
}

// TestInboxContactsRLS_NoTenantSetReturnsZero: the runtime pool without
// a WithTenant scope sees zero rows even though admin-inserted rows
// exist. This is the canonical fail-closed check from ADR 0072.
func TestInboxContactsRLS_NoTenantSetReturnsZero(t *testing.T) {
	db := freshDBWithInboxContacts(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	tenantA, _ := seedTenantUserMaster(t, db)
	if _, err := db.AdminPool().Exec(ctx,
		`INSERT INTO contact (id, tenant_id, display_name) VALUES (gen_random_uuid(), $1, 'Alice')`,
		tenantA); err != nil {
		t.Fatalf("seed contact: %v", err)
	}

	var n int
	if err := db.RuntimePool().QueryRow(ctx, `SELECT count(*) FROM contact`).Scan(&n); err != nil {
		t.Fatalf("count contact: %v", err)
	}
	if n != 0 {
		t.Errorf("runtime pool with no GUC saw %d contact rows, want 0", n)
	}
}

// TestInboxContactsRLS_InsertWrongTenantFails: with the GUC set to
// tenant A, an INSERT that names tenant B is rejected by the WITH CHECK
// clause. This protects against the body-tampering attack ADR 0072 §3
// (WITH CHECK) describes.
func TestInboxContactsRLS_InsertWrongTenantFails(t *testing.T) {
	db := freshDBWithInboxContacts(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	tenantA, _ := seedTenantUserMaster(t, db)
	tenantB := uuid.New()
	if _, err := db.AdminPool().Exec(ctx,
		`INSERT INTO tenants (id, name, host) VALUES ($1, $2, $3)`,
		tenantB, "tenantB", fmt.Sprintf("b-%s.crm.local", tenantB)); err != nil {
		t.Fatalf("seed tenant B: %v", err)
	}

	err := postgresadapter.WithTenant(ctx, db.RuntimePool(), tenantA, func(tx pgx.Tx) error {
		_, e := tx.Exec(ctx,
			`INSERT INTO contact (id, tenant_id, display_name) VALUES (gen_random_uuid(), $1, 'Mallory')`,
			tenantB)
		return e
	})
	if err == nil {
		t.Fatal("expected WITH CHECK violation when inserting under wrong tenant_id, got nil")
	}
	if !strings.Contains(err.Error(), "row-level security") {
		t.Errorf("expected row-level security error, got: %v", err)
	}
}

// TestInboxContactsForceRLS_AppliesToOwner: relforcerowsecurity=true on
// every tenanted inbox table. Canary against any future migration that
// drops FORCE — see ADR 0072 §FORCE ROW LEVEL SECURITY.
func TestInboxContactsForceRLS_AppliesToOwner(t *testing.T) {
	db := freshDBWithInboxContacts(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	tenantTables := []string{"contact", "contact_channel_identity", "conversation", "message", "assignment"}
	for _, table := range tenantTables {
		var force bool
		row := db.SuperuserPool().QueryRow(ctx,
			`SELECT relforcerowsecurity FROM pg_class WHERE relname = $1`, table)
		if err := row.Scan(&force); err != nil {
			t.Fatalf("read relforcerowsecurity(%s): %v", table, err)
		}
		if !force {
			t.Errorf("table %s: FORCE ROW LEVEL SECURITY = false (ADR 0072 violation)", table)
		}
	}
}

// TestContactChannelIdentity_UniqueChannelExternal: AC #3. The same
// (channel, external_id) pair MUST NOT be insertable twice, even across
// different tenants. Webhook routing depends on this invariant.
func TestContactChannelIdentity_UniqueChannelExternal(t *testing.T) {
	db := freshDBWithInboxContacts(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	tenantA, _ := seedTenantUserMaster(t, db)
	tenantB := uuid.New()
	if _, err := db.AdminPool().Exec(ctx,
		`INSERT INTO tenants (id, name, host) VALUES ($1, $2, $3)`,
		tenantB, "tenantB", fmt.Sprintf("b-%s.crm.local", tenantB)); err != nil {
		t.Fatalf("seed tenant B: %v", err)
	}
	contactA := uuid.New()
	contactB := uuid.New()
	if _, err := db.AdminPool().Exec(ctx,
		`INSERT INTO contact (id, tenant_id, display_name)
		 VALUES ($1, $2, 'A'), ($3, $4, 'B')`,
		contactA, tenantA, contactB, tenantB); err != nil {
		t.Fatalf("seed contacts: %v", err)
	}

	// First insert: succeeds.
	if _, err := db.AdminPool().Exec(ctx,
		`INSERT INTO contact_channel_identity (tenant_id, contact_id, channel, external_id)
		 VALUES ($1, $2, 'whatsapp', '+5511999990001')`,
		tenantA, contactA); err != nil {
		t.Fatalf("first insert: %v", err)
	}

	// Same (channel, external_id) under tenant B → expect violation.
	_, err := db.AdminPool().Exec(ctx,
		`INSERT INTO contact_channel_identity (tenant_id, contact_id, channel, external_id)
		 VALUES ($1, $2, 'whatsapp', '+5511999990001')`,
		tenantB, contactB)
	if err == nil {
		t.Fatal("expected unique-violation for (channel, external_id) across tenants, got nil")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "duplicate key value") {
		t.Errorf("expected duplicate-key error, got: %v", err)
	}
}

// TestContactChannelIdentity_UniqueContactChannel: a contact cannot have
// two identities on the same channel. Bookkeeping invariant that keeps
// the routing logic from picking arbitrarily between two phones.
func TestContactChannelIdentity_UniqueContactChannel(t *testing.T) {
	db := freshDBWithInboxContacts(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	tenantA, _ := seedTenantUserMaster(t, db)
	contactA := uuid.New()
	if _, err := db.AdminPool().Exec(ctx,
		`INSERT INTO contact (id, tenant_id, display_name) VALUES ($1, $2, 'A')`,
		contactA, tenantA); err != nil {
		t.Fatalf("seed contact: %v", err)
	}

	if _, err := db.AdminPool().Exec(ctx,
		`INSERT INTO contact_channel_identity (tenant_id, contact_id, channel, external_id)
		 VALUES ($1, $2, 'whatsapp', '+5511999990001')`,
		tenantA, contactA); err != nil {
		t.Fatalf("first insert: %v", err)
	}
	_, err := db.AdminPool().Exec(ctx,
		`INSERT INTO contact_channel_identity (tenant_id, contact_id, channel, external_id)
		 VALUES ($1, $2, 'whatsapp', '+5511999990002')`,
		tenantA, contactA)
	if err == nil {
		t.Fatal("expected unique-violation for (contact_id, channel), got nil")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "duplicate key value") {
		t.Errorf("expected duplicate-key error, got: %v", err)
	}
}

// TestInboundMessageDedup_UniqueChannelExternal: AC #4. The canonical
// idempotency invariant. Two attempts to insert the same (channel,
// channel_external_id) MUST fail at the database layer, not "succeed
// silently and double-emit the message".
func TestInboundMessageDedup_UniqueChannelExternal(t *testing.T) {
	db := freshDBWithInboxContacts(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if _, err := db.AdminPool().Exec(ctx,
		`INSERT INTO inbound_message_dedup (channel, channel_external_id)
		 VALUES ('whatsapp', 'wamid.abc')`); err != nil {
		t.Fatalf("first insert: %v", err)
	}
	_, err := db.AdminPool().Exec(ctx,
		`INSERT INTO inbound_message_dedup (channel, channel_external_id)
		 VALUES ('whatsapp', 'wamid.abc')`)
	if err == nil {
		t.Fatal("expected unique-violation for (channel, channel_external_id), got nil")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "duplicate key value") {
		t.Errorf("expected duplicate-key error, got: %v", err)
	}
}
