package inter

// SIN-62964 — Inter PIX webhook receiver adapter pieces.
//
// The HTTP handler (internal/adapter/transport/http/pix/inter) wires up
// the request pipeline: IP allowlist → rate limit → HMAC signature →
// body parse → reconciler.Apply. This file owns the two adapter-shaped
// pieces of that pipeline:
//
//  1. WebhookVerifier — HMAC-SHA256 hex compare against the configured
//     shared secret. Header name is configurable so a future Inter
//     revision (e.g. signing under a different header) does not force a
//     code change at the wire boundary.
//  2. WebhookParser   — translates Inter's "pix" webhook envelope into
//     a slice of pix.WebhookEvent (one per pix item). Inter batches
//     payment confirmations so we yield one normalised event per inner
//     entry; the receiver iterates and feeds the reconciler one at a
//     time so each (source, external_id, event_type) tuple lands its own
//     dedup row.
//
// IP allowlist defaults are exported as DefaultAllowedCIDRs so the wire
// keeps the "configurable via env, sane default" posture the AC asks
// for. The actual allow-vs-deny check happens in the handler — this
// package only owns the default list of CIDRs published by Inter.

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/pericles-luz/crm/internal/billing/pix"
)

// SourceName is the constant we mirror into pix.WebhookEvent.Source and
// the webhook_events ledger. Kept here next to the verifier so the same
// string never disagrees between the parser and the wiring.
const SourceName = "banco-inter"

// DefaultSignatureHeader is the canonical HTTP header Inter signs the
// webhook body under. Configurable via WebhookConfig.SignatureHeader so
// an Inter docs revision (or a sandbox quirk) does not force a code
// change.
const DefaultSignatureHeader = "X-Inter-Signature"

// DefaultAllowedCIDRs is the conservative starting allowlist of PSP
// egress ranges. Inter does not publish a stable, structured CIDR
// document — operators MUST override via PIX_INTER_WEBHOOK_IP_ALLOWLIST
// once Inter confirms the production ranges for the integration.
//
// Until then the default keeps the receiver effectively closed in
// production (loopback is fine for staging probes; no other range
// matches Inter egress). The handler exposes a temporary-disable flag
// for ops break-glass.
var DefaultAllowedCIDRs = []string{
	"127.0.0.1/32",
	"::1/128",
}

// ErrSignatureMissing is returned by WebhookVerifier.Verify when the
// configured signature header is absent or empty. Maps to a 401 at the
// HTTP boundary.
var ErrSignatureMissing = errors.New("pix.inter: webhook signature missing")

// ErrSignatureInvalid is returned by WebhookVerifier.Verify when the
// header is present but does not match HMAC(secret, body). Maps to a
// 401 at the HTTP boundary.
var ErrSignatureInvalid = errors.New("pix.inter: webhook signature invalid")

// ErrParse is returned by WebhookParser.Parse when the payload is not
// JSON or is missing the pix array. Maps to a 400 at the HTTP boundary
// — but the wiring also feeds it into the structured log so dropped
// payloads remain auditable.
var ErrParse = errors.New("pix.inter: webhook payload parse error")

// WebhookConfig is the constructor input for the verifier + parser.
//
//	Secret             — shared HMAC secret (env-loaded, never logged).
//	SignatureHeader    — header name, default DefaultSignatureHeader.
//	MaxClockSkewFuture — reject payments dated more than N in the future
//	                     (defends against a misconfigured upstream NTP).
//	                     0 (default) disables the check.
type WebhookConfig struct {
	Secret             string
	SignatureHeader    string
	MaxClockSkewFuture time.Duration
}

// WebhookVerifier compares the body against the HMAC header in constant
// time. Construct via NewWebhookVerifier. The verifier holds the secret
// as a []byte clone so a caller mutating the original string-backed
// buffer cannot race the verify path.
type WebhookVerifier struct {
	secret []byte
	header string
}

// NewWebhookVerifier validates cfg and returns a ready verifier. An
// empty Secret yields ErrMissingConfig — the receiver MUST never run
// signature-less because that collapses AC #2 into a 200-on-anything
// endpoint.
func NewWebhookVerifier(cfg WebhookConfig) (*WebhookVerifier, error) {
	if cfg.Secret == "" {
		return nil, fmt.Errorf("%w: webhook Secret is required", ErrMissingConfig)
	}
	header := cfg.SignatureHeader
	if header == "" {
		header = DefaultSignatureHeader
	}
	clone := make([]byte, len(cfg.Secret))
	copy(clone, cfg.Secret)
	return &WebhookVerifier{secret: clone, header: header}, nil
}

// HeaderName returns the HTTP header the verifier inspects. The wiring
// uses this so it can name the header in logs without re-reading the
// configuration struct.
func (v *WebhookVerifier) HeaderName() string { return v.header }

// Verify checks the HMAC-SHA256 hex digest in headers[v.header] against
// HMAC(secret, body). Returns:
//
//   - nil                       — signature matches
//   - ErrSignatureMissing       — header absent / empty
//   - ErrSignatureInvalid       — header present but does not match
//
// Comparison is constant-time so a side-channel timing attack cannot
// recover the secret. The header value is allowed to be prefixed with
// "sha256=" (some PSPs render it that way); the prefix is stripped
// before the hex decode.
func (v *WebhookVerifier) Verify(body []byte, headers map[string][]string) error {
	if v == nil {
		return ErrSignatureInvalid
	}
	got := firstHeader(headers, v.header)
	if got == "" {
		return ErrSignatureMissing
	}
	got = strings.TrimSpace(got)
	got = strings.TrimPrefix(got, "sha256=")
	got = strings.TrimPrefix(got, "SHA256=")
	want, err := computeHMAC(v.secret, body)
	if err != nil {
		return ErrSignatureInvalid
	}
	gotBytes, err := hex.DecodeString(got)
	if err != nil {
		return ErrSignatureInvalid
	}
	if !hmac.Equal(gotBytes, want) {
		return ErrSignatureInvalid
	}
	return nil
}

// computeHMAC is the one place we touch crypto/hmac so the test suite
// can pin the algorithm choice with a fixture. Returns the raw digest
// bytes (32 bytes for SHA-256).
func computeHMAC(secret, body []byte) ([]byte, error) {
	mac := hmac.New(sha256.New, secret)
	if _, err := mac.Write(body); err != nil {
		return nil, err
	}
	return mac.Sum(nil), nil
}

// firstHeader returns the first value of name from headers using a
// case-insensitive lookup so callers that pass http.Header directly do
// not need to canonicalise. Empty when missing.
func firstHeader(headers map[string][]string, name string) string {
	if len(headers) == 0 || name == "" {
		return ""
	}
	// Canonical form first — http.Header.Get behaviour.
	if vs, ok := headers[canonicalMIMEHeaderKey(name)]; ok && len(vs) > 0 {
		return vs[0]
	}
	// Linear fallback so a raw map (lowercase keys) still resolves.
	for k, vs := range headers {
		if len(vs) == 0 {
			continue
		}
		if strings.EqualFold(k, name) {
			return vs[0]
		}
	}
	return ""
}

// canonicalMIMEHeaderKey duplicates net/textproto.CanonicalMIMEHeaderKey
// behaviour for ASCII header names so this file can stay free of an
// extra import.
func canonicalMIMEHeaderKey(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	upper := true
	for i := 0; i < len(s); i++ {
		c := s[i]
		if upper && c >= 'a' && c <= 'z' {
			c -= 'a' - 'A'
		} else if !upper && c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		b.WriteByte(c)
		upper = c == '-'
	}
	return b.String()
}

// interWebhookEnvelope mirrors the JSON shape Inter pushes to the
// configured callback URL. The official spec packs one or more PIX
// payment confirmations into the `pix` array; future event_types (e.g.
// scheduled-cob cancellation) land alongside as siblings — for now
// only payment confirmations are normalised, and unknown event keys
// fall through to ErrParse so the receiver answers a 400 instead of
// silently dropping the payload.
type interWebhookEnvelope struct {
	Pix []interPixItem `json:"pix"`
}

// interPixItem is one payment confirmation inside the envelope. Field
// names follow Inter's documented camelCase exactly.
type interPixItem struct {
	EndToEndID string `json:"endToEndId"`
	TxID       string `json:"txid"`
	Valor      string `json:"valor"`
	Horario    string `json:"horario"`
}

// WebhookParser turns an Inter envelope into the normalised
// pix.WebhookEvent slice the reconciler consumes. Construct via
// NewWebhookParser.
type WebhookParser struct {
	now func() time.Time
}

// NewWebhookParser returns a parser that uses time.Now for missing
// timestamps. Tests pass a deterministic clock via WithNow.
func NewWebhookParser() *WebhookParser {
	return &WebhookParser{now: time.Now}
}

// WithNow returns a copy of p that reads the receive-time clock from
// fn. Tests use this so a missing/unparseable horario is replaced with
// a fixed instant.
func (p *WebhookParser) WithNow(fn func() time.Time) *WebhookParser {
	if fn == nil {
		return p
	}
	cp := *p
	cp.now = fn
	return &cp
}

// Parse decodes body and yields one pix.WebhookEvent per pix item.
//
// The Payload field on each returned event is the bytes of THAT item
// (re-serialised) rather than the full envelope, so the webhook_events
// ledger stores exactly the unit being deduped. This matters because
// the dedup key is per-item (`txid` → ExternalID) — keeping the
// per-item payload aligned with the dedup key keeps audit trails
// uncluttered.
//
// Returns ErrParse on:
//
//   - non-JSON body
//   - missing or empty `pix` array
//   - any item with an empty txid (the dedup key — empty = unusable)
//
// Future event_types (e.g. cancellation) land as additional sibling
// fields on the envelope; until they are documented and normalised,
// this parser refuses payloads that supply none of the known fields.
func (p *WebhookParser) Parse(body []byte) ([]pix.WebhookEvent, error) {
	if p == nil {
		return nil, ErrParse
	}
	if len(body) == 0 {
		return nil, fmt.Errorf("%w: empty body", ErrParse)
	}
	var env interWebhookEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrParse, err)
	}
	if len(env.Pix) == 0 {
		return nil, fmt.Errorf("%w: no pix entries", ErrParse)
	}
	out := make([]pix.WebhookEvent, 0, len(env.Pix))
	for i, item := range env.Pix {
		if strings.TrimSpace(item.TxID) == "" {
			return nil, fmt.Errorf("%w: pix[%d].txid empty", ErrParse, i)
		}
		occurred := parseHorario(item.Horario, p.now)
		itemPayload, err := json.Marshal(item)
		if err != nil {
			return nil, fmt.Errorf("%w: re-encode pix[%d]: %v", ErrParse, i, err)
		}
		out = append(out, pix.WebhookEvent{
			Source:     SourceName,
			ExternalID: item.TxID,
			EventType:  pix.WebhookEventPaid,
			Payload:    itemPayload,
			OccurredAt: occurred,
		})
	}
	return out, nil
}

// parseHorario tries the two timestamp shapes Inter is known to emit:
// RFC3339 (the documented BACEN format) and RFC3339Nano (some sandbox
// builds drop the fractional second). On miss we fall back to nowFn()
// — the reconciler treats OccurredAt purely as audit metadata, so a
// receive-time stand-in does not break the state machine.
func parseHorario(raw string, nowFn func() time.Time) time.Time {
	if raw = strings.TrimSpace(raw); raw != "" {
		if t, err := time.Parse(time.RFC3339Nano, raw); err == nil {
			return t.UTC()
		}
		if t, err := time.Parse(time.RFC3339, raw); err == nil {
			return t.UTC()
		}
	}
	return nowFn().UTC()
}

// ParseCIDRList parses a comma-separated CIDR string into a slice of
// *net.IPNet. Empty / invalid entries are dropped silently (the caller
// — wiring — logs the per-entry parse error at WARN before passing the
// raw string in). An empty input yields a nil slice.
//
// Exposed for the wiring + the handler tests; the handler does the
// allow vs. deny check using IPAllow below.
func ParseCIDRList(raw string) []*net.IPNet {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]*net.IPNet, 0, len(parts))
	for _, p := range parts {
		s := strings.TrimSpace(p)
		if s == "" {
			continue
		}
		_, cidr, err := net.ParseCIDR(s)
		if err != nil || cidr == nil {
			continue
		}
		out = append(out, cidr)
	}
	return out
}

// IPAllow reports whether peer is inside any cidr. nil peer or empty
// cidrs slice yield false (deny-by-default).
func IPAllow(peer net.IP, cidrs []*net.IPNet) bool {
	if peer == nil || len(cidrs) == 0 {
		return false
	}
	for _, c := range cidrs {
		if c != nil && c.Contains(peer) {
			return true
		}
	}
	return false
}

// DefaultAllowedCIDRSet returns the parsed form of DefaultAllowedCIDRs.
// Convenience for the wiring; the handler keeps the parsed slice in
// memory rather than re-parsing per request.
func DefaultAllowedCIDRSet() []*net.IPNet {
	return ParseCIDRList(strings.Join(DefaultAllowedCIDRs, ","))
}
