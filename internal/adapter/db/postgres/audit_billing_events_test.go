package postgres_test

// SIN-62883 / Fase 2.5 C8 acceptance: end-to-end smoke that the three
// new audit_log_security event types (master.grant.issued,
// subscription.created, invoice.cancelled_by_master) round-trip
// through the postgres SplitAuditLogger via the audit.Write* helpers.
//
// Migration chain mirrors freshDBWithSplitAudit + 0091 + 0100. We
// reuse the app_audit pool wiring because the new event types live on
// audit_log_security like every other security event — no new table,
// no new grants, just an extended CHECK clause.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	postgresadapter "github.com/pericles-luz/crm/internal/adapter/db/postgres"
	"github.com/pericles-luz/crm/internal/adapter/db/postgres/testpg"
	"github.com/pericles-luz/crm/internal/iam/audit"
)

func freshDBWithBillingAudit(t *testing.T) (*testpg.DB, *pgxpool.Pool) {
	t.Helper()
	db := harness.DB(t)
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
		{"0100_audit_log_security_billing_events.up.sql", false},
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

	password := "test_billing_audit_pw_" + uuid.New().String()[:12]
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

// TestBillingAudit_AC1_OneRowPerFlow exercises the three writer helpers
// against the real postgres CHECK constraint. Each call must land
// exactly one audit_log_security row whose event_type matches the
// constant. Failure here would mean either the migration didn't apply
// or the constant string drifted from the CHECK clause.
func TestBillingAudit_AC1_OneRowPerFlow(t *testing.T) {
	db, auditPool := freshDBWithBillingAudit(t)
	tenantID, userID := seedSplitTenantUser(t, db, "billing-audit")
	logger, err := postgresadapter.NewSplitAuditLogger(auditPool)
	if err != nil {
		t.Fatalf("NewSplitAuditLogger: %v", err)
	}
	ctx := newCtx(t)
	now := time.Now().UTC().Truncate(time.Microsecond)

	grantID := uuid.New()
	subID := uuid.New()
	invID := uuid.New()
	planID := uuid.New()
	periodStart := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	amount := int64(50000)

	if err := audit.WriteMasterGrantIssued(ctx, logger, audit.MasterGrantIssued{
		GrantID:     grantID,
		Kind:        "extra_tokens",
		TenantID:    tenantID,
		ActorUserID: userID,
		Reason:      "issued by ops for q2 onboarding",
		Amount:      &amount,
		Outcome:     audit.OutcomeAllow,
		OccurredAt:  now,
	}); err != nil {
		t.Fatalf("WriteMasterGrantIssued: %v", err)
	}
	if err := audit.WriteSubscriptionCreated(ctx, logger, audit.SubscriptionCreated{
		SubscriptionID:     subID,
		TenantID:           tenantID,
		PlanID:             planID,
		CurrentPeriodStart: periodStart,
		ActorUserID:        userID,
		OccurredAt:         now,
	}); err != nil {
		t.Fatalf("WriteSubscriptionCreated: %v", err)
	}
	if err := audit.WriteInvoiceCancelledByMaster(ctx, logger, audit.InvoiceCancelledByMaster{
		InvoiceID:   invID,
		TenantID:    tenantID,
		PeriodStart: periodStart,
		Reason:      "duplicate from upstream gateway",
		ActorUserID: userID,
		OccurredAt:  now,
	}); err != nil {
		t.Fatalf("WriteInvoiceCancelledByMaster: %v", err)
	}

	for _, want := range []struct {
		event   string
		probeID string
	}{
		{"master.grant.issued", grantID.String()},
		{"subscription.created", subID.String()},
		{"invoice.cancelled_by_master", invID.String()},
	} {
		var count int
		if err := db.AdminPool().QueryRow(ctx,
			`SELECT count(*) FROM audit_log_security
			  WHERE event_type = $1
			    AND actor_user_id = $2
			    AND tenant_id = $3`,
			want.event, userID, tenantID,
		).Scan(&count); err != nil {
			t.Fatalf("count %s: %v", want.event, err)
		}
		if count != 1 {
			t.Fatalf("event_type=%q rows=%d, want exactly 1", want.event, count)
		}
		var rawTarget string
		if err := db.AdminPool().QueryRow(ctx,
			`SELECT target::text FROM audit_log_security
			  WHERE event_type = $1
			    AND actor_user_id = $2
			    AND tenant_id = $3`,
			want.event, userID, tenantID,
		).Scan(&rawTarget); err != nil {
			t.Fatalf("read target %s: %v", want.event, err)
		}
		var target map[string]any
		if err := json.Unmarshal([]byte(rawTarget), &target); err != nil {
			t.Fatalf("decode target for %q: %v", want.event, err)
		}
		probeKey := map[string]string{
			"master.grant.issued":         "grant_id",
			"subscription.created":        "subscription_id",
			"invoice.cancelled_by_master": "invoice_id",
		}[want.event]
		if target[probeKey] != want.probeID {
			t.Fatalf("event_type=%q target[%s]=%v, want %q", want.event, probeKey, target[probeKey], want.probeID)
		}
		if target["outcome"] != "allow" {
			t.Fatalf("event_type=%q outcome=%v, want allow", want.event, target["outcome"])
		}
	}
}

// TestBillingAudit_RejectsLegacyDottedEvent_NotInCheck ensures the CHECK
// clause is the authoritative gate: an arbitrary "subscription.foo"
// event_type is rejected by the database even if the application
// layer mistakenly produced it. Protects against future drift between
// the SecurityEvent map and the migration vocabulary.
func TestBillingAudit_RejectsLegacyDottedEvent_NotInCheck(t *testing.T) {
	db, _ := freshDBWithBillingAudit(t)
	tenantID, userID := seedSplitTenantUser(t, db, "billing-audit-reject")
	ctx := newCtx(t)

	// Direct insert as admin so we exercise the CHECK constraint rather
	// than the SplitAuditLogger's IsKnown allowlist.
	_, err := db.AdminPool().Exec(ctx,
		`INSERT INTO audit_log_security (id, tenant_id, actor_user_id, event_type, target, occurred_at)
		 VALUES (gen_random_uuid(), $1, $2, 'subscription.foo', '{}'::jsonb, now())`,
		tenantID, userID,
	)
	if err == nil {
		t.Fatal("expected CHECK constraint violation for unlisted event_type")
	}
}
