package campaigns

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"regexp"

	"github.com/google/uuid"
)

// SIN-62982 — attribution marker integrity.
//
// The redirect handler (internal/web/public/campaign) injects an
// attribution marker into the pre-filled message the visitor sends back
// over WhatsApp / Telegram so the inbox-side hook
// (internal/inbox/usecase.linkContactToCampaign) can map the inbound
// message to the click row that introduced the contact.
//
// On-the-wire format:
//
//	[crm:<click_id>]                — legacy, unsigned (pre-SIN-62982)
//	[crm:<click_id>.<hmac8>]        — signed (SIN-62982+)
//
// hmac8 is the first 8 lowercase-hex chars (32 bits) of
//   HMAC-SHA256(MarkerKey, tenantID.String() + ":" + clickID)
//
// 32 bits is intentional. The threat model (issue SIN-62982) is
// medium-severity attribution forgery: an attacker who somehow learns a
// click_id forges `[crm:<id>]` to misattribute a campaign. The compound
// difficulty is (guess valid click_id ≈ uuid-v4 entropy) AND (forge 32
// bits of HMAC). With rate-limiting on the inbound carrier surface the
// expected time-to-forge is dominated by the uuid guess, so 8 hex chars
// keeps the marker short enough to fit inside a templated WhatsApp
// message without paying noticeable extra collision risk.
//
// Per-tenant semantics come from including the tenantID in the HMAC
// input: two tenants signing the same click_id produce distinct HMACs,
// so a marker minted for tenant A cannot be replayed as tenant B.
//
// Compat / migration: parse accepts both formats. VerifyClickToken
// honours an allowLegacy flag that production wiring leaves true for
// the 90-day cookie TTL window (the longest a pre-rollout marker can
// remain in flight). Once the window has elapsed a follow-up flips the
// flag to false to retire the legacy form.

// MarkerKey is the keyed HMAC secret used to sign attribution markers.
// Length is unrestricted at this layer (the composition root enforces
// the minimum on env load) so unit tests can use short fixtures.
type MarkerKey []byte

// HasValue reports whether the key has signing material. A zero-value
// MarkerKey collapses Build/Verify into a no-op compatible with the
// legacy unsigned format — used by dev wires that have not configured
// the key yet.
func (k MarkerKey) HasValue() bool { return len(k) > 0 }

// markerHMACLength is the length of the truncated hex signature.
const markerHMACLength = 8

// markerHMACByteLength is the binary length matching markerHMACLength.
const markerHMACByteLength = markerHMACLength / 2

// clickTokenRE captures the click_id and the optional hmac8 suffix
// embedded inside an attribution marker. The click_id alphabet matches
// the pre-SIN-62982 contract (hex+hyphen, 8–128 chars) so legacy markers
// minted before this change parse identically. The optional non-capture
// `(?:\.(...))?` keeps the suffix optional during the compat window.
var clickTokenRE = regexp.MustCompile(`\[crm:([A-Za-z0-9-]{8,128})(?:\.([0-9a-f]{8}))?\]`)

// BuildClickToken returns the substring that goes inside the marker
// brackets (`[crm:<TOKEN>]`). When key has no value, returns the bare
// click_id; otherwise returns `<click_id>.<hmac8>`.
//
// Callers MUST treat the returned string as opaque — the only consumer
// is the redirect handler's expandRedirect, which percent-encodes the
// token before substituting it into the campaign's redirect_url. The
// dot separator survives url.QueryEscape unchanged (RFC 3986 unreserved).
func BuildClickToken(key MarkerKey, tenantID uuid.UUID, clickID string) string {
	if !key.HasValue() {
		return clickID
	}
	return clickID + "." + computeMarkerHMACHex(key, tenantID, clickID)
}

// ParsedClickMarker is the structured result of ExtractClickMarker. The
// HMACHex field is empty for legacy markers (no `.<hmac8>` suffix) and
// for non-matches; Found discriminates the two.
type ParsedClickMarker struct {
	ClickID string
	HMACHex string
	Found   bool
}

// ExtractClickMarker parses the first attribution marker in body. The
// regex accepts both legacy `[crm:<click_id>]` and signed
// `[crm:<click_id>.<hmac8>]` forms; HMACHex is empty for the legacy
// case. A body without any marker returns Found=false.
//
// This is the structured counterpart to ExtractClickID; the latter
// exists for backwards compatibility with pre-SIN-62982 callers that
// only need the click_id half.
func ExtractClickMarker(body string) ParsedClickMarker {
	m := clickTokenRE.FindStringSubmatch(body)
	if len(m) < 2 {
		return ParsedClickMarker{}
	}
	return ParsedClickMarker{ClickID: m[1], HMACHex: m[2], Found: true}
}

// VerifyClickToken verifies the HMAC suffix of a parsed marker.
//
//   - When suppliedHMACHex is empty the marker is in the legacy
//     unsigned form. Returns the value of allowLegacy — true during the
//     compat window, false once the rollout has aged past the 90-day
//     cookie TTL.
//   - When suppliedHMACHex is non-empty but the key is unset the
//     verifier fails closed (a wire that loses its key in mid-rollout
//     cannot validate suffixed markers minted by sibling processes that
//     still have it).
//   - Otherwise the HMAC is recomputed and compared in constant time.
func VerifyClickToken(key MarkerKey, allowLegacy bool, tenantID uuid.UUID, clickID, suppliedHMACHex string) bool {
	if suppliedHMACHex == "" {
		return allowLegacy
	}
	if !key.HasValue() {
		return false
	}
	supplied, err := hex.DecodeString(suppliedHMACHex)
	if err != nil || len(supplied) != markerHMACByteLength {
		return false
	}
	expected := computeMarkerHMACBytes(key, tenantID, clickID)
	return hmac.Equal(expected, supplied)
}

// computeMarkerHMACBytes is the keyed digest the marker carries
// truncated to markerHMACByteLength. Exposed only via the higher-level
// Build/Verify helpers so callers cannot accidentally compare with a
// different truncation length.
func computeMarkerHMACBytes(key MarkerKey, tenantID uuid.UUID, clickID string) []byte {
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte(tenantID.String()))
	_, _ = mac.Write([]byte(":"))
	_, _ = mac.Write([]byte(clickID))
	return mac.Sum(nil)[:markerHMACByteLength]
}

// computeMarkerHMACHex is computeMarkerHMACBytes hex-encoded.
func computeMarkerHMACHex(key MarkerKey, tenantID uuid.UUID, clickID string) string {
	return hex.EncodeToString(computeMarkerHMACBytes(key, tenantID, clickID))
}
