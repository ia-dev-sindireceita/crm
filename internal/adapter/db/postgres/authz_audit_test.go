package postgres_test

// SIN-62254 regression: 11 deny decisions from authz.AuditingAuthorizer
// against the real postgres SplitAuditLogger produce 11 audit_log_security
// rows with event_type='authz_deny'. This is the "11x denies in 1 min"
// AC of ADR 0004 §6 — running through the same migration chain as the
// split-audit suite plus the new 0091 migration that extends the
// event_type CHECK to include 'authz_allow'.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	postgresadapter "github.com/pericles-luz/crm/internal/adapter/db/postgres"
	"github.com/pericles-luz/crm/internal/adapter/db/postgres/testpg"
	"github.com/pericles-luz/crm/internal/iam"
	"github.com/pericles-luz/crm/internal/iam/authz"
)

func freshDBWithAuthzAudit(t *testing.T) (db *testpg.DB, auditPool *pgxpool.Pool) {
	t.Helper()
	db = harness.DB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	for _, mig := range []struct {
		file      string
		superuser bool
	}{
		{"0004_create_tenant.up.sql", false},
		{"0005_create_users.up.sql", false},
		{"0006_create_sessions.up.sql", false},
		{"0007_create_audit_log.up.sql", false},
		{"0078_app_audit_role.up.sql", true},
		{"0083_split_audit_log.up.sql", false},
		{"0084_tenant_audit_data_retention.up.sql", false},
		{"0085_app_audit_role_split.up.sql", false},
		{"0091_audit_log_security_authz_allow.up.sql", false},
	} {
		path := filepath.Join(harness.MigrationsDir(), mig.file)
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", mig.file, err)
		}
		pool := db.AdminPool()
		if mig.superuser {
			pool = db.SuperuserPool()
		}
		if _, err := pool.Exec(ctx, string(body)); err != nil {
			t.Fatalf("apply %s: %v", mig.file, err)
		}
	}

	password := "test_authz_audit_pw_" + uuid.New().String()[:12]
	if _, err := db.SuperuserPool().Exec(ctx, fmt.Sprintf(`ALTER ROLE app_audit WITH PASSWORD '%s'`, password)); err != nil {
		t.Fatalf("set app_audit password: %v", err)
	}
	cfg := db.SuperuserPool().Config().ConnConfig
	dsn := fmt.Sprintf("host=%s port=%d user=app_audit password=%s dbname=%s sslmode=disable",
		cfg.Host, cfg.Port, password, db.Name())
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect app_audit: %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		t.Fatalf("ping app_audit: %v", err)
	}
	t.Cleanup(pool.Close)
	return db, pool
}

// constantDenyAuthorizer is an inner Authorizer that always returns the
// same deny Decision. It plays the role of "RBAC said no" for the
// regression test without standing up the full ADR 0090 matrix.
type constantDenyAuthorizer struct{ d iam.Decision }

func (c constantDenyAuthorizer) Can(context.Context, iam.Principal, iam.Action, iam.Resource) iam.Decision {
	return c.d
}

func TestAuthzAuditWrapper_RegressionElevenDeniesProduceElevenRows(t *testing.T) {
	db, auditPool := freshDBWithAuthzAudit(t)
	tenantID, userID := seedSplitTenantUser(t, db, "authz-regr")
	logger, err := postgresadapter.NewSplitAuditLogger(auditPool)
	if err != nil {
		t.Fatalf("NewSplitAuditLogger: %v", err)
	}

	recorder := authz.NewAuditRecorder(logger, authz.NewMetrics(nil), nil)
	inner := constantDenyAuthorizer{d: iam.Decision{
		Allow:      false,
		ReasonCode: iam.ReasonDeniedTenantMismatch,
		TargetKind: "conversation",
		TargetID:   "tenant-B-conversation",
	}}
	wrapped := authz.New(authz.Config{
		Inner:    inner,
		Recorder: recorder,
		Sampler:  authz.NeverSample{}, // we only assert deny rows
		Now:      func() time.Time { return time.Now().UTC() },
	})

	principal := iam.Principal{
		UserID:   userID,
		TenantID: tenantID,
		Roles:    []iam.Role{iam.RoleTenantAtendente},
	}
	resource := iam.Resource{
		TenantID: uuid.New().String(), // some OTHER tenant
		Kind:     "conversation",
		ID:       "tenant-B-conversation",
	}

	ctx := newCtx(t)
	for i := 0; i < 11; i++ {
		d := wrapped.Can(ctx, principal, iam.ActionTenantConversationRead, resource)
		if d.Allow {
			t.Fatalf("call %d: expected deny", i)
		}
		if d.ReasonCode != iam.ReasonDeniedTenantMismatch {
			t.Fatalf("call %d: ReasonCode = %s, want denied_tenant_mismatch", i, d.ReasonCode)
		}
	}

	var rows int
	if err := db.AdminPool().QueryRow(ctx,
		`SELECT count(*) FROM audit_log_security
		   WHERE actor_user_id = $1
		     AND event_type = 'authz_deny'
		     AND target->>'outcome' = 'deny'`,
		userID,
	).Scan(&rows); err != nil {
		t.Fatalf("count audit_log_security: %v", err)
	}
	if rows != 11 {
		t.Fatalf("expected 11 deny rows, got %d", rows)
	}

	// Sanity: ReasonCode and target_kind/id round-trip into target jsonb.
	var reasonCode, targetKind, targetID string
	if err := db.AdminPool().QueryRow(ctx,
		`SELECT target->>'reason_code', target->>'target_kind', target->>'target_id'
		   FROM audit_log_security
		  WHERE actor_user_id = $1
		  LIMIT 1`,
		userID,
	).Scan(&reasonCode, &targetKind, &targetID); err != nil {
		t.Fatalf("read back target jsonb: %v", err)
	}
	if reasonCode != "denied_tenant_mismatch" || targetKind != "conversation" || targetID != "tenant-B-conversation" {
		t.Fatalf("target jsonb shape drift: reason_code=%q target_kind=%q target_id=%q",
			reasonCode, targetKind, targetID)
	}
}

func TestAuthzAuditWrapper_SampledAllowProducesAuthzAllowRow(t *testing.T) {
	db, auditPool := freshDBWithAuthzAudit(t)
	tenantID, userID := seedSplitTenantUser(t, db, "authz-allow")
	logger, err := postgresadapter.NewSplitAuditLogger(auditPool)
	if err != nil {
		t.Fatalf("NewSplitAuditLogger: %v", err)
	}
	recorder := authz.NewAuditRecorder(logger, authz.NewMetrics(nil), nil)
	inner := constantDenyAuthorizer{d: iam.Decision{
		Allow:      true,
		ReasonCode: iam.ReasonAllowedRBAC,
		TargetKind: "contact",
		TargetID:   "c-1",
	}}
	wrapped := authz.New(authz.Config{
		Inner:    inner,
		Recorder: recorder,
		Sampler:  authz.AlwaysSample{},
		Now:      func() time.Time { return time.Now().UTC() },
	})

	ctx := newCtx(t)
	_ = wrapped.Can(ctx, iam.Principal{
		UserID:   userID,
		TenantID: tenantID,
		Roles:    []iam.Role{iam.RoleTenantGerente},
	}, iam.ActionTenantContactRead, iam.Resource{Kind: "contact", ID: "c-1"})

	var rows int
	if err := db.AdminPool().QueryRow(ctx,
		`SELECT count(*) FROM audit_log_security
		   WHERE actor_user_id = $1
		     AND event_type = 'authz_allow'
		     AND target->>'outcome' = 'allow'`,
		userID,
	).Scan(&rows); err != nil {
		t.Fatalf("count authz_allow rows: %v", err)
	}
	if rows != 1 {
		t.Fatalf("expected 1 authz_allow row, got %d", rows)
	}
}

func TestMigration0091_RejectsUnknownEventType(t *testing.T) {
	// Defense-in-depth: confirm 0091 left the closed enum closed (no
	// accidental wildcard). An unknown event_type must still fail
	// the CHECK constraint.
	db, _ := freshDBWithAuthzAudit(t)
	tenantID, userID := seedSplitTenantUser(t, db, "authz-check")
	ctx := newCtx(t)
	if _, err := db.AdminPool().Exec(ctx,
		`INSERT INTO audit_log_security (tenant_id, actor_user_id, event_type, target)
		 VALUES ($1, $2, 'not_a_real_event', '{}'::jsonb)`,
		tenantID, userID); err == nil {
		t.Fatal("INSERT with bogus event_type unexpectedly succeeded")
	}
}
