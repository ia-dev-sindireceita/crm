package postgres_test

// SIN-66389 integration tests for the channels Postgres adapter +
// migration 0128_channel_instances (tenant_channels, channel_access,
// conversation.channel_id + backfill).
//
// These live in the parent postgres_test package (not the
// internal/adapter/db/postgres/channels subpackage) so they share the
// TestMain / harness with the other adapter tests and dodge the
// shared-cluster ALTER ROLE race that a second test binary would trigger
// (same rationale documented in contacts_adapter_test.go).

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"

	pgchannels "github.com/pericles-luz/crm/internal/adapter/db/postgres/channels"
	"github.com/pericles-luz/crm/internal/adapter/db/postgres/testpg"
	"github.com/pericles-luz/crm/internal/channels"
)

// channelsMigrationChain is the migration set the channel feature needs:
// tenants (0004), users (0005), inbox/contacts incl. conversation (0088),
// then this feature (0128).
var channelsMigrationChain = []string{
	"0004_create_tenant.up.sql",
	"0005_create_users.up.sql",
	"0088_inbox_contacts.up.sql",
	"0128_channel_instances.up.sql",
}

func applyMigration(t *testing.T, db *testpg.DB, ctx context.Context, name string) {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(harness.MigrationsDir(), name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	if _, err := db.AdminPool().Exec(ctx, string(body)); err != nil {
		t.Fatalf("apply %s: %v", name, err)
	}
}

// freshDBWithChannels applies the full chain (through 0128) on top of the
// harness default 0001-0003.
func freshDBWithChannels(t *testing.T) *testpg.DB {
	t.Helper()
	db := harness.DB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	for _, name := range channelsMigrationChain {
		applyMigration(t, db, ctx, name)
	}
	return db
}

func seedChannelsTenant(t *testing.T, db *testpg.DB) uuid.UUID {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	id := uuid.New()
	if _, err := db.AdminPool().Exec(ctx,
		`INSERT INTO tenants (id, name, host) VALUES ($1, $2, $3)`,
		id, fmt.Sprintf("t-%s", id), fmt.Sprintf("%s.crm.local", id)); err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	return id
}

func seedChannelsUser(t *testing.T, db *testpg.DB, tenantID uuid.UUID) uuid.UUID {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	id := uuid.New()
	if _, err := db.AdminPool().Exec(ctx,
		`INSERT INTO users (id, tenant_id, email, password_hash, role, is_master)
		 VALUES ($1, $2, $3, 'x', 'agent', false)`,
		id, tenantID, fmt.Sprintf("u-%s@x", id)); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	return id
}

func newChannelsStore(t *testing.T, db *testpg.DB) *pgchannels.Store {
	t.Helper()
	s, err := pgchannels.New(db.RuntimePool())
	if err != nil {
		t.Fatalf("pgchannels.New: %v", err)
	}
	return s
}

func TestChannelsAdapter_New_RejectsNilPool(t *testing.T) {
	if _, err := pgchannels.New(nil); err == nil {
		t.Error("New(nil) err = nil, want postgres.ErrNilPool")
	}
}

// TestChannelsAdapter_GuardRails exercises the cheap argument-validation
// branches that return before touching the database: nil tenant, nil id,
// nil channel, and nil user/channel access probes.
func TestChannelsAdapter_GuardRails(t *testing.T) {
	db := freshDBWithChannels(t)
	ctx := context.Background()
	tenant := seedChannelsTenant(t, db)
	store := newChannelsStore(t, db)

	// Create: nil channel, nil tenant, nil id all rejected before the DB.
	if err := store.Create(ctx, nil); err == nil {
		t.Error("Create(nil) err = nil, want error")
	}
	if err := store.Create(ctx, &channels.Channel{ID: uuid.New(), TenantID: uuid.Nil, ChannelKey: "x"}); err == nil {
		t.Error("Create(nil tenant) err = nil, want error")
	}
	if err := store.Create(ctx, &channels.Channel{ID: uuid.Nil, TenantID: tenant, ChannelKey: "x"}); err == nil {
		t.Error("Create(nil id) err = nil, want error")
	}

	// Nil-tenant guards on every read/write path.
	if _, err := store.List(ctx, uuid.Nil); err == nil {
		t.Error("List(nil tenant) err = nil, want error")
	}
	if _, err := store.Get(ctx, uuid.Nil, uuid.New()); err == nil {
		t.Error("Get(nil tenant) err = nil, want error")
	}
	if err := store.Rename(ctx, uuid.Nil, uuid.New(), "x"); err == nil {
		t.Error("Rename(nil tenant) err = nil, want error")
	}
	if err := store.SetActive(ctx, uuid.Nil, uuid.New(), true); err == nil {
		t.Error("SetActive(nil tenant) err = nil, want error")
	}
	if _, err := store.CanAccessChannel(ctx, uuid.Nil, uuid.New(), uuid.New()); err == nil {
		t.Error("CanAccessChannel(nil tenant) err = nil, want error")
	}
	if _, err := store.ListAccessibleChannelIDs(ctx, uuid.Nil, uuid.New()); err == nil {
		t.Error("ListAccessibleChannelIDs(nil tenant) err = nil, want error")
	}

	// Nil-id guards: Get/Rename/SetActive treat a nil id as ErrNotFound.
	if _, err := store.Get(ctx, tenant, uuid.Nil); !errors.Is(err, channels.ErrNotFound) {
		t.Errorf("Get(nil id) err = %v, want ErrNotFound", err)
	}
	if err := store.Rename(ctx, tenant, uuid.Nil, "x"); !errors.Is(err, channels.ErrNotFound) {
		t.Errorf("Rename(nil id) err = %v, want ErrNotFound", err)
	}
	if err := store.SetActive(ctx, tenant, uuid.Nil, true); !errors.Is(err, channels.ErrNotFound) {
		t.Errorf("SetActive(nil id) err = %v, want ErrNotFound", err)
	}

	// Access probes short-circuit to deny on nil user/channel.
	if ok, err := store.CanAccessChannel(ctx, tenant, uuid.Nil, uuid.New()); err != nil || ok {
		t.Errorf("CanAccessChannel(nil user) = (%v, %v), want (false, nil)", ok, err)
	}
	if ok, err := store.CanAccessChannel(ctx, tenant, uuid.New(), uuid.Nil); err != nil || ok {
		t.Errorf("CanAccessChannel(nil channel) = (%v, %v), want (false, nil)", ok, err)
	}
	if ids, err := store.ListAccessibleChannelIDs(ctx, tenant, uuid.Nil); err != nil || ids != nil {
		t.Errorf("ListAccessibleChannelIDs(nil user) = (%v, %v), want (nil, nil)", ids, err)
	}
}

// TestChannelsMigration_UpDownUp proves both directions of
// 0128_channel_instances are idempotent and round-trip safe, including
// the conversation.channel_id column.
func TestChannelsMigration_UpDownUp(t *testing.T) {
	db := freshDBWithChannels(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	tableNames := []string{"tenant_channels", "channel_access"}
	tablesPresent := func() int {
		var n int
		if err := db.SuperuserPool().QueryRow(ctx,
			`SELECT count(*) FROM pg_class c
			   JOIN pg_namespace ns ON ns.oid = c.relnamespace
			  WHERE c.relname = ANY($1) AND ns.nspname = 'public'`, tableNames).Scan(&n); err != nil {
			t.Fatalf("table probe: %v", err)
		}
		return n
	}
	columnPresent := func() bool {
		var present bool
		if err := db.SuperuserPool().QueryRow(ctx,
			`SELECT EXISTS (
			   SELECT 1 FROM information_schema.columns
			    WHERE table_name = 'conversation' AND column_name = 'channel_id')`).Scan(&present); err != nil {
			t.Fatalf("column probe: %v", err)
		}
		return present
	}

	if got := tablesPresent(); got != len(tableNames) {
		t.Fatalf("after up: %d/%d channel tables", got, len(tableNames))
	}
	if !columnPresent() {
		t.Fatal("after up: conversation.channel_id missing")
	}

	downBody, err := os.ReadFile(filepath.Join(harness.MigrationsDir(), "0128_channel_instances.down.sql"))
	if err != nil {
		t.Fatalf("read down: %v", err)
	}
	if _, err := db.AdminPool().Exec(ctx, string(downBody)); err != nil {
		t.Fatalf("apply down: %v", err)
	}
	if got := tablesPresent(); got != 0 {
		t.Fatalf("after down: %d channel tables still present", got)
	}
	if columnPresent() {
		t.Fatal("after down: conversation.channel_id still present")
	}

	// Re-up + double-down for idempotency.
	applyMigration(t, db, ctx, "0128_channel_instances.up.sql")
	if got := tablesPresent(); got != len(tableNames) {
		t.Fatalf("after re-up: %d/%d channel tables", got, len(tableNames))
	}
	if _, err := db.AdminPool().Exec(ctx, string(downBody)); err != nil {
		t.Fatalf("down (idempotent): %v", err)
	}
	if _, err := db.AdminPool().Exec(ctx, string(downBody)); err != nil {
		t.Fatalf("down again: %v", err)
	}
}

// TestChannelsAdapter_CRUD exercises Create → Get → List → Rename →
// SetActive under a single tenant.
func TestChannelsAdapter_CRUD(t *testing.T) {
	db := freshDBWithChannels(t)
	ctx := context.Background()
	tenant := seedChannelsTenant(t, db)
	store := newChannelsStore(t, db)

	ch, err := channels.New(tenant, "whatsapp", "+5511999990000", "Atendimento")
	if err != nil {
		t.Fatalf("domain New: %v", err)
	}
	if err := store.Create(ctx, ch); err != nil {
		t.Fatalf("Create: %v", err)
	}

	got, err := store.Get(ctx, tenant, ch.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.ChannelKey != "whatsapp" || got.ExternalID != "+5511999990000" || got.DisplayName != "Atendimento" {
		t.Fatalf("Get mismatch: %+v", got)
	}
	if !got.IsActive || got.Restricted {
		t.Fatalf("Get flags: active=%v restricted=%v", got.IsActive, got.Restricted)
	}

	list, err := store.List(ctx, tenant)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 1 || list[0].ID != ch.ID {
		t.Fatalf("List = %+v, want single channel %s", list, ch.ID)
	}

	if err := store.Rename(ctx, tenant, ch.ID, "Suporte"); err != nil {
		t.Fatalf("Rename: %v", err)
	}
	if err := store.Rename(ctx, tenant, ch.ID, "   "); !errors.Is(err, channels.ErrEmptyDisplayName) {
		t.Fatalf("Rename(blank) err = %v, want ErrEmptyDisplayName", err)
	}
	if err := store.SetActive(ctx, tenant, ch.ID, false); err != nil {
		t.Fatalf("SetActive: %v", err)
	}

	got2, err := store.Get(ctx, tenant, ch.ID)
	if err != nil {
		t.Fatalf("Get after mutate: %v", err)
	}
	if got2.DisplayName != "Suporte" {
		t.Errorf("DisplayName = %q, want Suporte", got2.DisplayName)
	}
	if got2.IsActive {
		t.Error("channel should be inactive after SetActive(false)")
	}

	// Unknown id → ErrNotFound on read + write paths.
	if _, err := store.Get(ctx, tenant, uuid.New()); !errors.Is(err, channels.ErrNotFound) {
		t.Errorf("Get(unknown) err = %v, want ErrNotFound", err)
	}
	if err := store.Rename(ctx, tenant, uuid.New(), "x"); !errors.Is(err, channels.ErrNotFound) {
		t.Errorf("Rename(unknown) err = %v, want ErrNotFound", err)
	}
	if err := store.SetActive(ctx, tenant, uuid.New(), true); !errors.Is(err, channels.ErrNotFound) {
		t.Errorf("SetActive(unknown) err = %v, want ErrNotFound", err)
	}
}

// TestChannelsAdapter_SetRestricted proves the P3 restricted toggle
// persists under the tenant scope, is reversible, does not touch the
// stored access grants, and maps guard/unknown cases correctly.
func TestChannelsAdapter_SetRestricted(t *testing.T) {
	db := freshDBWithChannels(t)
	ctx := context.Background()
	tenant := seedChannelsTenant(t, db)
	user := seedChannelsUser(t, db, tenant)
	store := newChannelsStore(t, db)

	ch, err := channels.New(tenant, "whatsapp", "+5511900001111", "Suporte")
	if err != nil {
		t.Fatalf("domain New: %v", err)
	}
	if err := store.Create(ctx, ch); err != nil {
		t.Fatalf("Create: %v", err)
	}
	// A channel starts open (restricted=false) and carries a grant.
	if err := store.ReplaceAccess(ctx, tenant, ch.ID, []uuid.UUID{user}); err != nil {
		t.Fatalf("ReplaceAccess: %v", err)
	}

	// Nil-tenant / nil-id guards.
	if err := store.SetRestricted(ctx, uuid.Nil, ch.ID, true); err == nil {
		t.Error("SetRestricted(nil tenant) err = nil, want error")
	}
	if err := store.SetRestricted(ctx, tenant, uuid.Nil, true); !errors.Is(err, channels.ErrNotFound) {
		t.Errorf("SetRestricted(nil id) err = %v, want ErrNotFound", err)
	}
	if err := store.SetRestricted(ctx, tenant, uuid.New(), true); !errors.Is(err, channels.ErrNotFound) {
		t.Errorf("SetRestricted(unknown) err = %v, want ErrNotFound", err)
	}

	// Enable restricted, then confirm the flag flipped and the grant is intact.
	if err := store.SetRestricted(ctx, tenant, ch.ID, true); err != nil {
		t.Fatalf("SetRestricted(true): %v", err)
	}
	got, err := store.Get(ctx, tenant, ch.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !got.Restricted {
		t.Error("channel should be restricted after SetRestricted(true)")
	}
	ids, err := store.ChannelUserIDs(ctx, tenant, ch.ID)
	if err != nil {
		t.Fatalf("ChannelUserIDs: %v", err)
	}
	if len(ids) != 1 || ids[0] != user {
		t.Errorf("grants after toggle = %v, want [%s] (toggle must not touch grants)", ids, user)
	}

	// Reverse it — open→restricted→open round-trips.
	if err := store.SetRestricted(ctx, tenant, ch.ID, false); err != nil {
		t.Fatalf("SetRestricted(false): %v", err)
	}
	got2, err := store.Get(ctx, tenant, ch.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got2.Restricted {
		t.Error("channel should be open after SetRestricted(false)")
	}
}

// TestChannelsAdapter_Create_Conflict proves the UNIQUE(tenant, key,
// external) constraint maps to channels.ErrChannelConflict.
func TestChannelsAdapter_Create_Conflict(t *testing.T) {
	db := freshDBWithChannels(t)
	ctx := context.Background()
	tenant := seedChannelsTenant(t, db)
	store := newChannelsStore(t, db)

	a, _ := channels.New(tenant, "whatsapp", "+55", "A")
	if err := store.Create(ctx, a); err != nil {
		t.Fatalf("Create a: %v", err)
	}
	b, _ := channels.New(tenant, "WhatsApp", "+55", "B") // same key after lower-case, same external
	if err := store.Create(ctx, b); !errors.Is(err, channels.ErrChannelConflict) {
		t.Fatalf("Create dup err = %v, want ErrChannelConflict", err)
	}
}

// TestChannelsRLS_TenantIsolation: a channel created under tenant A must
// be invisible to tenant B via both List and Get.
func TestChannelsRLS_TenantIsolation(t *testing.T) {
	db := freshDBWithChannels(t)
	ctx := context.Background()
	tenantA := seedChannelsTenant(t, db)
	tenantB := seedChannelsTenant(t, db)
	store := newChannelsStore(t, db)

	ch, _ := channels.New(tenantA, "whatsapp", "+55", "A-only")
	if err := store.Create(ctx, ch); err != nil {
		t.Fatalf("Create under A: %v", err)
	}

	listB, err := store.List(ctx, tenantB)
	if err != nil {
		t.Fatalf("List under B: %v", err)
	}
	if len(listB) != 0 {
		t.Fatalf("tenant B sees %d of tenant A's channels, want 0", len(listB))
	}
	if _, err := store.Get(ctx, tenantB, ch.ID); !errors.Is(err, channels.ErrNotFound) {
		t.Fatalf("Get A's channel under B err = %v, want ErrNotFound", err)
	}
}

// TestChannelsAccessPolicy exercises CanAccessChannel +
// ListAccessibleChannelIDs against real channel_access grants.
func TestChannelsAccessPolicy(t *testing.T) {
	db := freshDBWithChannels(t)
	ctx := context.Background()
	tenant := seedChannelsTenant(t, db)
	user := seedChannelsUser(t, db, tenant)
	other := seedChannelsUser(t, db, tenant)
	store := newChannelsStore(t, db)

	granted, _ := channels.New(tenant, "whatsapp", "+55", "Granted")
	ungranted, _ := channels.New(tenant, "instagram", "@acme", "Ungranted")
	if err := store.Create(ctx, granted); err != nil {
		t.Fatalf("Create granted: %v", err)
	}
	if err := store.Create(ctx, ungranted); err != nil {
		t.Fatalf("Create ungranted: %v", err)
	}

	// Grant `user` access to the `granted` channel only.
	if _, err := db.AdminPool().Exec(ctx,
		`INSERT INTO channel_access (tenant_id, channel_id, user_id) VALUES ($1, $2, $3)`,
		tenant, granted.ID, user); err != nil {
		t.Fatalf("seed grant: %v", err)
	}

	ok, err := store.CanAccessChannel(ctx, tenant, user, granted.ID)
	if err != nil {
		t.Fatalf("CanAccessChannel granted: %v", err)
	}
	if !ok {
		t.Error("user should be able to access granted channel")
	}
	ok, err = store.CanAccessChannel(ctx, tenant, user, ungranted.ID)
	if err != nil {
		t.Fatalf("CanAccessChannel ungranted: %v", err)
	}
	if ok {
		t.Error("user should NOT access ungranted channel")
	}
	ok, err = store.CanAccessChannel(ctx, tenant, other, granted.ID)
	if err != nil {
		t.Fatalf("CanAccessChannel other: %v", err)
	}
	if ok {
		t.Error("other user should NOT access channel they were not granted")
	}

	ids, err := store.ListAccessibleChannelIDs(ctx, tenant, user)
	if err != nil {
		t.Fatalf("ListAccessibleChannelIDs: %v", err)
	}
	if len(ids) != 1 || ids[0] != granted.ID {
		t.Fatalf("accessible ids = %v, want [%s]", ids, granted.ID)
	}
	emptyIDs, err := store.ListAccessibleChannelIDs(ctx, tenant, other)
	if err != nil {
		t.Fatalf("ListAccessibleChannelIDs other: %v", err)
	}
	if len(emptyIDs) != 0 {
		t.Fatalf("other accessible ids = %v, want empty", emptyIDs)
	}
}

// TestChannelsBackfill_GrantAll is the migration's most important
// property: applying 0128 on a DB that already has conversations must
// materialize a channel per distinct (tenant, channel) string, link each
// conversation to its channel, and grant EVERY current tenant user
// access to EVERY backfilled channel — zero inbox regression.
func TestChannelsBackfill_GrantAll(t *testing.T) {
	db := harness.DB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// Apply up to (and including) 0088 — but NOT 0128 yet — then seed
	// legacy data, then apply 0128 so its backfill runs over that data.
	for _, name := range []string{
		"0004_create_tenant.up.sql",
		"0005_create_users.up.sql",
		"0088_inbox_contacts.up.sql",
	} {
		applyMigration(t, db, ctx, name)
	}

	tenant := seedChannelsTenant(t, db)
	userA := seedChannelsUser(t, db, tenant)
	userB := seedChannelsUser(t, db, tenant)

	// A second tenant with its own users + conversation proves the
	// grant-all is tenant-scoped (no cross-tenant grants).
	tenant2 := seedChannelsTenant(t, db)
	user2 := seedChannelsUser(t, db, tenant2)

	contact := uuid.New()
	if _, err := db.AdminPool().Exec(ctx,
		`INSERT INTO contact (id, tenant_id, display_name) VALUES ($1, $2, 'C')`,
		contact, tenant); err != nil {
		t.Fatalf("seed contact: %v", err)
	}
	contact2 := uuid.New()
	if _, err := db.AdminPool().Exec(ctx,
		`INSERT INTO contact (id, tenant_id, display_name) VALUES ($1, $2, 'C2')`,
		contact2, tenant2); err != nil {
		t.Fatalf("seed contact2: %v", err)
	}

	// Tenant 1 has conversations on two distinct channel strings (one of
	// them twice, to prove DISTINCT collapses duplicates).
	seedConv := func(tid, cid uuid.UUID, channel string) {
		if _, err := db.AdminPool().Exec(ctx,
			`INSERT INTO conversation (id, tenant_id, contact_id, channel) VALUES ($1, $2, $3, $4)`,
			uuid.New(), tid, cid, channel); err != nil {
			t.Fatalf("seed conversation (%s): %v", channel, err)
		}
	}
	seedConv(tenant, contact, "whatsapp")
	seedConv(tenant, contact, "whatsapp")
	seedConv(tenant, contact, "instagram")
	seedConv(tenant2, contact2, "whatsapp")

	// Now run the migration containing the backfill.
	applyMigration(t, db, ctx, "0128_channel_instances.up.sql")

	// (1) tenant_channels: 2 for tenant1, 1 for tenant2.
	countChannels := func(tid uuid.UUID) int {
		var n int
		if err := db.AdminPool().QueryRow(ctx,
			`SELECT count(*) FROM tenant_channels WHERE tenant_id = $1`, tid).Scan(&n); err != nil {
			t.Fatalf("count channels: %v", err)
		}
		return n
	}
	if got := countChannels(tenant); got != 2 {
		t.Errorf("tenant1 backfilled channels = %d, want 2 (whatsapp+instagram, dedup)", got)
	}
	if got := countChannels(tenant2); got != 1 {
		t.Errorf("tenant2 backfilled channels = %d, want 1", got)
	}

	// Backfilled rows must use external_id = '' and channel_key as both
	// key and display_name, active + unrestricted.
	var key, ext, disp string
	var active, restricted bool
	if err := db.AdminPool().QueryRow(ctx,
		`SELECT channel_key, external_id, display_name, is_active, restricted
		   FROM tenant_channels WHERE tenant_id = $1 AND channel_key = 'whatsapp'`, tenant).
		Scan(&key, &ext, &disp, &active, &restricted); err != nil {
		t.Fatalf("read backfilled channel: %v", err)
	}
	if ext != "" || disp != "whatsapp" || !active || restricted {
		t.Errorf("backfilled channel shape: ext=%q disp=%q active=%v restricted=%v", ext, disp, active, restricted)
	}

	// (2) every conversation got a channel_id linking to the matching
	// channel_key.
	var unlinked int
	if err := db.AdminPool().QueryRow(ctx,
		`SELECT count(*) FROM conversation WHERE channel_id IS NULL`).Scan(&unlinked); err != nil {
		t.Fatalf("count unlinked: %v", err)
	}
	if unlinked != 0 {
		t.Errorf("conversations with NULL channel_id after backfill = %d, want 0", unlinked)
	}
	var mismatched int
	if err := db.AdminPool().QueryRow(ctx,
		`SELECT count(*) FROM conversation c
		   JOIN tenant_channels tc ON tc.id = c.channel_id
		  WHERE tc.channel_key <> c.channel OR tc.tenant_id <> c.tenant_id`).Scan(&mismatched); err != nil {
		t.Fatalf("count mismatched: %v", err)
	}
	if mismatched != 0 {
		t.Errorf("conversations linked to wrong channel = %d, want 0", mismatched)
	}

	// (3) grant-all: tenant1 has 2 users × 2 channels = 4 grants; tenant2
	// has 1 user × 1 channel = 1 grant. No cross-tenant grants.
	countGrants := func(tid uuid.UUID) int {
		var n int
		if err := db.AdminPool().QueryRow(ctx,
			`SELECT count(*) FROM channel_access WHERE tenant_id = $1`, tid).Scan(&n); err != nil {
			t.Fatalf("count grants: %v", err)
		}
		return n
	}
	if got := countGrants(tenant); got != 4 {
		t.Errorf("tenant1 grants = %d, want 4 (2 users × 2 channels)", got)
	}
	if got := countGrants(tenant2); got != 1 {
		t.Errorf("tenant2 grants = %d, want 1", got)
	}

	// Each tenant1 user must have a grant on BOTH channels.
	for _, u := range []uuid.UUID{userA, userB} {
		var n int
		if err := db.AdminPool().QueryRow(ctx,
			`SELECT count(*) FROM channel_access WHERE tenant_id = $1 AND user_id = $2`, tenant, u).Scan(&n); err != nil {
			t.Fatalf("count user grants: %v", err)
		}
		if n != 2 {
			t.Errorf("user %s grants = %d, want 2", u, n)
		}
	}
	// No grant leaked to the cross-tenant user.
	var leaked int
	if err := db.AdminPool().QueryRow(ctx,
		`SELECT count(*) FROM channel_access WHERE user_id = $1 AND tenant_id = $2`, user2, tenant).Scan(&leaked); err != nil {
		t.Fatalf("count leaked: %v", err)
	}
	if leaked != 0 {
		t.Errorf("cross-tenant grant leak = %d, want 0", leaked)
	}
}
