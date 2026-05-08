package mfa

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"crypto/subtle"
	"encoding/base32"
	"encoding/binary"
	"fmt"
	"io"
	"net/url"
	"strconv"
	"time"
)

// Pinned RFC 6238 parameters. ADR 0074 §1 fixes algorithm, digits, and
// step. Changing any of them is a retroactive break for every enrolled
// authenticator and is therefore an ADR amendment.
const (
	// totpDigits is the number of decimal digits in a generated code.
	// Six is what every consumer authenticator (Google, Authy, 1Password)
	// renders by default.
	totpDigits = 6

	// totpStep is the time step (X in RFC 6238 §4.1). Thirty seconds is
	// the universal default.
	totpStep = 30 * time.Second

	// totpT0 is the start of the counting epoch (T0 in RFC 6238 §4.1).
	// Zero — the Unix epoch — matches every consumer authenticator.
	totpT0 = int64(0)

	// totpSeedSize is the number of bytes drawn from crypto/rand for a
	// freshly-minted master TOTP seed. ADR 0074 §1 pins this at 32 bytes
	// (256 bits, 96 bits of headroom over RFC 4226's 160-bit minimum).
	totpSeedSize = 32

	// totpSeedMin is the minimum acceptable seed length. RFC 4226 §4
	// requires 128 bits ("MUST" be at least 128) and recommends 160
	// (=20 bytes). We refuse to issue or verify any seed shorter than
	// 20 bytes — that is the security floor.
	totpSeedMin = 20
)

// totpAlphabet is the base32 alphabet used to encode/decode the seed.
// Padding is suppressed (matches every authenticator's QR scan).
var totpAlphabet = base32.StdEncoding.WithPadding(base32.NoPadding)

// NewSecret returns a fresh totpSeedSize-byte seed read from
// crypto/rand. Pass an io.Reader for vector tests; production callers
// leave randSrc nil.
func NewSecret(randSrc io.Reader) ([]byte, error) {
	if randSrc == nil {
		randSrc = rand.Reader
	}
	seed := make([]byte, totpSeedSize)
	if _, err := io.ReadFull(randSrc, seed); err != nil {
		return nil, fmt.Errorf("mfa: read totp seed: %w", err)
	}
	return seed, nil
}

// EncodeSecret renders the raw seed bytes as the unpadded base32 string
// that ends up in the otpauth:// URI's `secret=` parameter.
func EncodeSecret(seed []byte) (string, error) {
	if len(seed) < totpSeedMin {
		return "", ErrSeedTooShort
	}
	return totpAlphabet.EncodeToString(seed), nil
}

// DecodeSecret is the inverse of EncodeSecret. Returns ErrSeedTooShort
// if the decoded length falls below the RFC 4226 floor.
func DecodeSecret(s string) ([]byte, error) {
	raw, err := totpAlphabet.DecodeString(s)
	if err != nil {
		return nil, fmt.Errorf("mfa: decode totp secret: %w", err)
	}
	if len(raw) < totpSeedMin {
		return nil, ErrSeedTooShort
	}
	return raw, nil
}

// counter computes T (RFC 6238 §4.1): floor((unix - T0) / step).
func counter(t time.Time) uint64 {
	delta := t.Unix() - totpT0
	if delta < 0 {
		return 0
	}
	return uint64(delta) / uint64(totpStep.Seconds())
}

// hotp computes the truncated 6-digit HOTP value for a given
// (seed, counter) pair. RFC 4226 §5.3 dynamic truncation.
func hotp(seed []byte, c uint64) string {
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], c)

	mac := hmac.New(sha1.New, seed)
	mac.Write(buf[:])
	sum := mac.Sum(nil)

	offset := sum[len(sum)-1] & 0x0f
	bin := (uint32(sum[offset]&0x7f) << 24) |
		(uint32(sum[offset+1]) << 16) |
		(uint32(sum[offset+2]) << 8) |
		uint32(sum[offset+3])
	mod := uint32(1)
	for i := 0; i < totpDigits; i++ {
		mod *= 10
	}
	code := bin % mod
	return fmt.Sprintf("%0*d", totpDigits, code)
}

// Generate is the TOTP code for the given seed at instant t. It is
// exposed for vector tests and for adapters that want to mint codes
// out-of-band (e.g. the recovery flow's "your next code is …" hint).
// Production verification calls Verify, not Generate.
func Generate(seed []byte, t time.Time) (string, error) {
	if len(seed) < totpSeedMin {
		return "", ErrSeedTooShort
	}
	return hotp(seed, counter(t)), nil
}

// Verify checks that submitted matches the TOTP code derived from
// seed at instant t, allowing window steps of clock drift in either
// direction. ADR 0074 §1 pins window = 1 step (±30s). Returns
// ErrInvalidCode on mismatch — never a generic error — so callers
// can pattern-match without sniffing the message.
//
// The comparison is constant-time so a brute-force attacker can not
// distinguish a near-miss from a wrong-length input by timing.
func Verify(seed []byte, submitted string, t time.Time, window int) error {
	if len(seed) < totpSeedMin {
		return ErrSeedTooShort
	}
	if len(submitted) != totpDigits {
		return ErrInvalidCode
	}
	// Reject any non-digit early so we don't waste HMAC compute on
	// obvious garbage. Constant-time compare below still runs over the
	// well-formed cases, so the early return doesn't leak more than
	// "the input wasn't six digits".
	for _, r := range submitted {
		if r < '0' || r > '9' {
			return ErrInvalidCode
		}
	}
	current := counter(t)
	for delta := -window; delta <= window; delta++ {
		c := int64(current) + int64(delta)
		if c < 0 {
			continue
		}
		want := hotp(seed, uint64(c))
		if subtle.ConstantTimeCompare([]byte(want), []byte(submitted)) == 1 {
			return nil
		}
	}
	return ErrInvalidCode
}

// OTPAuthURI renders the otpauth:// URI consumed by every common
// authenticator (Google Authenticator, Authy, 1Password, …). ADR 0074
// §1 pins SHA1, digits=6, period=30; the URI is what those clients
// scan from the enrol QR code.
//
//	otpauth://totp/{issuer}:{label}?secret=…&issuer={issuer}
//	  &algorithm=SHA1&digits=6&period=30
//
// label is typically the master's email; issuer identifies the
// product (e.g. "Sindireceita"). Both are URL-escaped.
func OTPAuthURI(issuer, label string, seed []byte) (string, error) {
	encoded, err := EncodeSecret(seed)
	if err != nil {
		return "", err
	}
	q := url.Values{}
	q.Set("secret", encoded)
	q.Set("issuer", issuer)
	q.Set("algorithm", "SHA1")
	q.Set("digits", strconv.Itoa(totpDigits))
	q.Set("period", strconv.Itoa(int(totpStep.Seconds())))
	path := url.PathEscape(issuer + ":" + label)
	return "otpauth://totp/" + path + "?" + q.Encode(), nil
}
