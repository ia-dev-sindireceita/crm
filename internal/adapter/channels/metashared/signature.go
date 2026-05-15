package metashared

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
)

// SignatureHeader is the canonical header Meta uses to carry the
// HMAC-SHA256 digest of the request body, lowercase-hex encoded. The
// value is documented under "Validating Payloads" in the Meta webhook
// guide; every Meta product family (whatsapp / instagram / messenger)
// signs with this same header so we keep one constant in the shared
// package.
const SignatureHeader = "X-Hub-Signature-256"

// signaturePrefix is the optional `sha256=` marker Meta sometimes
// includes ahead of the hex digest. The wire format allows both with
// and without the prefix; we accept either form so an upstream tweak
// in Meta's encoding does not break verification overnight.
const signaturePrefix = "sha256="

// Signature errors. Callers MUST treat any non-nil return from
// VerifySignature as a verification failure — the discriminating
// sentinels exist so logs / metrics can distinguish "header absent"
// from "header malformed" from "bytes mismatch" without re-parsing.
var (
	// ErrSignatureMissing is returned when headerSig is empty or
	// whitespace-only. The carrier MUST send the header; absence is
	// either a misconfigured app dashboard or an attacker probe.
	ErrSignatureMissing = errors.New("metashared: signature header missing")

	// ErrSignatureFormat is returned when the header value cannot be
	// hex-decoded. The header MUST be lowercase hex per Meta's docs;
	// other encodings are protocol violations.
	ErrSignatureFormat = errors.New("metashared: signature header malformed")

	// ErrSignatureMismatch is returned when the HMAC bytes computed
	// from (secret, payload) do not match the header. This is the
	// signal callers translate to HTTP 401 — the only non-200 reply
	// the Meta dashboard surfaces back to the operator.
	ErrSignatureMismatch = errors.New("metashared: signature mismatch")
)

// VerifySignature reports whether headerSig is a valid Meta HMAC-SHA256
// signature over payload, keyed with secret. Comparison uses
// hmac.Equal (constant-time) so a timing-side-channel attacker cannot
// recover the digest byte by byte.
//
// Contract:
//
//   - secret is the Meta app_secret; an empty secret is a programming
//     error (composition root validates env at startup) — we still
//     compute a deterministic mismatch rather than panic so an
//     attacker cannot crash the process by sending a request before
//     the secret loads.
//   - payload is the raw request body, read once before any parsing
//     (signing happens on bytes, not on a re-marshalled struct).
//   - headerSig accepts both `sha256=<hex>` and bare `<hex>` forms.
//
// The function returns one of ErrSignatureMissing /
// ErrSignatureFormat / ErrSignatureMismatch or nil on success.
func VerifySignature(secret string, payload []byte, headerSig string) error {
	got := strings.TrimSpace(headerSig)
	if got == "" {
		return ErrSignatureMissing
	}
	got = strings.TrimPrefix(got, signaturePrefix)
	gotBytes, err := hex.DecodeString(got)
	if err != nil {
		return ErrSignatureFormat
	}
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write(payload)
	if !hmac.Equal(gotBytes, mac.Sum(nil)) {
		return ErrSignatureMismatch
	}
	return nil
}
