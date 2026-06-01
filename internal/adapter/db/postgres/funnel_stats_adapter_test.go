package postgres_test

// SIN-63962 integration tests for funnel.StatsRepository pgx adapter.
//
// Lives in the parent postgres_test package (not the
// internal/adapter/db/postgres/funnel subpackage) to share the
// TestMain / harness with the other postgres_test files — per ADR 0087.

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pericles-luz/crm/internal/adapter/db/postgres/testpg"
	"github.com/pericles-luz/crm/internal/funnel"
)

// freshDBWithFunnelStats applies the minimum migration chain for stats tests.
func freshDBWithFunnelStats(t *testing.T) *testpg.DB {
	t.Helper()
	db, _ := freshDBWithFunnelF2(t)
	return db
}

// seedStatsConversation inserts a contact + conversation with channel and
// optional assigned_user_id, and returns the conversation id.
func seedStatsConversation(t *testing.T, pool *pgxpool.Pool, tenantID, userID uuid.UUID, channel string) uuid.UUID {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	contactID := uuid.New()
	if _, err := pool.Exec(ctx,
		`INSERT INTO contact (id, tenant_id, display_name) VALUES ($1, $2, $3)`,
		contactID, tenantID, "User-"+contactID.String(),
	); err != nil {
		t.Fatalf("seed contact: %v", err)
	}
	convID := uuid.New()
	if _, err := pool.Exec(ctx,
		`INSERT INTO conversation (id, tenant_id, contact_id, channel, state, assigned_user_id)
		 VALUES ($1, $2, $3, $4, 'open', $5)`,
		convID, tenantID, contactID, channel, userID,
	); err != nil {
		t.Fatalf("seed conversation: %v", err)
	}
	return convID
}

// moveToStage inserts a funnel_transition to move convID to the stage
// identified by stageKey, at the given transitioned_at time.
func moveToStage(t *testing.T, pool *pgxpool.Pool, tenantID, convID, userID uuid.UUID, stageKey string, at time.Time) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var stageID uuid.UUID
	if err := pool.QueryRow(ctx,
		`SELECT id FROM funnel_stage WHERE tenant_id = $1 AND key = $2`,
		tenantID, stageKey,
	).Scan(&stageID); err != nil {
		t.Fatalf("resolve stage %q: %v", stageKey, err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO funnel_transition
		     (id, tenant_id, conversation_id, to_stage_id, transitioned_by_user_id, transitioned_at)
		 VALUES ($1, $2, $3, $4, $5, $6)`,
		uuid.New(), tenantID, convID, stageID, userID, at,
	); err != nil {
		t.Fatalf("move %v → %q: %v", convID, stageKey, err)
	}
}

func TestFunnelStats_TenantIsolation(t *testing.T) {
	db := freshDBWithFunnelStats(t)
	store := newFunnelStore(t, db)

	adminPool := db.AdminPool()
	now := time.Now().UTC()

	tenantA := seedFunnelTenant(t, adminPool)
	tenantB := seedFunnelTenant(t, adminPool)

	userA := seedFunnelUser(t, adminPool, tenantA)

	convA := seedStatsConversation(t, adminPool, tenantA, userA, "whatsapp")
	moveToStage(t, adminPool, tenantA, convA, userA, "novo", now.Add(-1*time.Hour))

	// Query from tenant A perspective — should see its conversations.
	q := funnel.StatsQuery{
		Period: funnel.Period{Kind: funnel.PeriodLast7d},
	}
	aggA, err := store.Stats(context.Background(), tenantA, q)
	if err != nil {
		t.Fatalf("Stats tenantA: %v", err)
	}
	if aggA.HeaderKPIs.TotalActive < 1 {
		t.Errorf("tenantA TotalActive = %d, want ≥ 1", aggA.HeaderKPIs.TotalActive)
	}

	// Query from tenant B — should see zero (RLS isolation).
	aggB, err := store.Stats(context.Background(), tenantB, q)
	if err != nil {
		t.Fatalf("Stats tenantB: %v", err)
	}
	if aggB.HeaderKPIs.TotalActive != 0 {
		t.Errorf("tenantB TotalActive = %d, want 0 (RLS leak!)", aggB.HeaderKPIs.TotalActive)
	}
}

func TestFunnelStats_WonLostCountsAndWonRate(t *testing.T) {
	db := freshDBWithFunnelStats(t)
	store := newFunnelStore(t, db)
	adminPool := db.AdminPool()

	now := time.Now().UTC()
	tenantID := seedFunnelTenant(t, adminPool)
	userID := seedFunnelUser(t, adminPool, tenantID)

	// Two ganho conversations in the period.
	for i := 0; i < 2; i++ {
		conv := seedStatsConversation(t, adminPool, tenantID, userID, "whatsapp")
		moveToStage(t, adminPool, tenantID, conv, userID, "novo", now.Add(-5*24*time.Hour))
		moveToStage(t, adminPool, tenantID, conv, userID, "ganho", now.Add(-1*time.Hour))
		// mark closed
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := adminPool.Exec(ctx,
			`UPDATE conversation SET state = 'closed' WHERE id = $1`, conv,
		); err != nil {
			t.Fatalf("close conv: %v", err)
		}
	}
	// One perdido conversation in the period.
	{
		conv := seedStatsConversation(t, adminPool, tenantID, userID, "instagram")
		moveToStage(t, adminPool, tenantID, conv, userID, "novo", now.Add(-5*24*time.Hour))
		moveToStage(t, adminPool, tenantID, conv, userID, "perdido", now.Add(-2*time.Hour))
	}

	q := funnel.StatsQuery{Period: funnel.Period{Kind: funnel.PeriodLast7d}}
	agg, err := store.Stats(context.Background(), tenantID, q)
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}

	if agg.HeaderKPIs.WonCount != 2 {
		t.Errorf("WonCount = %d, want 2", agg.HeaderKPIs.WonCount)
	}
	if agg.HeaderKPIs.LostCount != 1 {
		t.Errorf("LostCount = %d, want 1", agg.HeaderKPIs.LostCount)
	}
	wantRate := 2.0 / 3.0
	if diff := agg.HeaderKPIs.WonRate - wantRate; diff < -0.001 || diff > 0.001 {
		t.Errorf("WonRate = %.4f, want %.4f", agg.HeaderKPIs.WonRate, wantRate)
	}
}

func TestFunnelStats_TotalActiveExcludesTerminalStages(t *testing.T) {
	db := freshDBWithFunnelStats(t)
	store := newFunnelStore(t, db)
	adminPool := db.AdminPool()
	now := time.Now().UTC()

	tenantID := seedFunnelTenant(t, adminPool)
	userID := seedFunnelUser(t, adminPool, tenantID)

	// One active conversation in "novo".
	conv1 := seedStatsConversation(t, adminPool, tenantID, userID, "whatsapp")
	moveToStage(t, adminPool, tenantID, conv1, userID, "novo", now.Add(-1*time.Hour))

	// One in "ganho" (terminal — should not count toward TotalActive).
	conv2 := seedStatsConversation(t, adminPool, tenantID, userID, "whatsapp")
	moveToStage(t, adminPool, tenantID, conv2, userID, "ganho", now.Add(-2*time.Hour))

	q := funnel.StatsQuery{Period: funnel.Period{Kind: funnel.PeriodLast7d}}
	agg, err := store.Stats(context.Background(), tenantID, q)
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if agg.HeaderKPIs.TotalActive != 1 {
		t.Errorf("TotalActive = %d, want 1 (terminal should be excluded)", agg.HeaderKPIs.TotalActive)
	}
}

func TestFunnelStats_OwnerScopeUser(t *testing.T) {
	db := freshDBWithFunnelStats(t)
	store := newFunnelStore(t, db)
	adminPool := db.AdminPool()
	now := time.Now().UTC()

	tenantID := seedFunnelTenant(t, adminPool)
	userA := seedFunnelUser(t, adminPool, tenantID)
	userB := seedFunnelUser(t, adminPool, tenantID)

	// userA has one active conversation.
	convA := seedStatsConversation(t, adminPool, tenantID, userA, "whatsapp")
	moveToStage(t, adminPool, tenantID, convA, userA, "novo", now.Add(-1*time.Hour))

	// userB has two active conversations.
	for i := 0; i < 2; i++ {
		conv := seedStatsConversation(t, adminPool, tenantID, userB, "whatsapp")
		moveToStage(t, adminPool, tenantID, conv, userB, "novo", now.Add(-1*time.Hour))
	}

	q := funnel.StatsQuery{
		Period:     funnel.Period{Kind: funnel.PeriodLast7d},
		OwnerScope: funnel.OwnerScope{Kind: funnel.OwnerScopeUser, UserID: userA},
	}
	agg, err := store.Stats(context.Background(), tenantID, q)
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if agg.HeaderKPIs.TotalActive != 1 {
		t.Errorf("user-scoped TotalActive = %d, want 1", agg.HeaderKPIs.TotalActive)
	}
}

func TestFunnelStats_StoreCompilationGuard(t *testing.T) {
	// Compile-time assertion lives in stats.go; this test ensures the test
	// binary links and the interface is satisfied.
	db := freshDBWithFunnelStats(t)
	var _ funnel.StatsRepository = newFunnelStore(t, db)
}

// TestFunnelStats_AvgTimeInStageWeightedAcrossSubgroups guards the bug fix
// flagged in PR #285 review (B2): SQL groups by (stage, attendant, channel),
// so a single stage produces multiple rows. AvgTimeInStage must be the
// weighted mean across all sub-groups for the stage, not the first row's
// value. Setup: three conversations all in "novo" but assigned to three
// different attendants — older transitions weighted alongside newer ones.
func TestFunnelStats_AvgTimeInStageWeightedAcrossSubgroups(t *testing.T) {
	db := freshDBWithFunnelStats(t)
	store := newFunnelStore(t, db)
	adminPool := db.AdminPool()
	now := time.Now().UTC()

	tenantID := seedFunnelTenant(t, adminPool)
	userA := seedFunnelUser(t, adminPool, tenantID)
	userB := seedFunnelUser(t, adminPool, tenantID)
	userC := seedFunnelUser(t, adminPool, tenantID)

	// Three conversations all in "novo" with different transition times
	// and different attendants → three distinct sub-groups in the SQL.
	//   conv1 (userA, whatsapp): 2 hours ago
	//   conv2 (userB, whatsapp): 4 hours ago
	//   conv3 (userC, instagram): 6 hours ago
	// Expected weighted average = (2 + 4 + 6) / 3 = 4 hours.
	conv1 := seedStatsConversation(t, adminPool, tenantID, userA, "whatsapp")
	moveToStage(t, adminPool, tenantID, conv1, userA, "novo", now.Add(-2*time.Hour))
	conv2 := seedStatsConversation(t, adminPool, tenantID, userB, "whatsapp")
	moveToStage(t, adminPool, tenantID, conv2, userB, "novo", now.Add(-4*time.Hour))
	conv3 := seedStatsConversation(t, adminPool, tenantID, userC, "instagram")
	moveToStage(t, adminPool, tenantID, conv3, userC, "novo", now.Add(-6*time.Hour))

	q := funnel.StatsQuery{Period: funnel.Period{Kind: funnel.PeriodLast7d}}
	agg, err := store.Stats(context.Background(), tenantID, q)
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}

	var novo *funnel.StageStats
	for i := range agg.Stages {
		if agg.Stages[i].StageKey == "novo" {
			novo = &agg.Stages[i]
			break
		}
	}
	if novo == nil {
		t.Fatalf("expected 'novo' stage in result, got stages=%+v", agg.Stages)
	}
	if novo.ActiveCount != 3 {
		t.Errorf("novo.ActiveCount = %d, want 3", novo.ActiveCount)
	}
	// Weighted avg should be ~4 hours; allow ±2 minutes for clock drift
	// between moveToStage timestamps and now() inside the SQL.
	want := 4 * time.Hour
	got := novo.AvgTimeInStage
	if diff := got - want; diff < -2*time.Minute || diff > 2*time.Minute {
		t.Errorf("novo.AvgTimeInStage = %v, want ≈ %v (±2m)", got, want)
	}
}
