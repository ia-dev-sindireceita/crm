// Package aesgcm is the AES-256-GCM adapter for the mfa.SeedCipher
// port. ADR 0074 §1 pins encryption-at-rest of the master TOTP seed:
// the plaintext seed never reaches Postgres, only the ciphertext from
// this cipher does. The symmetric key lives in an env var (production
// wiring), is never written to disk, and is rotated only by issuing a
// new env var and re-enrolling masters — there is no in-place key
// rotation in this package because Phase 0 does not need it.
//
// The wire format is `nonce || ciphertext_and_tag` where:
//   - nonce: 12 random bytes (GCM standard)
//   - ciphertext_and_tag: AES-GCM output (plaintext + 16-byte auth tag)
//
// Hexagonal contract: this package depends only on the standard
// library and on the mfa port (for the compile-time assertion). It is
// a leaf package in the dependency graph.
package aesgcm

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"fmt"
	"io"

	"github.com/pericles-luz/crm/internal/iam/mfa"
)

// KeySize is the only key length this cipher accepts. AES-256-GCM is
// pinned by ADR 0074 — AES-128 keys are silently rejected at
// construction so a misconfigured deploy fails closed.
const KeySize = 32

// nonceSize is the GCM standard nonce length. Using anything else
// breaks interop with the stdlib's AEAD; the cipher.NewGCM call
// below would already enforce it, but naming the constant here makes
// the wire-format comment above checkable.
const nonceSize = 12

// ErrKeySize is returned by New when the supplied key is not exactly
// KeySize bytes.
var ErrKeySize = errors.New("aesgcm: key must be 32 bytes (AES-256)")

// ErrShortCiphertext is returned by Decrypt when the input is shorter
// than nonceSize + 1 byte. It signals a malformed payload — distinct
// from a tampered payload which surfaces as a GCM auth failure.
var ErrShortCiphertext = errors.New("aesgcm: ciphertext shorter than nonce")

// SeedCipher implements mfa.SeedCipher with AES-256-GCM. The aead is
// constructed once and reused — its internal state is goroutine-safe,
// so a single SeedCipher value can be shared across the http server's
// handlers.
type SeedCipher struct {
	aead     cipher.AEAD
	randRead func([]byte) (int, error)
}

// Compile-time assertion that SeedCipher satisfies the domain port.
var _ mfa.SeedCipher = (*SeedCipher)(nil)

// New constructs a SeedCipher bound to the supplied key. The caller
// owns the byte slice; New copies the bytes into the internal AES
// cipher so callers can zero their copy after construction (the AEAD
// retains its own derived key schedule, not the original bytes).
//
// Pass randSrc only in tests — production callers leave it nil and
// crypto/rand is used.
func New(key []byte, randSrc io.Reader) (*SeedCipher, error) {
	if len(key) != KeySize {
		return nil, ErrKeySize
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("aesgcm: aes.NewCipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("aesgcm: cipher.NewGCM: %w", err)
	}
	read := rand.Read
	if randSrc != nil {
		read = randSrc.Read
	}
	return &SeedCipher{aead: aead, randRead: read}, nil
}

// Encrypt produces `nonce || ciphertext_and_tag`. A fresh 12-byte
// nonce is drawn from crypto/rand for every call — never reused
// (re-using a (key, nonce) pair under GCM forfeits authentication
// AND confidentiality, so the function refuses to proceed if the
// random source is short).
func (c *SeedCipher) Encrypt(plaintext []byte) ([]byte, error) {
	if len(plaintext) == 0 {
		return nil, fmt.Errorf("aesgcm: Encrypt: empty plaintext")
	}
	out := make([]byte, nonceSize, nonceSize+len(plaintext)+c.aead.Overhead())
	if _, err := io.ReadFull(readerFunc(c.randRead), out[:nonceSize]); err != nil {
		return nil, fmt.Errorf("aesgcm: read nonce: %w", err)
	}
	out = c.aead.Seal(out, out[:nonceSize], plaintext, nil)
	return out, nil
}

// Decrypt is the inverse of Encrypt. Returns ErrShortCiphertext if
// the input cannot possibly carry a nonce + tag, or the underlying
// GCM auth error if the ciphertext was tampered with — the error is
// surfaced verbatim so callers can errors.As-check the standard
// library type for tighter handling.
func (c *SeedCipher) Decrypt(ciphertext []byte) ([]byte, error) {
	if len(ciphertext) <= nonceSize {
		return nil, ErrShortCiphertext
	}
	nonce, body := ciphertext[:nonceSize], ciphertext[nonceSize:]
	plaintext, err := c.aead.Open(nil, nonce, body, nil)
	if err != nil {
		return nil, fmt.Errorf("aesgcm: open: %w", err)
	}
	return plaintext, nil
}

// readerFunc adapts a func([]byte) (int, error) into io.Reader so we
// can reuse io.ReadFull for the nonce-drawing path. Cheaper than
// keeping a separate reader field on the struct.
type readerFunc func([]byte) (int, error)

func (f readerFunc) Read(p []byte) (int, error) { return f(p) }
