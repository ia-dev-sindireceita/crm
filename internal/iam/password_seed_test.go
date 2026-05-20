package iam

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestStgSeed_PasswordHashes_VerifyAgainstStgPassword guards against the
// SIN-63154 class of bug: migrations/seed/stg.sql shipping password
// hashes that VerifyPassword cannot consume.
//
// Staging deploy of SIN-63152 surfaced that the seed carried bcrypt
// placeholders while VerifyPassword only accepts argon2id PHC, so every
// /login against the seeded users collapsed to ErrInvalidCredentials.
// This test re-derives each hash in the seed against the verifier on
// every `go test ./...` so a future edit that drifts the algorithm or
// plaintext breaks CI loudly instead of silently breaking a fresh
// staging environment.
func TestStgSeed_PasswordHashes_VerifyAgainstStgPassword(t *testing.T) {
	const plaintext = "stg-password"
	const seedRelPath = "../../migrations/seed/stg.sql"

	absPath, err := filepath.Abs(seedRelPath)
	if err != nil {
		t.Fatalf("resolve seed path: %v", err)
	}
	raw, err := os.ReadFile(seedRelPath)
	if err != nil {
		t.Fatalf("read %s: %v", absPath, err)
	}
	contents := string(raw)

	// Match the argon2id PHC body between single quotes so the closing
	// quote does not become part of the captured hash. The pattern
	// requires the leading $argon2id$ marker so SQL comments that mention
	// the algorithm in prose are not mistaken for hashes.
	hashRe := regexp.MustCompile(`'(\$argon2id\$[^']+)'`)
	matches := hashRe.FindAllStringSubmatchIndex(contents, -1)
	if want := 3; len(matches) != want {
		t.Fatalf("expected %d argon2id hashes in %s, found %d", want, absPath, len(matches))
	}

	for i, m := range matches {
		start, end := m[2], m[3]
		hash := contents[start:end]
		line := 1 + strings.Count(contents[:start], "\n")

		ok, verr := VerifyPassword(plaintext, hash)
		if verr != nil {
			t.Fatalf("hash %d at %s:%d failed to decode: %v\n  hash=%q", i+1, absPath, line, verr, hash)
		}
		if !ok {
			t.Fatalf("hash %d at %s:%d does not verify %q\n  hash=%q", i+1, absPath, line, plaintext, hash)
		}
	}
}
