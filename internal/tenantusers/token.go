package tenantusers

import (
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"time"
)

// Purpose is the credential-token purpose vocabulary (mirrors the CHECK on
// user_credential_tokens.purpose).
type Purpose string

const (
	// PurposeInvite is the initial-credential invite issued on user creation.
	PurposeInvite Purpose = "invite"
	// PurposeReset is a self-service password reset (future; TTL differs).
	PurposeReset Purpose = "reset"
)

// InviteTTL is the lifetime of an initial invite token (SIN-66494 §1 — 72h
// gives onboarding slack). Self-service reset uses a shorter TTL, out of
// scope here.
const InviteTTL = 72 * time.Hour

// tokenBytes is the entropy of a token before base64url encoding (256 bits).
const tokenBytes = 32

// Token is a freshly minted credential token. Plaintext is returned exactly
// once (to build the invite link) and is NEVER persisted; only SHA256 is
// stored in user_credential_tokens.token_sha256.
type Token struct {
	Plaintext string
	SHA256    []byte
	Purpose   Purpose
	ExpiresAt time.Time
}

// GenerateToken mints a high-entropy opaque token. randRead is injectable for
// deterministic tests; production passes crypto/rand.Read. The SHA-256 (fast
// hash) is correct here because the token is high-entropy — argon2 is only
// for low-entropy human passwords.
func GenerateToken(randRead func([]byte) (int, error), purpose Purpose, now time.Time, ttl time.Duration) (Token, error) {
	b := make([]byte, tokenBytes)
	if _, err := randRead(b); err != nil {
		return Token{}, fmt.Errorf("tenantusers: generate token: %w", err)
	}
	plain := base64.RawURLEncoding.EncodeToString(b)
	return Token{
		Plaintext: plain,
		SHA256:    HashToken(plain),
		Purpose:   purpose,
		ExpiresAt: now.Add(ttl),
	}, nil
}

// HashToken returns the SHA-256 of a plaintext token. Used both when minting
// (to store) and when consuming (to look up by hash).
func HashToken(plain string) []byte {
	sum := sha256.Sum256([]byte(plain))
	return sum[:]
}
