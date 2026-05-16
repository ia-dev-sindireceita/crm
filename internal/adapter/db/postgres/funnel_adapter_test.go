package postgres_test

// SIN-62792 integration tests for the funnel Postgres adapter.
//
// These live in the parent postgres_test package (not the
// internal/adapter/db/postgres/funnel subpackage) to share the
// TestMain / harness with the other postgres_test files — tests that
// need testpg in a separate binary race the ALTER ROLE bootstrap on
// the shared CI cluster (SQLSTATE 28P01), per ADR 0087.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	pgfunnel "github.com/pericles-luz/crm/internal/adapter/db/postgres/funnel"
	"github.com/pericles-luz/crm/internal/adapter/db/postgres/testpg"
	"github.com/pericles-luz/crm/internal/funnel"
)

// seedFunnelTenant inserts a tenant and returns its id. The 0093
// after-insert trigger seeds the five default funnel_stage rows.
func seedFunnelTenant(t *testing.T, pool *pgxpool.Pool) uuid.UUID {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	id := uuid.New()
	if _, err := pool.Exec(ctx,
		`INSERT INTO tenants (id, name, host) VALUES ($1, $2, $3)`,
		id, "funnel-"+id.String(), id.String()+".funnel.test",
	); err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	return id
}

// seedFunnelUser inserts an agent user under the tenant scope so the
// funnel_transition.transitioned_by_user_id FK is satisfied.
func seedFunnelUser(t *testing.T, pool *pgxpool.Pool, tenantID uuid.UUID) uuid.UUID {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	userID := uuid.New()
	if _, err := pool.Exec(ctx,
		`INSERT INTO users (id, tenant_id, email, password_hash, role)
		 VALUES ($1, $2, $3, 'x', 'agent')`,
		userID, tenantID, userID.String()+"@funnel.test",
	); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	return userID
}

// seedFunnelContactAndConversation inserts a contact + conversation
// under the tenant scope so the funnel_transition.conversation_id FK
// resolves.
func seedFunnelContactAndConversation(t *testing.T, pool *pgxpool.Pool, tenantID uuid.UUID) uuid.UUID {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	contactID := uuid.New()
	if _, err := pool.Exec(ctx,
		`INSERT INTO contact (id, tenant_id, display_name) VALUES ($1, $2, $3)`,
		contactID, tenantID, "Alice",
	); err != nil {
		t.Fatalf("seed contact: %v", err)
	}
	convID := uuid.New()
	if _, err := pool.Exec(ctx,
		`INSERT INTO conversation (id, tenant_id, contact_id, channel, state)
		 VALUES ($1, $2, $3, 'whatsapp', 'open')`,
		convID, tenantID, contactID,
	); err != nil {
		t.Fatalf("seed conversation: %v", err)
	}
	return convID
}

// freshDBWithFunnelAdapter applies the full chain needed by the funnel
// adapter (tenants, users, inbox/contacts, identity, funnel itself).
func freshDBWithFunnelAdapter(t *testing.T) *testpg.DB {
	t.Helper()
	db, _ := freshDBWithFunnelF2(t)
	return db
}

func newFunnelStore(t *testing.T, db *testpg.DB) *pgfunnel.Store {
	t.Helper()
	s, err := pgfunnel.New(db.RuntimePool())
	if err != nil {
		t.Fatalf("pgfunnel.New: %v", err)
	}
	return s
}

func TestFunnelAdapter_New_RejectsNilPool(t *testing.T) {
	if _, err := pgfunnel.New(nil); err == nil {
		t.Error("New(nil) err = nil, want postgres.ErrNilPool")
	}
}

func TestFunnelAdapter_FindByKey_SeededDefaultsAreVisible(t *testing.T) {
	db := freshDBWithFunnelAdapter(t)
	store := newFunnelStore(t, db)
	tenant := seedFunnelTenant(t, db.AdminPool())

	wantKeys := []string{"novo", "qualificando", "proposta", "ganho", "perdido"}
	for _, key := range wantKeys {
		stage, err := store.FindByKey(context.Background(), tenant, key)
		if err != nil {
			t.Fatalf("FindByKey(%q): %v", key, err)
		}
		if stage.TenantID != tenant {
			t.Errorf("stage %q TenantID = %v, want %v", key, stage.TenantID, tenant)
		}
		if stage.Key != key {
			t.Errorf("stage Key = %q, want %q", stage.Key, key)
		}
		if !stage.IsDefault {
			t.Errorf("stage %q IsDefault = false, want true", key)
		}
		if stage.Position == 0 {
			t.Errorf("stage %q Position = 0, want non-zero", key)
		}
	}
}

func TestFunnelAdapter_FindByKey_UnknownKeyReturnsErrNotFound(t *testing.T) {
	db := freshDBWithFunnelAdapter(t)
	store := newFunnelStore(t, db)
	tenant := seedFunnelTenant(t, db.AdminPool())

	_, err := store.FindByKey(context.Background(), tenant, "imaginary-stage")
	if !errors.Is(err, funnel.ErrNotFound) {
		t.Errorf("FindByKey(imaginary) err = %v, want errors.Is(funnel.ErrNotFound)", err)
	}
}

func TestFunnelAdapter_FindByKey_OtherTenantHiddenByRLS(t *testing.T) {
	db := freshDBWithFunnelAdapter(t)
	store := newFunnelStore(t, db)
	tenantA := seedFunnelTenant(t, db.AdminPool())
	tenantB := seedFunnelTenant(t, db.AdminPool())

	// Both tenants have the seeded "novo" stage. Looking it up under
	// tenantA must hit tenantA's row, not tenantB's.
	stageA, err := store.FindByKey(context.Background(), tenantA, "novo")
	if err != nil {
		t.Fatalf("FindByKey tenantA: %v", err)
	}
	stageB, err := store.FindByKey(context.Background(), tenantB, "novo")
	if err != nil {
		t.Fatalf("FindByKey tenantB: %v", err)
	}
	if stageA.ID == stageB.ID {
		t.Error("RLS leaked: same stage id returned for two tenants")
	}
	if stageA.TenantID != tenantA || stageB.TenantID != tenantB {
		t.Errorf("RLS leaked: A.TenantID=%v B.TenantID=%v", stageA.TenantID, stageB.TenantID)
	}
}

func TestFunnelAdapter_FindByKey_RejectsZeroTenant(t *testing.T) {
	db := freshDBWithFunnelAdapter(t)
	store := newFunnelStore(t, db)
	if _, err := store.FindByKey(context.Background(), uuid.Nil, "novo"); err == nil {
		t.Error("FindByKey(uuid.Nil, novo) err = nil, want validation error")
	}
}

func TestFunnelAdapter_FindByKey_EmptyKeyIsNotFound(t *testing.T) {
	db := freshDBWithFunnelAdapter(t)
	store := newFunnelStore(t, db)
	tenant := seedFunnelTenant(t, db.AdminPool())
	if _, err := store.FindByKey(context.Background(), tenant, ""); !errors.Is(err, funnel.ErrNotFound) {
		t.Errorf("FindByKey(empty) err = %v, want errors.Is(funnel.ErrNotFound)", err)
	}
}

func TestFunnelAdapter_LatestForConversation_NotFoundOnEmpty(t *testing.T) {
	db := freshDBWithFunnelAdapter(t)
	store := newFunnelStore(t, db)
	tenant := seedFunnelTenant(t, db.AdminPool())
	conv := seedFunnelContactAndConversation(t, db.AdminPool(), tenant)

	_, err := store.LatestForConversation(context.Background(), tenant, conv)
	if !errors.Is(err, funnel.ErrNotFound) {
		t.Errorf("LatestForConversation err = %v, want errors.Is(funnel.ErrNotFound)", err)
	}
}

func TestFunnelAdapter_Create_AndLatestRoundTrip(t *testing.T) {
	db := freshDBWithFunnelAdapter(t)
	store := newFunnelStore(t, db)
	tenant := seedFunnelTenant(t, db.AdminPool())
	user := seedFunnelUser(t, db.AdminPool(), tenant)
	conv := seedFunnelContactAndConversation(t, db.AdminPool(), tenant)

	novo, err := store.FindByKey(context.Background(), tenant, "novo")
	if err != nil {
		t.Fatalf("FindByKey novo: %v", err)
	}
	pinned := time.Date(2026, 5, 16, 12, 0, 0, 0, time.UTC)
	first := &funnel.Transition{
		ID:                   uuid.New(),
		TenantID:             tenant,
		ConversationID:       conv,
		FromStageID:          nil,
		ToStageID:            novo.ID,
		TransitionedByUserID: user,
		TransitionedAt:       pinned,
		Reason:               "intake",
	}
	if err := store.Create(context.Background(), first); err != nil {
		t.Fatalf("Create first transition: %v", err)
	}
	got, err := store.LatestForConversation(context.Background(), tenant, conv)
	if err != nil {
		t.Fatalf("LatestForConversation: %v", err)
	}
	if got.ID != first.ID {
		t.Errorf("ID = %v, want %v", got.ID, first.ID)
	}
	if got.FromStageID != nil {
		t.Errorf("FromStageID = %v, want nil", *got.FromStageID)
	}
	if got.ToStageID != novo.ID {
		t.Errorf("ToStageID = %v, want %v", got.ToStageID, novo.ID)
	}
	if !got.TransitionedAt.Equal(pinned) {
		t.Errorf("TransitionedAt = %v, want %v", got.TransitionedAt, pinned)
	}
	if got.Reason != "intake" {
		t.Errorf("Reason = %q, want %q", got.Reason, "intake")
	}

	// Second transition with from_stage_id set must round-trip the
	// pointer correctly and become the new "latest".
	ganho, err := store.FindByKey(context.Background(), tenant, "ganho")
	if err != nil {
		t.Fatalf("FindByKey ganho: %v", err)
	}
	fromID := novo.ID
	second := &funnel.Transition{
		ID:                   uuid.New(),
		TenantID:             tenant,
		ConversationID:       conv,
		FromStageID:          &fromID,
		ToStageID:            ganho.ID,
		TransitionedByUserID: user,
		TransitionedAt:       pinned.Add(time.Hour),
		Reason:               "",
	}
	if err := store.Create(context.Background(), second); err != nil {
		t.Fatalf("Create second transition: %v", err)
	}
	latest, err := store.LatestForConversation(context.Background(), tenant, conv)
	if err != nil {
		t.Fatalf("LatestForConversation second: %v", err)
	}
	if latest.ID != second.ID {
		t.Errorf("latest.ID = %v, want %v (second)", latest.ID, second.ID)
	}
	if latest.FromStageID == nil || *latest.FromStageID != fromID {
		t.Errorf("latest.FromStageID = %v, want %v", latest.FromStageID, fromID)
	}
	if latest.Reason != "" {
		t.Errorf("latest.Reason = %q, want empty (stored as NULL)", latest.Reason)
	}
}

func TestFunnelAdapter_Create_ValidatesFields(t *testing.T) {
	db := freshDBWithFunnelAdapter(t)
	store := newFunnelStore(t, db)
	tenant := seedFunnelTenant(t, db.AdminPool())
	user := seedFunnelUser(t, db.AdminPool(), tenant)
	conv := seedFunnelContactAndConversation(t, db.AdminPool(), tenant)
	novo, err := store.FindByKey(context.Background(), tenant, "novo")
	if err != nil {
		t.Fatalf("FindByKey novo: %v", err)
	}
	pinned := time.Date(2026, 5, 16, 12, 0, 0, 0, time.UTC)

	base := funnel.Transition{
		ID:                   uuid.New(),
		TenantID:             tenant,
		ConversationID:       conv,
		ToStageID:            novo.ID,
		TransitionedByUserID: user,
		TransitionedAt:       pinned,
	}

	cases := []struct {
		name  string
		patch func(*funnel.Transition)
	}{
		{"nil transition", nil},
		{"zero tenant", func(t *funnel.Transition) { t.TenantID = uuid.Nil }},
		{"zero id", func(t *funnel.Transition) { t.ID = uuid.Nil }},
		{"zero conversation", func(t *funnel.Transition) { t.ConversationID = uuid.Nil }},
		{"zero to_stage", func(t *funnel.Transition) { t.ToStageID = uuid.Nil }},
		{"zero actor", func(t *funnel.Transition) { t.TransitionedByUserID = uuid.Nil }},
		{"zero timestamp", func(t *funnel.Transition) { t.TransitionedAt = time.Time{} }},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			var arg *funnel.Transition
			if c.patch != nil {
				cp := base
				cp.ID = uuid.New() // unique id per subtest in case any slips past validation
				c.patch(&cp)
				arg = &cp
			}
			if err := store.Create(context.Background(), arg); err == nil {
				t.Errorf("Create(%s) err = nil, want validation error", c.name)
			}
		})
	}
}
