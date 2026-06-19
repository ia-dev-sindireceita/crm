package main

// SIN-65244 / SIN-65243 — unified LLM model configuration.
//
// Two LLM call points share one central model knob:
//
//   - the operator AI-assist Summarizer (internal/aiassist via the
//     adapters/openrouter shim), and
//   - the fake-customer persona (internal/adapter/channels/llmcustomer/
//     openrouter).
//
// Each point resolves its effective model with the same precedence:
//
//	per-point override  →  OPENROUTER_MODEL  →  hardcoded default
//
//	AI-assist: AIASSIST_LLM_MODEL → OPENROUTER_MODEL → google/gemini-2.0-flash
//	Persona:   PERSONA_LLM_MODEL  → OPENROUTER_MODEL → google/gemini-2.0-flash
//
// The CEO product decision (SIN-65243) is "same model everywhere by
// default": leaving every knob unset routes both points to
// google/gemini-2.0-flash, setting only OPENROUTER_MODEL moves both at
// once, and a per-point override peels one point off the shared default.
//
// Hexagonal lens: resolveLLMModel is a pure function — it takes the
// already-read string values, never the process env, so it carries no
// I/O and is exhaustively table-testable. The ReadXxx wrappers are the
// thin env-reading shells the composition root calls.

import "strings"

const (
	// envOpenRouterModel names the central model knob both LLM call
	// points consult before their hardcoded default. Empty/whitespace
	// is treated as unset.
	envOpenRouterModel = "OPENROUTER_MODEL"

	// envAIAssistLLMModel names the AI-assist point override. When set
	// it wins over OPENROUTER_MODEL for the operator Summarizer only.
	envAIAssistLLMModel = "AIASSIST_LLM_MODEL"

	// defaultLLMModel is the hardcoded final fallback for every point
	// (SIN-65243 / ADR-0040 Gemini Flash). It matches both
	// adapters/openrouter.DefaultModel and the llmcustomer openrouter
	// persona DefaultModel so the env-unset behaviour is identical to
	// the adapters' own defaults — the env layer only makes that
	// default overridable without a code change.
	defaultLLMModel = "google/gemini-2.0-flash"
)

// resolveLLMModel returns the first non-empty (whitespace-trimmed)
// value of override then central, falling back to defaultLLMModel when
// both are blank. Pure: no env access, no logging, no I/O.
func resolveLLMModel(override, central string) string {
	for _, candidate := range []string{override, central} {
		if v := strings.TrimSpace(candidate); v != "" {
			return v
		}
	}
	return defaultLLMModel
}

// ReadAIAssistModel resolves the effective model for the operator
// AI-assist point: AIASSIST_LLM_MODEL → OPENROUTER_MODEL →
// defaultLLMModel. A nil getenv (test ergonomics) resolves to the
// hardcoded default.
func ReadAIAssistModel(getenv func(string) string) string {
	if getenv == nil {
		return defaultLLMModel
	}
	return resolveLLMModel(getenv(envAIAssistLLMModel), getenv(envOpenRouterModel))
}

// ReadPersonaModel resolves the effective model for the fake-customer
// persona point: PERSONA_LLM_MODEL → OPENROUTER_MODEL → defaultLLMModel.
// A nil getenv resolves to the hardcoded default. (envPersonaLLMModel
// is declared in persona_llm_provider_wire.go.)
func ReadPersonaModel(getenv func(string) string) string {
	if getenv == nil {
		return defaultLLMModel
	}
	return resolveLLMModel(getenv(envPersonaLLMModel), getenv(envOpenRouterModel))
}
