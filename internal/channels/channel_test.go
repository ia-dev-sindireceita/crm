package channels

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
)

// withFrozenClock pins the package clock for the duration of fn and
// restores it afterwards, so timestamp assertions are deterministic.
func withFrozenClock(t *testing.T, at time.Time, fn func()) {
	t.Helper()
	orig := now
	now = func() time.Time { return at }
	defer func() { now = orig }()
	fn()
}

func TestNew(t *testing.T) {
	tenant := uuid.New()
	frozen := time.Date(2026, 6, 30, 12, 0, 0, 0, time.UTC)

	tests := []struct {
		name        string
		tenantID    uuid.UUID
		channelKey  string
		externalID  string
		displayName string
		wantErr     error
		wantKey     string
		wantExt     string
		wantDisplay string
	}{
		{
			name:        "valid full",
			tenantID:    tenant,
			channelKey:  "whatsapp",
			externalID:  "+5511999998888",
			displayName: "Suporte WA",
			wantKey:     "whatsapp",
			wantExt:     "+5511999998888",
			wantDisplay: "Suporte WA",
		},
		{
			name:        "channel key lower-cased and trimmed",
			tenantID:    tenant,
			channelKey:  "  WhatsApp ",
			externalID:  "  +55 ",
			displayName: "  Main  ",
			wantKey:     "whatsapp",
			wantExt:     "+55",
			wantDisplay: "Main",
		},
		{
			name:        "blank display name defaults to channel key",
			tenantID:    tenant,
			channelKey:  "instagram",
			externalID:  "@acme",
			displayName: "   ",
			wantKey:     "instagram",
			wantExt:     "@acme",
			wantDisplay: "instagram",
		},
		{
			name:        "empty external id allowed (placeholder)",
			tenantID:    tenant,
			channelKey:  "whatsapp",
			externalID:  "",
			displayName: "Legacy",
			wantKey:     "whatsapp",
			wantExt:     "",
			wantDisplay: "Legacy",
		},
		{
			name:       "nil tenant rejected",
			tenantID:   uuid.Nil,
			channelKey: "whatsapp",
			wantErr:    ErrInvalidTenant,
		},
		{
			name:       "empty channel key rejected",
			tenantID:   tenant,
			channelKey: "   ",
			wantErr:    ErrInvalidChannelKey,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var got *Channel
			var err error
			withFrozenClock(t, frozen, func() {
				got, err = New(tc.tenantID, tc.channelKey, tc.externalID, tc.displayName)
			})
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("New err = %v, want %v", err, tc.wantErr)
				}
				if got != nil {
					t.Fatalf("New returned non-nil channel on error: %+v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("New unexpected err: %v", err)
			}
			if got.ID == uuid.Nil {
				t.Error("New left ID nil")
			}
			if got.TenantID != tc.tenantID {
				t.Errorf("TenantID = %v, want %v", got.TenantID, tc.tenantID)
			}
			if got.ChannelKey != tc.wantKey {
				t.Errorf("ChannelKey = %q, want %q", got.ChannelKey, tc.wantKey)
			}
			if got.ExternalID != tc.wantExt {
				t.Errorf("ExternalID = %q, want %q", got.ExternalID, tc.wantExt)
			}
			if got.DisplayName != tc.wantDisplay {
				t.Errorf("DisplayName = %q, want %q", got.DisplayName, tc.wantDisplay)
			}
			if !got.IsActive {
				t.Error("New channel should be active")
			}
			if got.Restricted {
				t.Error("New channel should be unrestricted")
			}
			if !got.CreatedAt.Equal(frozen) {
				t.Errorf("CreatedAt = %v, want %v", got.CreatedAt, frozen)
			}
		})
	}
}

func TestHydrate(t *testing.T) {
	id := uuid.New()
	tenant := uuid.New()
	created := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

	c := Hydrate(id, tenant, "whatsapp", "+55", "Vendas", false, true, created)
	if c.ID != id || c.TenantID != tenant {
		t.Fatalf("Hydrate ids mismatch: %+v", c)
	}
	if c.ChannelKey != "whatsapp" || c.ExternalID != "+55" || c.DisplayName != "Vendas" {
		t.Errorf("Hydrate fields mismatch: %+v", c)
	}
	if c.IsActive {
		t.Error("Hydrate should preserve isActive=false")
	}
	if !c.Restricted {
		t.Error("Hydrate should preserve restricted=true")
	}
	if !c.CreatedAt.Equal(created) {
		t.Errorf("CreatedAt = %v, want %v", c.CreatedAt, created)
	}
}

func TestChannel_Rename(t *testing.T) {
	tenant := uuid.New()

	t.Run("valid rename trims", func(t *testing.T) {
		c, _ := New(tenant, "whatsapp", "+55", "Old")
		if err := c.Rename("  New Name  "); err != nil {
			t.Fatalf("Rename err: %v", err)
		}
		if c.DisplayName != "New Name" {
			t.Errorf("DisplayName = %q, want %q", c.DisplayName, "New Name")
		}
	})

	t.Run("blank rejected, aggregate untouched", func(t *testing.T) {
		c, _ := New(tenant, "whatsapp", "+55", "Keep")
		if err := c.Rename("   "); !errors.Is(err, ErrEmptyDisplayName) {
			t.Fatalf("Rename err = %v, want %v", err, ErrEmptyDisplayName)
		}
		if c.DisplayName != "Keep" {
			t.Errorf("DisplayName mutated to %q after rejected rename", c.DisplayName)
		}
	})
}

func TestChannel_SetActiveAndRestricted(t *testing.T) {
	tenant := uuid.New()
	c, _ := New(tenant, "whatsapp", "+55", "X")

	c.SetActive(false)
	if c.IsActive {
		t.Error("SetActive(false) did not deactivate")
	}
	c.SetActive(true)
	if !c.IsActive {
		t.Error("SetActive(true) did not reactivate")
	}

	c.SetRestricted(true)
	if !c.Restricted {
		t.Error("SetRestricted(true) did not set restricted")
	}
	c.SetRestricted(false)
	if c.Restricted {
		t.Error("SetRestricted(false) did not clear restricted")
	}
}
