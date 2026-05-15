package metashared_test

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"testing"

	"github.com/pericles-luz/crm/internal/adapter/channels/metashared"
)

const testSecret = "super-secret-app-key"

// signHex returns the expected `sha256=<hex>` signature for body.
func signHex(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func TestVerifySignature_HappyPath(t *testing.T) {
	t.Parallel()
	body := []byte(`{"entry":[{"id":"1","time":1700000000}]}`)
	if err := metashared.VerifySignature(testSecret, body, signHex(testSecret, body)); err != nil {
		t.Fatalf("VerifySignature: %v", err)
	}
}

func TestVerifySignature_AcceptsHeaderWithoutPrefix(t *testing.T) {
	t.Parallel()
	body := []byte(`{"x":1}`)
	mac := hmac.New(sha256.New, []byte(testSecret))
	_, _ = mac.Write(body)
	bare := hex.EncodeToString(mac.Sum(nil))
	if err := metashared.VerifySignature(testSecret, body, bare); err != nil {
		t.Fatalf("VerifySignature without prefix: %v", err)
	}
}

func TestVerifySignature_TrimsWhitespace(t *testing.T) {
	t.Parallel()
	body := []byte(`{"x":1}`)
	sig := "  " + signHex(testSecret, body) + "  "
	if err := metashared.VerifySignature(testSecret, body, sig); err != nil {
		t.Fatalf("VerifySignature with surrounding whitespace: %v", err)
	}
}

func TestVerifySignature_Errors(t *testing.T) {
	t.Parallel()
	body := []byte(`{"x":1}`)
	cases := []struct {
		name    string
		header  string
		wantErr error
	}{
		{"empty", "", metashared.ErrSignatureMissing},
		{"whitespace", "   ", metashared.ErrSignatureMissing},
		{"bad hex", "sha256=not-hex-XYZ", metashared.ErrSignatureFormat},
		{"tampered", "sha256=deadbeef", metashared.ErrSignatureMismatch},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := metashared.VerifySignature(testSecret, body, tc.header)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v, want %v", err, tc.wantErr)
			}
		})
	}
}

func TestVerifySignature_WrongSecret_Mismatch(t *testing.T) {
	t.Parallel()
	body := []byte(`{"x":1}`)
	sigFromWrongSecret := signHex("WRONG_SECRET", body)
	if err := metashared.VerifySignature(testSecret, body, sigFromWrongSecret); !errors.Is(err, metashared.ErrSignatureMismatch) {
		t.Fatalf("err = %v, want ErrSignatureMismatch", err)
	}
}

// TestVerifySignature_BitFlip_DetectsTamper mutates a single byte of
// the body and asserts the signature no longer matches — the
// constant-time hmac.Equal check is what makes this a security
// guarantee rather than a hash-collision approximation.
func TestVerifySignature_BitFlip_DetectsTamper(t *testing.T) {
	t.Parallel()
	body := []byte(`{"entry":[{"id":"1","time":1700000000}]}`)
	sig := signHex(testSecret, body)
	tampered := append([]byte(nil), body...)
	tampered[0] = '['
	if err := metashared.VerifySignature(testSecret, tampered, sig); !errors.Is(err, metashared.ErrSignatureMismatch) {
		t.Fatalf("err = %v, want ErrSignatureMismatch", err)
	}
}

// TestVerifySignature_EmptyPayload_StillValidatable proves that an
// empty body produces a stable HMAC. Carriers occasionally post empty
// keep-alive envelopes; we accept them when the signature matches.
func TestVerifySignature_EmptyPayload_StillValidatable(t *testing.T) {
	t.Parallel()
	body := []byte{}
	if err := metashared.VerifySignature(testSecret, body, signHex(testSecret, body)); err != nil {
		t.Fatalf("VerifySignature empty body: %v", err)
	}
}

func TestSignatureHeader_Constant(t *testing.T) {
	t.Parallel()
	if metashared.SignatureHeader != "X-Hub-Signature-256" {
		t.Fatalf("SignatureHeader = %q", metashared.SignatureHeader)
	}
}
