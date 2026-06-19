package postgres_test

// SIN-65254 — real-pg coverage for the master-operator credential lookup,
// and an end-to-end check of the full master login path (adapter +
// iam.Service.MasterLogin) against a live cluster.
//
// This is the truthful substitute for the stub that hid the gap: the
// existing mastermfa/login_test.go and master_mfa_wire_test.go stub
// MasterLoginFunc to always succeed, so the tenant-scoped lookup that could
// never find the NULL-tenant master operator shipped green. Here the REAL
// MasterCredentialReader resolves the seeded operator over the app_master_ops
// pool and the REAL MasterLogin authenticates it.

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/adapter/db/postgres"
	"github.com/pericles-luz/crm/internal/iam"
)

func TestMasterCredentialReader_RejectsBadInputs(t *testing.T) {
	t.Parallel()
	if _, err := postgres.NewMasterCredentialReader(nil, uuid.New()); err != postgres.ErrNilPool {
		t.Fatalf("nil pool err = %v, want ErrNilPool", err)
	}
	if _, err := postgres.NewMasterCredentialReader(nil, uuid.Nil); err != postgres.ErrNilPool {
		t.Fatalf("nil pool precedence err = %v, want ErrNilPool", err)
	}
}

func TestMasterCredentialReader_LookupMasterCredentials(t *testing.T) {
	db := freshDBWithMasterMFA(t)
	ctx := context.Background()

	actor := seedMasterUser(t, db, "ops-actor@cred.test")

	const plain = "s3cret-master-pw"
	hash, err := iam.HashPassword(plain)
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	target := uuid.New()
	if _, err := db.AdminPool().Exec(ctx,
		`INSERT INTO users (id, tenant_id, email, password_hash, role, is_master)
		 VALUES ($1, NULL, $2, $3, 'master', true)`,
		target, "boss@crm.local", hash); err != nil {
		t.Fatalf("seed master target: %v", err)
	}

	// A tenant user (is_master=false) with a non-NULL tenant_id MUST NOT be
	// resolvable as the master operator, even though it lives in the same
	// users table. Seed a tenant + a tenant user to prove the filter.
	tenantID := uuid.New()
	if _, err := db.AdminPool().Exec(ctx,
		`INSERT INTO tenants (id, name, host) VALUES ($1, $2, $3)`,
		tenantID, "cred-test", "cred-test.crm.local"); err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	if _, err := db.AdminPool().Exec(ctx,
		`INSERT INTO users (id, tenant_id, email, password_hash, role, is_master)
		 VALUES ($1, $2, $3, $4, 'tenant_common', false)`,
		uuid.New(), tenantID, "tenant-user@cred.test", hash); err != nil {
		t.Fatalf("seed tenant user: %v", err)
	}

	r, err := postgres.NewMasterCredentialReader(db.MasterOpsPool(), actor)
	if err != nil {
		t.Fatalf("NewMasterCredentialReader: %v", err)
	}

	t.Run("resolves the master operator", func(t *testing.T) {
		id, gotHash, err := r.LookupMasterCredentials(ctx, "boss@crm.local")
		if err != nil {
			t.Fatalf("LookupMasterCredentials: %v", err)
		}
		if id != target {
			t.Errorf("id = %s, want %s", id, target)
		}
		ok, err := iam.VerifyPassword(plain, gotHash)
		if err != nil || !ok {
			t.Errorf("returned hash does not verify the seeded password (ok=%v err=%v)", ok, err)
		}
	})

	t.Run("email match is case-insensitive", func(t *testing.T) {
		id, _, err := r.LookupMasterCredentials(ctx, "BOSS@CRM.LOCAL")
		if err != nil {
			t.Fatalf("LookupMasterCredentials: %v", err)
		}
		if id != target {
			t.Errorf("id = %s, want %s", id, target)
		}
	})

	t.Run("unknown email returns the zero-id sentinel", func(t *testing.T) {
		id, hash, err := r.LookupMasterCredentials(ctx, "nobody@crm.local")
		if err != nil {
			t.Fatalf("err = %v, want nil", err)
		}
		if id != uuid.Nil || hash != "" {
			t.Errorf("got (%s, %q), want (nil, \"\")", id, hash)
		}
	})

	t.Run("tenant user is not resolvable as master", func(t *testing.T) {
		id, _, err := r.LookupMasterCredentials(ctx, "tenant-user@cred.test")
		if err != nil {
			t.Fatalf("err = %v, want nil", err)
		}
		if id != uuid.Nil {
			t.Errorf("tenant user resolved as master (id=%s); the is_master/tenant_id filter leaked", id)
		}
	})
}

// TestMasterLogin_EndToEnd_RealPG exercises the FULL master login path the
// /m/login handler delegates to — the real MasterCredentialReader over the
// app_master_ops pool plus iam.Service.MasterLogin — against a seeded
// operator. This is the assertion the stubbed CI was missing: a real master
// operator authenticates, a wrong password is rejected, and an unknown email
// is rejected, all without a tenant in context.
func TestMasterLogin_EndToEnd_RealPG(t *testing.T) {
	db := freshDBWithMasterMFA(t)
	ctx := context.Background()

	actor := seedMasterUser(t, db, "ops-actor@e2e.test")

	const (
		email = "master@e2e.local"
		plain = "correct-master-password"
	)
	hash, err := iam.HashPassword(plain)
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	userID := uuid.New()
	if _, err := db.AdminPool().Exec(ctx,
		`INSERT INTO users (id, tenant_id, email, password_hash, role, is_master)
		 VALUES ($1, NULL, $2, $3, 'master', true)`,
		userID, email, hash); err != nil {
		t.Fatalf("seed master operator: %v", err)
	}

	reader, err := postgres.NewMasterCredentialReader(db.MasterOpsPool(), actor)
	if err != nil {
		t.Fatalf("NewMasterCredentialReader: %v", err)
	}
	svc := &iam.Service{MasterUsers: reader}

	t.Run("correct credentials authenticate, host ignored", func(t *testing.T) {
		// A bogus host that resolves to no tenant — the master path must not
		// care, because it never does a host→tenant lookup.
		sess, err := svc.MasterLogin(ctx, "no-such-host.invalid", email, plain, nil, "", "/m/login")
		if err != nil {
			t.Fatalf("MasterLogin: %v", err)
		}
		if sess.UserID != userID {
			t.Errorf("UserID = %s, want %s", sess.UserID, userID)
		}
		if sess.TenantID != uuid.Nil {
			t.Errorf("TenantID = %s, want nil (tenant-less master session)", sess.TenantID)
		}
	})

	t.Run("wrong password is rejected", func(t *testing.T) {
		if _, err := svc.MasterLogin(ctx, "", email, "WRONG", nil, "", ""); !errors.Is(err, iam.ErrInvalidCredentials) {
			t.Fatalf("err = %v, want ErrInvalidCredentials", err)
		}
	})

	t.Run("unknown operator is rejected", func(t *testing.T) {
		if _, err := svc.MasterLogin(ctx, "", strings.ToUpper(email)+"x", plain, nil, "", ""); !errors.Is(err, iam.ErrInvalidCredentials) {
			t.Fatalf("err = %v, want ErrInvalidCredentials", err)
		}
	})
}
