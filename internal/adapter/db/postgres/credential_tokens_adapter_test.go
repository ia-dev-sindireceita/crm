package postgres_test

// SIN-66510 integration tests for the credential-token consumption methods on
// the tenantusers Postgres adapter (LookupToken / ConsumeToken). They run
// against the shared testpg cluster in the parent postgres_test package,
// reusing the SIN-66496 migration chain + seed helpers (freshDBWithTenantUsers,
// seedTUTenant, seedTUUser). Coverage of the adapter package is measured by
// CI's -coverpkg sweep, like the mint-side tests in tenantusers_adapter_test.go.
//
// Focus (the "never ships" list from the ticket):
//   - lookup is by HASH; a valid, unexpired, unconsumed token resolves to the
//     invitee's email/purpose; invalid/expired/consumed all → ErrTokenInvalid.
//   - consume is ATOMIC + single-use: the first call sets the password hash
//     and marks the row consumed; a second call (replay) → ErrTokenInvalid and
//     the password hash is NOT changed again.
//   - tenant isolation: a token minted for tenant B cannot be resolved or
//     consumed under a tenant-A RLS envelope.
//   - a deactivated user's invite can never be redeemed.

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/adapter/db/postgres/testpg"
	"github.com/pericles-luz/crm/internal/iam"
	"github.com/pericles-luz/crm/internal/tenantusers"
)

// seedTUToken inserts a credential-token row directly (admin pool, bypassing
// RLS) so tests can arrange invite/expired/consumed fixtures. Returns the
// plaintext token whose SHA-256 was stored.
func seedTUToken(t *testing.T, db *testpg.DB, tenantID, userID uuid.UUID, purpose tenantusers.Purpose, expiresAt time.Time, consumedAt *time.Time) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	plain := uuid.NewString() + "-" + uuid.NewString()
	if _, err := db.AdminPool().Exec(ctx,
		`INSERT INTO user_credential_tokens
			(tenant_id, user_id, token_sha256, purpose, expires_at, consumed_at)
		 VALUES ($1, $2, $3, $4, $5, $6)`,
		tenantID, userID, tenantusers.HashToken(plain), string(purpose), expiresAt, consumedAt); err != nil {
		t.Fatalf("seed token: %v", err)
	}
	return plain
}

func userPasswordHash(t *testing.T, db *testpg.DB, userID uuid.UUID) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var h string
	if err := db.AdminPool().QueryRow(ctx,
		`SELECT password_hash FROM users WHERE id = $1`, userID).Scan(&h); err != nil {
		t.Fatalf("read password_hash: %v", err)
	}
	return h
}

func tokenConsumedAt(t *testing.T, db *testpg.DB, plain string) *time.Time {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var c *time.Time
	if err := db.AdminPool().QueryRow(ctx,
		`SELECT consumed_at FROM user_credential_tokens WHERE token_sha256 = $1`,
		tenantusers.HashToken(plain)).Scan(&c); err != nil {
		t.Fatalf("read consumed_at: %v", err)
	}
	return c
}

func TestCredTokenAdapter_LookupToken_Valid(t *testing.T) {
	db := freshDBWithTenantUsers(t)
	store := newTUStore(t, db)
	tenant := seedTUTenant(t, db)
	uid := seedTUUser(t, db, tenant, iam.RoleTenantAtendente, "invitee@acme.example")
	now := time.Now()
	plain := seedTUToken(t, db, tenant, uid, tenantusers.PurposeInvite, now.Add(time.Hour), nil)

	inv, err := store.LookupToken(context.Background(), tenant, tenantusers.HashToken(plain), now)
	if err != nil {
		t.Fatalf("LookupToken: %v", err)
	}
	if inv.UserID != uid || inv.Email != "invitee@acme.example" || inv.Purpose != tenantusers.PurposeInvite {
		t.Fatalf("invite = %+v", inv)
	}
}

func TestCredTokenAdapter_LookupToken_ExpiredConsumedUnknown(t *testing.T) {
	db := freshDBWithTenantUsers(t)
	store := newTUStore(t, db)
	tenant := seedTUTenant(t, db)
	uid := seedTUUser(t, db, tenant, iam.RoleTenantAtendente, "invitee@acme.example")
	now := time.Now()
	past := now.Add(-time.Minute)

	expired := seedTUToken(t, db, tenant, uid, tenantusers.PurposeInvite, now.Add(-time.Hour), nil)
	consumed := seedTUToken(t, db, tenant, uid, tenantusers.PurposeInvite, now.Add(time.Hour), &past)

	cases := map[string][]byte{
		"expired":  tenantusers.HashToken(expired),
		"consumed": tenantusers.HashToken(consumed),
		"unknown":  tenantusers.HashToken("no-such-token"),
	}
	for name, hash := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := store.LookupToken(context.Background(), tenant, hash, now); err != tenantusers.ErrTokenInvalid {
				t.Fatalf("%s: err = %v, want ErrTokenInvalid", name, err)
			}
		})
	}
}

func TestCredTokenAdapter_LookupToken_TenantIsolation(t *testing.T) {
	db := freshDBWithTenantUsers(t)
	store := newTUStore(t, db)
	tenantA := seedTUTenant(t, db)
	tenantB := seedTUTenant(t, db)
	uidB := seedTUUser(t, db, tenantB, iam.RoleTenantAtendente, "b@acme.example")
	now := time.Now()
	plain := seedTUToken(t, db, tenantB, uidB, tenantusers.PurposeInvite, now.Add(time.Hour), nil)

	// Look up B's token under tenant-A scope → not found (RLS + code).
	if _, err := store.LookupToken(context.Background(), tenantA, tenantusers.HashToken(plain), now); err != tenantusers.ErrTokenInvalid {
		t.Fatalf("cross-tenant lookup err = %v, want ErrTokenInvalid", err)
	}
}

func TestCredTokenAdapter_LookupToken_DeactivatedUser(t *testing.T) {
	db := freshDBWithTenantUsers(t)
	store := newTUStore(t, db)
	tenant := seedTUTenant(t, db)
	uid := seedTUUser(t, db, tenant, iam.RoleTenantAtendente, "gone@acme.example")
	now := time.Now()
	plain := seedTUToken(t, db, tenant, uid, tenantusers.PurposeInvite, now.Add(time.Hour), nil)

	ctx := context.Background()
	if _, err := db.AdminPool().Exec(ctx, `UPDATE users SET deactivated_at = now() WHERE id = $1`, uid); err != nil {
		t.Fatalf("deactivate: %v", err)
	}
	if _, err := store.LookupToken(ctx, tenant, tenantusers.HashToken(plain), now); err != tenantusers.ErrTokenInvalid {
		t.Fatalf("deactivated-user lookup err = %v, want ErrTokenInvalid", err)
	}
}

func TestCredTokenAdapter_ConsumeToken_SingleUse(t *testing.T) {
	db := freshDBWithTenantUsers(t)
	store := newTUStore(t, db)
	tenant := seedTUTenant(t, db)
	uid := seedTUUser(t, db, tenant, iam.RoleTenantAtendente, "invitee@acme.example")
	now := time.Now()
	plain := seedTUToken(t, db, tenant, uid, tenantusers.PurposeInvite, now.Add(time.Hour), nil)
	hash := tenantusers.HashToken(plain)
	ctx := context.Background()

	// Before: placeholder hash seeded by seedTUUser ('x').
	if got := userPasswordHash(t, db, uid); got != "x" {
		t.Fatalf("pre password_hash = %q", got)
	}

	gotUID, err := store.ConsumeToken(ctx, tenant, hash, now, "argon2id$NEWHASH")
	if err != nil {
		t.Fatalf("ConsumeToken: %v", err)
	}
	if gotUID != uid {
		t.Fatalf("consumed uid = %v, want %v", gotUID, uid)
	}
	// Password overwritten and token stamped consumed.
	if got := userPasswordHash(t, db, uid); got != "argon2id$NEWHASH" {
		t.Fatalf("post password_hash = %q", got)
	}
	if tokenConsumedAt(t, db, plain) == nil {
		t.Fatalf("token consumed_at still NULL")
	}

	// Replay: a second consume must fail (single-use) and NOT touch the hash.
	if _, err := store.ConsumeToken(ctx, tenant, hash, now, "argon2id$SECOND"); err != tenantusers.ErrTokenInvalid {
		t.Fatalf("replay err = %v, want ErrTokenInvalid", err)
	}
	if got := userPasswordHash(t, db, uid); got != "argon2id$NEWHASH" {
		t.Fatalf("password_hash changed on replay = %q", got)
	}
}

func TestCredTokenAdapter_ConsumeToken_UserDeactivatedRollsBack(t *testing.T) {
	// The token is valid but the user was deactivated after the GET-side
	// lookup. The token UPDATE would succeed, but the guarded users UPDATE
	// affects 0 rows → the whole tx rolls back: ErrTokenInvalid, the token is
	// NOT burned (consumed_at stays NULL), and no password is written. This
	// proves the single-transaction atomicity.
	db := freshDBWithTenantUsers(t)
	store := newTUStore(t, db)
	tenant := seedTUTenant(t, db)
	uid := seedTUUser(t, db, tenant, iam.RoleTenantAtendente, "invitee@acme.example")
	now := time.Now()
	plain := seedTUToken(t, db, tenant, uid, tenantusers.PurposeInvite, now.Add(time.Hour), nil)
	ctx := context.Background()

	if _, err := db.AdminPool().Exec(ctx, `UPDATE users SET deactivated_at = now() WHERE id = $1`, uid); err != nil {
		t.Fatalf("deactivate: %v", err)
	}

	if _, err := store.ConsumeToken(ctx, tenant, tenantusers.HashToken(plain), now, "argon2id$X"); err != tenantusers.ErrTokenInvalid {
		t.Fatalf("consume of deactivated-user token err = %v, want ErrTokenInvalid", err)
	}
	// Atomic rollback: token must still be unconsumed and password untouched.
	if c := tokenConsumedAt(t, db, plain); c != nil {
		t.Fatalf("token consumed_at = %v, want NULL (rolled back)", c)
	}
	if got := userPasswordHash(t, db, uid); got != "x" {
		t.Fatalf("password_hash = %q, want unchanged placeholder", got)
	}
}

func TestCredTokenAdapter_ConsumeToken_ExpiredAndCrossTenant(t *testing.T) {
	db := freshDBWithTenantUsers(t)
	store := newTUStore(t, db)
	tenantA := seedTUTenant(t, db)
	tenantB := seedTUTenant(t, db)
	uidA := seedTUUser(t, db, tenantA, iam.RoleTenantAtendente, "a@acme.example")
	uidB := seedTUUser(t, db, tenantB, iam.RoleTenantAtendente, "b@acme.example")
	now := time.Now()
	ctx := context.Background()

	expired := seedTUToken(t, db, tenantA, uidA, tenantusers.PurposeInvite, now.Add(-time.Hour), nil)
	if _, err := store.ConsumeToken(ctx, tenantA, tenantusers.HashToken(expired), now, "argon2id$X"); err != tenantusers.ErrTokenInvalid {
		t.Fatalf("expired consume err = %v, want ErrTokenInvalid", err)
	}

	// B's token consumed under tenant-A scope → not found, and B's password
	// stays the seeded placeholder.
	bTok := seedTUToken(t, db, tenantB, uidB, tenantusers.PurposeInvite, now.Add(time.Hour), nil)
	if _, err := store.ConsumeToken(ctx, tenantA, tenantusers.HashToken(bTok), now, "argon2id$X"); err != tenantusers.ErrTokenInvalid {
		t.Fatalf("cross-tenant consume err = %v, want ErrTokenInvalid", err)
	}
	if got := userPasswordHash(t, db, uidB); got != "x" {
		t.Fatalf("cross-tenant consume changed B's password = %q", got)
	}
}
