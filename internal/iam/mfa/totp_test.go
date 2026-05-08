package mfa

import (
	"bytes"
	"errors"
	"net/url"
	"strings"
	"testing"
	"time"
)

// rfc6238Secret is the 20-byte ASCII secret RFC 6238 §Appendix B uses
// for the SHA1 vector set.
var rfc6238Secret = []byte("12345678901234567890")

// rfc6238Vectors are the RFC 6238 §Appendix B SHA1 8-digit values,
// truncated to the last 6 digits because ADR 0074 §1 pins digits=6.
// (Truncation is consistent with the dynamic-truncation step itself —
// only the modulus changes.)
var rfc6238Vectors = []struct {
	unix int64
	want string
}{
	{59, "287082"},
	{1111111109, "081804"},
	{1111111111, "050471"},
	{1234567890, "005924"},
	{2000000000, "279037"},
	{20000000000, "353130"},
}

func TestGenerate_RFC6238Vectors(t *testing.T) {
	for _, v := range rfc6238Vectors {
		t.Run(time.Unix(v.unix, 0).UTC().Format(time.RFC3339), func(t *testing.T) {
			got, err := Generate(rfc6238Secret, time.Unix(v.unix, 0))
			if err != nil {
				t.Fatalf("Generate: %v", err)
			}
			if got != v.want {
				t.Fatalf("got %q want %q", got, v.want)
			}
		})
	}
}

func TestGenerate_SeedTooShort(t *testing.T) {
	_, err := Generate(bytes.Repeat([]byte{0}, totpSeedMin-1), time.Unix(0, 0))
	if !errors.Is(err, ErrSeedTooShort) {
		t.Fatalf("err: got %v want ErrSeedTooShort", err)
	}
}

func TestVerify_AcceptsCurrentStep(t *testing.T) {
	for _, v := range rfc6238Vectors {
		t.Run(v.want, func(t *testing.T) {
			err := Verify(rfc6238Secret, v.want, time.Unix(v.unix, 0), 1)
			if err != nil {
				t.Fatalf("Verify: %v", err)
			}
		})
	}
}

func TestVerify_AcceptsWithinWindow(t *testing.T) {
	// At Unix=59, the verifier with window=1 must also accept the codes
	// that derive from steps 59-30 and 59+30. We compute those expected
	// codes directly so the assertion isn't tautological.
	prevWant, err := Generate(rfc6238Secret, time.Unix(59-int64(totpStep.Seconds()), 0))
	if err != nil {
		t.Fatalf("prev Generate: %v", err)
	}
	nextWant, err := Generate(rfc6238Secret, time.Unix(59+int64(totpStep.Seconds()), 0))
	if err != nil {
		t.Fatalf("next Generate: %v", err)
	}
	if err := Verify(rfc6238Secret, prevWant, time.Unix(59, 0), 1); err != nil {
		t.Fatalf("prev step inside window: %v", err)
	}
	if err := Verify(rfc6238Secret, nextWant, time.Unix(59, 0), 1); err != nil {
		t.Fatalf("next step inside window: %v", err)
	}
}

func TestVerify_RejectsOutsideWindow(t *testing.T) {
	// Two steps away (60 seconds) must NOT verify under window=1.
	twoStepsAhead, err := Generate(rfc6238Secret, time.Unix(59+2*int64(totpStep.Seconds()), 0))
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	err = Verify(rfc6238Secret, twoStepsAhead, time.Unix(59, 0), 1)
	if !errors.Is(err, ErrInvalidCode) {
		t.Fatalf("err: got %v want ErrInvalidCode", err)
	}
}

func TestVerify_RejectsMalformedInput(t *testing.T) {
	cases := []struct {
		name, in string
	}{
		{"empty", ""},
		{"too-short", "12345"},
		{"too-long", "1234567"},
		{"non-digit", "abcdef"},
		{"leading-space", " 12345"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := Verify(rfc6238Secret, c.in, time.Unix(59, 0), 1)
			if !errors.Is(err, ErrInvalidCode) {
				t.Fatalf("err: got %v want ErrInvalidCode", err)
			}
		})
	}
}

func TestVerify_RejectsShortSeed(t *testing.T) {
	err := Verify(bytes.Repeat([]byte{0}, totpSeedMin-1), "287082", time.Unix(59, 0), 1)
	if !errors.Is(err, ErrSeedTooShort) {
		t.Fatalf("err: got %v want ErrSeedTooShort", err)
	}
}

func TestNewSecret_LengthAndRandomness(t *testing.T) {
	a, err := NewSecret(nil)
	if err != nil {
		t.Fatalf("NewSecret: %v", err)
	}
	b, err := NewSecret(nil)
	if err != nil {
		t.Fatalf("NewSecret: %v", err)
	}
	if len(a) != totpSeedSize {
		t.Errorf("seed size: got %d want %d", len(a), totpSeedSize)
	}
	if bytes.Equal(a, b) {
		t.Errorf("two seeds collided — random source not seeded?")
	}
}

func TestEncodeDecodeSecret_RoundTrip(t *testing.T) {
	seed, err := NewSecret(nil)
	if err != nil {
		t.Fatalf("NewSecret: %v", err)
	}
	enc, err := EncodeSecret(seed)
	if err != nil {
		t.Fatalf("EncodeSecret: %v", err)
	}
	dec, err := DecodeSecret(enc)
	if err != nil {
		t.Fatalf("DecodeSecret: %v", err)
	}
	if !bytes.Equal(seed, dec) {
		t.Fatalf("round trip diverged: %x vs %x", seed, dec)
	}
}

func TestEncodeSecret_RejectsShortSeed(t *testing.T) {
	_, err := EncodeSecret(bytes.Repeat([]byte{0}, totpSeedMin-1))
	if !errors.Is(err, ErrSeedTooShort) {
		t.Fatalf("err: got %v want ErrSeedTooShort", err)
	}
}

func TestDecodeSecret_RejectsShortInput(t *testing.T) {
	short, _ := totpAlphabet.DecodeString("AAAA") // 2.5 bytes < 20
	_ = short
	_, err := DecodeSecret("AAAA")
	if !errors.Is(err, ErrSeedTooShort) {
		t.Fatalf("err: got %v want ErrSeedTooShort", err)
	}
}

func TestDecodeSecret_RejectsMalformed(t *testing.T) {
	_, err := DecodeSecret("not!base32")
	if err == nil {
		t.Fatalf("expected error on malformed base32 input")
	}
}

func TestOTPAuthURI_StructureAndQuery(t *testing.T) {
	uri, err := OTPAuthURI("Sindireceita", "ops@example.com", rfc6238Secret)
	if err != nil {
		t.Fatalf("OTPAuthURI: %v", err)
	}
	parsed, err := url.Parse(uri)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if parsed.Scheme != "otpauth" || parsed.Host != "totp" {
		t.Errorf("scheme/host: got %q://%q want otpauth://totp", parsed.Scheme, parsed.Host)
	}
	if want := "Sindireceita:ops@example.com"; !strings.Contains(parsed.Path, want) {
		t.Errorf("path: %q does not contain %q", parsed.Path, want)
	}
	q := parsed.Query()
	encodedSeed, _ := EncodeSecret(rfc6238Secret)
	if q.Get("secret") != encodedSeed {
		t.Errorf("secret param: got %q want %q", q.Get("secret"), encodedSeed)
	}
	if q.Get("algorithm") != "SHA1" {
		t.Errorf("algorithm: got %q want SHA1", q.Get("algorithm"))
	}
	if q.Get("digits") != "6" {
		t.Errorf("digits: got %q want 6", q.Get("digits"))
	}
	if q.Get("period") != "30" {
		t.Errorf("period: got %q want 30", q.Get("period"))
	}
	if q.Get("issuer") != "Sindireceita" {
		t.Errorf("issuer: got %q want Sindireceita", q.Get("issuer"))
	}
}

func TestOTPAuthURI_RejectsShortSeed(t *testing.T) {
	_, err := OTPAuthURI("Sindireceita", "ops", bytes.Repeat([]byte{0}, totpSeedMin-1))
	if !errors.Is(err, ErrSeedTooShort) {
		t.Fatalf("err: got %v want ErrSeedTooShort", err)
	}
}
