package mfa

import (
	"crypto/rand"
	"encoding/base32"
	"io"
	"strings"
)

// RecoveryCodeCount is the number of single-use recovery codes minted
// at every enrol or regenerate. Pinned by ADR 0074 §2.
const RecoveryCodeCount = 10

// RecoveryCodeLen is the length, in base32 characters, of each emitted
// code (50 bits of entropy — comfortably above the OWASP single-use
// minimum and short enough that a user can type it from paper without
// transcription errors). Pinned by ADR 0074 §2.
const RecoveryCodeLen = 10

// recoveryCodeRawBytes is the number of raw bytes drawn from
// crypto/rand before base32 encoding. base32 encodes 5 bits per
// character, so 10 chars need 50 bits = 7 bytes (the encoder rounds up
// to 8 chars, we slice the output to RecoveryCodeLen). We draw 10
// bytes for headroom — the slice is the security boundary, the extra
// bytes are dropped without ever being shown.
const recoveryCodeRawBytes = 10

// codeAlphabet is the base32 alphabet used by RFC 4648 (also what
// otpauth:// secrets use, so the user sees one consistent character
// set across enrol QR and recovery codes). Padding is suppressed.
var codeAlphabet = base32.StdEncoding.WithPadding(base32.NoPadding)

// GenerateRecoveryCodes returns RecoveryCodeCount fresh, plaintext
// recovery codes. Each is RecoveryCodeLen base32 characters, drawn
// from crypto/rand. The returned slice is what the caller hashes and
// also what the caller renders to the master ONCE — there is no other
// path to the plaintext. Pass an io.Reader for deterministic vector
// tests; production callers leave randSrc nil and crypto/rand is
// used.
func GenerateRecoveryCodes(randSrc io.Reader) ([]string, error) {
	if randSrc == nil {
		randSrc = rand.Reader
	}
	codes := make([]string, RecoveryCodeCount)
	for i := 0; i < RecoveryCodeCount; i++ {
		raw := make([]byte, recoveryCodeRawBytes)
		if _, err := io.ReadFull(randSrc, raw); err != nil {
			return nil, err
		}
		encoded := codeAlphabet.EncodeToString(raw)
		// EncodeToString of 10 raw bytes yields 16 base32 chars; we keep
		// the first RecoveryCodeLen.
		codes[i] = encoded[:RecoveryCodeLen]
	}
	return codes, nil
}

// FormatRecoveryCode inserts a single dash at the midpoint for
// display. The dash is presentation-only — never stored, never sent
// over the wire. NormalizeRecoveryCode strips it on the way back in.
//
//	"ABCDE12345" -> "ABCDE-12345"
func FormatRecoveryCode(code string) string {
	if len(code) != RecoveryCodeLen {
		return code
	}
	mid := RecoveryCodeLen / 2
	return code[:mid] + "-" + code[mid:]
}

// NormalizeRecoveryCode reduces a user-submitted plaintext to the
// canonical form the hasher saw at insert: dashes stripped, ASCII
// upper-cased. Returns ErrCodeFormat if the result is not exactly
// RecoveryCodeLen base32 characters. The caller passes the returned
// canonical string to the CodeHasher.Verify path.
func NormalizeRecoveryCode(submitted string) (string, error) {
	stripped := strings.Map(func(r rune) rune {
		switch {
		case r == '-' || r == ' ':
			return -1
		case r >= 'a' && r <= 'z':
			return r - ('a' - 'A')
		default:
			return r
		}
	}, submitted)
	if len(stripped) != RecoveryCodeLen {
		return "", ErrCodeFormat
	}
	for _, r := range stripped {
		if !isBase32Char(r) {
			return "", ErrCodeFormat
		}
	}
	return stripped, nil
}

// isBase32Char reports whether r is a member of the RFC 4648 base32
// alphabet (A–Z, 2–7). It is the inverse of the alphabet used by
// GenerateRecoveryCodes — anything outside this set is a typo or a
// hostile probe and must not reach the verifier.
func isBase32Char(r rune) bool {
	return (r >= 'A' && r <= 'Z') || (r >= '2' && r <= '7')
}
