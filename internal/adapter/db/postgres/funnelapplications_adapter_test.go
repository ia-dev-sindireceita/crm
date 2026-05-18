package postgres_test

// SIN-62960 integration tests for the funnel applications Postgres
// adapter.
//
// Lives in the parent postgres_test package (not the
// internal/adapter/db/postgres/funnelapplications subpackage) so it
// shares the TestMain / harness with the other postgres_test files —
// tests that need testpg in a separate binary race the ALTER ROLE
// bootstrap on the shared CI cluster (SQLSTATE 28P01), per memory
// `testpg shared-cluster ALTER ROLE race`.

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	pgfunnelapps "github.com/pericles-luz/crm/internal/adapter/db/postgres/funnelapplications"
	"github.com/pericles-luz/crm/internal/adapter/db/postgres/testpg"
	"github.com/pericles-luz/crm/internal/funnel/engine"
)

// freshDBWithFunnelApplications applies the migration chain the funnel
// applications adapter needs: tenants, the Fase 4 base schema (0102,
// which owns funnel_rules), and the new 0103 idempotency table.
func freshDBWithFunnelApplications(t *testing.T) *testpg.DB {
	t.Helper()
	db := harness.DB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	applyChain(t, ctx, db,
		"0004_create_tenant.up.sql",
		"0005_create_users.up.sql",
		"0088_inbox_contacts.up.sql",
		"0089_wallet_basic.up.sql",
		"0097_subscription_plan_invoice_master_grant.up.sql",
		"0102_phase4_marketing_billing_dunning.up.sql",
		"0103_funnel_rule_applications.up.sql",
	)
	return db
}

func newFunnelApplicationsStore(t *testing.T, db *testpg.DB) *pgfunnelapps.Store {
	t.Helper()
	s, err := pgfunnelapps.New(db.RuntimePool())
	if err != nil {
		t.Fatalf("pgfunnelapps.New: %v", err)
	}
	return s
}

func seedFunnelApplicationsTenant(t *testing.T, pool *pgxpool.Pool) uuid.UUID {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	id := uuid.New()
	if _, err := pool.Exec(ctx,
		`INSERT INTO tenants (id, name, host) VALUES ($1, $2, $3)`,
		id, "fa-"+id.String(), id.String()+".fa.test",
	); err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	return id
}

func seedFunnelRuleForApplications(t *testing.T, pool *pgxpool.Pool, tenantID uuid.UUID) uuid.UUID {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	triggerJSON, _ := json.Marshal(map[string]any{"phrase": "orçamento"})
	actionJSON, _ := json.Marshal(map[string]any{"stage_key": "qualificando"})
	id := uuid.New()
	now := time.Now().UTC().Truncate(time.Microsecond)
	if _, err := pool.Exec(ctx, `
		INSERT INTO funnel_rules
		  (id, tenant_id, channel, team_id, name,
		   trigger_type, trigger_config, action_type, action_config,
		   enabled, created_at, updated_at)
		VALUES ($1, $2, 'webchat', NULL, $3,
		        'message_contains', $4::jsonb,
		        'move_to_stage', $5::jsonb,
		        TRUE, $6, $6)`,
		id, tenantID, "applications-test-rule",
		triggerJSON, actionJSON, now,
	); err != nil {
		t.Fatalf("seed funnel_rule: %v", err)
	}
	return id
}

// ---------------------------------------------------------------------------
// Construction
// ---------------------------------------------------------------------------

func TestFunnelApplicationsAdapter_New_RejectsNilPool(t *testing.T) {
	t.Parallel()
	if _, err := pgfunnelapps.New(nil); err == nil {
		t.Fatal("expected error for nil pool, got nil")
	}
}

// ---------------------------------------------------------------------------
// IsApplied + Record happy path
// ---------------------------------------------------------------------------

func TestFunnelApplicationsAdapter_RecordThenIsApplied(t *testing.T) {
	db := freshDBWithFunnelApplications(t)
	store := newFunnelApplicationsStore(t, db)
	tenant := seedFunnelApplicationsTenant(t, db.AdminPool())
	ruleID := seedFunnelRuleForApplications(t, db.AdminPool(), tenant)
	messageID := uuid.New()
	conversationID := uuid.New()
	now := time.Now().UTC().Truncate(time.Microsecond)
	ctx := newCtx(t)

	applied, err := store.IsApplied(ctx, tenant, ruleID, messageID)
	if err != nil {
		t.Fatalf("IsApplied (pre): %v", err)
	}
	if applied {
		t.Fatalf("IsApplied (pre) = true, want false on fresh table")
	}

	app := engine.Application{
		TenantID:       tenant,
		RuleID:         ruleID,
		MessageID:      messageID,
		ConversationID: conversationID,
		ActionType:     "move_to_stage",
		AppliedAt:      now,
	}
	if err := store.Record(ctx, app); err != nil {
		t.Fatalf("Record: %v", err)
	}

	applied, err = store.IsApplied(ctx, tenant, ruleID, messageID)
	if err != nil {
		t.Fatalf("IsApplied (post): %v", err)
	}
	if !applied {
		t.Fatalf("IsApplied (post) = false, want true after Record")
	}
}

// ---------------------------------------------------------------------------
// UNIQUE conflict mapping
// ---------------------------------------------------------------------------

func TestFunnelApplicationsAdapter_Record_DuplicateReturnsErrAlreadyApplied(t *testing.T) {
	db := freshDBWithFunnelApplications(t)
	store := newFunnelApplicationsStore(t, db)
	tenant := seedFunnelApplicationsTenant(t, db.AdminPool())
	ruleID := seedFunnelRuleForApplications(t, db.AdminPool(), tenant)
	messageID := uuid.New()
	conversationID := uuid.New()
	now := time.Now().UTC().Truncate(time.Microsecond)
	ctx := newCtx(t)

	app := engine.Application{
		TenantID:       tenant,
		RuleID:         ruleID,
		MessageID:      messageID,
		ConversationID: conversationID,
		ActionType:     "move_to_stage",
		AppliedAt:      now,
	}
	if err := store.Record(ctx, app); err != nil {
		t.Fatalf("first Record: %v", err)
	}
	err := store.Record(ctx, app)
	if !errors.Is(err, engine.ErrAlreadyApplied) {
		t.Fatalf("second Record = %v, want ErrAlreadyApplied", err)
	}
}

// ---------------------------------------------------------------------------
// Cross-tenant isolation (RLS)
// ---------------------------------------------------------------------------

func TestFunnelApplicationsAdapter_IsApplied_CrossTenantInvisible(t *testing.T) {
	db := freshDBWithFunnelApplications(t)
	store := newFunnelApplicationsStore(t, db)
	adminPool := db.AdminPool()
	tenantA := seedFunnelApplicationsTenant(t, adminPool)
	tenantB := seedFunnelApplicationsTenant(t, adminPool)
	ruleID := seedFunnelRuleForApplications(t, adminPool, tenantA)
	messageID := uuid.New()
	conversationID := uuid.New()
	now := time.Now().UTC().Truncate(time.Microsecond)
	ctx := newCtx(t)

	if err := store.Record(ctx, engine.Application{
		TenantID:       tenantA,
		RuleID:         ruleID,
		MessageID:      messageID,
		ConversationID: conversationID,
		ActionType:     "move_to_stage",
		AppliedAt:      now,
	}); err != nil {
		t.Fatalf("Record tenantA: %v", err)
	}
	// Same (rule, message) but tenantB context — must look unapplied
	// because RLS hides tenantA's row from tenantB's view.
	applied, err := store.IsApplied(ctx, tenantB, ruleID, messageID)
	if err != nil {
		t.Fatalf("IsApplied tenantB: %v", err)
	}
	if applied {
		t.Fatalf("RLS leak: tenantB sees tenantA's application row")
	}
}

// ---------------------------------------------------------------------------
// Required-input validation
// ---------------------------------------------------------------------------

func TestFunnelApplicationsAdapter_RejectsBadInputs(t *testing.T) {
	t.Parallel()
	db := freshDBWithFunnelApplications(t)
	store := newFunnelApplicationsStore(t, db)
	ctx := newCtx(t)
	tenant := uuid.New()
	rule := uuid.New()
	msg := uuid.New()

	if _, err := store.IsApplied(ctx, uuid.Nil, rule, msg); err == nil {
		t.Error("IsApplied nil tenant: want error")
	}
	if _, err := store.IsApplied(ctx, tenant, uuid.Nil, msg); err == nil {
		t.Error("IsApplied nil rule: want error")
	}
	if _, err := store.IsApplied(ctx, tenant, rule, uuid.Nil); err == nil {
		t.Error("IsApplied nil message: want error")
	}

	base := engine.Application{
		TenantID:       tenant,
		RuleID:         rule,
		MessageID:      msg,
		ConversationID: uuid.New(),
		ActionType:     "move_to_stage",
		AppliedAt:      time.Now().UTC(),
	}
	cases := map[string]func(engine.Application) engine.Application{
		"nil tenant":       func(a engine.Application) engine.Application { a.TenantID = uuid.Nil; return a },
		"nil rule":         func(a engine.Application) engine.Application { a.RuleID = uuid.Nil; return a },
		"nil message":      func(a engine.Application) engine.Application { a.MessageID = uuid.Nil; return a },
		"nil conversation": func(a engine.Application) engine.Application { a.ConversationID = uuid.Nil; return a },
		"blank action":     func(a engine.Application) engine.Application { a.ActionType = ""; return a },
	}
	for name, mutate := range cases {
		name, mutate := name, mutate
		t.Run(name, func(t *testing.T) {
			if err := store.Record(ctx, mutate(base)); err == nil {
				t.Errorf("Record(%s): want error, got nil", name)
			}
		})
	}
}
