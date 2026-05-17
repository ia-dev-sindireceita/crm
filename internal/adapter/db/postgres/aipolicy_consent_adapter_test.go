package postgres_test

// SIN-62928 (Fase 3 decisão #8) integration tests for the
// ai_policy_consent Postgres adapter (ConsentStore).
//
// Lives in the parent postgres_test package (not the
// internal/adapter/db/postgres/aipolicy subpackage) to share the
// TestMain + harness with the other postgres_test files — tests in a
// separate binary race the ALTER ROLE bootstrap on the shared CI
// cluster (SQLSTATE 28P01), per ADR 0087 and the W1A migration test.

import (
	"context"
	"crypto/sha256"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"

	pgaipolicy "github.com/pericles-luz/crm/internal/adapter/db/postgres/aipolicy"
	"github.com/pericles-luz/crm/internal/adapter/db/postgres/testpg"
	"github.com/pericles-luz/crm/internal/aipolicy"
)

// freshDBWithConsentAdapter applies the migration chain
// 0101_ai_policy_consent needs (tenants + users + 0101). Mirrors the
// migration acceptance test so any chain drift is caught the next
// time the chain changes.
func freshDBWithConsentAdapter(t *testing.T) (*testpg.DB, context.Context) {
	return freshDBWithAIPolicyConsent(t)
}

func newConsentStore(t *testing.T, db *testpg.DB) *pgaipolicy.ConsentStore {
	t.Helper()
	s, err := pgaipolicy.NewConsentStore(db.RuntimePool())
	if err != nil {
		t.Fatalf("NewConsentStore: %v", err)
	}
	return s
}

func consentHashFor(seed string) [32]byte {
	return sha256.Sum256([]byte(seed))
}

func TestConsentAdapter_New_RejectsNilPool(t *testing.T) {
	t.Parallel()
	if _, err := pgaipolicy.NewConsentStore(nil); err == nil {
		t.Error("NewConsentStore(nil) err = nil, want postgres.ErrNilPool")
	}
}

func TestConsentAdapter_Get_MissingRowReturnsFalse(t *testing.T) {
	t.Parallel()
	db, ctx := freshDBWithConsentAdapter(t)
	store := newConsentStore(t, db)
	tenant, _ := seedTenantUserMaster(t, db)

	_, ok, err := store.Get(ctx, tenant, aipolicy.ScopeTenant, tenant.String())
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if ok {
		t.Error("Get returned ok=true for fresh tenant; want false")
	}
}

func TestConsentAdapter_UpsertAndGetRoundTrip(t *testing.T) {
	t.Parallel()
	db, ctx := freshDBWithConsentAdapter(t)
	store := newConsentStore(t, db)
	tenant, _ := seedTenantUserMaster(t, db)

	want := aipolicy.Consent{
		TenantID:          tenant,
		ScopeKind:         aipolicy.ScopeTenant,
		ScopeID:           tenant.String(),
		PayloadHash:       consentHashFor("round-trip"),
		AnonymizerVersion: "anon-v1",
		PromptVersion:     "prompt-v1",
	}
	if err := store.Upsert(ctx, want); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	got, ok, err := store.Get(ctx, tenant, aipolicy.ScopeTenant, tenant.String())
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !ok {
		t.Fatal("Get returned ok=false after Upsert")
	}
	if got.TenantID != tenant {
		t.Errorf("TenantID = %v; want %v", got.TenantID, tenant)
	}
	if got.ScopeKind != aipolicy.ScopeTenant {
		t.Errorf("ScopeKind = %q; want %q", got.ScopeKind, aipolicy.ScopeTenant)
	}
	if got.ScopeID != tenant.String() {
		t.Errorf("ScopeID = %q; want %q", got.ScopeID, tenant.String())
	}
	if got.PayloadHash != want.PayloadHash {
		t.Errorf("PayloadHash mismatch")
	}
	if got.AnonymizerVersion != want.AnonymizerVersion {
		t.Errorf("AnonymizerVersion = %q; want %q", got.AnonymizerVersion, want.AnonymizerVersion)
	}
	if got.PromptVersion != want.PromptVersion {
		t.Errorf("PromptVersion = %q; want %q", got.PromptVersion, want.PromptVersion)
	}
	if got.AcceptedAt.IsZero() {
		t.Errorf("AcceptedAt is zero; want column DEFAULT to populate")
	}
}

func TestConsentAdapter_UpsertUpdatesInPlace(t *testing.T) {
	t.Parallel()
	db, ctx := freshDBWithConsentAdapter(t)
	store := newConsentStore(t, db)
	tenant, _ := seedTenantUserMaster(t, db)
	scopeID := tenant.String()

	first := aipolicy.Consent{
		TenantID:          tenant,
		ScopeKind:         aipolicy.ScopeTenant,
		ScopeID:           scopeID,
		PayloadHash:       consentHashFor("first"),
		AnonymizerVersion: "anon-v1",
		PromptVersion:     "prompt-v1",
	}
	if err := store.Upsert(ctx, first); err != nil {
		t.Fatalf("first Upsert: %v", err)
	}
	got1, _, _ := store.Get(ctx, tenant, aipolicy.ScopeTenant, scopeID)

	second := first
	second.PayloadHash = consentHashFor("second")
	second.AnonymizerVersion = "anon-v2"
	second.PromptVersion = "prompt-v2"
	if err := store.Upsert(ctx, second); err != nil {
		t.Fatalf("second Upsert: %v", err)
	}
	got2, ok, err := store.Get(ctx, tenant, aipolicy.ScopeTenant, scopeID)
	if err != nil || !ok {
		t.Fatalf("Get after re-consent: ok=%v err=%v", ok, err)
	}
	if got2.PayloadHash != second.PayloadHash {
		t.Errorf("PayloadHash not updated; want second's hash")
	}
	if got2.AnonymizerVersion != "anon-v2" {
		t.Errorf("AnonymizerVersion = %q; want anon-v2", got2.AnonymizerVersion)
	}
	if got2.PromptVersion != "prompt-v2" {
		t.Errorf("PromptVersion = %q; want prompt-v2", got2.PromptVersion)
	}
	if !got2.AcceptedAt.After(got1.AcceptedAt) && !got2.AcceptedAt.Equal(got1.AcceptedAt) {
		t.Errorf("AcceptedAt went backwards: got2=%v got1=%v", got2.AcceptedAt, got1.AcceptedAt)
	}

	// Exactly one row exists for the scope (UPSERT, not second INSERT).
	var count int
	if err := db.AdminPool().QueryRow(ctx,
		`SELECT count(*) FROM ai_policy_consent
		  WHERE tenant_id = $1 AND scope_kind = 'tenant' AND scope_id = $2`,
		tenant, scopeID).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 1 {
		t.Errorf("rows after re-Upsert = %d; want 1", count)
	}
}

func TestConsentAdapter_UpsertPersistsActorUserID(t *testing.T) {
	t.Parallel()
	db, ctx := freshDBWithConsentAdapter(t)
	store := newConsentStore(t, db)
	tenant, _ := seedTenantUserMaster(t, db)
	actor := seedAIPolicyConsentActor(t, ctx, db, tenant)

	if err := store.Upsert(ctx, aipolicy.Consent{
		TenantID:          tenant,
		ScopeKind:         aipolicy.ScopeTenant,
		ScopeID:           tenant.String(),
		ActorUserID:       &actor,
		PayloadHash:       consentHashFor("actor"),
		AnonymizerVersion: "anon-v1",
		PromptVersion:     "prompt-v1",
	}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	got, _, _ := store.Get(ctx, tenant, aipolicy.ScopeTenant, tenant.String())
	if got.ActorUserID == nil || *got.ActorUserID != actor {
		t.Errorf("ActorUserID = %v; want %v", got.ActorUserID, actor)
	}
}

func TestConsentAdapter_UpsertNilActorRoundTripsAsNull(t *testing.T) {
	t.Parallel()
	db, ctx := freshDBWithConsentAdapter(t)
	store := newConsentStore(t, db)
	tenant, _ := seedTenantUserMaster(t, db)

	if err := store.Upsert(ctx, aipolicy.Consent{
		TenantID:          tenant,
		ScopeKind:         aipolicy.ScopeTenant,
		ScopeID:           tenant.String(),
		ActorUserID:       nil,
		PayloadHash:       consentHashFor("null-actor"),
		AnonymizerVersion: "anon-v1",
		PromptVersion:     "prompt-v1",
	}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	got, _, _ := store.Get(ctx, tenant, aipolicy.ScopeTenant, tenant.String())
	if got.ActorUserID != nil {
		t.Errorf("ActorUserID = %v; want nil", got.ActorUserID)
	}
}

func TestConsentAdapter_ScopeUniqueness(t *testing.T) {
	t.Parallel()
	db, ctx := freshDBWithConsentAdapter(t)
	store := newConsentStore(t, db)
	tenant, _ := seedTenantUserMaster(t, db)

	base := aipolicy.Consent{
		TenantID:          tenant,
		ScopeKind:         aipolicy.ScopeTenant,
		ScopeID:           tenant.String(),
		PayloadHash:       consentHashFor("first"),
		AnonymizerVersion: "anon-v1",
		PromptVersion:     "prompt-v1",
	}
	if err := store.Upsert(ctx, base); err != nil {
		t.Fatalf("base Upsert: %v", err)
	}

	// A second Upsert at the same triple must collapse into a single
	// row via ON CONFLICT (already exercised above) but a raw INSERT
	// must trip the UNIQUE constraint. The adapter funnels writes
	// through ON CONFLICT; this assertion guards against a future
	// adapter refactor that removes that branch.
	secondHash := consentHashFor("second-via-insert")
	_, err := db.AdminPool().Exec(ctx, `
		INSERT INTO ai_policy_consent
		  (tenant_id, scope_kind, scope_id, payload_hash,
		   anonymizer_version, prompt_version)
		VALUES ($1, 'tenant', $2, $3, 'anon-v2', 'prompt-v2')`,
		tenant, tenant.String(), secondHash[:])
	if err == nil {
		t.Fatal("expected unique-violation on raw duplicate INSERT, got nil")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "duplicate key value") {
		t.Errorf("expected duplicate-key error, got: %v", err)
	}
}

func TestConsentAdapter_Get_ValidationRejects(t *testing.T) {
	t.Parallel()
	db, ctx := freshDBWithConsentAdapter(t)
	store := newConsentStore(t, db)
	tenant, _ := seedTenantUserMaster(t, db)

	cases := []struct {
		name string
		t    uuid.UUID
		kind aipolicy.ScopeType
		id   string
		want error
	}{
		{"zero tenant", uuid.Nil, aipolicy.ScopeTenant, "x", aipolicy.ErrInvalidTenant},
		{"bad kind", tenant, aipolicy.ScopeType("bogus"), "x", aipolicy.ErrInvalidScopeType},
		{"blank id", tenant, aipolicy.ScopeTenant, "   ", aipolicy.ErrInvalidScopeID},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := store.Get(ctx, tc.t, tc.kind, tc.id)
			if !errors.Is(err, tc.want) {
				t.Errorf("Get(%q): %v; want %v", tc.name, err, tc.want)
			}
		})
	}
}

func TestConsentAdapter_Upsert_ValidationRejects(t *testing.T) {
	t.Parallel()
	db, ctx := freshDBWithConsentAdapter(t)
	store := newConsentStore(t, db)
	tenant, _ := seedTenantUserMaster(t, db)

	good := aipolicy.Consent{
		TenantID:          tenant,
		ScopeKind:         aipolicy.ScopeTenant,
		ScopeID:           tenant.String(),
		PayloadHash:       consentHashFor("validation"),
		AnonymizerVersion: "anon-v1",
		PromptVersion:     "prompt-v1",
	}

	cases := []struct {
		name string
		mut  func(c *aipolicy.Consent)
		want error
	}{
		{"zero tenant", func(c *aipolicy.Consent) { c.TenantID = uuid.Nil }, aipolicy.ErrInvalidTenant},
		{"bad kind", func(c *aipolicy.Consent) { c.ScopeKind = aipolicy.ScopeType("bogus") }, aipolicy.ErrInvalidScopeType},
		{"blank id", func(c *aipolicy.Consent) { c.ScopeID = " " }, aipolicy.ErrInvalidScopeID},
		{"blank anon", func(c *aipolicy.Consent) { c.AnonymizerVersion = "" }, aipolicy.ErrInvalidAnonymizerVersion},
		{"blank prompt", func(c *aipolicy.Consent) { c.PromptVersion = "" }, aipolicy.ErrInvalidPromptVersion},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			c := good
			tc.mut(&c)
			err := store.Upsert(ctx, c)
			if !errors.Is(err, tc.want) {
				t.Errorf("Upsert(%q): %v; want %v", tc.name, err, tc.want)
			}
		})
	}
}

func TestConsentAdapter_RLSScopeIsolation(t *testing.T) {
	t.Parallel()
	db, ctx := freshDBWithConsentAdapter(t)
	store := newConsentStore(t, db)
	tenantA, _ := seedTenantUserMaster(t, db)
	tenantB := seedTenantB(t, ctx, db)

	if err := store.Upsert(ctx, aipolicy.Consent{
		TenantID:          tenantA,
		ScopeKind:         aipolicy.ScopeTenant,
		ScopeID:           tenantA.String(),
		PayloadHash:       consentHashFor("a"),
		AnonymizerVersion: "anon-v1",
		PromptVersion:     "prompt-v1",
	}); err != nil {
		t.Fatalf("Upsert A: %v", err)
	}

	// Tenant B sees nothing for tenant A's scope.
	_, ok, err := store.Get(ctx, tenantB, aipolicy.ScopeTenant, tenantA.String())
	if err != nil {
		t.Fatalf("Get B: %v", err)
	}
	if ok {
		t.Errorf("tenant B saw tenant A's consent row; want hidden by RLS")
	}
}
