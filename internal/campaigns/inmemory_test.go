package campaigns_test

import (
	"context"
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/campaigns"
)

func TestInMemoryRepository_CreateAndGetBySlug(t *testing.T) {
	t.Parallel()
	r := campaigns.NewInMemoryRepository()
	tenant := uuid.New()
	now := time.Now().UTC()
	c, err := campaigns.NewCampaign(uuid.New(), tenant, "BlackFriday", "Black-Friday", "https://example.test/x", nil, now)
	if err != nil {
		t.Fatalf("NewCampaign: %v", err)
	}
	if err := r.CreateCampaign(context.Background(), c); err != nil {
		t.Fatalf("CreateCampaign: %v", err)
	}
	got, err := r.GetBySlug(context.Background(), tenant, "BLACK-FRIDAY")
	if err != nil {
		t.Fatalf("GetBySlug: %v", err)
	}
	if got.ID != c.ID {
		t.Fatalf("GetBySlug id mismatch: got %s, want %s", got, c)
	}
}

func TestInMemoryRepository_GetBySlug_OtherTenant_NotFound(t *testing.T) {
	t.Parallel()
	r := campaigns.NewInMemoryRepository()
	tenantA := uuid.New()
	tenantB := uuid.New()
	c, err := campaigns.NewCampaign(uuid.New(), tenantA, "promo", "promo-x", "https://example.test/x", nil, time.Now())
	if err != nil {
		t.Fatalf("NewCampaign: %v", err)
	}
	if err := r.CreateCampaign(context.Background(), c); err != nil {
		t.Fatalf("CreateCampaign: %v", err)
	}
	if _, err := r.GetBySlug(context.Background(), tenantB, "promo-x"); !errors.Is(err, campaigns.ErrNotFound) {
		t.Fatalf("GetBySlug other tenant err = %v, want ErrNotFound", err)
	}
}

func TestInMemoryRepository_CreateCampaign_DuplicateSlug(t *testing.T) {
	t.Parallel()
	r := campaigns.NewInMemoryRepository()
	tenant := uuid.New()
	c1, _ := campaigns.NewCampaign(uuid.New(), tenant, "n1", "s1", "https://example.test/x", nil, time.Now())
	c2, _ := campaigns.NewCampaign(uuid.New(), tenant, "n2", "s1", "https://example.test/y", nil, time.Now())
	if err := r.CreateCampaign(context.Background(), c1); err != nil {
		t.Fatalf("CreateCampaign first: %v", err)
	}
	if err := r.CreateCampaign(context.Background(), c2); !errors.Is(err, campaigns.ErrSlugAlreadyExists) {
		t.Fatalf("CreateCampaign duplicate err = %v, want ErrSlugAlreadyExists", err)
	}
}

func TestInMemoryRepository_RecordClick_Idempotent(t *testing.T) {
	t.Parallel()
	r := campaigns.NewInMemoryRepository()
	tenant := uuid.New()
	cid := uuid.New()
	c1, err := campaigns.NewCampaignClick(uuid.New(), tenant, cid, "click-token-1", time.Now())
	if err != nil {
		t.Fatalf("NewCampaignClick: %v", err)
	}
	got1, err := r.RecordClick(context.Background(), c1)
	if err != nil {
		t.Fatalf("RecordClick first: %v", err)
	}
	c2, _ := campaigns.NewCampaignClick(uuid.New(), tenant, cid, "click-token-1", time.Now())
	got2, err := r.RecordClick(context.Background(), c2)
	if err != nil {
		t.Fatalf("RecordClick duplicate: %v", err)
	}
	if got1.ID != got2.ID {
		t.Fatalf("duplicate insert returned a different id: got %s, want %s", got2.ID, got1.ID)
	}
}

func TestInMemoryRepository_LinkContactToCampaign(t *testing.T) {
	t.Parallel()
	r := campaigns.NewInMemoryRepository()
	tenant := uuid.New()
	cid := uuid.New()
	c, _ := campaigns.NewCampaignClick(uuid.New(), tenant, cid, "click-Z", time.Now())
	if _, err := r.RecordClick(context.Background(), c); err != nil {
		t.Fatalf("RecordClick: %v", err)
	}
	contact := uuid.New()
	if err := r.LinkContactToCampaign(context.Background(), tenant, "click-Z", contact); err != nil {
		t.Fatalf("LinkContactToCampaign: %v", err)
	}
	if err := r.LinkContactToCampaign(context.Background(), tenant, "click-MISSING", contact); !errors.Is(err, campaigns.ErrNotFound) {
		t.Fatalf("missing click err = %v, want ErrNotFound", err)
	}
}

func TestInMemoryRepository_ListByTenant(t *testing.T) {
	t.Parallel()
	r := campaigns.NewInMemoryRepository()
	tenant := uuid.New()
	t0 := time.Date(2026, 5, 17, 0, 0, 0, 0, time.UTC)
	c1, _ := campaigns.NewCampaign(uuid.New(), tenant, "n1", "older", "https://example.test/x", nil, t0)
	c2, _ := campaigns.NewCampaign(uuid.New(), tenant, "n2", "newer", "https://example.test/y", nil, t0.Add(time.Hour))
	_ = r.CreateCampaign(context.Background(), c1)
	_ = r.CreateCampaign(context.Background(), c2)
	got, err := r.ListByTenant(context.Background(), tenant)
	if err != nil {
		t.Fatalf("ListByTenant: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("ListByTenant len = %d, want 2", len(got))
	}
	if got[0].ID != c2.ID {
		t.Fatalf("ListByTenant ordering: first = %s, want %s", got[0].ID, c2.ID)
	}
}

func TestInMemoryRepository_StatsByTenant(t *testing.T) {
	t.Parallel()
	r := campaigns.NewInMemoryRepository()
	tenant := uuid.New()
	c, _ := campaigns.NewCampaign(uuid.New(), tenant, "n", "promo", "https://example.test/x", nil, time.Now())
	_ = r.CreateCampaign(context.Background(), c)
	// Two clicks, one linked.
	c1, _ := campaigns.NewCampaignClick(uuid.New(), tenant, c.ID, "ck-1", time.Now())
	c2, _ := campaigns.NewCampaignClick(uuid.New(), tenant, c.ID, "ck-2", time.Now())
	_, _ = r.RecordClick(context.Background(), c1)
	_, _ = r.RecordClick(context.Background(), c2)
	_ = r.LinkContactToCampaign(context.Background(), tenant, "ck-1", uuid.New())

	stats, err := r.StatsByTenant(context.Background(), tenant)
	if err != nil {
		t.Fatalf("StatsByTenant: %v", err)
	}
	s := stats[c.ID]
	if s.Clicks != 2 {
		t.Errorf("Clicks = %d, want 2", s.Clicks)
	}
	if s.Attributions != 1 {
		t.Errorf("Attributions = %d, want 1", s.Attributions)
	}

	if _, err := r.StatsByTenant(context.Background(), uuid.Nil); !errors.Is(err, campaigns.ErrInvalidTenant) {
		t.Fatalf("StatsByTenant(nil) err = %v, want ErrInvalidTenant", err)
	}
}

func TestInMemoryRepository_ListClicks(t *testing.T) {
	t.Parallel()
	r := campaigns.NewInMemoryRepository()
	tenant := uuid.New()
	c, _ := campaigns.NewCampaign(uuid.New(), tenant, "n", "p", "https://example.test/x", nil, time.Now())
	_ = r.CreateCampaign(context.Background(), c)
	t0 := time.Date(2026, 5, 17, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 3; i++ {
		click, _ := campaigns.NewCampaignClick(uuid.New(), tenant, c.ID, "ck-"+strconv.Itoa(i), t0.Add(time.Duration(i)*time.Hour))
		_, _ = r.RecordClick(context.Background(), click)
	}
	// Also add a click for a different campaign — must not bleed in.
	other, _ := campaigns.NewCampaign(uuid.New(), tenant, "other", "other", "https://example.test/y", nil, time.Now())
	_ = r.CreateCampaign(context.Background(), other)
	otherClick, _ := campaigns.NewCampaignClick(uuid.New(), tenant, other.ID, "ck-other", time.Now())
	_, _ = r.RecordClick(context.Background(), otherClick)

	got, err := r.ListClicks(context.Background(), tenant, c.ID, 2)
	if err != nil {
		t.Fatalf("ListClicks: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("ListClicks len = %d, want 2 (limit)", len(got))
	}
	if got[0].ClickID != "ck-2" {
		t.Errorf("first ClickID = %q, want ck-2 (newest)", got[0].ClickID)
	}

	all, err := r.ListClicks(context.Background(), tenant, c.ID, 0)
	if err != nil {
		t.Fatalf("ListClicks (default limit): %v", err)
	}
	if len(all) != 3 {
		t.Errorf("ListClicks default limit returned %d, want 3", len(all))
	}

	if _, err := r.ListClicks(context.Background(), uuid.Nil, c.ID, 1); !errors.Is(err, campaigns.ErrInvalidTenant) {
		t.Fatalf("nil tenant err = %v, want ErrInvalidTenant", err)
	}
	if _, err := r.ListClicks(context.Background(), tenant, uuid.Nil, 1); !errors.Is(err, campaigns.ErrInvalidCampaign) {
		t.Fatalf("nil campaign err = %v, want ErrInvalidCampaign", err)
	}
}

func TestInMemoryRepository_DumpClicksForTest(t *testing.T) {
	t.Parallel()
	r := campaigns.NewInMemoryRepository()
	tenant := uuid.New()
	c, _ := campaigns.NewCampaign(uuid.New(), tenant, "n", "p", "https://example.test/x", nil, time.Now())
	_ = r.CreateCampaign(context.Background(), c)
	click, _ := campaigns.NewCampaignClick(uuid.New(), tenant, c.ID, "ck-1", time.Now())
	_, _ = r.RecordClick(context.Background(), click)

	dump := r.DumpClicksForTest()
	if len(dump) != 1 {
		t.Fatalf("DumpClicksForTest len = %d, want 1", len(dump))
	}
	if dump[0].ClickID != "ck-1" {
		t.Errorf("ClickID = %q, want ck-1", dump[0].ClickID)
	}
}

func TestInMemoryRepository_InvalidInputs(t *testing.T) {
	t.Parallel()
	r := campaigns.NewInMemoryRepository()
	if err := r.CreateCampaign(context.Background(), nil); err == nil {
		t.Fatal("CreateCampaign(nil) err = nil, want non-nil")
	}
	if _, err := r.GetBySlug(context.Background(), uuid.Nil, "x"); !errors.Is(err, campaigns.ErrInvalidTenant) {
		t.Fatalf("GetBySlug nil tenant err = %v, want ErrInvalidTenant", err)
	}
	if _, err := r.GetBySlug(context.Background(), uuid.New(), "INVALID!!"); !errors.Is(err, campaigns.ErrNotFound) {
		t.Fatalf("GetBySlug invalid slug err = %v, want ErrNotFound", err)
	}
	if _, err := r.RecordClick(context.Background(), nil); err == nil {
		t.Fatal("RecordClick(nil) err = nil, want non-nil")
	}
	if err := r.LinkContactToCampaign(context.Background(), uuid.Nil, "x", uuid.New()); !errors.Is(err, campaigns.ErrInvalidTenant) {
		t.Fatalf("LinkContactToCampaign nil tenant err = %v, want ErrInvalidTenant", err)
	}
	if err := r.LinkContactToCampaign(context.Background(), uuid.New(), "", uuid.New()); !errors.Is(err, campaigns.ErrInvalidClickID) {
		t.Fatalf("LinkContactToCampaign empty clickID err = %v, want ErrInvalidClickID", err)
	}
	if err := r.LinkContactToCampaign(context.Background(), uuid.New(), "c", uuid.Nil); !errors.Is(err, campaigns.ErrInvalidCampaign) {
		t.Fatalf("LinkContactToCampaign nil contact err = %v, want ErrInvalidCampaign", err)
	}
	if _, err := r.ListByTenant(context.Background(), uuid.Nil); !errors.Is(err, campaigns.ErrInvalidTenant) {
		t.Fatalf("ListByTenant nil tenant err = %v, want ErrInvalidTenant", err)
	}
}
