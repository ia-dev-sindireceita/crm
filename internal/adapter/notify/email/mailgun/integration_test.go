package mailgun_test

import (
	"context"
	"os"
	"testing"

	"github.com/pericles-luz/crm/internal/adapter/notify/email/mailgun"
	"github.com/pericles-luz/crm/internal/notify/email"
)

// TestIntegration_SendAgainstRealMailgun is the opt-in soak test that
// exercises the adapter against a real Mailgun account. It is gated by
// MAILGUN_INTEGRATION_TESTS=1 so CI never invokes it (no credentials
// exist in CI). A developer running it locally must export:
//
//	MAILGUN_INTEGRATION_TESTS=1
//	MAILGUN_API_KEY=key-...
//	MAILGUN_DOMAIN=mg.example.com   # an authenticated sending domain
//	MAILGUN_REGION=us               # or eu
//	MAILGUN_INTEGRATION_TO=destination@example.com
//
// The test sends one minimal text-only message and asserts no error.
// It DOES NOT assert delivery — that is observable only via the
// recipient mailbox. The goal is to verify the on-the-wire request
// shape still satisfies a real Mailgun account, end-to-end.
func TestIntegration_SendAgainstRealMailgun(t *testing.T) {
	if os.Getenv("MAILGUN_INTEGRATION_TESTS") != "1" {
		t.Skip("set MAILGUN_INTEGRATION_TESTS=1 to run the live Mailgun test")
	}
	apiKey := os.Getenv("MAILGUN_API_KEY")
	domain := os.Getenv("MAILGUN_DOMAIN")
	region := mailgun.Region(os.Getenv("MAILGUN_REGION"))
	to := os.Getenv("MAILGUN_INTEGRATION_TO")
	if apiKey == "" || domain == "" || region == "" || to == "" {
		t.Fatal("MAILGUN_API_KEY, MAILGUN_DOMAIN, MAILGUN_REGION, MAILGUN_INTEGRATION_TO must all be set")
	}
	s, err := mailgun.New(mailgun.Config{APIKey: apiKey, Domain: domain, Region: region})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	err = s.Send(context.Background(), email.Message{
		From:    email.Address{Email: "noreply@" + domain, Name: "CRM Integration"},
		To:      []email.Address{{Email: to}},
		Subject: "CRM integration smoke test",
		Text:    "If you read this, the Mailgun adapter is live.",
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
}
