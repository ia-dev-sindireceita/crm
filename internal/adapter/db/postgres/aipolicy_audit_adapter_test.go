package postgres_test

// SIN-62353 (Fase 3 H1) integration tests for the ai_policy_audit
// adapter (insert + cursor-paginated list + purge).
//
// Lives in postgres_test for the same reason as the W2A adapter
// tests: shares the bootstrap state, avoids the SQLSTATE 28P01
// regression that bites when a fresh test binary races the
// ALTER ROLE step on the shared CI cluster (mastersession pattern,
// SIN-62726 / SIN-62750).

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"

	pgaipolicy "github.com/pericles-luz/crm/internal/adapter/db/postgres/aipolicy"
	"github.com/pericles-luz/crm/internal/adapter/db/postgres/testpg"
	"github.com/pericles-luz/crm/internal/aipolicy"
)

// freshDBWithAIPolicyAudit applies the W1A chain plus migration 0099.
// Re-uses freshDBWithAIW1A to avoid duplicating the FK chain.
func freshDBWithAIPolicyAudit(t *testing.T) (*testpg.DB, context.Context) {
	t.Helper()
	db, ctx := freshDBWithAIW1A(t)
	body, err := os.ReadFile(filepath.Join(harness.MigrationsDir(),
		"0099_ai_policy_audit.up.sql"))
	if err != nil {
		t.Fatalf("read 0099 up: %v", err)
	}
	if _, err := db.AdminPool().Exec(ctx, string(body)); err != nil {
		t.Fatalf("apply 0099: %v", err)
	}
	return db, ctx
}

// newAIPolicyAuditStore wires the runtime pool through the
// AuditStore constructor. Errors fail-fast so the test does not
// silently exercise a nil-pool path.
func newAIPolicyAuditStore(t *testing.T, db *testpg.DB) *pgaipolicy.AuditStore {
	t.Helper()
	s, err := pgaipolicy.NewAuditStore(db.RuntimePool(), db.MasterOpsPool())
	if err != nil {
		t.Fatalf("NewAuditStore: %v", err)
	}
	return s
}

// seedAIPolicyUser inserts a tenant-bound non-master user the audit
// rows can reference via actor_user_id. The users.tenant_id FK is
// nullable for masters; for the regular operator path we tie the
// user to the tenant we just seeded.
func seedAIPolicyUser(t *testing.T, ctx context.Context, db *testpg.DB, tenantID uuid.UUID) uuid.UUID {
	t.Helper()
	userID := uuid.New()
	if _, err := db.AdminPool().Exec(ctx,
		`INSERT INTO users (id, tenant_id, email, password_hash, role, is_master)
		 VALUES ($1, $2, $3, 'x', 'tenant_gerente', false)`,
		userID, tenantID, fmt.Sprintf("op-%s@x", userID)); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	return userID
}

func seedAIPolicyMaster(t *testing.T, ctx context.Context, db *testpg.DB) uuid.UUID {
	t.Helper()
	masterID := uuid.New()
	if _, err := db.AdminPool().Exec(ctx,
		`INSERT INTO users (id, tenant_id, email, password_hash, role, is_master)
		 VALUES ($1, NULL, $2, 'x', 'master', true)`,
		masterID, fmt.Sprintf("master-%s@x", masterID)); err != nil {
		t.Fatalf("seed master: %v", err)
	}
	return masterID
}

// TestAIPolicyAuditAdapter_RecordPersistsRow exercises the insert
// surface: the row lands with the right scope, field, and JSONB
// payloads.
func TestAIPolicyAuditAdapter_RecordPersistsRow(t *testing.T) {
	t.Parallel()
	db, ctx := freshDBWithAIPolicyAudit(t)
	store := newAIPolicyAuditStore(t, db)
	tenant := seedAIPolicyTenant(t, db.AdminPool())
	user := seedAIPolicyUser(t, ctx, db, tenant)

	occurred := time.Now().UTC().Truncate(time.Microsecond)
	err := store.Record(ctx, aipolicy.AuditEvent{
		TenantID:   tenant,
		ScopeType:  aipolicy.ScopeTenant,
		ScopeID:    tenant.String(),
		Field:      "ai_enabled",
		OldValue:   true,
		NewValue:   false,
		Actor:      aipolicy.Actor{UserID: user, Master: false},
		OccurredAt: occurred,
	})
	if err != nil {
		t.Fatalf("Record: %v", err)
	}

	var (
		scope   string
		field   string
		oldRaw  []byte
		newRaw  []byte
		actorID uuid.UUID
		master  bool
	)
	if err := db.AdminPool().QueryRow(ctx,
		`SELECT scope_kind, field, old_value::text::bytea, new_value::text::bytea,
		        actor_user_id, actor_master
		   FROM ai_policy_audit
		  WHERE tenant_id = $1`, tenant).Scan(&scope, &field, &oldRaw, &newRaw, &actorID, &master); err != nil {
		t.Fatalf("select: %v", err)
	}
	if scope != "tenant" || field != "ai_enabled" {
		t.Fatalf("scope/field = %q/%q, want tenant/ai_enabled", scope, field)
	}
	if string(oldRaw) != "true" || string(newRaw) != "false" {
		t.Fatalf("old/new payload = %s/%s, want true/false", oldRaw, newRaw)
	}
	if actorID != user {
		t.Fatalf("actor_user_id = %v, want %v", actorID, user)
	}
	if master {
		t.Fatalf("actor_master = true on tenant actor row")
	}
}

// TestAIPolicyAuditAdapter_RecordRejectsBadInputs covers each typed
// reject branch so the decorator can rely on the sentinel returns.
func TestAIPolicyAuditAdapter_RecordRejectsBadInputs(t *testing.T) {
	t.Parallel()
	db, ctx := freshDBWithAIPolicyAudit(t)
	store := newAIPolicyAuditStore(t, db)
	tenant := seedAIPolicyTenant(t, db.AdminPool())
	user := seedAIPolicyUser(t, ctx, db, tenant)
	valid := aipolicy.Actor{UserID: user}

	cases := []struct {
		name string
		ev   aipolicy.AuditEvent
	}{
		{"zero tenant", aipolicy.AuditEvent{ScopeType: aipolicy.ScopeTenant, ScopeID: "x", Field: "f", Actor: valid}},
		{"bad scope", aipolicy.AuditEvent{TenantID: tenant, ScopeType: "bogus", ScopeID: "x", Field: "f", Actor: valid}},
		{"blank scope id", aipolicy.AuditEvent{TenantID: tenant, ScopeType: aipolicy.ScopeTenant, ScopeID: "   ", Field: "f", Actor: valid}},
		{"blank field", aipolicy.AuditEvent{TenantID: tenant, ScopeType: aipolicy.ScopeTenant, ScopeID: "x", Field: "", Actor: valid}},
		{"missing actor", aipolicy.AuditEvent{TenantID: tenant, ScopeType: aipolicy.ScopeTenant, ScopeID: "x", Field: "f"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if err := store.Record(context.Background(), c.ev); err == nil {
				t.Errorf("err = nil, want a typed reject")
			}
		})
	}
}

// TestAIPolicyAuditAdapter_AC2_MasterActorBitPersists is the AC #2
// integration test: a record stamped with Master = true round-trips
// the bit through ai_policy_audit.actor_master.
func TestAIPolicyAuditAdapter_AC2_MasterActorBitPersists(t *testing.T) {
	t.Parallel()
	db, ctx := freshDBWithAIPolicyAudit(t)
	store := newAIPolicyAuditStore(t, db)
	tenant := seedAIPolicyTenant(t, db.AdminPool())
	master := seedAIPolicyMaster(t, ctx, db)

	if err := store.Record(ctx, aipolicy.AuditEvent{
		TenantID:  tenant,
		ScopeType: aipolicy.ScopeTenant,
		ScopeID:   tenant.String(),
		Field:     "ai_enabled",
		OldValue:  true,
		NewValue:  false,
		Actor:     aipolicy.Actor{UserID: master, Master: true},
	}); err != nil {
		t.Fatalf("Record: %v", err)
	}

	var (
		gotMaster bool
		gotActor  uuid.UUID
	)
	if err := db.AdminPool().QueryRow(ctx,
		`SELECT actor_master, actor_user_id
		   FROM ai_policy_audit
		  WHERE tenant_id = $1`, tenant).Scan(&gotMaster, &gotActor); err != nil {
		t.Fatalf("select: %v", err)
	}
	if !gotMaster {
		t.Fatalf("actor_master = false, want true")
	}
	if gotActor != master {
		t.Fatalf("actor_user_id = %v, want master %v", gotActor, master)
	}
}

// TestAIPolicyAuditAdapter_PageReturnsKeysetPagination probes the
// (created_at DESC, id DESC) keyset cursor: inserting 5 rows and
// paging by 2 must return exactly 2+2+1 and the cursor must not
// repeat a row.
func TestAIPolicyAuditAdapter_PageReturnsKeysetPagination(t *testing.T) {
	t.Parallel()
	db, ctx := freshDBWithAIPolicyAudit(t)
	store := newAIPolicyAuditStore(t, db)
	tenant := seedAIPolicyTenant(t, db.AdminPool())
	user := seedAIPolicyUser(t, ctx, db, tenant)

	// Insert 5 events spaced 1 ms apart so the timestamp keyset is
	// unambiguous.
	for i := 0; i < 5; i++ {
		if err := store.Record(ctx, aipolicy.AuditEvent{
			TenantID:   tenant,
			ScopeType:  aipolicy.ScopeTenant,
			ScopeID:    tenant.String(),
			Field:      fmt.Sprintf("field_%d", i),
			OldValue:   i,
			NewValue:   i + 1,
			Actor:      aipolicy.Actor{UserID: user},
			OccurredAt: time.Now().UTC().Add(time.Duration(i) * time.Millisecond),
		}); err != nil {
			t.Fatalf("Record %d: %v", i, err)
		}
	}

	seen := map[uuid.UUID]bool{}
	cursor := aipolicy.AuditCursor{}
	pages := 0
	for {
		page, err := store.Page(ctx, aipolicy.AuditPageQuery{
			TenantID: tenant,
			Cursor:   cursor,
			Limit:    2,
		})
		if err != nil {
			t.Fatalf("Page: %v", err)
		}
		for _, ev := range page.Events {
			if seen[ev.ID] {
				t.Fatalf("cursor repeated row id %v", ev.ID)
			}
			seen[ev.ID] = true
		}
		pages++
		if page.Next.IsZero() {
			break
		}
		cursor = page.Next
		if pages > 10 {
			t.Fatalf("pagination did not terminate")
		}
	}
	if len(seen) != 5 {
		t.Fatalf("paginated rows = %d, want 5", len(seen))
	}
}

// TestAIPolicyAuditAdapter_PageFiltersByScope verifies the (scope_kind,
// scope_id) filter narrows the result. Records two scopes, requests
// only the 'channel:whatsapp' slice, expects only those rows back.
func TestAIPolicyAuditAdapter_PageFiltersByScope(t *testing.T) {
	t.Parallel()
	db, ctx := freshDBWithAIPolicyAudit(t)
	store := newAIPolicyAuditStore(t, db)
	tenant := seedAIPolicyTenant(t, db.AdminPool())
	user := seedAIPolicyUser(t, ctx, db, tenant)

	for _, scope := range []struct {
		t aipolicy.ScopeType
		i string
	}{
		{aipolicy.ScopeTenant, tenant.String()},
		{aipolicy.ScopeChannel, "whatsapp"},
		{aipolicy.ScopeChannel, "whatsapp"},
	} {
		if err := store.Record(ctx, aipolicy.AuditEvent{
			TenantID: tenant, ScopeType: scope.t, ScopeID: scope.i,
			Field: "ai_enabled", OldValue: false, NewValue: true,
			Actor: aipolicy.Actor{UserID: user},
		}); err != nil {
			t.Fatalf("Record: %v", err)
		}
	}
	page, err := store.Page(ctx, aipolicy.AuditPageQuery{
		TenantID:  tenant,
		ScopeType: aipolicy.ScopeChannel,
		ScopeID:   "whatsapp",
		Limit:     50,
	})
	if err != nil {
		t.Fatalf("Page: %v", err)
	}
	if len(page.Events) != 2 {
		t.Fatalf("filtered rows = %d, want 2 channel:whatsapp", len(page.Events))
	}
	for _, ev := range page.Events {
		if ev.ScopeType != aipolicy.ScopeChannel || ev.ScopeID != "whatsapp" {
			t.Fatalf("leak: %+v", ev)
		}
	}
}

// TestAIPolicyAuditAdapter_PageFiltersByPeriod verifies the (Since,
// Until) window predicate.
func TestAIPolicyAuditAdapter_PageFiltersByPeriod(t *testing.T) {
	t.Parallel()
	db, ctx := freshDBWithAIPolicyAudit(t)
	store := newAIPolicyAuditStore(t, db)
	tenant := seedAIPolicyTenant(t, db.AdminPool())
	user := seedAIPolicyUser(t, ctx, db, tenant)

	now := time.Now().UTC()
	stamps := []time.Time{
		now.Add(-24 * time.Hour),
		now.Add(-12 * time.Hour),
		now,
	}
	for _, ts := range stamps {
		if err := store.Record(ctx, aipolicy.AuditEvent{
			TenantID: tenant, ScopeType: aipolicy.ScopeTenant, ScopeID: tenant.String(),
			Field: "ai_enabled", OldValue: true, NewValue: false,
			Actor: aipolicy.Actor{UserID: user}, OccurredAt: ts,
		}); err != nil {
			t.Fatalf("Record: %v", err)
		}
	}
	page, err := store.Page(ctx, aipolicy.AuditPageQuery{
		TenantID: tenant,
		Since:    now.Add(-18 * time.Hour),
		Until:    now.Add(-1 * time.Hour),
		Limit:    50,
	})
	if err != nil {
		t.Fatalf("Page: %v", err)
	}
	if len(page.Events) != 1 {
		t.Fatalf("window rows = %d, want 1", len(page.Events))
	}
}

// TestAIPolicyAuditAdapter_PageRLSIsolatesByTenant proves a Page call
// scoped to tenant A cannot see tenant B's audit rows.
func TestAIPolicyAuditAdapter_PageRLSIsolatesByTenant(t *testing.T) {
	t.Parallel()
	db, ctx := freshDBWithAIPolicyAudit(t)
	store := newAIPolicyAuditStore(t, db)
	tenantA := seedAIPolicyTenant(t, db.AdminPool())
	tenantB := seedAIPolicyTenant(t, db.AdminPool())
	userA := seedAIPolicyUser(t, ctx, db, tenantA)
	userB := seedAIPolicyUser(t, ctx, db, tenantB)

	for _, c := range []struct {
		tid  uuid.UUID
		user uuid.UUID
	}{{tenantA, userA}, {tenantB, userB}} {
		if err := store.Record(ctx, aipolicy.AuditEvent{
			TenantID: c.tid, ScopeType: aipolicy.ScopeTenant, ScopeID: c.tid.String(),
			Field: "ai_enabled", OldValue: false, NewValue: true,
			Actor: aipolicy.Actor{UserID: c.user},
		}); err != nil {
			t.Fatalf("Record: %v", err)
		}
	}
	for _, tid := range []uuid.UUID{tenantA, tenantB} {
		page, err := store.Page(ctx, aipolicy.AuditPageQuery{TenantID: tid, Limit: 50})
		if err != nil {
			t.Fatalf("Page(%v): %v", tid, err)
		}
		if len(page.Events) != 1 {
			t.Fatalf("Page(%v) = %d rows, want 1 (RLS leak?)", tid, len(page.Events))
		}
		if page.Events[0].TenantID != tid {
			t.Fatalf("RLS leak: tenant %v saw row from %v", tid, page.Events[0].TenantID)
		}
	}
}

// TestAIPolicyAuditAdapter_PurgeRemovesOldRows covers the LGPD-job
// surface: rows older than the threshold are deleted, recent rows
// stay. The purge runs under WithMasterOps; the master ledger
// captures the cross-tenant DELETE so the audit trail of the sweep
// is itself audited (defence in depth).
func TestAIPolicyAuditAdapter_PurgeRemovesOldRows(t *testing.T) {
	t.Parallel()
	db, ctx := freshDBWithAIPolicyAudit(t)
	store := newAIPolicyAuditStore(t, db)
	tenant := seedAIPolicyTenant(t, db.AdminPool())
	user := seedAIPolicyUser(t, ctx, db, tenant)
	master := seedAIPolicyMaster(t, ctx, db)

	now := time.Now().UTC()
	for _, ts := range []time.Time{
		now.Add(-400 * 24 * time.Hour), // ~13 months — stale
		now.Add(-365 * 24 * time.Hour), // ~12 months — stale
		now.Add(-30 * 24 * time.Hour),  // 30 days — fresh
	} {
		if err := store.Record(ctx, aipolicy.AuditEvent{
			TenantID: tenant, ScopeType: aipolicy.ScopeTenant, ScopeID: tenant.String(),
			Field: "ai_enabled", OldValue: false, NewValue: true,
			Actor: aipolicy.Actor{UserID: user}, OccurredAt: ts,
		}); err != nil {
			t.Fatalf("Record: %v", err)
		}
	}

	deleted, err := store.Purge(ctx, master, now.Add(-180*24*time.Hour))
	if err != nil {
		t.Fatalf("Purge: %v", err)
	}
	if deleted != 2 {
		t.Fatalf("deleted = %d, want 2", deleted)
	}

	page, err := store.Page(ctx, aipolicy.AuditPageQuery{TenantID: tenant, Limit: 50})
	if err != nil {
		t.Fatalf("Page after purge: %v", err)
	}
	if len(page.Events) != 1 {
		t.Fatalf("post-purge rows = %d, want 1 (fresh row only)", len(page.Events))
	}
}
