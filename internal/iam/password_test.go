package iam

import (
	"errors"
	"strings"
	"testing"
)

func TestHashPassword_RoundTrip(t *testing.T) {
	encoded, err := HashPassword("correct-horse-battery-staple")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	if !strings.HasPrefix(encoded, "$argon2id$v=19$m=65536,t=3,p=4$") {
		t.Fatalf("encoded prefix wrong: %q", encoded)
	}
	ok, err := VerifyPassword("correct-horse-battery-staple", encoded)
	if err != nil || !ok {
		t.Fatalf("VerifyPassword: ok=%v err=%v", ok, err)
	}
	bad, err := VerifyPassword("wrong-password", encoded)
	if err != nil || bad {
		t.Fatalf("VerifyPassword wrong: ok=%v err=%v", bad, err)
	}
}

func TestHashPassword_SaltUnique(t *testing.T) {
	a, _ := HashPassword("same-input")
	b, _ := HashPassword("same-input")
	if a == b {
		t.Fatalf("two HashPassword calls with same input produced identical encoding (salt not random)")
	}
}

func TestVerifyPassword_RejectsMalformed(t *testing.T) {
	cases := map[string]string{
		"empty":            "",
		"no-prefix":        "argon2id$v=19$m=65536,t=3,p=4$abcdABCD$abcdABCD",
		"wrong-algo":       "$argon2i$v=19$m=65536,t=3,p=4$YWJjZGFiY2RhYmNkYWJjZA$YWJjZGFiY2RhYmNkYWJjZGFiY2RhYmNkYWJjZGFiY2RhYmNkYWJjZA",
		"wrong-version":    "$argon2id$v=18$m=65536,t=3,p=4$YWJjZGFiY2RhYmNkYWJjZA$YWJjZGFiY2RhYmNkYWJjZGFiY2RhYmNkYWJjZGFiY2RhYmNkYWJjZA",
		"downgrade-memory": "$argon2id$v=19$m=4096,t=3,p=4$YWJjZGFiY2RhYmNkYWJjZA$YWJjZGFiY2RhYmNkYWJjZGFiY2RhYmNkYWJjZGFiY2RhYmNkYWJjZA",
		"downgrade-time":   "$argon2id$v=19$m=65536,t=1,p=4$YWJjZGFiY2RhYmNkYWJjZA$YWJjZGFiY2RhYmNkYWJjZGFiY2RhYmNkYWJjZGFiY2RhYmNkYWJjZA",
		"missing-params":   "$argon2id$v=19$$YWJjZGFiY2RhYmNkYWJjZA$YWJjZGFiY2RhYmNkYWJjZGFiY2RhYmNkYWJjZGFiY2RhYmNkYWJjZA",
		"non-numeric-mem":  "$argon2id$v=19$m=abc,t=3,p=4$YWJjZGFiY2RhYmNkYWJjZA$YWJjZGFiY2RhYmNkYWJjZGFiY2RhYmNkYWJjZGFiY2RhYmNkYWJjZA",
		"bad-base64-salt":  "$argon2id$v=19$m=65536,t=3,p=4$!!!!!!$YWJjZGFiY2RhYmNkYWJjZGFiY2RhYmNkYWJjZGFiY2RhYmNkYWJjZA",
		"short-salt":       "$argon2id$v=19$m=65536,t=3,p=4$YWJjZA$YWJjZGFiY2RhYmNkYWJjZGFiY2RhYmNkYWJjZGFiY2RhYmNkYWJjZA",
		"short-hash":       "$argon2id$v=19$m=65536,t=3,p=4$YWJjZGFiY2RhYmNkYWJjZA$YWJjZA",
		"too-many-fields":  "$argon2id$v=19$m=65536,t=3,p=4$YWJjZGFiY2RhYmNkYWJjZA$YWJjZGFiY2RhYmNkYWJjZGFiY2RhYmNkYWJjZGFiY2RhYmNkYWJjZA$extra",
	}
	for name, encoded := range cases {
		t.Run(name, func(t *testing.T) {
			ok, err := VerifyPassword("anything", encoded)
			if ok {
				t.Fatalf("ok=true for malformed input %q", name)
			}
			if !errors.Is(err, ErrInvalidEncoding) {
				t.Fatalf("err=%v want ErrInvalidEncoding", err)
			}
		})
	}
}

func TestVerifyPassword_NoPanicOnAdversarial(t *testing.T) {
	// Property-style: any garbage input must return either (false, nil)
	// or (false, ErrInvalidEncoding); never panic.
	cases := []string{"", "$", "$$$$$", "$argon2id$", "$argon2id$v=19$"}
	for _, encoded := range cases {
		t.Run(encoded, func(t *testing.T) {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("panic on adversarial input %q: %v", encoded, r)
				}
			}()
			_, _ = VerifyPassword("x", encoded)
		})
	}
}
