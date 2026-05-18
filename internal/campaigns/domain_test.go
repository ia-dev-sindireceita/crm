package campaigns_test

import (
	"errors"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/campaigns"
)

func TestStatus_Valid(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   campaigns.Status
		want bool
	}{
		{campaigns.StatusActive, true},
		{campaigns.StatusExpired, true},
		{campaigns.Status(""), false},
		{campaigns.Status("paused"), false},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(string(tc.in), func(t *testing.T) {
			t.Parallel()
			if got := tc.in.Valid(); got != tc.want {
				t.Fatalf("Valid(%q): got %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

func TestNewCampaign_Success(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)
	tenant := uuid.New()
	id := uuid.New()
	expires := now.Add(24 * time.Hour)

	c, err := campaigns.NewCampaign(id, tenant, "  Black Friday  ", "Black-Friday-2026", "https://example.test/promo", &expires, now)
	if err != nil {
		t.Fatalf("NewCampaign: %v", err)
	}
	if c.ID != id {
		t.Errorf("id: got %s, want %s", c.ID, id)
	}
	if c.TenantID != tenant {
		t.Errorf("tenant: got %s, want %s", c.TenantID, tenant)
	}
	if c.Name != "Black Friday" {
		t.Errorf("name: got %q, want trimmed", c.Name)
	}
	if c.Slug != "black-friday-2026" {
		t.Errorf("slug: got %q, want lowercased", c.Slug)
	}
	if c.Status != campaigns.StatusActive {
		t.Errorf("status: got %s, want active", c.Status)
	}
	if !c.CreatedAt.Equal(now) || !c.UpdatedAt.Equal(now) {
		t.Errorf("timestamps: got %s/%s, want %s", c.CreatedAt, c.UpdatedAt, now)
	}
	if c.ExpiresAt == nil || !c.ExpiresAt.Equal(expires) {
		t.Errorf("expiresAt: got %v, want %v", c.ExpiresAt, expires)
	}
}

func TestNewCampaign_DefaultsIDAndClock(t *testing.T) {
	t.Parallel()
	tenant := uuid.New()
	c, err := campaigns.NewCampaign(uuid.Nil, tenant, "Promo", "promo", "https://example.test", nil, time.Time{})
	if err != nil {
		t.Fatalf("NewCampaign: %v", err)
	}
	if c.ID == uuid.Nil {
		t.Error("expected generated ID, got Nil")
	}
	if c.CreatedAt.IsZero() {
		t.Error("expected stamped CreatedAt, got zero")
	}
	if c.ExpiresAt != nil {
		t.Errorf("expected nil ExpiresAt for evergreen campaign, got %v", c.ExpiresAt)
	}
}

func TestNewCampaign_ValidationErrors(t *testing.T) {
	t.Parallel()
	tenant := uuid.New()
	now := time.Now().UTC()

	cases := []struct {
		name      string
		tenant    uuid.UUID
		cName     string
		slug      string
		redirect  string
		wantErrIs error
	}{
		{"nil tenant", uuid.Nil, "Promo", "promo", "https://example.test", campaigns.ErrInvalidTenant},
		{"blank name", tenant, "   ", "promo", "https://example.test", campaigns.ErrInvalidName},
		{"blank slug", tenant, "Promo", "  ", "https://example.test", campaigns.ErrInvalidSlug},
		{"slug bad chars", tenant, "Promo", "promo_2026", "https://example.test", campaigns.ErrInvalidSlug},
		{"slug leading hyphen", tenant, "Promo", "-promo", "https://example.test", campaigns.ErrInvalidSlug},
		{"slug trailing hyphen", tenant, "Promo", "promo-", "https://example.test", campaigns.ErrInvalidSlug},
		{"redirect blank", tenant, "Promo", "promo", " ", campaigns.ErrInvalidRedirectURL},
		{"redirect javascript", tenant, "Promo", "promo", "javascript:alert(1)", campaigns.ErrInvalidRedirectURL},
		{"redirect data", tenant, "Promo", "promo", "data:text/html,<script>", campaigns.ErrInvalidRedirectURL},
		{"redirect missing host", tenant, "Promo", "promo", "https://", campaigns.ErrInvalidRedirectURL},
		{"redirect control char", tenant, "Promo", "promo", "https://example.test/\x00", campaigns.ErrInvalidRedirectURL},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := campaigns.NewCampaign(uuid.New(), tc.tenant, tc.cName, tc.slug, tc.redirect, nil, now)
			if !errors.Is(err, tc.wantErrIs) {
				t.Fatalf("got %v, want errors.Is(%v)", err, tc.wantErrIs)
			}
		})
	}
}

func TestCampaign_IsExpired(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 5, 17, 0, 0, 0, 0, time.UTC)
	expires := base.Add(24 * time.Hour)

	c := &campaigns.Campaign{ExpiresAt: &expires}
	if c.IsExpired(base) {
		t.Fatal("now < expires: should NOT be expired")
	}
	if c.IsExpired(base.Add(23*time.Hour + 59*time.Minute)) {
		t.Fatal("now < expires: should NOT be expired")
	}
	if !c.IsExpired(expires) {
		t.Fatal("now == expires: should be expired (>= boundary)")
	}
	if !c.IsExpired(expires.Add(time.Second)) {
		t.Fatal("now > expires: should be expired")
	}

	evergreen := &campaigns.Campaign{ExpiresAt: nil}
	if evergreen.IsExpired(time.Date(3000, 1, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatal("evergreen campaign should never expire")
	}
}

func TestCampaign_WithUTM(t *testing.T) {
	t.Parallel()
	tenant := uuid.New()
	c, err := campaigns.NewCampaign(uuid.New(), tenant, "Promo", "promo", "https://example.test", nil, time.Now())
	if err != nil {
		t.Fatalf("NewCampaign: %v", err)
	}
	got := c.WithUTM("google", "cpc", "summer", "shoes", "ad-1")
	if got != c {
		t.Error("WithUTM should return the same pointer for chaining")
	}
	if c.UTMSource != "google" || c.UTMMedium != "cpc" || c.UTMCampaign != "summer" || c.UTMTerm != "shoes" || c.UTMContent != "ad-1" {
		t.Errorf("UTM fields not set: %+v", c)
	}
	c.WithUTM("", "", "", "", "")
	if c.UTMSource != "" || c.UTMMedium != "" {
		t.Error("WithUTM with empty strings should clear fields")
	}
}

func TestNormalizeSlug(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in        string
		want      string
		wantErrIs error
	}{
		{"BlackFriday", "blackfriday", nil},
		{"  Promo-2026  ", "promo-2026", nil},
		{"summer-sale", "summer-sale", nil},
		{"123", "123", nil},
		{"", "", campaigns.ErrInvalidSlug},
		{"   ", "", campaigns.ErrInvalidSlug},
		{"with space", "", campaigns.ErrInvalidSlug},
		{"with.dot", "", campaigns.ErrInvalidSlug},
		{"with_underscore", "", campaigns.ErrInvalidSlug},
		{"emoji-😀", "", campaigns.ErrInvalidSlug},
		{"-leading", "", campaigns.ErrInvalidSlug},
		{"trailing-", "", campaigns.ErrInvalidSlug},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(strings.ReplaceAll(tc.in, " ", "_"), func(t *testing.T) {
			t.Parallel()
			got, err := campaigns.NormalizeSlug(tc.in)
			if tc.wantErrIs != nil {
				if !errors.Is(err, tc.wantErrIs) {
					t.Fatalf("err: got %v, want errors.Is(%v)", err, tc.wantErrIs)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestNewCampaignClick_Success(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)
	tenant := uuid.New()
	campaign := uuid.New()
	id := uuid.New()

	click, err := campaigns.NewCampaignClick(id, tenant, campaign, "  click-abc-123  ", now)
	if err != nil {
		t.Fatalf("NewCampaignClick: %v", err)
	}
	if click.ClickID != "click-abc-123" {
		t.Errorf("clickID should be trimmed: got %q", click.ClickID)
	}
	if click.TenantID != tenant || click.CampaignID != campaign || click.ID != id {
		t.Errorf("ids not set: %+v", click)
	}
	if click.Meta == nil {
		t.Error("Meta should default to empty map, got nil")
	}
	if !click.CreatedAt.Equal(now) {
		t.Errorf("createdAt: got %v, want %v", click.CreatedAt, now)
	}
	if click.IP.IsValid() {
		t.Errorf("IP should default to zero netip.Addr, got %v", click.IP)
	}
	if click.ContactID != nil {
		t.Errorf("ContactID should default to nil, got %v", click.ContactID)
	}
}

func TestNewCampaignClick_DefaultsIDAndClock(t *testing.T) {
	t.Parallel()
	click, err := campaigns.NewCampaignClick(uuid.Nil, uuid.New(), uuid.New(), "click", time.Time{})
	if err != nil {
		t.Fatalf("NewCampaignClick: %v", err)
	}
	if click.ID == uuid.Nil {
		t.Error("expected generated id")
	}
	if click.CreatedAt.IsZero() {
		t.Error("expected stamped createdAt")
	}
}

func TestNewCampaignClick_ValidationErrors(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	cases := []struct {
		name      string
		tenant    uuid.UUID
		campaign  uuid.UUID
		clickID   string
		wantErrIs error
	}{
		{"nil tenant", uuid.Nil, uuid.New(), "click", campaigns.ErrInvalidTenant},
		{"nil campaign", uuid.New(), uuid.Nil, "click", campaigns.ErrInvalidCampaign},
		{"blank clickID", uuid.New(), uuid.New(), "", campaigns.ErrInvalidClickID},
		{"whitespace clickID", uuid.New(), uuid.New(), "   ", campaigns.ErrInvalidClickID},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := campaigns.NewCampaignClick(uuid.New(), tc.tenant, tc.campaign, tc.clickID, now)
			if !errors.Is(err, tc.wantErrIs) {
				t.Fatalf("got %v, want errors.Is(%v)", err, tc.wantErrIs)
			}
		})
	}
}

func TestCampaignClick_AcceptsValidIPAndContact(t *testing.T) {
	t.Parallel()
	tenant := uuid.New()
	campaign := uuid.New()
	contact := uuid.New()

	click, err := campaigns.NewCampaignClick(uuid.New(), tenant, campaign, "click", time.Now())
	if err != nil {
		t.Fatalf("NewCampaignClick: %v", err)
	}
	click.IP = netip.MustParseAddr("203.0.113.7")
	click.ContactID = &contact
	click.UserAgent = "Mozilla/5.0"
	click.Referrer = "https://example.test/landing"
	click.Meta["geoip"] = "BR"

	if !click.IP.IsValid() {
		t.Error("expected valid IP")
	}
	if click.ContactID == nil || *click.ContactID != contact {
		t.Error("expected contact assignment to take")
	}
	if got := click.Meta["geoip"]; got != "BR" {
		t.Errorf("meta.geoip: got %v, want BR", got)
	}
}
