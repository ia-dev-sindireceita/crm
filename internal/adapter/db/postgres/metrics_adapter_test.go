package postgres_test

// SIN-65007 integration tests for the metrics aggregation Postgres
// adapter (managerial dashboard read-model).
//
// These live in the parent postgres_test package (not the
// internal/adapter/db/postgres/metrics subpackage) to share the single
// TestMain / harness with the other postgres_test files — tests that need
// testpg in a separate binary race the ALTER ROLE bootstrap on the shared
// CI cluster (SQLSTATE 28P01), per ADR 0087 and the testpg-shared-cluster
// race memory.

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	pgmetrics "github.com/pericles-luz/crm/internal/adapter/db/postgres/metrics"
	"github.com/pericles-luz/crm/internal/adapter/db/postgres/testpg"
)

// metricsBase is the deterministic anchor for every seeded timestamp.
var metricsBase = time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)

// freshDBWithMetricsAdapter applies the full chain the metrics adapter
// reads from: tenants, users, inbox (conversation/message) + identity,
// and the funnel stage/transition tables.
func freshDBWithMetricsAdapter(t *testing.T) (*testpg.DB, context.Context) {
	t.Helper()
	db := harness.DB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	applyChain(t, ctx, db,
		"0004_create_tenant.up.sql",
		"0005_create_users.up.sql",
		"0088_inbox_contacts.up.sql",
		"0092_identity_link_assignment_history.up.sql",
		"0093_funnel_stage_transition.up.sql",
	)
	return db, ctx
}

func newMetricsStore(t *testing.T, db *testpg.DB) *pgmetrics.Store {
	t.Helper()
	s, err := pgmetrics.New(db.RuntimePool())
	if err != nil {
		t.Fatalf("pgmetrics.New: %v", err)
	}
	return s
}

func seedMetricsTenant(t *testing.T, pool *pgxpool.Pool) uuid.UUID {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	id := uuid.New()
	if _, err := pool.Exec(ctx,
		`INSERT INTO tenants (id, name, host) VALUES ($1, $2, $3)`,
		id, "metrics-"+id.String(), id.String()+".metrics.test",
	); err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	return id
}

func seedMetricsUser(t *testing.T, pool *pgxpool.Pool, tenantID uuid.UUID) uuid.UUID {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	userID := uuid.New()
	if _, err := pool.Exec(ctx,
		`INSERT INTO users (id, tenant_id, email, password_hash, role)
		 VALUES ($1, $2, $3, 'x', 'tenant_common')`,
		userID, tenantID, userID.String()+"@metrics.test",
	); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	return userID
}

func seedMetricsContact(t *testing.T, pool *pgxpool.Pool, tenantID uuid.UUID) uuid.UUID {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	contactID := uuid.New()
	if _, err := pool.Exec(ctx,
		`INSERT INTO contact (id, tenant_id, display_name) VALUES ($1, $2, $3)`,
		contactID, tenantID, "Contact",
	); err != nil {
		t.Fatalf("seed contact: %v", err)
	}
	return contactID
}

// seedMetricsConversation inserts a conversation with an explicit
// created_at / state / channel and an optional last_message_at. A nil
// lastMessageAt leaves the column NULL.
func seedMetricsConversation(t *testing.T, pool *pgxpool.Pool, tenantID, contactID uuid.UUID,
	channel, state string, createdAt time.Time, lastMessageAt *time.Time) uuid.UUID {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	convID := uuid.New()
	if _, err := pool.Exec(ctx,
		`INSERT INTO conversation (id, tenant_id, contact_id, channel, state, created_at, last_message_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		convID, tenantID, contactID, channel, state, createdAt, lastMessageAt,
	); err != nil {
		t.Fatalf("seed conversation: %v", err)
	}
	return convID
}

func seedMetricsMessage(t *testing.T, pool *pgxpool.Pool, tenantID, convID uuid.UUID,
	direction string, createdAt time.Time) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	status := "delivered"
	if direction == "out" {
		status = "sent"
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO message (id, tenant_id, conversation_id, direction, body, status, created_at)
		 VALUES ($1, $2, $3, $4, 'x', $5, $6)`,
		uuid.New(), tenantID, convID, direction, status, createdAt,
	); err != nil {
		t.Fatalf("seed message: %v", err)
	}
}

func metricsStageID(t *testing.T, pool *pgxpool.Pool, tenantID uuid.UUID, key string) uuid.UUID {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var id uuid.UUID
	if err := pool.QueryRow(ctx,
		`SELECT id FROM funnel_stage WHERE tenant_id = $1 AND key = $2`, tenantID, key,
	).Scan(&id); err != nil {
		t.Fatalf("stage id for %q: %v", key, err)
	}
	return id
}

func seedMetricsTransition(t *testing.T, pool *pgxpool.Pool, tenantID, convID, stageID, userID uuid.UUID, at time.Time) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := pool.Exec(ctx,
		`INSERT INTO funnel_transition
		   (id, tenant_id, conversation_id, from_stage_id, to_stage_id, transitioned_by_user_id, transitioned_at, reason)
		 VALUES ($1, $2, $3, NULL, $4, $5, $6, '')`,
		uuid.New(), tenantID, convID, stageID, userID, at,
	); err != nil {
		t.Fatalf("seed transition: %v", err)
	}
}

func TestMetricsAdapter_New_RejectsNilPool(t *testing.T) {
	if _, err := pgmetrics.New(nil); err == nil {
		t.Error("New(nil) err = nil, want postgres.ErrNilPool")
	}
}

func TestMetricsAdapter_Snapshot_RejectsZeroTenant(t *testing.T) {
	db, _ := freshDBWithMetricsAdapter(t)
	store := newMetricsStore(t, db)
	if _, err := store.Snapshot(context.Background(), uuid.Nil, metricsBase); err == nil {
		t.Error("Snapshot(uuid.Nil) err = nil, want validation error")
	}
}

func TestMetricsAdapter_Snapshot_EmptyTenantIsAllZero(t *testing.T) {
	db, _ := freshDBWithMetricsAdapter(t)
	store := newMetricsStore(t, db)
	tenant := seedMetricsTenant(t, db.AdminPool())

	since := metricsBase.Add(-time.Hour)
	got, err := store.Snapshot(context.Background(), tenant, since)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if !got.Since.Equal(since) {
		t.Errorf("Since = %v, want %v", got.Since, since)
	}
	if len(got.ConversationsByState) != 0 {
		t.Errorf("ConversationsByState = %+v, want empty", got.ConversationsByState)
	}
	if len(got.VolumeByChannel) != 0 {
		t.Errorf("VolumeByChannel = %+v, want empty", got.VolumeByChannel)
	}
	if got.FirstResponse.P50 != 0 || got.FirstResponse.P90 != 0 {
		t.Errorf("FirstResponse = %+v, want zero", got.FirstResponse)
	}
	if got.Resolution.P50 != 0 || got.Resolution.P90 != 0 {
		t.Errorf("Resolution = %+v, want zero", got.Resolution)
	}
	// The funnel distribution still lists every seeded stage, all zero.
	if len(got.FunnelByStage) != 5 {
		t.Fatalf("FunnelByStage len = %d, want 5 seeded stages", len(got.FunnelByStage))
	}
	for _, sc := range got.FunnelByStage {
		if sc.Count != 0 {
			t.Errorf("stage %q count = %d, want 0", sc.Key, sc.Count)
		}
	}
}

func TestMetricsAdapter_Snapshot_AggregatesAcrossDimensions(t *testing.T) {
	db, _ := freshDBWithMetricsAdapter(t)
	store := newMetricsStore(t, db)
	admin := db.AdminPool()
	tenant := seedMetricsTenant(t, admin)
	user := seedMetricsUser(t, admin, tenant)
	contact := seedMetricsContact(t, admin, tenant)

	// Conversation A: open, whatsapp, replied 10s after creation.
	convA := seedMetricsConversation(t, admin, tenant, contact, "whatsapp", "open", metricsBase, nil)
	seedMetricsMessage(t, admin, tenant, convA, "in", metricsBase)
	seedMetricsMessage(t, admin, tenant, convA, "out", metricsBase.Add(10*time.Second))

	// Conversation B: closed, whatsapp, replied 20s after creation, last
	// message 100s after creation (the resolution proxy span).
	lastB := metricsBase.Add(100 * time.Second)
	convB := seedMetricsConversation(t, admin, tenant, contact, "whatsapp", "closed", metricsBase, &lastB)
	seedMetricsMessage(t, admin, tenant, convB, "out", metricsBase.Add(20*time.Second))

	// Conversation C: open, telegram, never replied — excluded from the
	// first-response sample but counted in state + channel volume.
	seedMetricsConversation(t, admin, tenant, contact, "telegram", "open", metricsBase, nil)

	// Funnel placement: A → novo, B → ganho.
	seedMetricsTransition(t, admin, tenant, convA, metricsStageID(t, admin, tenant, "novo"), user, metricsBase.Add(time.Minute))
	seedMetricsTransition(t, admin, tenant, convB, metricsStageID(t, admin, tenant, "ganho"), user, metricsBase.Add(time.Minute))

	got, err := store.Snapshot(context.Background(), tenant, metricsBase.Add(-time.Hour))
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}

	// State split: closed=1, open=2 (ordered by state).
	wantState := map[string]int64{"closed": 1, "open": 2}
	if len(got.ConversationsByState) != 2 {
		t.Fatalf("ConversationsByState = %+v, want 2 entries", got.ConversationsByState)
	}
	if got.ConversationsByState[0].State != "closed" {
		t.Errorf("ConversationsByState[0].State = %q, want closed (sorted)", got.ConversationsByState[0].State)
	}
	for _, sc := range got.ConversationsByState {
		if wantState[sc.State] != sc.Count {
			t.Errorf("state %q count = %d, want %d", sc.State, sc.Count, wantState[sc.State])
		}
	}

	// Channel volume: telegram=1, whatsapp=2 (ordered by channel).
	if len(got.VolumeByChannel) != 2 || got.VolumeByChannel[0].Channel != "telegram" {
		t.Fatalf("VolumeByChannel = %+v, want telegram first then whatsapp", got.VolumeByChannel)
	}
	wantChan := map[string]int64{"telegram": 1, "whatsapp": 2}
	for _, cc := range got.VolumeByChannel {
		if wantChan[cc.Channel] != cc.Count {
			t.Errorf("channel %q count = %d, want %d", cc.Channel, cc.Count, wantChan[cc.Channel])
		}
	}

	// First response over samples {10s, 20s}: p50=15s, p90=19s.
	if got.FirstResponse.P50 != 15*time.Second {
		t.Errorf("FirstResponse.P50 = %v, want 15s", got.FirstResponse.P50)
	}
	if got.FirstResponse.P90 != 19*time.Second {
		t.Errorf("FirstResponse.P90 = %v, want 19s", got.FirstResponse.P90)
	}

	// Resolution proxy over the single closed conversation: 100s.
	if got.Resolution.P50 != 100*time.Second {
		t.Errorf("Resolution.P50 = %v, want 100s", got.Resolution.P50)
	}
	if got.Resolution.P90 != 100*time.Second {
		t.Errorf("Resolution.P90 = %v, want 100s", got.Resolution.P90)
	}

	// Funnel distribution: novo=1, ganho=1, the rest 0, ordered by position.
	wantStage := map[string]int64{"novo": 1, "qualificando": 0, "proposta": 0, "ganho": 1, "perdido": 0}
	if len(got.FunnelByStage) != 5 {
		t.Fatalf("FunnelByStage len = %d, want 5", len(got.FunnelByStage))
	}
	lastPos := -1
	for _, sc := range got.FunnelByStage {
		if sc.Position <= lastPos {
			t.Errorf("FunnelByStage not ordered by position: %q at %d after %d", sc.Key, sc.Position, lastPos)
		}
		lastPos = sc.Position
		if wantStage[sc.Key] != sc.Count {
			t.Errorf("stage %q count = %d, want %d", sc.Key, sc.Count, wantStage[sc.Key])
		}
	}
}

func TestMetricsAdapter_Snapshot_WindowExcludesOlderConversations(t *testing.T) {
	db, _ := freshDBWithMetricsAdapter(t)
	store := newMetricsStore(t, db)
	admin := db.AdminPool()
	tenant := seedMetricsTenant(t, admin)
	contact := seedMetricsContact(t, admin, tenant)

	// In-window conversation and a much older one.
	seedMetricsConversation(t, admin, tenant, contact, "whatsapp", "open", metricsBase, nil)
	seedMetricsConversation(t, admin, tenant, contact, "whatsapp", "open", metricsBase.Add(-72*time.Hour), nil)

	got, err := store.Snapshot(context.Background(), tenant, metricsBase.Add(-time.Hour))
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	var total int64
	for _, sc := range got.ConversationsByState {
		total += sc.Count
	}
	if total != 1 {
		t.Errorf("windowed conversation count = %d, want 1 (older excluded)", total)
	}
}

// freshDBWithoutFunnel applies the inbox chain but NOT the funnel
// migration (0093), so funnel_stage / funnel_transition are absent. The
// metrics adapter's funnel aggregation must surface that query error
// wrapped through Snapshot rather than panicking or returning a partial
// snapshot.
func freshDBWithoutFunnel(t *testing.T) *testpg.DB {
	t.Helper()
	db := harness.DB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	applyChain(t, ctx, db,
		"0004_create_tenant.up.sql",
		"0005_create_users.up.sql",
		"0088_inbox_contacts.up.sql",
		"0092_identity_link_assignment_history.up.sql",
	)
	return db
}

func TestMetricsAdapter_Snapshot_PropagatesQueryError(t *testing.T) {
	db := freshDBWithoutFunnel(t)
	store := newMetricsStore(t, db)
	tenant := seedMetricsTenant(t, db.AdminPool())

	_, err := store.Snapshot(context.Background(), tenant, metricsBase.Add(-time.Hour))
	if err == nil {
		t.Fatal("Snapshot err = nil, want funnel-by-stage query error (funnel tables absent)")
	}
}

func TestMetricsAdapter_Snapshot_TenantIsolatedByRLS(t *testing.T) {
	db, _ := freshDBWithMetricsAdapter(t)
	store := newMetricsStore(t, db)
	admin := db.AdminPool()
	tenantA := seedMetricsTenant(t, admin)
	tenantB := seedMetricsTenant(t, admin)
	contactB := seedMetricsContact(t, admin, tenantB)

	// Only tenant B has a conversation; tenant A must see none of it.
	seedMetricsConversation(t, admin, tenantB, contactB, "whatsapp", "open", metricsBase, nil)

	gotA, err := store.Snapshot(context.Background(), tenantA, metricsBase.Add(-time.Hour))
	if err != nil {
		t.Fatalf("Snapshot tenantA: %v", err)
	}
	if len(gotA.ConversationsByState) != 0 || len(gotA.VolumeByChannel) != 0 {
		t.Errorf("RLS leaked tenantB data into tenantA: %+v", gotA)
	}

	gotB, err := store.Snapshot(context.Background(), tenantB, metricsBase.Add(-time.Hour))
	if err != nil {
		t.Fatalf("Snapshot tenantB: %v", err)
	}
	var totalB int64
	for _, sc := range gotB.ConversationsByState {
		totalB += sc.Count
	}
	if totalB != 1 {
		t.Errorf("tenantB conversation count = %d, want 1", totalB)
	}
}
