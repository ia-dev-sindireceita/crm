package postgres_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	pgstore "github.com/pericles-luz/crm/internal/adapter/store/postgres"
)

// TestChannelAssociationWriter_RoundTrip is the SIN-67143 real-Postgres
// round-trip: the write-side ChannelAssociationWriter upsert must land a row
// that the read-side ChannelAssociationLookup resolves back to the same
// tenant, and re-registering the same number under a different tenant must
// repoint the row (ON CONFLICT upsert) instead of failing on the
// (channel, association) primary key.
//
// It lives in the db/postgres package to reuse the testpg harness + TestMain;
// the writer/lookup types come from the store/postgres package.
func TestChannelAssociationWriter_RoundTrip(t *testing.T) {
	db := harness.DB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// tenant_channel_associations (migration 0075a2) is a standalone table
	// with no FK dependencies; apply just its up migration onto the fresh DB.
	if err := runMigrationFile(ctx, db.AdminPool(), harness.MigrationsDir(), "0075a2_tenant_channel_associations.up.sql"); err != nil {
		t.Fatalf("apply 0075a2: %v", err)
	}

	writer := pgstore.NewChannelAssociationWriter(db.AdminPool())
	lookup := pgstore.NewChannelAssociationLookup(db.AdminPool())

	const phone = "5511999990000"
	tenantA := uuid.New()
	if err := writer.SaveAssociation(ctx, tenantA, "whatsapp", phone); err != nil {
		t.Fatalf("SaveAssociation(tenantA): %v", err)
	}
	got, err := lookup.Resolve(ctx, "whatsapp", phone)
	if err != nil {
		t.Fatalf("Resolve after create: %v", err)
	}
	if got != tenantA {
		t.Fatalf("Resolve after create = %v, want %v", got, tenantA)
	}

	// Upsert: same (channel, association), different tenant → row repoints,
	// no duplicate-key error.
	tenantB := uuid.New()
	if err := writer.SaveAssociation(ctx, tenantB, "whatsapp", phone); err != nil {
		t.Fatalf("SaveAssociation(tenantB upsert): %v", err)
	}
	got, err = lookup.Resolve(ctx, "whatsapp", phone)
	if err != nil {
		t.Fatalf("Resolve after upsert: %v", err)
	}
	if got != tenantB {
		t.Fatalf("Resolve after upsert = %v, want %v (repointed)", got, tenantB)
	}
}
