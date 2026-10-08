package llmmodels_test

import (
	"testing"

	operatorrouter "github.com/pericles-luz/crm/adapters/openrouter"
	personarouter "github.com/pericles-luz/crm/internal/adapter/channels/llmcustomer/openrouter"
	"github.com/pericles-luz/crm/internal/llmmodels"
)

// TestAdapterDefaultModelsAreConsistentAndLive is the recurrence guard
// for SIN-65406 / SIN-65412. It runs with no network round-trip, so it
// is safe in CI, yet it fails the build the moment an adapter default
// points at a slug OpenRouter has retired (which would otherwise only
// surface as an upstream 404 on the first real call).
func TestAdapterDefaultModelsAreConsistentAndLive(t *testing.T) {
	// Every adapter model constant that ops can end up routing to must
	// be an allowlisted (live) slug.
	liveConstants := []struct {
		name string
		slug string
	}{
		{"adapters/openrouter.DefaultModel (operator AI-assist)", operatorrouter.DefaultModel},
		{"adapters/openrouter.FallbackModel (ops failover tier)", operatorrouter.FallbackModel},
		{"llmcustomer/openrouter.DefaultModel (persona)", personarouter.DefaultModel},
	}
	for _, tc := range liveConstants {
		t.Run(tc.name, func(t *testing.T) {
			if !llmmodels.IsLive(tc.slug) {
				t.Fatalf("%s = %q is not in the llmmodels live allowlist; a retired slug 404s on the first real round-trip. Update internal/llmmodels/allowlist.go and the constant together.", tc.name, tc.slug)
			}
		})
	}

	// SIN-65243 invariant: both LLM call points share one default model.
	if operatorrouter.DefaultModel != personarouter.DefaultModel {
		t.Fatalf("DefaultModel diverged: adapters/openrouter=%q vs llmcustomer/openrouter=%q; the two call points must share one default (SIN-65243 \"same model everywhere\").",
			operatorrouter.DefaultModel, personarouter.DefaultModel)
	}
}

// TestIsLive pins the allowlist's accept/reject behaviour, including that
// the retired slug which triggered SIN-65406 stays rejected.
func TestIsLive(t *testing.T) {
	cases := []struct {
		name string
		slug string
		want bool
	}{
		{"current shared default", "google/gemini-2.5-flash-lite", true},
		{"ops failover tier", "anthropic/claude-haiku-4.5", true},
		{"retired slug (SIN-65406)", "google/gemini-2.0-flash", false},
		{"unknown slug", "openai/gpt-4o", false},
		{"empty slug", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := llmmodels.IsLive(tc.slug); got != tc.want {
				t.Fatalf("IsLive(%q) = %v, want %v", tc.slug, got, tc.want)
			}
		})
	}
}
