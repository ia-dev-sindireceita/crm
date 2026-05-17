package postgres_test

// SIN-62955 integration tests for the funnel rules Postgres adapter.
//
// These live in the parent postgres_test package (not the
// internal/adapter/db/postgres/funnelrules subpackage) so they share
// the TestMain / harness with the other postgres_test files — tests
// that need testpg in a separate binary race the ALTER ROLE bootstrap
// on the shared CI cluster (SQLSTATE 28P01), per ADR 0087 and memory
// `testpg shared-cluster ALTER ROLE race`.

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	pgfunnelrules "github.com/pericles-luz/crm/internal/adapter/db/postgres/funnelrules"
	"github.com/pericles-luz/crm/internal/adapter/db/postgres/testpg"
	"github.com/pericles-luz/crm/internal/funnel/rules"
)

// freshDBWithFunnelRules applies the migration chain funnel rules
// needs: tenants (0004) + the phase 4 migration 0102 that owns
// funnel_rules.
func freshDBWithFunnelRules(t *testing.T) *testpg.DB {
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
	)
	return db
}

func newFunnelRulesStore(t *testing.T, db *testpg.DB) *pgfunnelrules.Store {
	t.Helper()
	s, err := pgfunnelrules.New(db.RuntimePool())
	if err != nil {
		t.Fatalf("pgfunnelrules.New: %v", err)
	}
	return s
}

func seedFunnelRulesTenant(t *testing.T, pool *pgxpool.Pool) uuid.UUID {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	id := uuid.New()
	if _, err := pool.Exec(ctx,
		`INSERT INTO tenants (id, name, host) VALUES ($1, $2, $3)`,
		id, "fr-"+id.String(), id.String()+".fr.test",
	); err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	return id
}

// insertFunnelRule writes one row directly via the admin pool
// (BYPASSRLS) so the test owns the canonical timestamp and id.
// `channel` empty + `teamID == uuid.Nil` => tenant scope; provide
// the other slot to mint team/channel scope.
func insertFunnelRule(t *testing.T, pool *pgxpool.Pool, r rules.Rule) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	triggerJSON, err := json.Marshal(r.TriggerConfig)
	if err != nil {
		t.Fatalf("marshal trigger_config: %v", err)
	}
	actionJSON, err := json.Marshal(r.ActionConfig)
	if err != nil {
		t.Fatalf("marshal action_config: %v", err)
	}
	var channel any
	if r.Channel != "" {
		channel = r.Channel
	}
	var teamID any
	if r.TeamID != nil {
		teamID = *r.TeamID
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO funnel_rules
		  (id, tenant_id, channel, team_id, name,
		   trigger_type, trigger_config, action_type, action_config,
		   enabled, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7::jsonb, $8, $9::jsonb, $10, $11, $12)`,
		r.ID, r.TenantID, channel, teamID, r.Name,
		string(r.TriggerType), triggerJSON,
		string(r.ActionType), actionJSON,
		r.Enabled, r.CreatedAt, r.UpdatedAt,
	); err != nil {
		t.Fatalf("insert funnel_rule: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Construction
// ---------------------------------------------------------------------------

func TestFunnelRulesAdapter_New_RejectsNilPool(t *testing.T) {
	t.Parallel()
	if _, err := pgfunnelrules.New(nil); err == nil {
		t.Fatal("expected error for nil pool, got nil")
	}
}

// ---------------------------------------------------------------------------
// ListEffectiveForChannel — the only port method, exercised end-to-end
// ---------------------------------------------------------------------------

func TestFunnelRulesAdapter_ListEffectiveForChannel_HonoursAllThreeScopes(t *testing.T) {
	db := freshDBWithFunnelRules(t)
	store := newFunnelRulesStore(t, db)
	adminPool := db.AdminPool()
	tenant := seedFunnelRulesTenant(t, adminPool)
	team := uuid.New()
	otherTeam := uuid.New()
	now := time.Now().UTC().Truncate(time.Microsecond)
	ctx := newCtx(t)

	mk := func(id byte, name string, opt func(*rules.Rule)) rules.Rule {
		r := rules.Rule{
			ID:            uuid.New(),
			TenantID:      tenant,
			Name:          name,
			TriggerType:   rules.TriggerTypeMessageContains,
			TriggerConfig: map[string]any{"phrase": "preço"},
			ActionType:    rules.ActionTypeMoveToStage,
			ActionConfig:  map[string]any{"stage_key": name},
			Enabled:       true,
			CreatedAt:     now.Add(time.Duration(id) * time.Minute),
			UpdatedAt:     now.Add(time.Duration(id) * time.Minute),
		}
		if opt != nil {
			opt(&r)
		}
		return r
	}

	// One rule per bucket + two negative-case rules (wrong channel,
	// wrong team, disabled).
	chRule := mk(1, "channel-hit", func(r *rules.Rule) { r.Channel = "webchat" })
	wrongChannel := mk(2, "wrong-channel", func(r *rules.Rule) { r.Channel = "whatsapp" })
	teamRule := mk(3, "team-hit", func(r *rules.Rule) { r.TeamID = &team })
	wrongTeam := mk(4, "wrong-team", func(r *rules.Rule) { r.TeamID = &otherTeam })
	tenantRule := mk(5, "tenant-default", nil)
	disabled := mk(6, "disabled-channel", func(r *rules.Rule) {
		r.Channel = "webchat"
		r.Enabled = false
	})
	for _, r := range []rules.Rule{chRule, wrongChannel, teamRule, wrongTeam, tenantRule, disabled} {
		insertFunnelRule(t, adminPool, r)
	}

	got, err := store.ListEffectiveForChannel(ctx, tenant, "webchat", team)
	if err != nil {
		t.Fatalf("ListEffectiveForChannel: %v", err)
	}
	wantNames := []string{"channel-hit", "team-hit", "tenant-default"}
	if len(got) != len(wantNames) {
		t.Fatalf("want %d rules, got %d (%+v)", len(wantNames), len(got), got)
	}
	for i, want := range wantNames {
		if got[i].ActionConfig["stage_key"] != want {
			t.Fatalf("rule[%d]: want %q, got %+v", i, want, got[i])
		}
	}
}

func TestFunnelRulesAdapter_ListEffectiveForChannel_TeamNilSkipsTeamScope(t *testing.T) {
	db := freshDBWithFunnelRules(t)
	store := newFunnelRulesStore(t, db)
	adminPool := db.AdminPool()
	tenant := seedFunnelRulesTenant(t, adminPool)
	team := uuid.New()
	now := time.Now().UTC().Truncate(time.Microsecond)
	ctx := newCtx(t)

	insertFunnelRule(t, adminPool, rules.Rule{
		ID:            uuid.New(),
		TenantID:      tenant,
		TeamID:        &team,
		Name:          "team-rule",
		TriggerType:   rules.TriggerTypeMessageContains,
		TriggerConfig: map[string]any{"phrase": "preço"},
		ActionType:    rules.ActionTypeMoveToStage,
		ActionConfig:  map[string]any{"stage_key": "team-rule"},
		Enabled:       true,
		CreatedAt:     now,
		UpdatedAt:     now,
	})
	insertFunnelRule(t, adminPool, rules.Rule{
		ID:            uuid.New(),
		TenantID:      tenant,
		Name:          "tenant-rule",
		TriggerType:   rules.TriggerTypeMessageContains,
		TriggerConfig: map[string]any{"phrase": "preço"},
		ActionType:    rules.ActionTypeMoveToStage,
		ActionConfig:  map[string]any{"stage_key": "tenant-rule"},
		Enabled:       true,
		CreatedAt:     now.Add(1 * time.Minute),
		UpdatedAt:     now.Add(1 * time.Minute),
	})

	// teamID = uuid.Nil — the SQL branch (channel IS NULL AND team_id
	// = $2) collapses to NULL = NULL → false, so the team row is
	// excluded. Only the tenant rule surfaces.
	got, err := store.ListEffectiveForChannel(ctx, tenant, "", uuid.Nil)
	if err != nil {
		t.Fatalf("ListEffectiveForChannel: %v", err)
	}
	if len(got) != 1 || got[0].ActionConfig["stage_key"] != "tenant-rule" {
		t.Fatalf("want 1 tenant rule, got %+v", got)
	}
}

func TestFunnelRulesAdapter_ListEffectiveForChannel_CrossTenantIsolation(t *testing.T) {
	db := freshDBWithFunnelRules(t)
	store := newFunnelRulesStore(t, db)
	adminPool := db.AdminPool()
	tenantA := seedFunnelRulesTenant(t, adminPool)
	tenantB := seedFunnelRulesTenant(t, adminPool)
	now := time.Now().UTC().Truncate(time.Microsecond)
	ctx := newCtx(t)

	insertFunnelRule(t, adminPool, rules.Rule{
		ID:            uuid.New(),
		TenantID:      tenantA,
		Channel:       "webchat",
		Name:          "tenant-a",
		TriggerType:   rules.TriggerTypeMessageContains,
		TriggerConfig: map[string]any{"phrase": "preço"},
		ActionType:    rules.ActionTypeMoveToStage,
		ActionConfig:  map[string]any{"stage_key": "a-hit"},
		Enabled:       true,
		CreatedAt:     now,
		UpdatedAt:     now,
	})
	insertFunnelRule(t, adminPool, rules.Rule{
		ID:            uuid.New(),
		TenantID:      tenantB,
		Channel:       "webchat",
		Name:          "tenant-b",
		TriggerType:   rules.TriggerTypeMessageContains,
		TriggerConfig: map[string]any{"phrase": "preço"},
		ActionType:    rules.ActionTypeMoveToStage,
		ActionConfig:  map[string]any{"stage_key": "b-hit"},
		Enabled:       true,
		CreatedAt:     now,
		UpdatedAt:     now,
	})

	got, err := store.ListEffectiveForChannel(ctx, tenantA, "webchat", uuid.Nil)
	if err != nil {
		t.Fatalf("ListEffectiveForChannel: %v", err)
	}
	if len(got) != 1 || got[0].TenantID != tenantA || got[0].ActionConfig["stage_key"] != "a-hit" {
		t.Fatalf("want tenantA's rule only, got %+v", got)
	}
}

func TestFunnelRulesAdapter_ListEffectiveForChannel_RejectsNilTenant(t *testing.T) {
	t.Parallel()
	db := freshDBWithFunnelRules(t)
	store := newFunnelRulesStore(t, db)
	if _, err := store.ListEffectiveForChannel(newCtx(t), uuid.Nil, "webchat", uuid.Nil); err == nil {
		t.Fatal("want error for nil tenant id, got nil")
	}
}

// ---------------------------------------------------------------------------
// SIN-62961 admin port — Create / Get / Update / SetEnabled / Delete / ListAll
// ---------------------------------------------------------------------------

func TestFunnelRulesAdapter_CreateGetUpdateDelete_RoundTrip(t *testing.T) {
	db := freshDBWithFunnelRules(t)
	store := newFunnelRulesStore(t, db)
	tenant := seedFunnelRulesTenant(t, db.AdminPool())
	ctx := newCtx(t)

	r, err := rules.NewRule(uuid.New(), tenant, "webchat", nil, "rule-1",
		rules.TriggerTypeMessageContains, map[string]any{"phrase": "preço"},
		rules.ActionTypeMoveToStage, map[string]any{"stage_key": "novo"},
		true, time.Now().UTC().Truncate(time.Microsecond))
	if err != nil {
		t.Fatalf("NewRule: %v", err)
	}
	if err := store.Create(ctx, r); err != nil {
		t.Fatalf("Create: %v", err)
	}

	got, err := store.Get(ctx, tenant, r.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Name != "rule-1" || got.Channel != "webchat" {
		t.Fatalf("Get returned wrong row: %+v", got)
	}

	r.Name = "rule-1-renamed"
	r.Enabled = false
	if err := store.Update(ctx, r); err != nil {
		t.Fatalf("Update: %v", err)
	}
	got, _ = store.Get(ctx, tenant, r.ID)
	if got.Name != "rule-1-renamed" || got.Enabled {
		t.Fatalf("Update did not stick: %+v", got)
	}

	if err := store.SetEnabled(ctx, tenant, r.ID, true); err != nil {
		t.Fatalf("SetEnabled: %v", err)
	}
	got, _ = store.Get(ctx, tenant, r.ID)
	if !got.Enabled {
		t.Fatal("SetEnabled(true) did not flip the flag")
	}

	if err := store.Delete(ctx, tenant, r.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := store.Get(ctx, tenant, r.ID); err == nil {
		t.Fatal("Get after Delete: want error, got nil")
	}
}

func TestFunnelRulesAdapter_Admin_ListAllOrdersByCascade(t *testing.T) {
	db := freshDBWithFunnelRules(t)
	store := newFunnelRulesStore(t, db)
	adminPool := db.AdminPool()
	tenant := seedFunnelRulesTenant(t, adminPool)
	team := uuid.New()
	ctx := newCtx(t)
	now := time.Now().UTC().Truncate(time.Microsecond)

	mustCreate := func(channel string, teamID *uuid.UUID, name string, enabled bool, off time.Duration) {
		t.Helper()
		r, err := rules.NewRule(uuid.New(), tenant, channel, teamID, name,
			rules.TriggerTypeMessageContains, map[string]any{"phrase": "x"},
			rules.ActionTypeMoveToStage, map[string]any{"stage_key": "novo"},
			enabled, now.Add(off))
		if err != nil {
			t.Fatalf("NewRule(%s): %v", name, err)
		}
		if err := store.Create(ctx, r); err != nil {
			t.Fatalf("Create(%s): %v", name, err)
		}
	}
	mustCreate("", nil, "tenant-default", true, 0)
	mustCreate("", &team, "team-rule", false, 1*time.Minute) // disabled still surfaces
	mustCreate("webchat", nil, "channel-rule", true, 2*time.Minute)

	all, err := store.ListAll(ctx, tenant)
	if err != nil {
		t.Fatalf("ListAll: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("want 3 rules, got %d (%+v)", len(all), all)
	}
	wantOrder := []string{"channel-rule", "team-rule", "tenant-default"}
	for i, w := range wantOrder {
		if all[i].Name != w {
			t.Fatalf("rule[%d]: want %q, got %q", i, w, all[i].Name)
		}
	}
}

func TestFunnelRulesAdapter_Admin_RejectsNilTenant(t *testing.T) {
	t.Parallel()
	db := freshDBWithFunnelRules(t)
	store := newFunnelRulesStore(t, db)
	ctx := newCtx(t)
	if _, err := store.ListAll(ctx, uuid.Nil); err == nil {
		t.Fatal("ListAll(nil): want error")
	}
	if _, err := store.Get(ctx, uuid.Nil, uuid.New()); err == nil {
		t.Fatal("Get(nil): want error")
	}
	if err := store.Create(ctx, rules.Rule{}); err == nil {
		t.Fatal("Create(nil tenant): want error")
	}
	if err := store.Update(ctx, rules.Rule{}); err == nil {
		t.Fatal("Update(nil tenant): want error")
	}
	if err := store.SetEnabled(ctx, uuid.Nil, uuid.New(), true); err == nil {
		t.Fatal("SetEnabled(nil): want error")
	}
	if err := store.Delete(ctx, uuid.Nil, uuid.New()); err == nil {
		t.Fatal("Delete(nil): want error")
	}
}

func TestFunnelRulesAdapter_Admin_NotFoundPaths(t *testing.T) {
	db := freshDBWithFunnelRules(t)
	store := newFunnelRulesStore(t, db)
	tenant := seedFunnelRulesTenant(t, db.AdminPool())
	ctx := newCtx(t)
	missing := uuid.New()

	if _, err := store.Get(ctx, tenant, missing); err == nil {
		t.Fatal("Get(missing): want ErrNotFound")
	}
	if err := store.Update(ctx, rules.Rule{
		ID: missing, TenantID: tenant,
		TriggerType: rules.TriggerTypeMessageContains,
		ActionType:  rules.ActionTypeMoveToStage,
	}); err == nil {
		t.Fatal("Update(missing): want ErrNotFound")
	}
	if err := store.SetEnabled(ctx, tenant, missing, true); err == nil {
		t.Fatal("SetEnabled(missing): want ErrNotFound")
	}
	if err := store.Delete(ctx, tenant, missing); err == nil {
		t.Fatal("Delete(missing): want ErrNotFound")
	}
}
