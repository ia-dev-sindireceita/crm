package contacts

import (
	"errors"
	"strings"
	"testing"
)

func TestNewChannelIdentity_HappyPaths(t *testing.T) {
	tests := []struct {
		name           string
		channel        string
		externalID     string
		wantChannel    string
		wantExternalID string
	}{
		{name: "whatsapp E.164 BR", channel: "whatsapp", externalID: "+5511999990001", wantChannel: "whatsapp", wantExternalID: "+5511999990001"},
		{name: "whatsapp E.164 US", channel: "whatsapp", externalID: "+12025550100", wantChannel: "whatsapp", wantExternalID: "+12025550100"},
		{name: "whatsapp minimum E.164 (2 chars)", channel: "whatsapp", externalID: "+1", wantChannel: "whatsapp", wantExternalID: "+1"},
		{name: "whatsapp max length 15 digits", channel: "whatsapp", externalID: "+123456789012345", wantChannel: "whatsapp", wantExternalID: "+123456789012345"},
		{name: "uppercase channel normalised", channel: "WhatsApp", externalID: "+5511999990001", wantChannel: "whatsapp", wantExternalID: "+5511999990001"},
		{name: "whitespace trimmed", channel: "  whatsapp  ", externalID: "  +5511999990001  ", wantChannel: "whatsapp", wantExternalID: "+5511999990001"},
		{name: "email no E.164 check", channel: "email", externalID: "alice@example.com", wantChannel: "email", wantExternalID: "alice@example.com"},
		{name: "telegram username", channel: "telegram", externalID: "@alice", wantChannel: "telegram", wantExternalID: "@alice"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			id, err := NewChannelIdentity(tc.channel, tc.externalID)
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if id.Channel != tc.wantChannel {
				t.Errorf("Channel = %q, want %q", id.Channel, tc.wantChannel)
			}
			if id.ExternalID != tc.wantExternalID {
				t.Errorf("ExternalID = %q, want %q", id.ExternalID, tc.wantExternalID)
			}
		})
	}
}

func TestNewChannelIdentity_RejectsInvalid(t *testing.T) {
	tests := []struct {
		name       string
		channel    string
		externalID string
		wantErr    error
	}{
		{name: "empty channel", channel: "", externalID: "x", wantErr: ErrInvalidChannel},
		{name: "whitespace-only channel", channel: "   ", externalID: "x", wantErr: ErrInvalidChannel},
		{name: "empty externalID", channel: "whatsapp", externalID: "", wantErr: ErrInvalidExternalID},
		{name: "whitespace-only externalID", channel: "whatsapp", externalID: "   ", wantErr: ErrInvalidExternalID},
		{name: "whatsapp without +", channel: "whatsapp", externalID: "5511999990001", wantErr: ErrInvalidE164},
		{name: "whatsapp with letters", channel: "whatsapp", externalID: "+551199999000A", wantErr: ErrInvalidE164},
		{name: "whatsapp leading zero", channel: "whatsapp", externalID: "+05511999990001", wantErr: ErrInvalidE164},
		{name: "whatsapp too long (16 digits)", channel: "whatsapp", externalID: "+1234567890123456", wantErr: ErrInvalidE164},
		{name: "whatsapp only + (no digits)", channel: "whatsapp", externalID: "+", wantErr: ErrInvalidE164},
		{name: "whatsapp with spaces inside", channel: "whatsapp", externalID: "+55 11 99999 0001", wantErr: ErrInvalidE164},
		{name: "whatsapp with hyphens", channel: "whatsapp", externalID: "+55-11-99999-0001", wantErr: ErrInvalidE164},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			id, err := NewChannelIdentity(tc.channel, tc.externalID)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v, want %v", err, tc.wantErr)
			}
			if id != (ChannelIdentity{}) {
				t.Errorf("identity = %+v, want zero value", id)
			}
		})
	}
}

func TestNewChannelIdentity_E164OnlyEnforcedForWhatsApp(t *testing.T) {
	// "sms" carries phone numbers but the spec only requires E.164 for
	// "whatsapp". Confirm sms accepts a non-E.164 string today; if the
	// rule expands to sms later, this test must fail loudly so the
	// surrounding code is reviewed.
	if _, err := NewChannelIdentity("sms", "not-e164"); err != nil {
		t.Errorf("sms with non-E.164 err = %v, want nil (E.164 only enforced for whatsapp)", err)
	}
}

func TestIsE164_Boundary(t *testing.T) {
	tests := map[string]bool{
		"":                  false,
		"+":                 false,
		"+1":                true,
		"1":                 false,
		"+1234567890123456": false, // 16 digits
		"+123456789012345":  true,  // 15 digits
		"+0123456789":       false, // leading zero
	}
	for input, want := range tests {
		if got := isE164(input); got != want {
			t.Errorf("isE164(%q) = %v, want %v", input, got, want)
		}
	}
}

// Sanity check that the ChannelWhatsApp constant matches the
// migration-side string literal in 0088_inbox_contacts.up.sql tests.
func TestChannelWhatsAppMatchesMigrationLiteral(t *testing.T) {
	if !strings.EqualFold(ChannelWhatsApp, "whatsapp") {
		t.Errorf("ChannelWhatsApp = %q, want 'whatsapp'", ChannelWhatsApp)
	}
}
