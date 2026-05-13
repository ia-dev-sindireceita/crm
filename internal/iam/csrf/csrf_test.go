package csrf

import (
	"encoding/base64"
	"errors"
	"testing"
)

func TestGenerateToken_LengthAndUniqueness(t *testing.T) {
	seen := make(map[string]struct{}, 100)
	for i := 0; i < 100; i++ {
		tok, err := GenerateToken()
		if err != nil {
			t.Fatalf("GenerateToken: %v", err)
		}
		raw, err := base64.RawURLEncoding.DecodeString(tok)
		if err != nil {
			t.Fatalf("token is not valid raw URL base64: %v", err)
		}
		if len(raw) != TokenBytes {
			t.Fatalf("decoded token length = %d, want %d", len(raw), TokenBytes)
		}
		if _, dup := seen[tok]; dup {
			t.Fatalf("duplicate token in 100 GenerateToken calls — entropy broken")
		}
		seen[tok] = struct{}{}
	}
}

func TestVerify(t *testing.T) {
	const session = "session-token-value-32B-base64x"
	const other = "totally-different-token-value-x"

	cases := []struct {
		name         string
		sessionToken string
		header       string
		form         string
		want         error
	}{
		{"both-missing-rejects", session, "", "", ErrTokenMissing},
		{"session-token-empty-rejects", "", "x", "", ErrSessionTokenMissing},
		{"header-only-match", session, session, "", nil},
		{"form-only-match", session, "", session, nil},
		{"header-mismatch", session, other, "", ErrTokenMismatch},
		{"form-mismatch-when-no-header", session, "", other, ErrTokenMismatch},
		{"header-wins-over-form-mismatch", session, other, session, ErrTokenMismatch},
		{"header-wins-over-form-match", session, session, other, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Verify(tc.sessionToken, tc.header, tc.form)
			if !errors.Is(got, tc.want) {
				t.Fatalf("Verify = %v, want %v", got, tc.want)
			}
		})
	}
}
