package postgres_test

// SIN-62954 integration tests for the campaigns Postgres adapter.
//
// These live in the parent postgres_test package (not the
// internal/adapter/db/postgres/campaigns subpackage) so they share the
// TestMain / harness with the other postgres_test files — tests that
// need testpg in a separate binary race the ALTER ROLE bootstrap on
// the shared CI cluster (SQLSTATE 28P01), per ADR 0087 and memory
// `testpg shared-cluster ALTER ROLE race`.

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	pgcampaigns "github.com/pericles-luz/crm/internal/adapter/db/postgres/campaigns"
	"github.com/pericles-luz/crm/internal/adapter/db/postgres/testpg"
	"github.com/pericles-luz/crm/internal/campaigns"
)

// seedCampaignsTenant inserts a tenant under app_admin and returns
// its id. The campaign tables in 0102 only need a tenant to honour
// the FK; contact rows are seeded by tests that exercise
// LinkContactToCampaign.
func seedCampaignsTenant(t *testing.T, pool *pgxpool.Pool) uuid.UUID {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	id := uuid.New()
	if _, err := pool.Exec(ctx,
		`INSERT INTO tenants (id, name, host) VALUES ($1, $2, $3)`,
		id, "camp-"+id.String(), id.String()+".camp.test",
	); err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	return id
}

// seedCampaignsContact inserts a contact under tenantID so
// LinkContactToCampaign has a valid FK target.
func seedCampaignsContact(t *testing.T, pool *pgxpool.Pool, tenantID uuid.UUID) uuid.UUID {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	id := uuid.New()
	if _, err := pool.Exec(ctx,
		`INSERT INTO contact (id, tenant_id, display_name) VALUES ($1, $2, $3)`,
		id, tenantID, "Camp Tester",
	); err != nil {
		t.Fatalf("seed contact: %v", err)
	}
	return id
}

// freshDBWithCampaigns applies the migration chain campaigns needs:
// tenants + users + contacts (0088) + the phase 4 migration 0102.
func freshDBWithCampaigns(t *testing.T) *testpg.DB {
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

func newCampaignsStore(t *testing.T, db *testpg.DB) *pgcampaigns.Store {
	t.Helper()
	s, err := pgcampaigns.New(db.RuntimePool())
	if err != nil {
		t.Fatalf("pgcampaigns.New: %v", err)
	}
	return s
}

// ---------------------------------------------------------------------------
// Construction
// ---------------------------------------------------------------------------

func TestCampaignsAdapter_New_RejectsNilPool(t *testing.T) {
	t.Parallel()
	if _, err := pgcampaigns.New(nil); err == nil {
		t.Fatal("expected error for nil pool, got nil")
	}
}

// ---------------------------------------------------------------------------
// CreateCampaign + GetBySlug + ListByTenant
// ---------------------------------------------------------------------------

func TestCampaignsAdapter_CreateAndGetBySlug_Roundtrip(t *testing.T) {
	db := freshDBWithCampaigns(t)
	store := newCampaignsStore(t, db)
	tenant := seedCampaignsTenant(t, db.AdminPool())
	ctx := newCtx(t)

	now := time.Now().UTC().Truncate(time.Microsecond)
	expires := now.Add(48 * time.Hour)
	c, err := campaigns.NewCampaign(
		uuid.New(),
		tenant,
		"Black Friday 2026",
		"black-friday-2026",
		"https://example.test/promo",
		&expires,
		now,
	)
	if err != nil {
		t.Fatalf("NewCampaign: %v", err)
	}
	c.WithUTM("google", "cpc", "summer", "shoes", "ad-1")

	if err := store.CreateCampaign(ctx, c); err != nil {
		t.Fatalf("CreateCampaign: %v", err)
	}

	got, err := store.GetBySlug(ctx, tenant, "BLACK-FRIDAY-2026")
	if err != nil {
		t.Fatalf("GetBySlug: %v", err)
	}
	if got.ID != c.ID || got.TenantID != tenant {
		t.Errorf("identifiers mismatch: got id=%s tenant=%s, want id=%s tenant=%s", got.ID, got.TenantID, c.ID, tenant)
	}
	if got.Name != "Black Friday 2026" || got.Slug != "black-friday-2026" {
		t.Errorf("name/slug mismatch: %+v", got)
	}
	if got.UTMSource != "google" || got.UTMMedium != "cpc" || got.UTMCampaign != "summer" || got.UTMTerm != "shoes" || got.UTMContent != "ad-1" {
		t.Errorf("UTM mismatch: %+v", got)
	}
	if got.RedirectURL != "https://example.test/promo" {
		t.Errorf("redirect mismatch: %q", got.RedirectURL)
	}
	if got.ExpiresAt == nil || !got.ExpiresAt.Equal(expires) {
		t.Errorf("expiresAt mismatch: got %v, want %v", got.ExpiresAt, expires)
	}
	if got.Status != campaigns.StatusActive {
		t.Errorf("status: got %s, want active", got.Status)
	}
}

func TestCampaignsAdapter_CreateCampaign_RejectsNilCampaign(t *testing.T) {
	t.Parallel()
	db := freshDBWithCampaigns(t)
	store := newCampaignsStore(t, db)
	if err := store.CreateCampaign(newCtx(t), nil); err == nil {
		t.Fatal("expected error for nil campaign, got nil")
	}
}

func TestCampaignsAdapter_CreateCampaign_RejectsNilTenant(t *testing.T) {
	t.Parallel()
	db := freshDBWithCampaigns(t)
	store := newCampaignsStore(t, db)
	c := &campaigns.Campaign{
		ID:          uuid.New(),
		TenantID:    uuid.Nil,
		Name:        "Promo",
		Slug:        "promo",
		RedirectURL: "https://example.test",
		Status:      campaigns.StatusActive,
		CreatedAt:   time.Now().UTC(),
		UpdatedAt:   time.Now().UTC(),
	}
	if err := store.CreateCampaign(newCtx(t), c); !errors.Is(err, campaigns.ErrInvalidTenant) {
		t.Fatalf("got %v, want ErrInvalidTenant", err)
	}
}

func TestCampaignsAdapter_CreateCampaign_RejectsNilID(t *testing.T) {
	t.Parallel()
	db := freshDBWithCampaigns(t)
	store := newCampaignsStore(t, db)
	tenant := seedCampaignsTenant(t, db.AdminPool())
	c := &campaigns.Campaign{
		ID:          uuid.Nil,
		TenantID:    tenant,
		Name:        "Promo",
		Slug:        "promo",
		RedirectURL: "https://example.test",
		Status:      campaigns.StatusActive,
		CreatedAt:   time.Now().UTC(),
		UpdatedAt:   time.Now().UTC(),
	}
	if err := store.CreateCampaign(newCtx(t), c); err == nil {
		t.Fatal("expected id-nil error")
	}
}

func TestCampaignsAdapter_CreateCampaign_RejectsInvalidStatus(t *testing.T) {
	t.Parallel()
	db := freshDBWithCampaigns(t)
	store := newCampaignsStore(t, db)
	tenant := seedCampaignsTenant(t, db.AdminPool())
	c := &campaigns.Campaign{
		ID:          uuid.New(),
		TenantID:    tenant,
		Name:        "Promo",
		Slug:        "promo",
		RedirectURL: "https://example.test",
		Status:      campaigns.Status("paused"),
		CreatedAt:   time.Now().UTC(),
		UpdatedAt:   time.Now().UTC(),
	}
	if err := store.CreateCampaign(newCtx(t), c); err == nil {
		t.Fatal("expected invalid status error")
	}
}

func TestCampaignsAdapter_CreateCampaign_DuplicateSlugSameTenant(t *testing.T) {
	t.Parallel()
	db := freshDBWithCampaigns(t)
	store := newCampaignsStore(t, db)
	tenant := seedCampaignsTenant(t, db.AdminPool())
	ctx := newCtx(t)

	first, err := campaigns.NewCampaign(uuid.New(), tenant, "First", "promo", "https://example.test", nil, time.Now().UTC())
	if err != nil {
		t.Fatalf("NewCampaign 1: %v", err)
	}
	if err := store.CreateCampaign(ctx, first); err != nil {
		t.Fatalf("CreateCampaign 1: %v", err)
	}

	second, err := campaigns.NewCampaign(uuid.New(), tenant, "Second", "promo", "https://example.test/b", nil, time.Now().UTC())
	if err != nil {
		t.Fatalf("NewCampaign 2: %v", err)
	}
	if err := store.CreateCampaign(ctx, second); !errors.Is(err, campaigns.ErrSlugAlreadyExists) {
		t.Fatalf("got %v, want ErrSlugAlreadyExists", err)
	}
}

func TestCampaignsAdapter_CreateCampaign_SameSlugAcrossTenants(t *testing.T) {
	t.Parallel()
	db := freshDBWithCampaigns(t)
	store := newCampaignsStore(t, db)
	tenantA := seedCampaignsTenant(t, db.AdminPool())
	tenantB := seedCampaignsTenant(t, db.AdminPool())
	ctx := newCtx(t)

	a, _ := campaigns.NewCampaign(uuid.New(), tenantA, "A", "shared", "https://a.example.test", nil, time.Now().UTC())
	b, _ := campaigns.NewCampaign(uuid.New(), tenantB, "B", "shared", "https://b.example.test", nil, time.Now().UTC())

	if err := store.CreateCampaign(ctx, a); err != nil {
		t.Fatalf("CreateCampaign A: %v", err)
	}
	if err := store.CreateCampaign(ctx, b); err != nil {
		t.Fatalf("CreateCampaign B: %v", err)
	}

	gotA, err := store.GetBySlug(ctx, tenantA, "shared")
	if err != nil {
		t.Fatalf("GetBySlug A: %v", err)
	}
	if gotA.ID != a.ID {
		t.Errorf("tenant A: got id %s, want %s", gotA.ID, a.ID)
	}
	gotB, err := store.GetBySlug(ctx, tenantB, "shared")
	if err != nil {
		t.Fatalf("GetBySlug B: %v", err)
	}
	if gotB.ID != b.ID {
		t.Errorf("tenant B: got id %s, want %s", gotB.ID, b.ID)
	}
}

func TestCampaignsAdapter_GetBySlug_RLSHidesOtherTenant(t *testing.T) {
	t.Parallel()
	db := freshDBWithCampaigns(t)
	store := newCampaignsStore(t, db)
	tenantA := seedCampaignsTenant(t, db.AdminPool())
	tenantB := seedCampaignsTenant(t, db.AdminPool())
	ctx := newCtx(t)

	a, _ := campaigns.NewCampaign(uuid.New(), tenantA, "A", "only-a", "https://a.example.test", nil, time.Now().UTC())
	if err := store.CreateCampaign(ctx, a); err != nil {
		t.Fatalf("CreateCampaign A: %v", err)
	}

	if _, err := store.GetBySlug(ctx, tenantB, "only-a"); !errors.Is(err, campaigns.ErrNotFound) {
		t.Fatalf("tenant B should NOT see tenant A's campaign: got %v, want ErrNotFound", err)
	}
}

func TestCampaignsAdapter_GetBySlug_Missing(t *testing.T) {
	t.Parallel()
	db := freshDBWithCampaigns(t)
	store := newCampaignsStore(t, db)
	tenant := seedCampaignsTenant(t, db.AdminPool())

	if _, err := store.GetBySlug(newCtx(t), tenant, "ghost"); !errors.Is(err, campaigns.ErrNotFound) {
		t.Fatalf("got %v, want ErrNotFound", err)
	}
}

func TestCampaignsAdapter_GetBySlug_RejectsNilTenant(t *testing.T) {
	t.Parallel()
	db := freshDBWithCampaigns(t)
	store := newCampaignsStore(t, db)
	if _, err := store.GetBySlug(newCtx(t), uuid.Nil, "x"); !errors.Is(err, campaigns.ErrInvalidTenant) {
		t.Fatalf("got %v, want ErrInvalidTenant", err)
	}
}

func TestCampaignsAdapter_GetBySlug_BadSlugIsNotFound(t *testing.T) {
	t.Parallel()
	db := freshDBWithCampaigns(t)
	store := newCampaignsStore(t, db)
	tenant := seedCampaignsTenant(t, db.AdminPool())
	if _, err := store.GetBySlug(newCtx(t), tenant, "  "); !errors.Is(err, campaigns.ErrNotFound) {
		t.Fatalf("got %v, want ErrNotFound", err)
	}
}

func TestCampaignsAdapter_ListByTenant(t *testing.T) {
	t.Parallel()
	db := freshDBWithCampaigns(t)
	store := newCampaignsStore(t, db)
	tenantA := seedCampaignsTenant(t, db.AdminPool())
	tenantB := seedCampaignsTenant(t, db.AdminPool())
	ctx := newCtx(t)
	base := time.Now().UTC().Truncate(time.Microsecond)

	for i := 0; i < 3; i++ {
		c, _ := campaigns.NewCampaign(uuid.New(), tenantA, fmt.Sprintf("Campaign %d", i),
			fmt.Sprintf("camp-%d", i), "https://example.test", nil, base.Add(time.Duration(i)*time.Second))
		if err := store.CreateCampaign(ctx, c); err != nil {
			t.Fatalf("seed campaign %d: %v", i, err)
		}
	}
	otherCamp, _ := campaigns.NewCampaign(uuid.New(), tenantB, "Other", "other", "https://example.test", nil, base)
	if err := store.CreateCampaign(ctx, otherCamp); err != nil {
		t.Fatalf("seed other: %v", err)
	}

	got, err := store.ListByTenant(ctx, tenantA)
	if err != nil {
		t.Fatalf("ListByTenant A: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d rows, want 3", len(got))
	}
	// Verify newest-first ordering.
	for i := 0; i < len(got)-1; i++ {
		if got[i].CreatedAt.Before(got[i+1].CreatedAt) {
			t.Errorf("ordering: got[%d].CreatedAt %v < got[%d].CreatedAt %v",
				i, got[i].CreatedAt, i+1, got[i+1].CreatedAt)
		}
	}
	for _, c := range got {
		if c.TenantID != tenantA {
			t.Errorf("tenant leak: got tenant %s in list for %s", c.TenantID, tenantA)
		}
	}
}

func TestCampaignsAdapter_ListByTenant_Empty(t *testing.T) {
	t.Parallel()
	db := freshDBWithCampaigns(t)
	store := newCampaignsStore(t, db)
	tenant := seedCampaignsTenant(t, db.AdminPool())
	got, err := store.ListByTenant(newCtx(t), tenant)
	if err != nil {
		t.Fatalf("ListByTenant: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("got %d rows, want 0", len(got))
	}
}

func TestCampaignsAdapter_ListByTenant_RejectsNilTenant(t *testing.T) {
	t.Parallel()
	db := freshDBWithCampaigns(t)
	store := newCampaignsStore(t, db)
	if _, err := store.ListByTenant(newCtx(t), uuid.Nil); !errors.Is(err, campaigns.ErrInvalidTenant) {
		t.Fatalf("got %v, want ErrInvalidTenant", err)
	}
}

// ---------------------------------------------------------------------------
// RecordClick + idempotency
// ---------------------------------------------------------------------------

func seedActiveCampaign(t *testing.T, ctx context.Context, store *pgcampaigns.Store, tenant uuid.UUID) *campaigns.Campaign {
	t.Helper()
	c, err := campaigns.NewCampaign(uuid.New(), tenant, "Promo", "promo-"+uuid.NewString()[:8],
		"https://example.test", nil, time.Now().UTC())
	if err != nil {
		t.Fatalf("NewCampaign: %v", err)
	}
	if err := store.CreateCampaign(ctx, c); err != nil {
		t.Fatalf("seedActiveCampaign: %v", err)
	}
	return c
}

func TestCampaignsAdapter_RecordClick_Insert(t *testing.T) {
	t.Parallel()
	db := freshDBWithCampaigns(t)
	store := newCampaignsStore(t, db)
	tenant := seedCampaignsTenant(t, db.AdminPool())
	ctx := newCtx(t)
	camp := seedActiveCampaign(t, ctx, store, tenant)

	click, err := campaigns.NewCampaignClick(uuid.New(), tenant, camp.ID, "click-token-1", time.Now().UTC())
	if err != nil {
		t.Fatalf("NewCampaignClick: %v", err)
	}
	click.IP = netip.MustParseAddr("203.0.113.7")
	click.UserAgent = "Mozilla/5.0"
	click.Referrer = "https://example.test/landing"
	click.Meta["fbclid"] = "abc"

	got, err := store.RecordClick(ctx, click)
	if err != nil {
		t.Fatalf("RecordClick: %v", err)
	}
	if got.ClickID != "click-token-1" {
		t.Errorf("clickID: got %s, want click-token-1", got.ClickID)
	}
	if got.IP.String() != "203.0.113.7" {
		t.Errorf("IP roundtrip: got %v, want 203.0.113.7", got.IP)
	}
	if got.Meta["fbclid"] != "abc" {
		t.Errorf("meta.fbclid: got %v, want abc", got.Meta["fbclid"])
	}
}

func TestCampaignsAdapter_RecordClick_IdempotentSameClickID(t *testing.T) {
	t.Parallel()
	db := freshDBWithCampaigns(t)
	store := newCampaignsStore(t, db)
	tenant := seedCampaignsTenant(t, db.AdminPool())
	ctx := newCtx(t)
	camp := seedActiveCampaign(t, ctx, store, tenant)

	first, err := campaigns.NewCampaignClick(uuid.New(), tenant, camp.ID, "dedupe-me", time.Now().UTC())
	if err != nil {
		t.Fatalf("NewCampaignClick 1: %v", err)
	}
	first.UserAgent = "first-agent"
	first.Meta["origin"] = "first"

	out1, err := store.RecordClick(ctx, first)
	if err != nil {
		t.Fatalf("RecordClick 1: %v", err)
	}

	// A second call with the SAME click_id but DIFFERENT id/meta/UA
	// must return the original row, not insert a second.
	second, err := campaigns.NewCampaignClick(uuid.New(), tenant, camp.ID, "dedupe-me", time.Now().UTC().Add(time.Hour))
	if err != nil {
		t.Fatalf("NewCampaignClick 2: %v", err)
	}
	second.UserAgent = "second-agent"
	second.Meta["origin"] = "second"

	out2, err := store.RecordClick(ctx, second)
	if err != nil {
		t.Fatalf("RecordClick 2: %v", err)
	}

	if out1.ID != out2.ID {
		t.Errorf("ID drift: first %s, second %s", out1.ID, out2.ID)
	}
	if out2.UserAgent != "first-agent" {
		t.Errorf("second call MUST return first row's UA: got %q", out2.UserAgent)
	}
	if out2.Meta["origin"] != "first" {
		t.Errorf("second call MUST return first row's meta: got %v", out2.Meta["origin"])
	}

	// Database must hold exactly one row.
	assertClickCount(t, db.AdminPool(), camp.ID, 1)
}

func TestCampaignsAdapter_RecordClick_ConcurrentSameClickID(t *testing.T) {
	t.Parallel()
	db := freshDBWithCampaigns(t)
	store := newCampaignsStore(t, db)
	tenant := seedCampaignsTenant(t, db.AdminPool())
	ctx := newCtx(t)
	camp := seedActiveCampaign(t, ctx, store, tenant)

	const workers = 8
	results := make([]*campaigns.CampaignClick, workers)
	errs := make([]error, workers)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			click, err := campaigns.NewCampaignClick(uuid.New(), tenant, camp.ID, "race-token", time.Now().UTC())
			if err != nil {
				errs[idx] = err
				return
			}
			click.Meta[fmt.Sprintf("worker-%d", idx)] = idx
			results[idx], errs[idx] = store.RecordClick(ctx, click)
		}(i)
	}
	wg.Wait()

	var winner uuid.UUID
	for i, err := range errs {
		if err != nil {
			t.Fatalf("worker %d: %v", i, err)
		}
		if results[i] == nil {
			t.Fatalf("worker %d: nil result", i)
		}
		if i == 0 {
			winner = results[i].ID
			continue
		}
		if results[i].ID != winner {
			t.Errorf("worker %d returned id %s, want winner %s", i, results[i].ID, winner)
		}
	}
	assertClickCount(t, db.AdminPool(), camp.ID, 1)
}

func TestCampaignsAdapter_RecordClick_DifferentClickIDsAreDistinct(t *testing.T) {
	t.Parallel()
	db := freshDBWithCampaigns(t)
	store := newCampaignsStore(t, db)
	tenant := seedCampaignsTenant(t, db.AdminPool())
	ctx := newCtx(t)
	camp := seedActiveCampaign(t, ctx, store, tenant)

	for i := 0; i < 3; i++ {
		click, err := campaigns.NewCampaignClick(uuid.New(), tenant, camp.ID, fmt.Sprintf("c-%d", i), time.Now().UTC())
		if err != nil {
			t.Fatalf("NewCampaignClick: %v", err)
		}
		if _, err := store.RecordClick(ctx, click); err != nil {
			t.Fatalf("RecordClick: %v", err)
		}
	}
	assertClickCount(t, db.AdminPool(), camp.ID, 3)
}

func TestCampaignsAdapter_RecordClick_RejectsBadArgs(t *testing.T) {
	t.Parallel()
	db := freshDBWithCampaigns(t)
	store := newCampaignsStore(t, db)
	ctx := newCtx(t)

	if _, err := store.RecordClick(ctx, nil); err == nil {
		t.Fatal("expected error on nil click")
	}
	if _, err := store.RecordClick(ctx, &campaigns.CampaignClick{}); !errors.Is(err, campaigns.ErrInvalidTenant) {
		t.Fatalf("got %v, want ErrInvalidTenant", err)
	}
	if _, err := store.RecordClick(ctx, &campaigns.CampaignClick{TenantID: uuid.New()}); !errors.Is(err, campaigns.ErrInvalidCampaign) {
		t.Fatalf("got %v, want ErrInvalidCampaign", err)
	}
	if _, err := store.RecordClick(ctx, &campaigns.CampaignClick{
		TenantID:   uuid.New(),
		CampaignID: uuid.New(),
	}); err == nil {
		t.Fatal("expected error on nil id")
	}
	if _, err := store.RecordClick(ctx, &campaigns.CampaignClick{
		ID:         uuid.New(),
		TenantID:   uuid.New(),
		CampaignID: uuid.New(),
		ClickID:    "",
	}); !errors.Is(err, campaigns.ErrInvalidClickID) {
		t.Fatalf("got %v, want ErrInvalidClickID", err)
	}
}

// ---------------------------------------------------------------------------
// LinkContactToCampaign
// ---------------------------------------------------------------------------

func TestCampaignsAdapter_LinkContactToCampaign_Success(t *testing.T) {
	t.Parallel()
	db := freshDBWithCampaigns(t)
	store := newCampaignsStore(t, db)
	tenant := seedCampaignsTenant(t, db.AdminPool())
	contact := seedCampaignsContact(t, db.AdminPool(), tenant)
	ctx := newCtx(t)
	camp := seedActiveCampaign(t, ctx, store, tenant)

	click, err := campaigns.NewCampaignClick(uuid.New(), tenant, camp.ID, "link-me", time.Now().UTC())
	if err != nil {
		t.Fatalf("NewCampaignClick: %v", err)
	}
	if _, err := store.RecordClick(ctx, click); err != nil {
		t.Fatalf("RecordClick: %v", err)
	}

	if err := store.LinkContactToCampaign(ctx, tenant, "link-me", contact); err != nil {
		t.Fatalf("LinkContactToCampaign: %v", err)
	}

	// Verify the row updated.
	var gotContact uuid.UUID
	if err := db.AdminPool().QueryRow(ctx,
		`SELECT contact_id FROM campaign_clicks WHERE click_id = $1`, "link-me",
	).Scan(&gotContact); err != nil {
		t.Fatalf("verify link: %v", err)
	}
	if gotContact != contact {
		t.Errorf("contact_id: got %s, want %s", gotContact, contact)
	}
}

func TestCampaignsAdapter_LinkContactToCampaign_UnknownClick(t *testing.T) {
	t.Parallel()
	db := freshDBWithCampaigns(t)
	store := newCampaignsStore(t, db)
	tenant := seedCampaignsTenant(t, db.AdminPool())
	contact := seedCampaignsContact(t, db.AdminPool(), tenant)

	if err := store.LinkContactToCampaign(newCtx(t), tenant, "missing-click", contact); !errors.Is(err, campaigns.ErrNotFound) {
		t.Fatalf("got %v, want ErrNotFound", err)
	}
}

func TestCampaignsAdapter_LinkContactToCampaign_RLSCannotCrossTenants(t *testing.T) {
	t.Parallel()
	db := freshDBWithCampaigns(t)
	store := newCampaignsStore(t, db)
	tenantA := seedCampaignsTenant(t, db.AdminPool())
	tenantB := seedCampaignsTenant(t, db.AdminPool())
	contactB := seedCampaignsContact(t, db.AdminPool(), tenantB)
	ctx := newCtx(t)
	campA := seedActiveCampaign(t, ctx, store, tenantA)

	click, _ := campaigns.NewCampaignClick(uuid.New(), tenantA, campA.ID, "tenant-a-click", time.Now().UTC())
	if _, err := store.RecordClick(ctx, click); err != nil {
		t.Fatalf("RecordClick: %v", err)
	}

	// Tenant B tries to link to tenant A's click. RLS hides the row;
	// the UPDATE affects zero rows; we surface ErrNotFound.
	if err := store.LinkContactToCampaign(ctx, tenantB, "tenant-a-click", contactB); !errors.Is(err, campaigns.ErrNotFound) {
		t.Fatalf("got %v, want ErrNotFound", err)
	}
}

func TestCampaignsAdapter_LinkContactToCampaign_RejectsBadArgs(t *testing.T) {
	t.Parallel()
	db := freshDBWithCampaigns(t)
	store := newCampaignsStore(t, db)
	tenant := seedCampaignsTenant(t, db.AdminPool())
	ctx := newCtx(t)

	if err := store.LinkContactToCampaign(ctx, uuid.Nil, "x", uuid.New()); !errors.Is(err, campaigns.ErrInvalidTenant) {
		t.Fatalf("got %v, want ErrInvalidTenant", err)
	}
	if err := store.LinkContactToCampaign(ctx, tenant, "", uuid.New()); !errors.Is(err, campaigns.ErrInvalidClickID) {
		t.Fatalf("got %v, want ErrInvalidClickID", err)
	}
	if err := store.LinkContactToCampaign(ctx, tenant, "x", uuid.Nil); err == nil {
		t.Fatal("expected error for nil contact id")
	}
}

// ---------------------------------------------------------------------------
// StatsByTenant + ListClicks (SIN-62962 — dashboard inputs)
// ---------------------------------------------------------------------------

func TestCampaignsAdapter_StatsByTenant_EmptyMapWhenNoClicks(t *testing.T) {
	t.Parallel()
	db := freshDBWithCampaigns(t)
	store := newCampaignsStore(t, db)
	tenant := seedCampaignsTenant(t, db.AdminPool())
	ctx := newCtx(t)
	camp := seedActiveCampaign(t, ctx, store, tenant)
	got, err := store.StatsByTenant(ctx, tenant)
	if err != nil {
		t.Fatalf("StatsByTenant: %v", err)
	}
	if _, present := got[camp.ID]; present {
		t.Fatalf("campaign with zero clicks must be ABSENT from stats; got %#v", got)
	}
}

func TestCampaignsAdapter_StatsByTenant_CountsClicksAndAttributions(t *testing.T) {
	t.Parallel()
	db := freshDBWithCampaigns(t)
	store := newCampaignsStore(t, db)
	tenant := seedCampaignsTenant(t, db.AdminPool())
	contact := seedCampaignsContact(t, db.AdminPool(), tenant)
	ctx := newCtx(t)
	camp := seedActiveCampaign(t, ctx, store, tenant)
	other := seedActiveCampaign(t, ctx, store, tenant)

	for i := 0; i < 3; i++ {
		click, _ := campaigns.NewCampaignClick(uuid.New(), tenant, camp.ID,
			fmt.Sprintf("ck-%d", i), time.Now().UTC())
		if _, err := store.RecordClick(ctx, click); err != nil {
			t.Fatalf("seed click %d: %v", i, err)
		}
	}
	// Link two of them to a contact (attributions = 2).
	if err := store.LinkContactToCampaign(ctx, tenant, "ck-0", contact); err != nil {
		t.Fatalf("link 0: %v", err)
	}
	if err := store.LinkContactToCampaign(ctx, tenant, "ck-1", contact); err != nil {
		t.Fatalf("link 1: %v", err)
	}
	// One click on the other campaign so the GROUP BY emits two rows.
	otherClick, _ := campaigns.NewCampaignClick(uuid.New(), tenant, other.ID, "other-click", time.Now().UTC())
	if _, err := store.RecordClick(ctx, otherClick); err != nil {
		t.Fatalf("seed other: %v", err)
	}

	got, err := store.StatsByTenant(ctx, tenant)
	if err != nil {
		t.Fatalf("StatsByTenant: %v", err)
	}
	if got[camp.ID].Clicks != 3 || got[camp.ID].Attributions != 2 {
		t.Errorf("camp stats: got %+v, want clicks=3 attributions=2", got[camp.ID])
	}
	if got[other.ID].Clicks != 1 || got[other.ID].Attributions != 0 {
		t.Errorf("other stats: got %+v, want clicks=1 attributions=0", got[other.ID])
	}
}

func TestCampaignsAdapter_StatsByTenant_RLSHidesOtherTenant(t *testing.T) {
	t.Parallel()
	db := freshDBWithCampaigns(t)
	store := newCampaignsStore(t, db)
	tenantA := seedCampaignsTenant(t, db.AdminPool())
	tenantB := seedCampaignsTenant(t, db.AdminPool())
	ctx := newCtx(t)
	campA := seedActiveCampaign(t, ctx, store, tenantA)
	click, _ := campaigns.NewCampaignClick(uuid.New(), tenantA, campA.ID, "rls-token", time.Now().UTC())
	if _, err := store.RecordClick(ctx, click); err != nil {
		t.Fatalf("RecordClick: %v", err)
	}
	got, err := store.StatsByTenant(ctx, tenantB)
	if err != nil {
		t.Fatalf("StatsByTenant B: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("RLS leak: tenant B saw %d stat rows from tenant A", len(got))
	}
}

func TestCampaignsAdapter_StatsByTenant_RejectsNilTenant(t *testing.T) {
	t.Parallel()
	db := freshDBWithCampaigns(t)
	store := newCampaignsStore(t, db)
	if _, err := store.StatsByTenant(newCtx(t), uuid.Nil); !errors.Is(err, campaigns.ErrInvalidTenant) {
		t.Fatalf("got %v, want ErrInvalidTenant", err)
	}
}

func TestCampaignsAdapter_ListClicks_NewestFirstAndLimited(t *testing.T) {
	t.Parallel()
	db := freshDBWithCampaigns(t)
	store := newCampaignsStore(t, db)
	tenant := seedCampaignsTenant(t, db.AdminPool())
	ctx := newCtx(t)
	camp := seedActiveCampaign(t, ctx, store, tenant)

	base := time.Now().UTC().Truncate(time.Microsecond)
	for i := 0; i < 5; i++ {
		click, _ := campaigns.NewCampaignClick(uuid.New(), tenant, camp.ID,
			fmt.Sprintf("lc-%d", i), base.Add(time.Duration(i)*time.Second))
		if _, err := store.RecordClick(ctx, click); err != nil {
			t.Fatalf("seed click %d: %v", i, err)
		}
	}
	got, err := store.ListClicks(ctx, tenant, camp.ID, 3)
	if err != nil {
		t.Fatalf("ListClicks: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d clicks, want 3", len(got))
	}
	for i := 0; i < len(got)-1; i++ {
		if got[i].CreatedAt.Before(got[i+1].CreatedAt) {
			t.Errorf("ordering: got[%d].CreatedAt %v < got[%d].CreatedAt %v",
				i, got[i].CreatedAt, i+1, got[i+1].CreatedAt)
		}
	}
	if got[0].ClickID != "lc-4" {
		t.Errorf("got[0].ClickID = %q, want %q", got[0].ClickID, "lc-4")
	}
}

func TestCampaignsAdapter_ListClicks_ZeroLimitFallsBackToDefault(t *testing.T) {
	t.Parallel()
	db := freshDBWithCampaigns(t)
	store := newCampaignsStore(t, db)
	tenant := seedCampaignsTenant(t, db.AdminPool())
	ctx := newCtx(t)
	camp := seedActiveCampaign(t, ctx, store, tenant)
	for i := 0; i < 2; i++ {
		click, _ := campaigns.NewCampaignClick(uuid.New(), tenant, camp.ID,
			fmt.Sprintf("zl-%d", i), time.Now().UTC())
		if _, err := store.RecordClick(ctx, click); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	got, err := store.ListClicks(ctx, tenant, camp.ID, 0)
	if err != nil {
		t.Fatalf("ListClicks: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d, want 2 (default cap higher than 2)", len(got))
	}
}

func TestCampaignsAdapter_ListClicks_RLSHidesOtherTenant(t *testing.T) {
	t.Parallel()
	db := freshDBWithCampaigns(t)
	store := newCampaignsStore(t, db)
	tenantA := seedCampaignsTenant(t, db.AdminPool())
	tenantB := seedCampaignsTenant(t, db.AdminPool())
	ctx := newCtx(t)
	campA := seedActiveCampaign(t, ctx, store, tenantA)
	click, _ := campaigns.NewCampaignClick(uuid.New(), tenantA, campA.ID, "only-a-click", time.Now().UTC())
	if _, err := store.RecordClick(ctx, click); err != nil {
		t.Fatalf("RecordClick: %v", err)
	}
	got, err := store.ListClicks(ctx, tenantB, campA.ID, 10)
	if err != nil {
		t.Fatalf("ListClicks B: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("RLS leak: tenant B saw %d clicks from tenant A", len(got))
	}
}

func TestCampaignsAdapter_ListClicks_RejectsBadArgs(t *testing.T) {
	t.Parallel()
	db := freshDBWithCampaigns(t)
	store := newCampaignsStore(t, db)
	tenant := seedCampaignsTenant(t, db.AdminPool())
	ctx := newCtx(t)

	if _, err := store.ListClicks(ctx, uuid.Nil, uuid.New(), 10); !errors.Is(err, campaigns.ErrInvalidTenant) {
		t.Fatalf("got %v, want ErrInvalidTenant", err)
	}
	if _, err := store.ListClicks(ctx, tenant, uuid.Nil, 10); !errors.Is(err, campaigns.ErrInvalidCampaign) {
		t.Fatalf("got %v, want ErrInvalidCampaign", err)
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// assertClickCount uses the admin pool (BYPASSRLS) so test assertions
// see EVERY row regardless of tenant scope.
func assertClickCount(t *testing.T, pool *pgxpool.Pool, campaignID uuid.UUID, want int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var got int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM campaign_clicks WHERE campaign_id = $1`, campaignID,
	).Scan(&got); err != nil {
		t.Fatalf("count clicks: %v", err)
	}
	if got != want {
		t.Errorf("click count: got %d, want %d", got, want)
	}
}
