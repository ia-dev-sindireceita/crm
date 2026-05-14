package contacts

import "strings"

// ChannelWhatsApp is the canonical channel name for WhatsApp identities.
// The webhook receiver (PR6) writes this exact string; the dedup ledger
// (migration 0088) keys on it; the inbox renderer (PR4) reads it. Keeping
// the constant in the domain package avoids any of those layers
// drifting to "wa" / "WhatsApp" / "whatsApp" and silently splitting the
// (channel, external_id) UNIQUE index.
const ChannelWhatsApp = "whatsapp"

// e164MaxDigits is the ITU-T E.164 cap: a country code plus subscriber
// number MUST NOT exceed 15 digits. With the leading '+' the string is
// at most 16 bytes.
const e164MaxDigits = 15

// ChannelIdentity is a value object that pairs a channel with the
// external identifier the carrier uses to address the contact on that
// channel — e.g. ("whatsapp", "+5511999990001") or ("email",
// "alice@example.com"). Construct via NewChannelIdentity so the
// normalisation and E.164 check apply uniformly.
type ChannelIdentity struct {
	// Channel is the normalised (lower-case, trimmed) channel name.
	Channel string
	// ExternalID is the carrier-side identifier, trimmed of surrounding
	// whitespace. For WhatsApp this is the sender's phone number in
	// strict E.164 form.
	ExternalID string
}

// NewChannelIdentity normalises channel + externalID and applies the
// per-channel validation rules. For "whatsapp" the external id MUST be
// a valid E.164 number; other channels currently have no shape
// constraint beyond non-empty.
func NewChannelIdentity(channel, externalID string) (ChannelIdentity, error) {
	channel = strings.ToLower(strings.TrimSpace(channel))
	externalID = strings.TrimSpace(externalID)
	if channel == "" {
		return ChannelIdentity{}, ErrInvalidChannel
	}
	if externalID == "" {
		return ChannelIdentity{}, ErrInvalidExternalID
	}
	if channel == ChannelWhatsApp && !isE164(externalID) {
		return ChannelIdentity{}, ErrInvalidE164
	}
	return ChannelIdentity{Channel: channel, ExternalID: externalID}, nil
}

// isE164 reports whether s matches the ITU-T E.164 shape: a leading '+',
// followed by 1..15 decimal digits, with no leading zero on the country
// code. We deliberately do NOT validate country-code allocation — the
// recommendation E.164 list changes faster than this code will, and a
// stricter check belongs in a per-country library if we ever need it.
func isE164(s string) bool {
	if len(s) < 2 || len(s) > 1+e164MaxDigits {
		return false
	}
	if s[0] != '+' {
		return false
	}
	if s[1] == '0' {
		return false
	}
	for i := 1; i < len(s); i++ {
		c := s[i]
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}
