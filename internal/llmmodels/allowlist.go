// Package llmmodels is the single source of truth for the OpenRouter
// model slugs the CRM is allowed to route to across every LLM call point
// (the operator AI-assist summarizer and the llmcustomer persona).
//
// Why this exists: a retired model slug does not fail at boot. It only
// surfaces as "upstream status 404" on the first real round-trip — the
// exact path the staging smoke does not exercise (AI-assist is
// deny-by-default). That is how google/gemini-2.0-flash reached staging
// after OpenRouter retired it (SIN-65406). The allowlist below, paired
// with TestAdapterDefaultModelsAreConsistentAndLive, converts that silent
// runtime failure into a build-time one.
//
// When the board rotates the default model (the SIN-65243 "same model
// everywhere by default" decision), edit in this order:
//  1. the live map below (add the new slug; drop the retired one);
//  2. the two DefaultModel constants
//     (adapters/openrouter.DefaultModel and
//     internal/adapter/channels/llmcustomer/openrouter.DefaultModel);
//  3. docs/ops/llm-config-runbook.md defaults/examples.
package llmmodels

// live is the allowlist of OpenRouter model slugs known to be active
// (not retired, so no upstream 404). Keep it in sync with the OpenRouter
// model catalogue whenever a model is rotated. This map is the single
// authoritative list referenced by the runbook.
var live = map[string]struct{}{
	"google/gemini-2.5-flash-lite": {}, // current shared default (SIN-65412)
	"anthropic/claude-haiku-4.5":   {}, // ops failover tier (FallbackModel)
}

// IsLive reports whether slug is present in the live allowlist.
func IsLive(slug string) bool {
	_, ok := live[slug]
	return ok
}
