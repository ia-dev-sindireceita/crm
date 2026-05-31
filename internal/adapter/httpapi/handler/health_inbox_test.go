package handler_test

// SIN-63825 / SIN-63793 W6 — tests for the WithInboxChannelProvider
// option on handler.Health. The staging smoke
// (scripts/ci/stg-smoke-inbox.sh) refuses to proceed unless /health
// reports inbox_channel_provider="llmcustomer", so the JSON shape and
// the omitempty default are both load-bearing.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/pericles-luz/crm/internal/adapter/httpapi/handler"
)

// TestHealth_DefaultOmitsInboxChannelProvider locks the legacy JSON
// shape: pre-W6 callers that pass only the SHA MUST not see the new
// inbox_channel_provider field. The smoke parses the field with strict
// equality, but the LB / oncall tooling that reads /health predates
// W6 and expects the original shape.
func TestHealth_DefaultOmitsInboxChannelProvider(t *testing.T) {
	t.Parallel()
	rec := httptest.NewRecorder()
	handler.Health("0123456789abcdef0123456789abcdef01234567").ServeHTTP(
		rec,
		httptest.NewRequest(http.MethodGet, "/health", nil),
	)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if strings.Contains(body, "inbox_channel_provider") {
		t.Fatalf("body=%q must omit inbox_channel_provider when option not set", body)
	}
}

// TestHealth_WithInboxChannelProvider_RendersField proves the smoke
// pre-condition probe works: when cmd/server passes the resolved
// INBOX_CHANNEL_PROVIDER value (e.g. "llmcustomer"), /health renders
// it verbatim so scripts/ci/stg-smoke-inbox.sh can match
// .inbox_channel_provider == "llmcustomer" via jq.
func TestHealth_WithInboxChannelProvider_RendersField(t *testing.T) {
	t.Parallel()
	rec := httptest.NewRecorder()
	handler.Health(
		"0123456789abcdef0123456789abcdef01234567",
		handler.WithInboxChannelProvider("llmcustomer"),
	).ServeHTTP(
		rec,
		httptest.NewRequest(http.MethodGet, "/health", nil),
	)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200", rec.Code)
	}
	var body map[string]string
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got := body["inbox_channel_provider"]; got != "llmcustomer" {
		t.Fatalf("inbox_channel_provider=%q, want %q", got, "llmcustomer")
	}
	if body["status"] != "ok" {
		t.Fatalf("status=%q, want ok", body["status"])
	}
	// commit_sha must still be present alongside the new field so the
	// SIN-63146 deploy gate is unaffected by the W6 addition.
	if _, ok := body["commit_sha"]; !ok {
		t.Fatalf("body missing commit_sha field; got %v", body)
	}
}

// TestHealth_WithInboxChannelProvider_EmptyOmits ensures an explicit
// empty string also omits the field. cmd/server's boot path keeps the
// pre-W6 default safely — a misconfigured provider read MUST NOT
// surface as `"inbox_channel_provider":""`, which would silently
// false-pass the smoke's strict-equality check against "llmcustomer".
func TestHealth_WithInboxChannelProvider_EmptyOmits(t *testing.T) {
	t.Parallel()
	rec := httptest.NewRecorder()
	handler.Health(
		"0123456789abcdef0123456789abcdef01234567",
		handler.WithInboxChannelProvider(""),
	).ServeHTTP(
		rec,
		httptest.NewRequest(http.MethodGet, "/health", nil),
	)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if strings.Contains(body, "inbox_channel_provider") {
		t.Fatalf("body=%q must omit inbox_channel_provider when WithInboxChannelProvider(\"\")", body)
	}
}

// TestHealth_OptionOrder_LastWins documents the contract for repeated
// options: the last invocation wins. Defensive coverage so a future
// caller chaining WithInboxChannelProvider doesn't get surprised by an
// implementation that silently dropped or merged values.
func TestHealth_OptionOrder_LastWins(t *testing.T) {
	t.Parallel()
	rec := httptest.NewRecorder()
	handler.Health(
		"0123456789abcdef0123456789abcdef01234567",
		handler.WithInboxChannelProvider("disabled"),
		handler.WithInboxChannelProvider("llmcustomer"),
	).ServeHTTP(
		rec,
		httptest.NewRequest(http.MethodGet, "/health", nil),
	)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200", rec.Code)
	}
	var body map[string]string
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got := body["inbox_channel_provider"]; got != "llmcustomer" {
		t.Fatalf("last-wins: got %q, want %q", got, "llmcustomer")
	}
}
