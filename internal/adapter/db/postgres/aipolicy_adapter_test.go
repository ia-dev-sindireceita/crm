package postgres_test

// SIN-62351 (Fase 3 W2A) integration tests for the aipolicy Postgres
// adapter.
//
// Lives in the parent postgres_test package (not the
// internal/adapter/db/postgres/aipolicy subpackage) to share the
// TestMain + harness with the other postgres_test files — tests that
// need testpg in a separate binary race the ALTER ROLE bootstrap on
// the shared CI cluster (SQLSTATE 28P01), per ADR 0087 and the W1A
// migration test (ai_policy_summary_product_migration_test.go).

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	pgaipolicy "github.com/pericles-luz/crm/internal/adapter/db/postgres/aipolicy"
	"github.com/pericles-luz/crm/internal/adapter/db/postgres/testpg"
	"github.com/pericles-luz/crm/internal/aipolicy"
)

// seedAIPolicyTenant inserts a tenant via the admin pool and returns
// its id. ai_policy carries an FK to tenants(id) so we cannot stand
// up a row without a real tenant.
func seedAIPolicyTenant(t *testing.T, pool *pgxpool.Pool) uuid.UUID {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	id := uuid.New()
	if _, err := pool.Exec(ctx,
		`INSERT INTO tenants (id, name, host) VALUES ($1, $2, $3)`,
		id, "aipolicy-"+id.String(), id.String()+".aipolicy.test",
	); err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	return id
}

// freshDBWithAIPolicyAdapter applies the migration chain needed by
// ai_policy. Reuses the same prerequisites as the W1A migration
// acceptance test so any drift between the two is caught the next
// time the chain changes.
func freshDBWithAIPolicyAdapter(t *testing.T) *testpg.DB {
	t.Helper()
	db, _ := freshDBWithAIW1A(t)
	return db
}

func newAIPolicyStore(t *testing.T, db *testpg.DB) *pgaipolicy.Store {
	t.Helper()
	s, err := pgaipolicy.New(db.RuntimePool())
	if err != nil {
		t.Fatalf("pgaipolicy.New: %v", err)
	}
	return s
}

func TestAIPolicyAdapter_New_RejectsNilPool(t *testing.T) {
	t.Parallel()
	if _, err := pgaipolicy.New(nil); err == nil {
		t.Error("New(nil) err = nil, want postgres.ErrNilPool")
	}
}

func TestAIPolicyAdapter_Get_MissingRowReturnsFalse(t *testing.T) {
	t.Parallel()
	db := freshDBWithAIPolicyAdapter(t)
	store := newAIPolicyStore(t, db)
	tenant := seedAIPolicyTenant(t, db.AdminPool())

	_, ok, err := store.Get(context.Background(), tenant, aipolicy.ScopeTenant, tenant.String())
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if ok {
		t.Error("Get returned ok=true for fresh tenant; want false")
	}
}

func TestAIPolicyAdapter_UpsertAndGetRoundTrip(t *testing.T) {
	t.Parallel()
	db := freshDBWithAIPolicyAdapter(t)
	store := newAIPolicyStore(t, db)
	tenant := seedAIPolicyTenant(t, db.AdminPool())

	want := aipolicy.Policy{
		TenantID:      tenant,
		ScopeType:     aipolicy.ScopeTenant,
		ScopeID:       tenant.String(),
		Model:         "openrouter/anthropic/claude-3.5-sonnet",
		PromptVersion: "v2",
		Tone:          "formal",
		Language:      "pt-BR",
		AIEnabled:     true,
		Anonymize:     true,
		OptIn:         true,
	}
	if err := store.Upsert(context.Background(), want); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	got, ok, err := store.Get(context.Background(), tenant, aipolicy.ScopeTenant, tenant.String())
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !ok {
		t.Fatal("Get ok=false after Upsert; want true")
	}
	if got.TenantID != tenant {
		t.Errorf("TenantID = %v, want %v", got.TenantID, tenant)
	}
	if got.ScopeType != aipolicy.ScopeTenant {
		t.Errorf("ScopeType = %q, want %q", got.ScopeType, aipolicy.ScopeTenant)
	}
	if got.ScopeID != want.ScopeID {
		t.Errorf("ScopeID = %q, want %q", got.ScopeID, want.ScopeID)
	}
	if got.Model != want.Model {
		t.Errorf("Model = %q, want %q", got.Model, want.Model)
	}
	if got.PromptVersion != want.PromptVersion {
		t.Errorf("PromptVersion = %q, want %q", got.PromptVersion, want.PromptVersion)
	}
	if got.Tone != want.Tone {
		t.Errorf("Tone = %q, want %q", got.Tone, want.Tone)
	}
	if got.Language != want.Language {
		t.Errorf("Language = %q, want %q", got.Language, want.Language)
	}
	if !got.AIEnabled {
		t.Errorf("AIEnabled = false, want true")
	}
	if !got.Anonymize {
		t.Errorf("Anonymize = false, want true")
	}
	if !got.OptIn {
		t.Errorf("OptIn = false, want true")
	}
	if got.CreatedAt.IsZero() {
		t.Error("CreatedAt is zero; DB DEFAULT should have populated it")
	}
	if got.UpdatedAt.IsZero() {
		t.Error("UpdatedAt is zero; DB DEFAULT should have populated it")
	}
}

func TestAIPolicyAdapter_Upsert_SecondCallUpdatesRow(t *testing.T) {
	t.Parallel()
	db := freshDBWithAIPolicyAdapter(t)
	store := newAIPolicyStore(t, db)
	tenant := seedAIPolicyTenant(t, db.AdminPool())

	first := aipolicy.Policy{
		TenantID:      tenant,
		ScopeType:     aipolicy.ScopeChannel,
		ScopeID:       "whatsapp",
		Model:         "openrouter/auto",
		PromptVersion: "v1",
		Tone:          "neutro",
		Language:      "pt-BR",
		AIEnabled:     false,
		Anonymize:     true,
		OptIn:         false,
	}
	if err := store.Upsert(context.Background(), first); err != nil {
		t.Fatalf("Upsert first: %v", err)
	}
	got, _, err := store.Get(context.Background(), tenant, aipolicy.ScopeChannel, "whatsapp")
	if err != nil {
		t.Fatalf("Get after first: %v", err)
	}
	createdAt := got.CreatedAt
	firstUpdatedAt := got.UpdatedAt
	// Sleep just enough that now() advances past the previous
	// timestamp resolution; the ON CONFLICT branch sets updated_at =
	// now() and we want to confirm it advanced.
	time.Sleep(10 * time.Millisecond)

	second := first
	second.Model = "openrouter/anthropic/claude-3.5-sonnet"
	second.AIEnabled = true
	if err := store.Upsert(context.Background(), second); err != nil {
		t.Fatalf("Upsert second: %v", err)
	}
	got, ok, err := store.Get(context.Background(), tenant, aipolicy.ScopeChannel, "whatsapp")
	if err != nil {
		t.Fatalf("Get after second: %v", err)
	}
	if !ok {
		t.Fatal("Get ok=false after second Upsert; want true")
	}
	if got.Model != "openrouter/anthropic/claude-3.5-sonnet" {
		t.Errorf("Model not updated: got %q", got.Model)
	}
	if !got.AIEnabled {
		t.Errorf("AIEnabled not updated; want true")
	}
	if !got.CreatedAt.Equal(createdAt) {
		t.Errorf("CreatedAt drifted: got %v, want %v (upsert must preserve original insert time)", got.CreatedAt, createdAt)
	}
	if !got.UpdatedAt.After(firstUpdatedAt) {
		t.Errorf("UpdatedAt did not advance: first=%v, second=%v", firstUpdatedAt, got.UpdatedAt)
	}
}

func TestAIPolicyAdapter_Get_OtherTenantHiddenByRLS(t *testing.T) {
	t.Parallel()
	db := freshDBWithAIPolicyAdapter(t)
	store := newAIPolicyStore(t, db)
	tenantA := seedAIPolicyTenant(t, db.AdminPool())
	tenantB := seedAIPolicyTenant(t, db.AdminPool())

	// Both tenants insert a tenant-scoped policy with the same
	// scope_id shape (their own tenant uuid as text). The (tenant_id,
	// scope_type, scope_id) UNIQUE index permits this because
	// tenant_id differs.
	for _, tid := range []uuid.UUID{tenantA, tenantB} {
		err := store.Upsert(context.Background(), aipolicy.Policy{
			TenantID:      tid,
			ScopeType:     aipolicy.ScopeTenant,
			ScopeID:       tid.String(),
			Model:         "openrouter/" + tid.String(),
			PromptVersion: "v1",
			Tone:          "neutro",
			Language:      "pt-BR",
			AIEnabled:     true,
			Anonymize:     true,
			OptIn:         true,
		})
		if err != nil {
			t.Fatalf("Upsert tenant %v: %v", tid, err)
		}
	}

	gotA, ok, err := store.Get(context.Background(), tenantA, aipolicy.ScopeTenant, tenantA.String())
	if err != nil {
		t.Fatalf("Get A: %v", err)
	}
	if !ok {
		t.Fatal("Get A ok=false; want true")
	}
	gotB, ok, err := store.Get(context.Background(), tenantB, aipolicy.ScopeTenant, tenantB.String())
	if err != nil {
		t.Fatalf("Get B: %v", err)
	}
	if !ok {
		t.Fatal("Get B ok=false; want true")
	}
	if gotA.TenantID != tenantA {
		t.Errorf("Get(A).TenantID = %v, want %v", gotA.TenantID, tenantA)
	}
	if gotB.TenantID != tenantB {
		t.Errorf("Get(B).TenantID = %v, want %v", gotB.TenantID, tenantB)
	}
	if gotA.Model == gotB.Model {
		t.Errorf("RLS leak: A model = B model = %q", gotA.Model)
	}

	// The load-bearing RLS assertion: looking up tenantB's scope_id
	// under tenantA's session must NOT return tenantB's row. RLS
	// strips the row before the WHERE clause sees it, so the
	// (tenantA + scope_id=tenantB.String()) probe returns false.
	_, leaked, err := store.Get(context.Background(), tenantA, aipolicy.ScopeTenant, tenantB.String())
	if err != nil {
		t.Fatalf("Get cross-tenant probe: %v", err)
	}
	if leaked {
		t.Error("RLS leak: tenant A read tenant B's policy by literal scope_id")
	}
}

func TestAIPolicyAdapter_Get_RejectsInvalidArgs(t *testing.T) {
	t.Parallel()
	db := freshDBWithAIPolicyAdapter(t)
	store := newAIPolicyStore(t, db)
	tenant := seedAIPolicyTenant(t, db.AdminPool())

	cases := []struct {
		name      string
		tenantID  uuid.UUID
		scopeType aipolicy.ScopeType
		scopeID   string
	}{
		{"nil tenant", uuid.Nil, aipolicy.ScopeTenant, tenant.String()},
		{"bad scope type", tenant, aipolicy.ScopeType("global"), tenant.String()},
		{"blank scope id", tenant, aipolicy.ScopeTenant, "   "},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, _, err := store.Get(context.Background(), tc.tenantID, tc.scopeType, tc.scopeID); err == nil {
				t.Errorf("Get(%s) err = nil, want validation error", tc.name)
			}
		})
	}
}

func TestAIPolicyAdapter_Upsert_RejectsInvalidArgs(t *testing.T) {
	t.Parallel()
	db := freshDBWithAIPolicyAdapter(t)
	store := newAIPolicyStore(t, db)
	tenant := seedAIPolicyTenant(t, db.AdminPool())

	base := aipolicy.Policy{
		TenantID:      tenant,
		ScopeType:     aipolicy.ScopeTenant,
		ScopeID:       tenant.String(),
		Model:         "openrouter/auto",
		PromptVersion: "v1",
		Tone:          "neutro",
		Language:      "pt-BR",
		AIEnabled:     false,
		Anonymize:     true,
		OptIn:         false,
	}

	cases := []struct {
		name  string
		patch func(*aipolicy.Policy)
	}{
		{"nil tenant", func(p *aipolicy.Policy) { p.TenantID = uuid.Nil }},
		{"bad scope type", func(p *aipolicy.Policy) { p.ScopeType = aipolicy.ScopeType("global") }},
		{"blank scope id", func(p *aipolicy.Policy) { p.ScopeID = "" }},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			p := base
			tc.patch(&p)
			if err := store.Upsert(context.Background(), p); err == nil {
				t.Errorf("Upsert(%s) err = nil, want validation error", tc.name)
			}
		})
	}
}

// TestAIPolicyAdapter_ResolverCascadeAgainstRealDB end-to-end-tests
// the resolver against the live adapter for the four cascade outcomes
// that matter operationally: channel hit, team hit, tenant hit, and
// default fallback. The resolver unit tests already cover all eight
// combinatorial branches against a fake; this case proves the wiring
// works against Postgres + RLS.
func TestAIPolicyAdapter_ResolverCascadeAgainstRealDB(t *testing.T) {
	t.Parallel()
	db := freshDBWithAIPolicyAdapter(t)
	store := newAIPolicyStore(t, db)
	tenant := seedAIPolicyTenant(t, db.AdminPool())

	resolver, err := aipolicy.NewResolver(store)
	if err != nil {
		t.Fatalf("NewResolver: %v", err)
	}

	channelKey := "whatsapp"
	teamKey := "11111111-2222-3333-4444-555555555555"

	mk := func(scope aipolicy.ScopeType, scopeID, model string) aipolicy.Policy {
		return aipolicy.Policy{
			TenantID:      tenant,
			ScopeType:     scope,
			ScopeID:       scopeID,
			Model:         model,
			PromptVersion: "v1",
			Tone:          "neutro",
			Language:      "pt-BR",
			AIEnabled:     true,
			Anonymize:     true,
			OptIn:         true,
		}
	}

	channelStr := channelKey
	teamStr := teamKey
	ctx := context.Background()

	// 1) No rows at all → SourceDefault.
	pol, src, err := resolver.Resolve(ctx, aipolicy.ResolveInput{
		TenantID:  tenant,
		ChannelID: &channelStr,
		TeamID:    &teamStr,
	})
	if err != nil {
		t.Fatalf("Resolve default: %v", err)
	}
	if src != aipolicy.SourceDefault {
		t.Errorf("default fallback source = %q, want %q", src, aipolicy.SourceDefault)
	}
	if pol.AIEnabled {
		t.Errorf("default fallback AIEnabled = true; want false (LGPD opt-in)")
	}

	// 2) Tenant row only → SourceTenant.
	if err := store.Upsert(ctx, mk(aipolicy.ScopeTenant, tenant.String(), "openrouter/tenant-model")); err != nil {
		t.Fatalf("seed tenant row: %v", err)
	}
	pol, src, err = resolver.Resolve(ctx, aipolicy.ResolveInput{
		TenantID:  tenant,
		ChannelID: &channelStr,
		TeamID:    &teamStr,
	})
	if err != nil {
		t.Fatalf("Resolve tenant: %v", err)
	}
	if src != aipolicy.SourceTenant {
		t.Errorf("tenant source = %q, want %q", src, aipolicy.SourceTenant)
	}
	if pol.Model != "openrouter/tenant-model" {
		t.Errorf("tenant Model = %q, want tenant-model", pol.Model)
	}

	// 3) Add team row → SourceTeam wins over tenant.
	if err := store.Upsert(ctx, mk(aipolicy.ScopeTeam, teamKey, "openrouter/team-model")); err != nil {
		t.Fatalf("seed team row: %v", err)
	}
	pol, src, err = resolver.Resolve(ctx, aipolicy.ResolveInput{
		TenantID:  tenant,
		ChannelID: &channelStr,
		TeamID:    &teamStr,
	})
	if err != nil {
		t.Fatalf("Resolve team: %v", err)
	}
	if src != aipolicy.SourceTeam {
		t.Errorf("team source = %q, want %q", src, aipolicy.SourceTeam)
	}
	if pol.Model != "openrouter/team-model" {
		t.Errorf("team Model = %q, want team-model", pol.Model)
	}

	// 4) Add channel row → SourceChannel wins over team and tenant.
	if err := store.Upsert(ctx, mk(aipolicy.ScopeChannel, channelKey, "openrouter/channel-model")); err != nil {
		t.Fatalf("seed channel row: %v", err)
	}
	pol, src, err = resolver.Resolve(ctx, aipolicy.ResolveInput{
		TenantID:  tenant,
		ChannelID: &channelStr,
		TeamID:    &teamStr,
	})
	if err != nil {
		t.Fatalf("Resolve channel: %v", err)
	}
	if src != aipolicy.SourceChannel {
		t.Errorf("channel source = %q, want %q", src, aipolicy.SourceChannel)
	}
	if pol.Model != "openrouter/channel-model" {
		t.Errorf("channel Model = %q, want channel-model", pol.Model)
	}
}
