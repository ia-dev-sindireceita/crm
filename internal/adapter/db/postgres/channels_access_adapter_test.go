package postgres_test

// SIN-66391 (P2) integration tests for the channels access adapter
// methods added on top of the SIN-66389 store: ListRosterUsers,
// ChannelUserIDs, ReplaceAccess. They share the postgres_test harness
// and the SIN-66389 helpers (freshDBWithChannels / seedChannelsTenant /
// newChannelsStore) — same shared-cluster rationale as
// channels_adapter_test.go.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/channels"
)

// mkUser inserts a user with the given role + email under tenantID via
// the admin pool and returns its id.
func mkUser(t *testing.T, adminExec func(context.Context, string, ...any) error, tenantID uuid.UUID, role, email string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := adminExec(ctx,
		`INSERT INTO users (id, tenant_id, email, password_hash, role, is_master)
		 VALUES ($1, $2, $3, 'x', $4, false)`,
		id, tenantID, email, role); err != nil {
		t.Fatalf("seed user %s: %v", email, err)
	}
	return id
}

func TestChannelsAdapter_ListRosterUsers(t *testing.T) {
	db := freshDBWithChannels(t)
	store := newChannelsStore(t, db)
	tenant := seedChannelsTenant(t, db)

	adminExec := func(ctx context.Context, sql string, args ...any) error {
		_, err := db.AdminPool().Exec(ctx, sql, args...)
		return err
	}

	// A gerente and an atendente must show up; a plain 'agent'/common
	// role user must be filtered out (only channel-attending roles).
	ger := mkUser(t, adminExec, tenant, "tenant_gerente", "bravo@x")
	att := mkUser(t, adminExec, tenant, "tenant_atendente", "alpha@x")
	_ = mkUser(t, adminExec, tenant, "agent", "charlie@x") // excluded

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	got, err := store.ListRosterUsers(ctx, tenant)
	if err != nil {
		t.Fatalf("ListRosterUsers: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("roster len = %d, want 2 (%+v)", len(got), got)
	}
	// Ordered by e-mail ASC: alpha@x (atendente) then bravo@x (gerente).
	if got[0].ID != att || got[1].ID != ger {
		t.Fatalf("roster order = [%s %s], want [%s %s]", got[0].ID, got[1].ID, att, ger)
	}
	if got[0].DisplayName != "alpha" || got[1].DisplayName != "bravo" {
		t.Fatalf("display names = [%q %q], want [alpha bravo]", got[0].DisplayName, got[1].DisplayName)
	}
	if got[0].Role != "tenant_atendente" || got[1].Role != "tenant_gerente" {
		t.Fatalf("roles = [%q %q]", got[0].Role, got[1].Role)
	}

	// Nil tenant is a clean error, not a leak.
	if _, err := store.ListRosterUsers(ctx, uuid.Nil); err == nil {
		t.Fatalf("ListRosterUsers(nil tenant) = nil error, want error")
	}

	// A tenant with no attending users yields an empty roster.
	empty := seedChannelsTenant(t, db)
	got2, err := store.ListRosterUsers(ctx, empty)
	if err != nil {
		t.Fatalf("ListRosterUsers(empty tenant): %v", err)
	}
	if len(got2) != 0 {
		t.Fatalf("empty-tenant roster len = %d, want 0", len(got2))
	}
}

func TestChannelsAdapter_ReplaceAccess_AndChannelUserIDs(t *testing.T) {
	db := freshDBWithChannels(t)
	store := newChannelsStore(t, db)
	tenant := seedChannelsTenant(t, db)
	adminExec := func(ctx context.Context, sql string, args ...any) error {
		_, err := db.AdminPool().Exec(ctx, sql, args...)
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	ch, err := channels.New(tenant, "whatsapp", "+5511999990000", "Atendimento")
	if err != nil {
		t.Fatalf("channels.New: %v", err)
	}
	if err := store.Create(ctx, ch); err != nil {
		t.Fatalf("Create: %v", err)
	}
	u1 := mkUser(t, adminExec, tenant, "tenant_atendente", "u1@x")
	u2 := mkUser(t, adminExec, tenant, "tenant_atendente", "u2@x")

	// A fresh channel has no grants.
	if ids, err := store.ChannelUserIDs(ctx, tenant, ch.ID); err != nil || len(ids) != 0 {
		t.Fatalf("ChannelUserIDs(fresh) = %v, %v; want empty, nil", ids, err)
	}

	// Grant both (with a duplicate to exercise de-dup).
	if err := store.ReplaceAccess(ctx, tenant, ch.ID, []uuid.UUID{u1, u2, u1}); err != nil {
		t.Fatalf("ReplaceAccess grant: %v", err)
	}
	ids, err := store.ChannelUserIDs(ctx, tenant, ch.ID)
	if err != nil {
		t.Fatalf("ChannelUserIDs: %v", err)
	}
	if !sameSet(ids, []uuid.UUID{u1, u2}) {
		t.Fatalf("grants = %v, want {%s,%s}", ids, u1, u2)
	}

	// Replace with a strict subset: u1 dropped.
	if err := store.ReplaceAccess(ctx, tenant, ch.ID, []uuid.UUID{u2}); err != nil {
		t.Fatalf("ReplaceAccess subset: %v", err)
	}
	ids, _ = store.ChannelUserIDs(ctx, tenant, ch.ID)
	if !sameSet(ids, []uuid.UUID{u2}) {
		t.Fatalf("grants after subset = %v, want {%s}", ids, u2)
	}

	// Clear all.
	if err := store.ReplaceAccess(ctx, tenant, ch.ID, nil); err != nil {
		t.Fatalf("ReplaceAccess clear: %v", err)
	}
	ids, _ = store.ChannelUserIDs(ctx, tenant, ch.ID)
	if len(ids) != 0 {
		t.Fatalf("grants after clear = %v, want empty", ids)
	}

	// Unknown channel → ErrNotFound (never writes orphan grants).
	if err := store.ReplaceAccess(ctx, tenant, uuid.New(), []uuid.UUID{u1}); !errors.Is(err, channels.ErrNotFound) {
		t.Fatalf("ReplaceAccess(unknown channel) = %v, want ErrNotFound", err)
	}
	// Nil channel → ErrNotFound; nil tenant → error.
	if err := store.ReplaceAccess(ctx, tenant, uuid.Nil, nil); !errors.Is(err, channels.ErrNotFound) {
		t.Fatalf("ReplaceAccess(nil channel) = %v, want ErrNotFound", err)
	}
	if err := store.ReplaceAccess(ctx, uuid.Nil, ch.ID, nil); err == nil {
		t.Fatalf("ReplaceAccess(nil tenant) = nil, want error")
	}
	if _, err := store.ChannelUserIDs(ctx, uuid.Nil, ch.ID); err == nil {
		t.Fatalf("ChannelUserIDs(nil tenant) = nil, want error")
	}
}

// TestChannelsAdapter_ReplaceAccess_TenantIsolation proves ReplaceAccess
// cannot reach a channel owned by another tenant: the existence probe
// runs under the caller's RLS scope so a foreign channel collapses to
// ErrNotFound rather than a cross-tenant write.
func TestChannelsAdapter_ReplaceAccess_TenantIsolation(t *testing.T) {
	db := freshDBWithChannels(t)
	store := newChannelsStore(t, db)
	tenantA := seedChannelsTenant(t, db)
	tenantB := seedChannelsTenant(t, db)
	adminExec := func(ctx context.Context, sql string, args ...any) error {
		_, err := db.AdminPool().Exec(ctx, sql, args...)
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	chB, _ := channels.New(tenantB, "whatsapp", "+55", "B")
	if err := store.Create(ctx, chB); err != nil {
		t.Fatalf("Create B: %v", err)
	}
	uA := mkUser(t, adminExec, tenantA, "tenant_atendente", "a@x")

	// tenantA trying to grant on tenantB's channel sees ErrNotFound.
	if err := store.ReplaceAccess(ctx, tenantA, chB.ID, []uuid.UUID{uA}); !errors.Is(err, channels.ErrNotFound) {
		t.Fatalf("cross-tenant ReplaceAccess = %v, want ErrNotFound", err)
	}
	// And no grant leaked onto chB.
	ids, err := store.ChannelUserIDs(ctx, tenantB, chB.ID)
	if err != nil {
		t.Fatalf("ChannelUserIDs B: %v", err)
	}
	if len(ids) != 0 {
		t.Fatalf("cross-tenant write leaked %d grants onto B", len(ids))
	}
}

func sameSet(got, want []uuid.UUID) bool {
	if len(got) != len(want) {
		return false
	}
	seen := make(map[uuid.UUID]int, len(got))
	for _, id := range got {
		seen[id]++
	}
	for _, id := range want {
		if seen[id] == 0 {
			return false
		}
		seen[id]--
	}
	return true
}
