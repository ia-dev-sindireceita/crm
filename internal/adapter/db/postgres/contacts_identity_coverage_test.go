// Integration tests that extend F2-13 / F2-06 contacts adapter coverage.
// Lives in package postgres_test so it shares the testpg harness with
// identity_integration_test.go / contacts_adapter_test.go — running these
// from a separate test binary would race ALTER ROLE on the CI cluster
// (SIN-62794 / memory note testpg_shared_cluster_race).
//
// SIN-62856 follow-up: pushes internal/adapter/db/postgres/contacts above
// the 80% AC by exercising the previously-uncovered paths:
//   - WithClock / WithMergeEnabled toggles.
//   - FindByContactID (happy / not-found / cross-tenant).
//   - Resolve cross-channel phone & email match (Link).
//   - Resolve MergeActionMerge (auto-merge no-leader) + MergeActionPropose
//     (two leaders ⇒ MergeProposal).
//   - Zero-tenant guards on Resolve / Merge / Split.
//   - postgres.WithTenant cross-tenant ErrNotFound for Split.
package postgres_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	pgcontacts "github.com/pericles-luz/crm/internal/adapter/db/postgres/contacts"
	"github.com/pericles-luz/crm/internal/contacts"
)

// seedIdentityUser inserts an in-tenant (non-master) user so it can satisfy
// assignment_history.user_id FK. The existing seedTenantUser helper
// (account_lockout_test.go) takes a different signature and also creates
// its own tenant — we already have a tenant id, so a thin local seeder is
// clearer than threading extra parameters through it.
func seedIdentityUser(t *testing.T, pool *pgxpool.Pool, tenantID uuid.UUID) uuid.UUID {
	t.Helper()
	ctx := newCtx(t)
	userID := uuid.New()
	if _, err := pool.Exec(ctx,
		`INSERT INTO users (id, tenant_id, email, password_hash, role)
		 VALUES ($1, $2, $3, 'x', 'agent')`,
		userID, tenantID, userID.String()+"@x.test"); err != nil {
		t.Fatalf("seed tenant user: %v", err)
	}
	return userID
}

// seedConversationAssignment links contactID to a conversation + an
// assignment_history row so the EXISTS(...) leader probe baked into
// IdentityStore.lookupByPhone / lookupByEmail returns has_leader=true.
func seedConversationAssignment(t *testing.T, pool *pgxpool.Pool, tenantID, contactID, userID uuid.UUID) {
	t.Helper()
	ctx := newCtx(t)
	convID := uuid.New()
	if _, err := pool.Exec(ctx,
		`INSERT INTO conversation (id, tenant_id, contact_id, channel)
		 VALUES ($1, $2, $3, 'whatsapp')`,
		convID, tenantID, contactID); err != nil {
		t.Fatalf("seed conversation: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO assignment_history (tenant_id, conversation_id, user_id, reason)
		 VALUES ($1, $2, $3, 'lead')`,
		tenantID, convID, userID); err != nil {
		t.Fatalf("seed assignment_history: %v", err)
	}
}

// resolveExisting runs Resolve and fails the test if it errors or emits
// a proposal; returns the identity for follow-up assertions.
func resolveExisting(
	t *testing.T, s *pgcontacts.IdentityStore,
	tenantID uuid.UUID, channel, external, phone, email string,
) *contacts.Identity {
	t.Helper()
	id, prop, err := s.Resolve(context.Background(), tenantID, channel, external, phone, email)
	if err != nil {
		t.Fatalf("Resolve(%s,%s): %v", channel, external, err)
	}
	if prop != nil {
		t.Fatalf("Resolve(%s,%s) unexpected proposal: %+v", channel, external, prop)
	}
	return id
}

func TestIdentityStore_WithClock_PinsCreatedAt(t *testing.T) {
	t.Parallel()
	store, db := newIdentityStore(t)
	tenantID := seedTenantForIdentity(t, db.AdminPool())
	seedContact(t, db.AdminPool(), tenantID, "whatsapp", "+5511999990100", "Hank")

	pinned := time.Date(2026, 5, 16, 10, 0, 0, 0, time.UTC)
	id := resolveExisting(t, store.WithClock(func() time.Time { return pinned }),
		tenantID, "whatsapp", "+5511999990100", "", "")
	if !id.CreatedAt.Equal(pinned) {
		t.Errorf("CreatedAt = %v, want %v", id.CreatedAt, pinned)
	}
}

func TestIdentityStore_WithMergeEnabled_False_SkipsPhoneAndEmail(t *testing.T) {
	t.Parallel()
	store, db := newIdentityStore(t)
	tenantID := seedTenantForIdentity(t, db.AdminPool())
	existing := resolveExisting(t, store, tenantID, "whatsapp", "+5511999990200", "", "")

	// New (channel, externalID) with phone+email that match a different
	// existing contact. With merge disabled, those candidates are skipped
	// and Resolve creates a fresh identity instead of linking back.
	seedContact(t, db.AdminPool(), tenantID, "whatsapp", "+5511999990200", "Ivy")
	fresh, prop, err := store.WithMergeEnabled(false).Resolve(
		context.Background(), tenantID, "instagram", "ig_no_merge",
		"+5511999990200", "ivy@example.com")
	if err != nil {
		t.Fatalf("Resolve(merge disabled): %v", err)
	}
	if prop != nil {
		t.Errorf("merge disabled should not propose: %+v", prop)
	}
	if fresh.ID == existing.ID {
		t.Error("merge disabled but Resolve linked to existing identity")
	}
}

func TestIdentityStore_FindByContactID_HappyPath(t *testing.T) {
	t.Parallel()
	store, db := newIdentityStore(t)
	tenantID := seedTenantForIdentity(t, db.AdminPool())
	seedContact(t, db.AdminPool(), tenantID, "whatsapp", "+5511999990300", "Jack")
	created := resolveExisting(t, store, tenantID, "whatsapp", "+5511999990300", "", "")
	if len(created.Links) == 0 {
		t.Fatal("seed identity missing link")
	}
	contactID := created.Links[0].ContactID

	got, err := store.FindByContactID(context.Background(), tenantID, contactID)
	if err != nil {
		t.Fatalf("FindByContactID: %v", err)
	}
	if got.ID != created.ID {
		t.Errorf("identity ID = %v, want %v", got.ID, created.ID)
	}
	if len(got.Links) != 1 || got.Links[0].ContactID != contactID {
		t.Errorf("links = %+v, want one for %v", got.Links, contactID)
	}
}

func TestIdentityStore_FindByContactID_NotFound(t *testing.T) {
	t.Parallel()
	store, db := newIdentityStore(t)
	tenantID := seedTenantForIdentity(t, db.AdminPool())
	_, err := store.FindByContactID(context.Background(), tenantID, uuid.New())
	if !isErrContaining(err, contacts.ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestIdentityStore_FindByContactID_RejectsZeroTenant(t *testing.T) {
	t.Parallel()
	store, _ := newIdentityStore(t)
	if _, err := store.FindByContactID(context.Background(), uuid.Nil, uuid.New()); err == nil {
		t.Error("FindByContactID(zero tenant) err = nil")
	}
}

func TestIdentityStore_FindByContactID_CrossTenantHiddenByRLS(t *testing.T) {
	t.Parallel()
	store, db := newIdentityStore(t)
	tenantA := seedTenantForIdentity(t, db.AdminPool())
	tenantB := seedTenantForIdentity(t, db.AdminPool())
	seedContact(t, db.AdminPool(), tenantA, "whatsapp", "+5511999990310", "Kim")
	idA := resolveExisting(t, store, tenantA, "whatsapp", "+5511999990310", "", "")
	contactID := idA.Links[0].ContactID

	_, err := store.FindByContactID(context.Background(), tenantB, contactID)
	if !isErrContaining(err, contacts.ErrNotFound) {
		t.Errorf("cross-tenant err = %v, want ErrNotFound", err)
	}
}

func TestIdentityStore_Resolve_RejectsZeroTenant(t *testing.T) {
	t.Parallel()
	store, _ := newIdentityStore(t)
	if _, _, err := store.Resolve(context.Background(), uuid.Nil, "whatsapp", "x", "", ""); err == nil {
		t.Error("Resolve(zero tenant) err = nil")
	}
}

func TestIdentityStore_Merge_RejectsZeroTenant(t *testing.T) {
	t.Parallel()
	store, _ := newIdentityStore(t)
	if err := store.Merge(context.Background(), uuid.Nil, uuid.New(), uuid.New(), "x"); err == nil {
		t.Error("Merge(zero tenant) err = nil")
	}
}

func TestIdentityStore_Split_RejectsZeroTenant(t *testing.T) {
	t.Parallel()
	store, _ := newIdentityStore(t)
	if err := store.Split(context.Background(), uuid.Nil, uuid.New()); err == nil {
		t.Error("Split(zero tenant) err = nil")
	}
}

func TestIdentityStore_Resolve_PhoneMatch_LinksAcrossChannel(t *testing.T) {
	t.Parallel()
	store, db := newIdentityStore(t)
	tenantID := seedTenantForIdentity(t, db.AdminPool())
	seedContact(t, db.AdminPool(), tenantID, "whatsapp", "+5511999990400", "Liam")
	existing := resolveExisting(t, store, tenantID, "whatsapp", "+5511999990400", "", "")

	// A second contact on a different channel whose phone matches the
	// already-resolved identity: direct (instagram, ig_liam) lookup misses
	// because that contact has no contact_identity_link yet; the phone
	// lookup hits the whatsapp identity → MergeActionLink.
	seedContact(t, db.AdminPool(), tenantID, "instagram", "ig_liam", "Liam")
	linked := resolveExisting(t, store, tenantID, "instagram", "ig_liam", "+5511999990400", "")
	if linked.ID != existing.ID {
		t.Errorf("identity = %v, want %v", linked.ID, existing.ID)
	}
}

func TestIdentityStore_Resolve_EmailMatch_LinksAcrossChannel(t *testing.T) {
	t.Parallel()
	store, db := newIdentityStore(t)
	tenantID := seedTenantForIdentity(t, db.AdminPool())
	seedContact(t, db.AdminPool(), tenantID, "email", "mia@example.com", "Mia")
	existing := resolveExisting(t, store, tenantID, "email", "mia@example.com", "", "")

	seedContact(t, db.AdminPool(), tenantID, "webchat", "wc_mia", "Mia")
	linked := resolveExisting(t, store, tenantID, "webchat", "wc_mia", "", "mia@example.com")
	if linked.ID != existing.ID {
		t.Errorf("identity = %v, want %v", linked.ID, existing.ID)
	}
}

func TestIdentityStore_Resolve_AutoMerges_NoLeaderConflict(t *testing.T) {
	t.Parallel()
	store, db := newIdentityStore(t)
	tenantID := seedTenantForIdentity(t, db.AdminPool())
	// Identity P (phone-only) and Identity E (email-only) — neither has
	// an assignment_history row so both report has_leader=false.
	seedContact(t, db.AdminPool(), tenantID, "whatsapp", "+5511999990500", "Noa")
	pIdent := resolveExisting(t, store, tenantID, "whatsapp", "+5511999990500", "", "")
	seedContact(t, db.AdminPool(), tenantID, "email", "noa@example.com", "Noa")
	eIdent := resolveExisting(t, store, tenantID, "email", "noa@example.com", "", "")
	if pIdent.ID == eIdent.ID {
		t.Fatal("setup invalid: phone and email already share identity")
	}

	// Resolve a new (instagram, ig_noa) carrying the same phone + email:
	// two candidates, no leaders → MergeActionMerge collapses to the
	// smallest-UUID survivor.
	seedContact(t, db.AdminPool(), tenantID, "instagram", "ig_noa", "Noa")
	merged, prop, err := store.Resolve(context.Background(), tenantID,
		"instagram", "ig_noa", "+5511999990500", "noa@example.com")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if prop != nil {
		t.Errorf("no-leader merge should not propose: %+v", prop)
	}
	if merged.ID != pIdent.ID && merged.ID != eIdent.ID {
		t.Errorf("merged identity = %v, want one of (%v, %v)", merged.ID, pIdent.ID, eIdent.ID)
	}
	loser := pIdent.ID
	if merged.ID == pIdent.ID {
		loser = eIdent.ID
	}
	var into *uuid.UUID
	if err := db.AdminPool().QueryRow(newCtx(t),
		`SELECT merged_into_id FROM identity WHERE id = $1`, loser).Scan(&into); err != nil {
		t.Fatalf("inspect loser: %v", err)
	}
	if into == nil || *into != merged.ID {
		t.Errorf("loser.merged_into_id = %v, want %v", into, merged.ID)
	}
}

func TestIdentityStore_Resolve_ProposesWhenBothCandidatesHaveLeaders(t *testing.T) {
	t.Parallel()
	store, db := newIdentityStore(t)
	tenantID := seedTenantForIdentity(t, db.AdminPool())
	userID := seedIdentityUser(t, db.AdminPool(), tenantID)

	phoneContact := seedContact(t, db.AdminPool(), tenantID, "whatsapp", "+5511999990600", "Olive")
	pIdent := resolveExisting(t, store, tenantID, "whatsapp", "+5511999990600", "", "")
	seedConversationAssignment(t, db.AdminPool(), tenantID, phoneContact, userID)

	emailContact := seedContact(t, db.AdminPool(), tenantID, "email", "olive@example.com", "Olive")
	eIdent := resolveExisting(t, store, tenantID, "email", "olive@example.com", "", "")
	seedConversationAssignment(t, db.AdminPool(), tenantID, emailContact, userID)

	if pIdent.ID == eIdent.ID {
		t.Fatal("setup invalid: phone and email share identity")
	}

	seedContact(t, db.AdminPool(), tenantID, "instagram", "ig_olive", "Olive")
	id, prop, err := store.Resolve(context.Background(), tenantID,
		"instagram", "ig_olive", "+5511999990600", "olive@example.com")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if prop == nil {
		t.Fatal("expected MergeProposal, got nil")
	}
	if prop.TenantID != tenantID {
		t.Errorf("proposal TenantID = %v, want %v", prop.TenantID, tenantID)
	}
	if prop.SourceID == prop.TargetID {
		t.Errorf("proposal source == target = %v", prop.TargetID)
	}
	if id == nil || id.ID != prop.TargetID {
		t.Errorf("Resolve identity %+v does not match proposal target %v", id, prop.TargetID)
	}
}

func TestIdentityStore_Split_CrossTenant_NotFound(t *testing.T) {
	t.Parallel()
	store, db := newIdentityStore(t)
	tenantA := seedTenantForIdentity(t, db.AdminPool())
	tenantB := seedTenantForIdentity(t, db.AdminPool())
	seedContact(t, db.AdminPool(), tenantA, "whatsapp", "+5511999990800", "Sam")
	idA := resolveExisting(t, store, tenantA, "whatsapp", "+5511999990800", "", "")
	if len(idA.Links) == 0 {
		t.Fatal("missing link")
	}
	linkID := idA.Links[0].ID

	err := store.Split(context.Background(), tenantB, linkID)
	if !isErrContaining(err, contacts.ErrNotFound) {
		t.Errorf("cross-tenant Split err = %v, want ErrNotFound", err)
	}
}
