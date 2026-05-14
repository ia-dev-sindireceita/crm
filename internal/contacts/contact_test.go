package contacts

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestNew_TableDriven(t *testing.T) {
	tenant := uuid.New()
	tests := []struct {
		name        string
		tenantID    uuid.UUID
		displayName string
		wantErr     error
	}{
		{name: "happy", tenantID: tenant, displayName: "Alice"},
		{name: "trims whitespace", tenantID: tenant, displayName: "  Bob  "},
		{name: "rejects nil tenant", tenantID: uuid.Nil, displayName: "Alice", wantErr: ErrInvalidTenant},
		{name: "rejects empty name", tenantID: tenant, displayName: "", wantErr: ErrEmptyDisplayName},
		{name: "rejects whitespace-only name", tenantID: tenant, displayName: "   \t  ", wantErr: ErrEmptyDisplayName},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c, err := New(tc.tenantID, tc.displayName)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("err = %v, want %v", err, tc.wantErr)
				}
				if c != nil {
					t.Fatalf("contact = %+v, want nil on error", c)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if c.ID == uuid.Nil {
				t.Error("ID is uuid.Nil")
			}
			if c.TenantID != tc.tenantID {
				t.Errorf("TenantID = %s, want %s", c.TenantID, tc.tenantID)
			}
			// Display name is trimmed.
			if c.DisplayName == "" || c.DisplayName[0] == ' ' || c.DisplayName[len(c.DisplayName)-1] == ' ' {
				t.Errorf("DisplayName not trimmed: %q", c.DisplayName)
			}
			if c.CreatedAt.IsZero() {
				t.Error("CreatedAt is zero")
			}
			if !c.CreatedAt.Equal(c.UpdatedAt) {
				t.Errorf("CreatedAt %v != UpdatedAt %v at construction", c.CreatedAt, c.UpdatedAt)
			}
			if len(c.Identities()) != 0 {
				t.Errorf("new contact has %d identities, want 0", len(c.Identities()))
			}
		})
	}
}

func TestNew_GeneratesDistinctIDs(t *testing.T) {
	tenant := uuid.New()
	a, err := New(tenant, "A")
	if err != nil {
		t.Fatalf("a: %v", err)
	}
	b, err := New(tenant, "B")
	if err != nil {
		t.Fatalf("b: %v", err)
	}
	if a.ID == b.ID {
		t.Errorf("expected distinct ids, got %s twice", a.ID)
	}
}

func TestAddChannelIdentity_HappyPath(t *testing.T) {
	c := mustNewContact(t)
	originalUpdate := c.UpdatedAt
	time.Sleep(time.Microsecond) // make sure now() advances

	if err := c.AddChannelIdentity(ChannelWhatsApp, "+5511999990001"); err != nil {
		t.Fatalf("AddChannelIdentity: %v", err)
	}
	ids := c.Identities()
	if len(ids) != 1 {
		t.Fatalf("Identities len = %d, want 1", len(ids))
	}
	if ids[0].Channel != ChannelWhatsApp {
		t.Errorf("Channel = %q, want %q", ids[0].Channel, ChannelWhatsApp)
	}
	if ids[0].ExternalID != "+5511999990001" {
		t.Errorf("ExternalID = %q", ids[0].ExternalID)
	}
	if !c.UpdatedAt.After(originalUpdate) {
		t.Errorf("UpdatedAt did not advance: was %v, now %v", originalUpdate, c.UpdatedAt)
	}
}

func TestAddChannelIdentity_RejectsSecondOnSameChannel(t *testing.T) {
	c := mustNewContact(t)
	if err := c.AddChannelIdentity(ChannelWhatsApp, "+5511999990001"); err != nil {
		t.Fatalf("first add: %v", err)
	}
	err := c.AddChannelIdentity(ChannelWhatsApp, "+5511999990002")
	if !errors.Is(err, ErrChannelIdentityExists) {
		t.Fatalf("second add err = %v, want %v", err, ErrChannelIdentityExists)
	}
	if got := len(c.Identities()); got != 1 {
		t.Errorf("identity count = %d, want 1 after rejected second add", got)
	}
}

func TestAddChannelIdentity_AllowsDifferentChannels(t *testing.T) {
	c := mustNewContact(t)
	if err := c.AddChannelIdentity(ChannelWhatsApp, "+5511999990001"); err != nil {
		t.Fatalf("whatsapp: %v", err)
	}
	if err := c.AddChannelIdentity("email", "alice@example.com"); err != nil {
		t.Fatalf("email: %v", err)
	}
	if got := len(c.Identities()); got != 2 {
		t.Errorf("identity count = %d, want 2", got)
	}
}

func TestAddChannelIdentity_NormalisedChannelStillCollides(t *testing.T) {
	c := mustNewContact(t)
	if err := c.AddChannelIdentity("whatsapp", "+5511999990001"); err != nil {
		t.Fatalf("first: %v", err)
	}
	// "WhatsApp" normalises to "whatsapp" — must be rejected.
	err := c.AddChannelIdentity("WhatsApp", "+5511999990002")
	if !errors.Is(err, ErrChannelIdentityExists) {
		t.Errorf("case-variant err = %v, want ErrChannelIdentityExists", err)
	}
}

func TestAddChannelIdentity_PropagatesValidationError(t *testing.T) {
	c := mustNewContact(t)
	err := c.AddChannelIdentity(ChannelWhatsApp, "not-e164")
	if !errors.Is(err, ErrInvalidE164) {
		t.Errorf("err = %v, want ErrInvalidE164", err)
	}
}

func TestIdentities_IsDefensiveCopy(t *testing.T) {
	c := mustNewContact(t)
	if err := c.AddChannelIdentity(ChannelWhatsApp, "+5511999990001"); err != nil {
		t.Fatalf("add: %v", err)
	}
	ids := c.Identities()
	ids[0].Channel = "tampered"
	if c.Identities()[0].Channel != ChannelWhatsApp {
		t.Errorf("internal slice mutated through returned copy")
	}
}

func TestHasChannel(t *testing.T) {
	c := mustNewContact(t)
	if err := c.AddChannelIdentity(ChannelWhatsApp, "+5511999990001"); err != nil {
		t.Fatalf("add: %v", err)
	}
	if !c.HasChannel("whatsapp") {
		t.Error("HasChannel(whatsapp) = false")
	}
	if !c.HasChannel(" WhatsApp ") {
		t.Error("HasChannel(' WhatsApp ') = false (should normalise)")
	}
	if c.HasChannel("email") {
		t.Error("HasChannel(email) = true (no such identity)")
	}
}

func TestHydrate(t *testing.T) {
	id := uuid.New()
	tenant := uuid.New()
	created := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	updated := created.Add(time.Hour)
	ids := []ChannelIdentity{
		{Channel: "whatsapp", ExternalID: "+5511999990001"},
		{Channel: "email", ExternalID: "a@b.c"},
	}
	c := Hydrate(id, tenant, "Alice", ids, created, updated)
	if c.ID != id {
		t.Errorf("ID = %s, want %s", c.ID, id)
	}
	if c.TenantID != tenant {
		t.Errorf("TenantID mismatch")
	}
	if len(c.Identities()) != 2 {
		t.Errorf("Identities len = %d, want 2", len(c.Identities()))
	}
	if !c.CreatedAt.Equal(created) {
		t.Errorf("CreatedAt drifted")
	}
	if !c.UpdatedAt.Equal(updated) {
		t.Errorf("UpdatedAt drifted")
	}
}

func TestHydrate_HandlesNilIdentities(t *testing.T) {
	c := Hydrate(uuid.New(), uuid.New(), "Alice", nil, time.Now(), time.Now())
	if got := len(c.Identities()); got != 0 {
		t.Errorf("Identities len = %d, want 0", got)
	}
	if c.HasChannel("whatsapp") {
		t.Errorf("HasChannel = true on hydrated empty contact")
	}
}

func mustNewContact(t *testing.T) *Contact {
	t.Helper()
	c, err := New(uuid.New(), "Alice")
	if err != nil {
		t.Fatalf("seed contact: %v", err)
	}
	return c
}
