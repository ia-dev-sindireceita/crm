package mfa

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
)

func TestGenerateRecoveryCodes_ShapeAndAlphabet(t *testing.T) {
	codes, err := GenerateRecoveryCodes(nil)
	if err != nil {
		t.Fatalf("GenerateRecoveryCodes: %v", err)
	}
	if len(codes) != RecoveryCodeCount {
		t.Fatalf("count: got %d want %d", len(codes), RecoveryCodeCount)
	}
	seen := make(map[string]struct{}, len(codes))
	for i, c := range codes {
		if len(c) != RecoveryCodeLen {
			t.Errorf("code %d: len=%d want %d (%q)", i, len(c), RecoveryCodeLen, c)
		}
		for _, r := range c {
			if !isBase32Char(r) {
				t.Errorf("code %d: rune %q outside RFC 4648 alphabet", i, r)
			}
		}
		if _, dup := seen[c]; dup {
			t.Errorf("duplicate code at index %d: %q", i, c)
		}
		seen[c] = struct{}{}
	}
}

func TestGenerateRecoveryCodes_DeterministicWithSeed(t *testing.T) {
	// Reproducible reader so we can pin the exact codes a fixed seed
	// produces. This is what guards us against a future refactor that
	// silently swaps the encoding alphabet or slice width.
	src := bytes.NewReader(bytes.Repeat([]byte{0x00}, recoveryCodeRawBytes*RecoveryCodeCount))
	codes, err := GenerateRecoveryCodes(src)
	if err != nil {
		t.Fatalf("GenerateRecoveryCodes: %v", err)
	}
	want := strings.Repeat("A", RecoveryCodeLen) // 0x00 → 'A' in base32
	for i, c := range codes {
		if c != want {
			t.Errorf("code %d: got %q want %q", i, c, want)
		}
	}
}

func TestGenerateRecoveryCodes_ReaderError(t *testing.T) {
	_, err := GenerateRecoveryCodes(failingReader{})
	if err == nil {
		t.Fatalf("expected error from failing reader, got nil")
	}
}

type failingReader struct{}

func (failingReader) Read(_ []byte) (int, error) { return 0, io.ErrUnexpectedEOF }

func TestFormatRecoveryCode(t *testing.T) {
	// Sample codes are valid RFC 4648 base32 (A–Z, 2–7).
	cases := []struct {
		in, want string
	}{
		{"ABCDE23456", "ABCDE-23456"},
		{"AAAAAAAAAA", "AAAAA-AAAAA"},
		{"too-short", "too-short"},                     // not RecoveryCodeLen → returned as-is
		{"WAYTOOLONGCODE23456", "WAYTOOLONGCODE23456"}, // wrong length → returned as-is
	}
	for _, c := range cases {
		if got := FormatRecoveryCode(c.in); got != c.want {
			t.Errorf("FormatRecoveryCode(%q): got %q want %q", c.in, got, c.want)
		}
	}
}

func TestNormalizeRecoveryCode(t *testing.T) {
	cases := []struct {
		name, in, want string
		wantErr        bool
	}{
		{"canonical", "ABCDE23456", "ABCDE23456", false},
		{"with-dash", "ABCDE-23456", "ABCDE23456", false},
		{"lowercase-with-dash", "abcde-23456", "ABCDE23456", false},
		{"with-spaces", "ab cde 23456", "ABCDE23456", false},
		{"too-short", "ABCDE", "", true},
		{"too-long", "ABCDEABCDE23", "", true},
		{"illegal-rune-0", "ABCDE23450", "", true}, // 0 is not in base32 alphabet
		{"illegal-rune-1", "ABCDE23451", "", true}, // 1 is not in base32 alphabet
		{"illegal-rune-8", "ABCDE23458", "", true}, // 8 is not in base32 alphabet
		{"illegal-rune-9", "ABCDE23459", "", true}, // 9 is not in base32 alphabet
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := NormalizeRecoveryCode(c.in)
			if c.wantErr {
				if !errors.Is(err, ErrCodeFormat) {
					t.Fatalf("err: got %v want ErrCodeFormat", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != c.want {
				t.Fatalf("normalized: got %q want %q", got, c.want)
			}
		})
	}
}
