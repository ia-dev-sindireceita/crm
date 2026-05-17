package inter_test

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/pericles-luz/crm/internal/adapter/pix/inter"
	"github.com/pericles-luz/crm/internal/billing/pix"
)

func hexHMAC(secret, body []byte) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

func TestNewWebhookVerifier_RequiresSecret(t *testing.T) {
	if _, err := inter.NewWebhookVerifier(inter.WebhookConfig{}); !errors.Is(err, inter.ErrMissingConfig) {
		t.Fatalf("empty secret returned %v, want ErrMissingConfig", err)
	}
}

func TestWebhookVerifier_VerifyHappyPath(t *testing.T) {
	v, err := inter.NewWebhookVerifier(inter.WebhookConfig{Secret: "topsecret"})
	if err != nil {
		t.Fatalf("NewWebhookVerifier: %v", err)
	}
	if got, want := v.HeaderName(), inter.DefaultSignatureHeader; got != want {
		t.Errorf("HeaderName = %q, want %q", got, want)
	}
	body := []byte(`{"pix":[{"txid":"X"}]}`)
	sig := hexHMAC([]byte("topsecret"), body)
	headers := map[string][]string{inter.DefaultSignatureHeader: {sig}}
	if err := v.Verify(body, headers); err != nil {
		t.Fatalf("Verify happy path: %v", err)
	}
}

func TestWebhookVerifier_VerifyMissingHeader(t *testing.T) {
	v, _ := inter.NewWebhookVerifier(inter.WebhookConfig{Secret: "s"})
	if err := v.Verify([]byte(`x`), nil); !errors.Is(err, inter.ErrSignatureMissing) {
		t.Errorf("Verify(nil) = %v, want ErrSignatureMissing", err)
	}
	headers := map[string][]string{inter.DefaultSignatureHeader: {""}}
	if err := v.Verify([]byte(`x`), headers); !errors.Is(err, inter.ErrSignatureMissing) {
		t.Errorf("Verify(empty header) = %v, want ErrSignatureMissing", err)
	}
}

func TestWebhookVerifier_VerifyTamperedBody(t *testing.T) {
	v, _ := inter.NewWebhookVerifier(inter.WebhookConfig{Secret: "topsecret"})
	body := []byte(`{"pix":[{"txid":"X"}]}`)
	sig := hexHMAC([]byte("topsecret"), body)
	tampered := []byte(`{"pix":[{"txid":"Y"}]}`)
	headers := map[string][]string{inter.DefaultSignatureHeader: {sig}}
	if err := v.Verify(tampered, headers); !errors.Is(err, inter.ErrSignatureInvalid) {
		t.Errorf("Verify(tampered) = %v, want ErrSignatureInvalid", err)
	}
}

func TestWebhookVerifier_VerifyWrongSecret(t *testing.T) {
	v, _ := inter.NewWebhookVerifier(inter.WebhookConfig{Secret: "topsecret"})
	body := []byte(`{}`)
	sig := hexHMAC([]byte("wrong"), body)
	headers := map[string][]string{inter.DefaultSignatureHeader: {sig}}
	if err := v.Verify(body, headers); !errors.Is(err, inter.ErrSignatureInvalid) {
		t.Errorf("Verify(wrong secret) = %v, want ErrSignatureInvalid", err)
	}
}

func TestWebhookVerifier_VerifyMalformedHexAndPrefix(t *testing.T) {
	v, _ := inter.NewWebhookVerifier(inter.WebhookConfig{Secret: "topsecret"})
	body := []byte(`x`)
	headers := map[string][]string{inter.DefaultSignatureHeader: {"NOT_HEX"}}
	if err := v.Verify(body, headers); !errors.Is(err, inter.ErrSignatureInvalid) {
		t.Errorf("Verify(non-hex) = %v, want ErrSignatureInvalid", err)
	}
	sig := "sha256=" + hexHMAC([]byte("topsecret"), body)
	headers = map[string][]string{inter.DefaultSignatureHeader: {sig}}
	if err := v.Verify(body, headers); err != nil {
		t.Errorf("Verify(sha256= prefix) = %v, want nil", err)
	}
}

func TestWebhookVerifier_VerifyCustomHeaderAndCase(t *testing.T) {
	v, _ := inter.NewWebhookVerifier(inter.WebhookConfig{
		Secret:          "topsecret",
		SignatureHeader: "x-custom-sig",
	})
	body := []byte(`hello`)
	sig := hexHMAC([]byte("topsecret"), body)
	// Header lookup MUST be case-insensitive for raw maps that come in
	// non-canonical form.
	headers := map[string][]string{"x-custom-sig": {sig}}
	if err := v.Verify(body, headers); err != nil {
		t.Errorf("Verify(custom-header lowercase) = %v, want nil", err)
	}
}

func TestNewWebhookParser_HappyPath(t *testing.T) {
	parser := inter.NewWebhookParser()
	body := []byte(`{"pix":[{"endToEndId":"E1","txid":"tx-abc","valor":"12.34","horario":"2026-05-17T12:00:00Z"}]}`)
	evts, err := parser.Parse(body)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(evts) != 1 {
		t.Fatalf("got %d events, want 1", len(evts))
	}
	if got, want := evts[0].Source, inter.SourceName; got != want {
		t.Errorf("Source = %q, want %q", got, want)
	}
	if got, want := evts[0].ExternalID, "tx-abc"; got != want {
		t.Errorf("ExternalID = %q, want %q", got, want)
	}
	if got, want := evts[0].EventType, pix.WebhookEventPaid; got != want {
		t.Errorf("EventType = %q, want %q", got, want)
	}
	if got, want := evts[0].OccurredAt.UTC(), time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC); !got.Equal(want) {
		t.Errorf("OccurredAt = %v, want %v", got, want)
	}
	// Payload is the per-item re-encoded JSON, NOT the full envelope.
	if !strings.Contains(string(evts[0].Payload), `"txid":"tx-abc"`) {
		t.Errorf("payload missing txid: %s", evts[0].Payload)
	}
}

func TestWebhookParser_MultipleItems(t *testing.T) {
	parser := inter.NewWebhookParser()
	body := []byte(`{"pix":[{"txid":"a","horario":"2026-05-17T01:00:00Z"},{"txid":"b","horario":"2026-05-17T02:00:00Z"}]}`)
	evts, err := parser.Parse(body)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(evts) != 2 {
		t.Fatalf("got %d events, want 2", len(evts))
	}
	if evts[0].ExternalID != "a" || evts[1].ExternalID != "b" {
		t.Errorf("ordering broken: %q %q", evts[0].ExternalID, evts[1].ExternalID)
	}
}

func TestWebhookParser_MissingHorarioFallsBackToNow(t *testing.T) {
	fixed := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	parser := inter.NewWebhookParser().WithNow(func() time.Time { return fixed })
	body := []byte(`{"pix":[{"txid":"only","horario":""}]}`)
	evts, err := parser.Parse(body)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !evts[0].OccurredAt.Equal(fixed) {
		t.Errorf("OccurredAt = %v, want %v (fallback)", evts[0].OccurredAt, fixed)
	}
}

func TestWebhookParser_BadInputs(t *testing.T) {
	parser := inter.NewWebhookParser()
	cases := []struct {
		name string
		body []byte
	}{
		{"empty", nil},
		{"non-json", []byte(`not json`)},
		{"no-pix", []byte(`{}`)},
		{"pix-empty", []byte(`{"pix":[]}`)},
		{"missing-txid", []byte(`{"pix":[{"txid":""}]}`)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parser.Parse(tc.body)
			if !errors.Is(err, inter.ErrParse) {
				t.Errorf("Parse(%s) = %v, want ErrParse", tc.name, err)
			}
		})
	}
}

func TestParseCIDRList(t *testing.T) {
	cases := []struct {
		raw  string
		want int
	}{
		{"", 0},
		{"   ", 0},
		{"10.0.0.0/8", 1},
		{"10.0.0.0/8, 172.16.0.0/12 , bad", 2},
		{",,,", 0},
	}
	for _, tc := range cases {
		t.Run(tc.raw, func(t *testing.T) {
			got := inter.ParseCIDRList(tc.raw)
			if len(got) != tc.want {
				t.Errorf("ParseCIDRList(%q) yielded %d, want %d", tc.raw, len(got), tc.want)
			}
		})
	}
}

func TestIPAllow(t *testing.T) {
	cidrs := inter.ParseCIDRList("10.0.0.0/8,192.168.1.0/24")
	cases := []struct {
		ip   string
		want bool
	}{
		{"10.1.2.3", true},
		{"192.168.1.42", true},
		{"192.168.2.1", false},
		{"127.0.0.1", false},
		{"", false},
	}
	for _, tc := range cases {
		t.Run(tc.ip, func(t *testing.T) {
			peer := net.ParseIP(tc.ip)
			if got := inter.IPAllow(peer, cidrs); got != tc.want {
				t.Errorf("IPAllow(%q) = %v, want %v", tc.ip, got, tc.want)
			}
		})
	}
	if inter.IPAllow(net.ParseIP("10.0.0.1"), nil) {
		t.Error("nil cidrs should deny")
	}
}

func TestDefaultAllowedCIDRSet(t *testing.T) {
	set := inter.DefaultAllowedCIDRSet()
	if len(set) == 0 {
		t.Fatal("DefaultAllowedCIDRSet returned empty slice")
	}
	if !inter.IPAllow(net.ParseIP("127.0.0.1"), set) {
		t.Errorf("default allowlist should contain loopback")
	}
}
