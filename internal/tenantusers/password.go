package tenantusers

import (
	"crypto/rand"
	"math/big"
)

// tempPasswordAlphabet is the character set for a system-generated temporary
// password. It omits visually ambiguous glyphs (0/O, 1/l/I) so the gerente
// can read the one-time value aloud or copy it without transcription errors,
// while still spanning upper, lower and digits.
const tempPasswordAlphabet = "ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnpqrstuvwxyz23456789" // gitleaks:allow -- fixed character set, not a secret

// tempPasswordLen is the length of a generated temporary password. 20 chars
// over the 55-symbol alphabet is ~115 bits of entropy — far beyond any
// online-guessing concern for a single-use credential that is force-rotated
// on first login.
const tempPasswordLen = 20

// GenerateTempPassword returns a cryptographically-random temporary password.
// It is shown to the gerente exactly once at create time and never stored in
// plaintext, logged, or placed in a URL; only its Argon2id hash is persisted.
// A crypto/rand failure is surfaced (never silently degraded to a weak
// value).
func GenerateTempPassword() (string, error) {
	out := make([]byte, tempPasswordLen)
	max := big.NewInt(int64(len(tempPasswordAlphabet)))
	for i := range out {
		n, err := rand.Int(rand.Reader, max)
		if err != nil {
			return "", err
		}
		out[i] = tempPasswordAlphabet[n.Int64()]
	}
	return string(out), nil
}
