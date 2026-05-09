package postgres_test

// SIN-62377 (FAIL-4) integration tests for the Touch surface added to
// postgres.SessionStore plus the new last_activity / role columns
// landed by migration 0011_session_activity. Uses freshDBWithIAM
// (which now applies 0011) so the schema has both columns.

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/adapter/db/postgres"
	"github.com/pericles-luz/crm/internal/iam"
)

func TestSessionStore_Touch_BumpsLastActivity(t *testing.T) {
	db := freshDBWithIAM(t)
	tenantID, userID, _ := seedTenant(t, db, "acme.crm.local", "alice@acme.test")
	store := postgres.NewSessionStore(db.RuntimePool())
	ctx := context.Background()

	id, err := iam.NewSessionID()
	if err != nil {
		t.Fatalf("NewSessionID: %v", err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	sess := iam.Session{
		ID:           id,
		TenantID:     tenantID,
		UserID:       userID,
		ExpiresAt:    now.Add(time.Hour),
		CreatedAt:    now,
		LastActivity: now,
		Role:         iam.RoleTenantCommon,
		IPAddr:       net.IPv4(192, 0, 2, 8).To4(),
	}
	if err := store.Create(ctx, sess); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Initial Get round-trips the new fields.
	got, err := store.Get(ctx, tenantID, id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Role != iam.RoleTenantCommon {
		t.Fatalf("Role = %q, want %q", got.Role, iam.RoleTenantCommon)
	}
	if !got.LastActivity.Equal(now) {
		t.Fatalf("LastActivity initial = %v, want %v", got.LastActivity, now)
	}

	// Touch with a later timestamp; Get must reflect it.
	bump := now.Add(7 * time.Minute)
	if err := store.Touch(ctx, tenantID, id, bump); err != nil {
		t.Fatalf("Touch: %v", err)
	}
	got2, err := store.Get(ctx, tenantID, id)
	if err != nil {
		t.Fatalf("Get post-touch: %v", err)
	}
	if !got2.LastActivity.Equal(bump) {
		t.Fatalf("LastActivity after Touch = %v, want %v", got2.LastActivity, bump)
	}
}

func TestSessionStore_Touch_MissingRowReturnsErrSessionNotFound(t *testing.T) {
	db := freshDBWithIAM(t)
	tenantID, _, _ := seedTenant(t, db, "acme.crm.local", "alice@acme.test")
	store := postgres.NewSessionStore(db.RuntimePool())
	err := store.Touch(context.Background(), tenantID, uuid.New(), time.Now().UTC())
	if !errors.Is(err, iam.ErrSessionNotFound) {
		t.Fatalf("Touch missing id err = %v, want ErrSessionNotFound", err)
	}
}

func TestSessionStore_Touch_NilSessionIDReturnsErrSessionNotFound(t *testing.T) {
	db := freshDBWithIAM(t)
	tenantID, _, _ := seedTenant(t, db, "acme.crm.local", "alice@acme.test")
	store := postgres.NewSessionStore(db.RuntimePool())
	err := store.Touch(context.Background(), tenantID, uuid.Nil, time.Now().UTC())
	if !errors.Is(err, iam.ErrSessionNotFound) {
		t.Fatalf("Touch uuid.Nil err = %v, want ErrSessionNotFound", err)
	}
}

// Defaults: a Session created with zero Role / zero LastActivity must
// land with role = 'tenant_common' and last_activity = CreatedAt.
// SIN-62377 guards against an in-memory caller that constructs Session
// without the new fields; the adapter must DTRT.
func TestSessionStore_Create_DefaultsRoleAndLastActivity(t *testing.T) {
	db := freshDBWithIAM(t)
	tenantID, userID, _ := seedTenant(t, db, "acme.crm.local", "alice@acme.test")
	store := postgres.NewSessionStore(db.RuntimePool())
	ctx := context.Background()
	id, _ := iam.NewSessionID()
	now := time.Now().UTC().Truncate(time.Microsecond)
	if err := store.Create(ctx, iam.Session{
		ID: id, TenantID: tenantID, UserID: userID,
		ExpiresAt: now.Add(time.Hour), CreatedAt: now,
		// Role + LastActivity intentionally left zero
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	got, err := store.Get(ctx, tenantID, id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Role != iam.RoleTenantCommon {
		t.Fatalf("Role default = %q, want %q", got.Role, iam.RoleTenantCommon)
	}
	if !got.LastActivity.Equal(now) {
		t.Fatalf("LastActivity default = %v, want CreatedAt %v", got.LastActivity, now)
	}
}
