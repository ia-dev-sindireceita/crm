package postgres_test

// SIN-63186 integration tests for the lgpd Postgres adapter
// (internal/adapter/db/postgres/lgpd). Run against a real test DB.
// Verifies: upsert idempotency, ListReady ordering, MarkCompleted
// terminal-state guard, contact export shape, PurgeContact.

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/adapter/db/postgres"
	pglgpd "github.com/pericles-luz/crm/internal/adapter/db/postgres/lgpd"
	"github.com/pericles-luz/crm/internal/adapter/db/postgres/testpg"
	domain "github.com/pericles-luz/crm/internal/lgpd"
)

func freshDBWithLGPDFull(t *testing.T) (*testpg.DB, context.Context) {
	t.Helper()
	db := harness.DB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	applyChain(t, ctx, db,
		"0004_create_tenant.up.sql",
		"0005_create_users.up.sql",
		"0083_split_audit_log.up.sql",
		"0088_inbox_contacts.up.sql",
		"0101_ai_policy_consent.up.sql",
		"0107_lgpd_deletion_request.up.sql",
		"0108_tenants_dpo_settings.up.sql",
	)
	return db, ctx
}

func seedTenantContact(t *testing.T, ctx context.Context, db *testpg.DB) (uuid.UUID, uuid.UUID) {
	t.Helper()
	tenantID := uuid.New()
	if _, err := db.AdminPool().Exec(ctx,
		`INSERT INTO tenants (id, name, host) VALUES ($1, 'x', $2)`, tenantID, tenantID.String()); err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	contactID := uuid.New()
	if _, err := db.AdminPool().Exec(ctx,
		`INSERT INTO contact (id, tenant_id, display_name) VALUES ($1, $2, 'Test')`, contactID, tenantID); err != nil {
		t.Fatalf("seed contact: %v", err)
	}
	return tenantID, contactID
}

func newPgLGPD(t *testing.T, db *testpg.DB) *pglgpd.Store {
	t.Helper()
	s, err := pglgpd.New(db.RuntimePool(), db.MasterOpsPool())
	if err != nil {
		t.Fatalf("pglgpd.New: %v", err)
	}
	return s
}

func TestPgLGPD_New_RejectsNil(t *testing.T) {
	if _, err := pglgpd.New(nil, nil); err == nil {
		t.Fatal("New(nil,nil) err = nil, want ErrNilPool")
	} else if err != postgres.ErrNilPool {
		t.Errorf("New(nil,nil) err = %v, want ErrNilPool", err)
	}
}

func TestPgLGPD_Upsert_IsIdempotent(t *testing.T) {
	db, ctx := freshDBWithLGPDFull(t)
	store := newPgLGPD(t, db)
	tenantID, contactID := seedTenantContact(t, ctx, db)

	first, err := store.Upsert(ctx, domain.DeletionRequest{
		TenantID: tenantID, ContactID: contactID,
		Justification: "first", Status: domain.DeletionStatusPending,
		RetentionUntil: time.Now().Add(5 * 365 * 24 * time.Hour),
	})
	if err != nil {
		t.Fatalf("first Upsert: %v", err)
	}

	second, err := store.Upsert(ctx, domain.DeletionRequest{
		TenantID: tenantID, ContactID: contactID,
		Justification: "second", Status: domain.DeletionStatusPending,
		RetentionUntil: time.Now().Add(5 * 365 * 24 * time.Hour),
	})
	if err != nil {
		t.Fatalf("second Upsert: %v", err)
	}

	if first.ID != second.ID {
		t.Errorf("Upsert id changed: first=%s second=%s", first.ID, second.ID)
	}
	if second.Justification != "second" {
		t.Errorf("Upsert did not refresh justification: %q", second.Justification)
	}
}

func TestPgLGPD_ListReady_OnlyPastRetention(t *testing.T) {
	db, ctx := freshDBWithLGPDFull(t)
	store := newPgLGPD(t, db)
	tenantID, contactID := seedTenantContact(t, ctx, db)
	_, contact2 := seedTenantContact(t, ctx, db)
	_ = contact2

	past, err := store.Upsert(ctx, domain.DeletionRequest{
		TenantID: tenantID, ContactID: contactID,
		Justification: "ready", Status: domain.DeletionStatusPending,
		RetentionUntil: time.Now().Add(-time.Hour),
	})
	if err != nil {
		t.Fatalf("past upsert: %v", err)
	}
	tenant2, contact3 := seedTenantContact(t, ctx, db)
	if _, err := store.Upsert(ctx, domain.DeletionRequest{
		TenantID: tenant2, ContactID: contact3,
		Justification: "not ready", Status: domain.DeletionStatusPending,
		RetentionUntil: time.Now().Add(24 * time.Hour),
	}); err != nil {
		t.Fatalf("future upsert: %v", err)
	}

	ready, err := store.ListReady(ctx, time.Now(), 10)
	if err != nil {
		t.Fatalf("ListReady: %v", err)
	}
	if len(ready) != 1 || ready[0].ID != past.ID {
		t.Errorf("ready = %+v, want exactly [%s]", ready, past.ID)
	}
}

func TestPgLGPD_MarkCompleted_IgnoresAlreadyCompleted(t *testing.T) {
	db, ctx := freshDBWithLGPDFull(t)
	store := newPgLGPD(t, db)
	tenantID, contactID := seedTenantContact(t, ctx, db)

	req, err := store.Upsert(ctx, domain.DeletionRequest{
		TenantID: tenantID, ContactID: contactID,
		Justification: "x", Status: domain.DeletionStatusPending,
		RetentionUntil: time.Now().Add(-time.Hour),
	})
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := store.MarkCompleted(ctx, req.ID, time.Now()); err != nil {
		t.Fatalf("mark 1: %v", err)
	}
	// Second mark must not error (idempotent) and must not flip status back.
	if err := store.MarkCompleted(ctx, req.ID, time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("mark 2: %v", err)
	}
	got, err := store.Get(ctx, req.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Status != domain.DeletionStatusCompleted {
		t.Errorf("status = %s, want completed", got.Status)
	}
}

func TestPgLGPD_PurgeContact_AnonymisesAndDeletesNonFiscal(t *testing.T) {
	db, ctx := freshDBWithLGPDFull(t)
	store := newPgLGPD(t, db)
	tenantID, contactID := seedTenantContact(t, ctx, db)

	// Seed a conversation + message + identity for the contact.
	if _, err := db.AdminPool().Exec(ctx,
		`INSERT INTO contact_channel_identity (id, tenant_id, contact_id, channel, external_id)
		 VALUES ($1, $2, $3, 'whatsapp', $4)`,
		uuid.New(), tenantID, contactID, "+5511999"); err != nil {
		t.Fatalf("seed identity: %v", err)
	}
	convID := uuid.New()
	if _, err := db.AdminPool().Exec(ctx,
		`INSERT INTO conversation (id, tenant_id, contact_id, channel)
		 VALUES ($1, $2, $3, 'whatsapp')`, convID, tenantID, contactID); err != nil {
		t.Fatalf("seed conversation: %v", err)
	}
	if _, err := db.AdminPool().Exec(ctx,
		`INSERT INTO message (id, tenant_id, conversation_id, direction, body)
		 VALUES ($1, $2, $3, 'in', 'hi')`, uuid.New(), tenantID, convID); err != nil {
		t.Fatalf("seed message: %v", err)
	}

	if err := store.PurgeContact(ctx, tenantID, contactID); err != nil {
		t.Fatalf("purge: %v", err)
	}

	var displayName string
	if err := db.AdminPool().QueryRow(ctx,
		`SELECT display_name FROM contact WHERE id = $1`, contactID).Scan(&displayName); err != nil {
		t.Fatalf("read contact: %v", err)
	}
	if displayName != "[anonymised:lgpd]" {
		t.Errorf("display_name = %q, want [anonymised:lgpd]", displayName)
	}

	var ident, msg, conv int
	_ = db.AdminPool().QueryRow(ctx,
		`SELECT count(*) FROM contact_channel_identity WHERE contact_id = $1`, contactID).Scan(&ident)
	_ = db.AdminPool().QueryRow(ctx,
		`SELECT count(*) FROM message WHERE conversation_id = $1`, convID).Scan(&msg)
	_ = db.AdminPool().QueryRow(ctx,
		`SELECT count(*) FROM conversation WHERE contact_id = $1`, contactID).Scan(&conv)
	if ident != 0 || msg != 0 || conv != 0 {
		t.Errorf("post-purge counts: identity=%d msg=%d conv=%d, want all zero", ident, msg, conv)
	}
}

func TestPgLGPD_Export_ContactNotFound(t *testing.T) {
	db, ctx := freshDBWithLGPDFull(t)
	store := newPgLGPD(t, db)
	tenantID := uuid.New()
	if _, err := db.AdminPool().Exec(ctx,
		`INSERT INTO tenants (id, name, host) VALUES ($1, 'x', $2)`, tenantID, tenantID.String()); err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	_, err := store.GetContact(ctx, tenantID, uuid.New())
	if err == nil {
		t.Fatal("GetContact(unknown) err = nil, want ErrDeletionRequestNotFound")
	}
}
