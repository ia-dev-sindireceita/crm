package postgres_test

// SIN-63184 acceptance: migration 0112_user_mfa round-trips up/down/up;
// the new tenant MFA adapters store + load + invalidate per tenant.
// (Originally numbered 0107, renumbered to 0112 by SIN-63230 to
// resolve a three-way collision on 0107.)

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/adapter/db/postgres"
	"github.com/pericles-luz/crm/internal/adapter/db/postgres/testpg"
)

func TestUserMFAMigration_UpDownUp(t *testing.T) {
	db := freshDBWithUserMFA(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if !userMFATablesExist(t, ctx, db) {
		t.Fatal("tables missing after initial up")
	}

	downBody, err := os.ReadFile(filepath.Join(harness.MigrationsDir(), "0112_user_mfa.down.sql"))
	if err != nil {
		t.Fatalf("read down: %v", err)
	}
	if _, err := db.AdminPool().Exec(ctx, string(downBody)); err != nil {
		t.Fatalf("apply down: %v", err)
	}
	if userMFATablesExist(t, ctx, db) {
		t.Fatal("tables still present after down")
	}

	upBody, err := os.ReadFile(filepath.Join(harness.MigrationsDir(), "0112_user_mfa.up.sql"))
	if err != nil {
		t.Fatalf("read up: %v", err)
	}
	if _, err := db.AdminPool().Exec(ctx, string(upBody)); err != nil {
		t.Fatalf("re-apply up: %v", err)
	}
	if !userMFATablesExist(t, ctx, db) {
		t.Fatal("tables missing after re-up")
	}
}

func TestTenantUserMFA_StoreLoadVerifyReenroll(t *testing.T) {
	db := freshDBWithUserMFA(t)
	tenant, user := seedTenantUser(t, db, "acme-mfa.crm.local", "admin@acme-mfa.test")
	ctx := context.Background()

	a, err := postgres.NewTenantUserMFA(db.RuntimePool(), tenant)
	if err != nil {
		t.Fatalf("NewTenantUserMFA: %v", err)
	}

	// LoadSeed on missing → ErrNotEnrolled.
	if _, err := a.LoadSeed(ctx, user); err == nil {
		t.Fatalf("expected ErrNotEnrolled on missing row")
	}
	enrolled, err := a.IsEnrolled(ctx, user)
	if err != nil {
		t.Fatalf("IsEnrolled: %v", err)
	}
	if enrolled {
		t.Fatalf("expected !IsEnrolled before StoreSeed")
	}

	ct := []byte("opaque-ciphertext-xyz")
	if err := a.StoreSeed(ctx, user, ct); err != nil {
		t.Fatalf("StoreSeed: %v", err)
	}
	got, err := a.LoadSeed(ctx, user)
	if err != nil {
		t.Fatalf("LoadSeed: %v", err)
	}
	if string(got) != string(ct) {
		t.Fatalf("LoadSeed: want %q got %q", ct, got)
	}
	enrolled, err = a.IsEnrolled(ctx, user)
	if err != nil {
		t.Fatalf("IsEnrolled: %v", err)
	}
	if !enrolled {
		t.Fatalf("expected IsEnrolled after StoreSeed")
	}
	if err := a.MarkVerified(ctx, user); err != nil {
		t.Fatalf("MarkVerified: %v", err)
	}
	if err := a.MarkReenrollRequired(ctx, user); err != nil {
		t.Fatalf("MarkReenrollRequired: %v", err)
	}
	enrolled, err = a.IsEnrolled(ctx, user)
	if err != nil {
		t.Fatalf("IsEnrolled after reenroll: %v", err)
	}
	if enrolled {
		t.Fatalf("expected !IsEnrolled after MarkReenrollRequired")
	}
}

func TestTenantUserRecoveryCodes_RoundTrip(t *testing.T) {
	db := freshDBWithUserMFA(t)
	tenant, user := seedTenantUser(t, db, "acme-rc.crm.local", "admin@acme-rc.test")
	ctx := context.Background()

	a, err := postgres.NewTenantUserRecoveryCodes(db.RuntimePool(), tenant)
	if err != nil {
		t.Fatalf("NewTenantUserRecoveryCodes: %v", err)
	}
	hashes := []string{"h1", "h2", "h3"}
	if err := a.InsertHashes(ctx, user, hashes); err != nil {
		t.Fatalf("InsertHashes: %v", err)
	}
	rows, err := a.ListActive(ctx, user)
	if err != nil {
		t.Fatalf("ListActive: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("rows after insert: want 3 got %d", len(rows))
	}
	if err := a.MarkConsumed(ctx, rows[0].ID); err != nil {
		t.Fatalf("MarkConsumed: %v", err)
	}
	rows, err = a.ListActive(ctx, user)
	if err != nil {
		t.Fatalf("ListActive 2: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows after consume: want 2 got %d", len(rows))
	}
	n, err := a.InvalidateAll(ctx, user)
	if err != nil {
		t.Fatalf("InvalidateAll: %v", err)
	}
	if n != 2 {
		t.Fatalf("invalidated: want 2 got %d", n)
	}
	rows, err = a.ListActive(ctx, user)
	if err != nil {
		t.Fatalf("ListActive 3: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("expected zero active rows after InvalidateAll, got %d", len(rows))
	}
}

func TestTenantUserMFAPending_CreateGetDelete(t *testing.T) {
	db := freshDBWithUserMFA(t)
	tenant, user := seedTenantUser(t, db, "acme-pending.crm.local", "admin@acme-pending.test")
	ctx := context.Background()

	a, err := postgres.NewTenantUserMFAPending(db.RuntimePool(), tenant)
	if err != nil {
		t.Fatalf("NewTenantUserMFAPending: %v", err)
	}
	row, err := a.Create(ctx, user, 5*time.Minute, "/inbox")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	got, err := a.Get(ctx, row.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.UserID != user || got.TenantID != tenant || got.NextPath != "/inbox" {
		t.Fatalf("Get mismatch: %#v", got)
	}
	if err := a.Delete(ctx, row.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := a.Get(ctx, row.ID); err == nil {
		t.Fatalf("expected ErrPendingMFANotFound after delete")
	}
}

func TestTenantUserMFARequirement_AdminRoleEnforcesTOTP(t *testing.T) {
	db := freshDBWithUserMFA(t)
	tenant, user := seedTenantUser(t, db, "acme-req.crm.local", "admin@acme-req.test")
	// Promote the seed user to admin role.
	ctx := context.Background()
	if _, err := db.AdminPool().Exec(ctx,
		`UPDATE users SET role = 'admin' WHERE id = $1`, user); err != nil {
		t.Fatalf("promote: %v", err)
	}
	r, err := postgres.NewTenantUserMFARequirement(db.RuntimePool(), tenant)
	if err != nil {
		t.Fatalf("NewTenantUserMFARequirement: %v", err)
	}
	req, err := r.Load(ctx, user)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !req.TOTPRequired {
		t.Fatalf("admin must require TOTP; got %#v", req)
	}
	if req.TOTPEnrolled {
		t.Fatalf("admin must not be enrolled until user_mfa row exists; got %#v", req)
	}
	if req.Role != "admin" {
		t.Fatalf("role: want admin got %q", req.Role)
	}
}

func TestTenantUserMFARequirement_MemberOptIn(t *testing.T) {
	db := freshDBWithUserMFA(t)
	tenant, user := seedTenantUser(t, db, "acme-opt.crm.local", "member@acme-opt.test")
	ctx := context.Background()
	r, err := postgres.NewTenantUserMFARequirement(db.RuntimePool(), tenant)
	if err != nil {
		t.Fatalf("NewTenantUserMFARequirement: %v", err)
	}
	req, err := r.Load(ctx, user)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if req.TOTPRequired {
		t.Fatalf("plain member must not require TOTP by default; got %#v", req)
	}
	if err := r.SetTOTPRequired(ctx, user); err != nil {
		t.Fatalf("SetTOTPRequired: %v", err)
	}
	req, err = r.Load(ctx, user)
	if err != nil {
		t.Fatalf("Load 2: %v", err)
	}
	if !req.TOTPRequired {
		t.Fatalf("member after opt-in must require TOTP; got %#v", req)
	}
}

func TestTenantUserMFARequirement_RejectsBadInputs(t *testing.T) {
	t.Parallel()
	if _, err := postgres.NewTenantUserMFARequirement(nil, uuid.New()); err == nil {
		t.Fatalf("expected error for nil pool")
	}
}

func TestNewTenantUserMFA_RejectsBadInputs(t *testing.T) {
	t.Parallel()
	if _, err := postgres.NewTenantUserMFA(nil, uuid.New()); err == nil {
		t.Fatalf("expected error for nil pool")
	}
}

func TestNewTenantUserRecoveryCodes_RejectsBadInputs(t *testing.T) {
	t.Parallel()
	if _, err := postgres.NewTenantUserRecoveryCodes(nil, uuid.New()); err == nil {
		t.Fatalf("expected error for nil pool")
	}
}

func TestNewTenantUserMFAPending_RejectsBadInputs(t *testing.T) {
	t.Parallel()
	if _, err := postgres.NewTenantUserMFAPending(nil, uuid.New()); err == nil {
		t.Fatalf("expected error for nil pool")
	}
}

// freshDBWithUserMFA brings up the migrations stack needed by the
// SIN-63184 tenant 2FA adapters: tenants + users + sessions +
// audit_log_security stack + 0107 itself.
func freshDBWithUserMFA(t *testing.T) *testpg.DB {
	t.Helper()
	db := harness.DB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	for _, name := range []string{
		"0004_create_tenant.up.sql",
		"0005_create_users.up.sql",
		"0006_create_sessions.up.sql",
		"0083_split_audit_log.up.sql",
		"0091_audit_log_security_authz_allow.up.sql",
		"0100_audit_log_security_billing_events.up.sql",
		"0112_user_mfa.up.sql",
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

func userMFATablesExist(t *testing.T, ctx context.Context, db *testpg.DB) bool {
	t.Helper()
	var count int
	row := db.SuperuserPool().QueryRow(ctx,
		`SELECT count(*) FROM pg_class c
		   JOIN pg_namespace n ON n.oid = c.relnamespace
		  WHERE c.relname IN ('user_mfa', 'user_recovery_code', 'user_mfa_pending_session')
		    AND n.nspname = 'public'`)
	if err := row.Scan(&count); err != nil {
		t.Fatalf("table-exists probe: %v", err)
	}
	// Also verify users.totp_required_at column exists.
	var col int
	row = db.SuperuserPool().QueryRow(ctx,
		`SELECT count(*) FROM information_schema.columns
		  WHERE table_schema = 'public' AND table_name = 'users'
		    AND column_name = 'totp_required_at'`)
	if err := row.Scan(&col); err != nil {
		t.Fatalf("column-exists probe: %v", err)
	}
	return count == 3 && col == 1
}

// Suppress unused-import warning when the harness var isn't used elsewhere.
var _ = strings.HasPrefix
