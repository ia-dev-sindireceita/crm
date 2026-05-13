package iam

// argon2id parameters for SIN-62213. Tuned for staging hardware against the
// RFC 9106 second-recommended profile; revisit once we have prod CPU budget
// data (tracked in ADR-0004 follow-up). Encoded format is the PHC string:
//
//	$argon2id$v=19$m=65536,t=3,p=4$<base64(salt)>$<base64(hash)>
//
// Salt: 16 bytes from crypto/rand. Output: 32 bytes. Comparison is
// constant-time. Plaintext, derived hash, and the encoded string MUST NOT
// be logged at any verbosity. Errors returned from this file are sentinels;
// callers decide what to log (see internal/iam/login.go for the policy).

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"

	"golang.org/x/crypto/argon2"
)

const (
	argonTime      uint32 = 3
	argonMemoryKiB uint32 = 64 * 1024
	argonThreads   uint8  = 4
	argonKeyLen    uint32 = 32
	argonSaltLen   int    = 16
	argonVersion          = argon2.Version // 0x13 / 19
)

// HashPassword returns a PHC-encoded argon2id hash of plaintext. A fresh
// 16-byte salt is read from crypto/rand on every call. The returned string
// is safe to store in the users.password_hash column; it embeds all
// parameters needed to verify later, so a future tuning of argonTime/Memory
// will not invalidate already-stored hashes.
func HashPassword(plaintext string) (string, error) {
	salt := make([]byte, argonSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("iam: read salt: %w", err)
	}
	derived := argon2.IDKey([]byte(plaintext), salt, argonTime, argonMemoryKiB, argonThreads, argonKeyLen)
	return encodePHC(salt, derived), nil
}

// VerifyPassword reports whether plaintext, when re-derived under the
// parameters embedded in encoded, matches the stored hash. Comparison is
// constant-time. A malformed encoded string returns ErrInvalidEncoding,
// never panics.
//
// VerifyPassword does not return an "invalid encoding vs wrong password"
// distinction beyond the sentinel — Login always collapses both paths to
// ErrInvalidCredentials so the wire result is identical.
func VerifyPassword(plaintext, encoded string) (bool, error) {
	salt, want, err := decodePHC(encoded)
	if err != nil {
		return false, err
	}
	got := argon2.IDKey([]byte(plaintext), salt, argonTime, argonMemoryKiB, argonThreads, argonKeyLen)
	return subtle.ConstantTimeCompare(got, want) == 1, nil
}

// encodePHC produces $argon2id$v=19$m=…,t=…,p=…$<salt>$<hash>. base64 is
// raw (no padding), matching the standard PHC convention.
func encodePHC(salt, hash []byte) string {
	var b strings.Builder
	b.Grow(96)
	b.WriteString("$argon2id$v=")
	b.WriteString(strconv.Itoa(argonVersion))
	b.WriteString("$m=")
	b.WriteString(strconv.FormatUint(uint64(argonMemoryKiB), 10))
	b.WriteString(",t=")
	b.WriteString(strconv.FormatUint(uint64(argonTime), 10))
	b.WriteString(",p=")
	b.WriteString(strconv.FormatUint(uint64(argonThreads), 10))
	b.WriteByte('$')
	b.WriteString(base64.RawStdEncoding.EncodeToString(salt))
	b.WriteByte('$')
	b.WriteString(base64.RawStdEncoding.EncodeToString(hash))
	return b.String()
}

// decodePHC parses the PHC encoding defensively. The valid shape is six
// segments after splitting on '$' (the leading empty segment plus five
// fields). Anything else, including unknown algorithms or versions,
// returns ErrInvalidEncoding.
//
// We deliberately ignore the embedded m/t/p values when re-deriving and
// use the package-level constants instead (argonMemoryKiB / argonTime /
// argonThreads). Honouring caller-supplied parameters here would let a
// hostile DB row downgrade verification cost; pinning to constants closes
// that vector. A future migration that bumps the constants will need a
// re-hash path (out of scope for SIN-62213).
func decodePHC(encoded string) ([]byte, []byte, error) {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[0] != "" {
		return nil, nil, ErrInvalidEncoding
	}
	if parts[1] != "argon2id" {
		return nil, nil, ErrInvalidEncoding
	}
	if parts[2] != "v="+strconv.Itoa(argonVersion) {
		return nil, nil, ErrInvalidEncoding
	}
	if err := validateParams(parts[3]); err != nil {
		return nil, nil, err
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil || len(salt) != argonSaltLen {
		return nil, nil, ErrInvalidEncoding
	}
	hash, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil || uint32(len(hash)) != argonKeyLen {
		return nil, nil, ErrInvalidEncoding
	}
	return salt, hash, nil
}

// validateParams parses "m=…,t=…,p=…" and rejects values that don't
// match the package constants. Values are parsed with strconv (no panic on
// non-numeric input) and compared exactly — see decodePHC's note on why.
func validateParams(s string) error {
	fields := strings.Split(s, ",")
	if len(fields) != 3 {
		return ErrInvalidEncoding
	}
	want := []struct {
		key string
		val uint64
	}{
		{"m=", uint64(argonMemoryKiB)},
		{"t=", uint64(argonTime)},
		{"p=", uint64(argonThreads)},
	}
	for i, w := range want {
		if !strings.HasPrefix(fields[i], w.key) {
			return ErrInvalidEncoding
		}
		got, err := strconv.ParseUint(fields[i][len(w.key):], 10, 32)
		if err != nil || got != w.val {
			return ErrInvalidEncoding
		}
	}
	return nil
}
