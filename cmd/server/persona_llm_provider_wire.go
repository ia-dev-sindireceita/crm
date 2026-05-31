package main

// SIN-63826 / SIN-63793 W3 — PERSONA_LLM_PROVIDER activation gate.
//
// Owns the typed enum the llmcustomer selector uses to choose between
// the canned (deterministic) script and the OpenRouter-backed persona
// when assembling the /inbox/* subtree under provider=llmcustomer.
//
// Two values are wired today:
//   - canned     (default): deterministic in-process script. Safe in
//                CI / unit tests / dev with no secrets.
//   - openrouter:           wraps adapters/openrouter-style HTTP calls
//                against OpenRouter's chat-completions API. Requires
//                OPENROUTER_API_KEY in env.
//
// Defense-in-depth around the openrouter branch:
//   - default value is `canned`, so a bare deploy never spends $$ on a
//     persona reply even when INBOX_CHANNEL_PROVIDER=llmcustomer;
//   - the boot gate PersonaLLMRefusedWithoutKey fires before the HTTP
//     listener binds and aborts startup if openrouter is selected
//     without OPENROUTER_API_KEY (hard-refuse-at-parse, per the
//     SIN-63793 W3 scope).
//   - the W4 production-tier refuse (InboxChannelProviderRefusedInProd)
//     still prevents the fake channel from binding in production /
//     staging-prod, so openrouter is reachable only on dev / regular
//     staging.

import (
	"errors"
	"fmt"
	"log/slog"
	"strings"
)

const (
	// envPersonaLLMProvider names the canonical knob the cmd/server
	// llmcustomer selector reads to decide which PersonaLLM
	// implementation is wired. Defaults to `canned` when unset so a
	// misconfigured deploy is safe (no $$ surprise) and works without
	// any secret material.
	envPersonaLLMProvider = "PERSONA_LLM_PROVIDER"

	// envPersonaLLMModel names the OpenRouter-routed model the
	// openrouter persona impl uses. Empty falls back to
	// openrouter.DefaultModel. Only consulted when
	// PERSONA_LLM_PROVIDER=openrouter.
	envPersonaLLMModel = "PERSONA_LLM_MODEL"

	// envOpenRouterAPIKey names the OpenRouter bearer token the
	// openrouter persona impl forwards as the Authorization header.
	// Required (hard-refuse-at-parse) when
	// PERSONA_LLM_PROVIDER=openrouter.
	envOpenRouterAPIKey = "OPENROUTER_API_KEY"
)

// PersonaLLMProvider names the PersonaLLM implementation the
// llmcustomer adapter binds to at boot. Only the two constants below
// are valid; UnmarshalText rejects any other string with a wrapped
// error that includes the offending value so misconfiguration is
// obvious from the boot log.
type PersonaLLMProvider string

const (
	// PersonaLLMProviderCanned (default) selects the deterministic
	// in-process script from internal/adapter/channels/llmcustomer/
	// canned. Safe in every environment because it has no external
	// dependencies and no secret material.
	PersonaLLMProviderCanned PersonaLLMProvider = "canned"

	// PersonaLLMProviderOpenRouter selects the OpenRouter-backed
	// PersonaLLM from internal/adapter/channels/llmcustomer/openrouter.
	// Requires OPENROUTER_API_KEY; refused at boot otherwise.
	PersonaLLMProviderOpenRouter PersonaLLMProvider = "openrouter"
)

// ErrPersonaLLMProviderUnknown is returned by UnmarshalText / validate
// when the env value does not match one of the two documented
// constants. Wrapped with the offending raw input so the boot log line
// is self-explanatory.
var ErrPersonaLLMProviderUnknown = errors.New("persona-llm: PERSONA_LLM_PROVIDER must be one of canned, openrouter")

// ErrPersonaLLMOpenRouterKeyMissing is returned by
// PersonaLLMRefusedWithoutKey when openrouter is selected but the
// OPENROUTER_API_KEY env var is empty / whitespace. The boot gate
// surfaces this before the HTTP listener binds so a misconfigured
// deploy aborts on startup with a clear error rather than dispatching
// the first inbox bootstrap and failing on every persona reply.
var ErrPersonaLLMOpenRouterKeyMissing = errors.New("persona-llm: OPENROUTER_API_KEY is required when PERSONA_LLM_PROVIDER=openrouter")

// UnmarshalText decodes a textual env value into the typed enum.
// Implementing encoding.TextUnmarshaler keeps the option open for
// future generic config decoders. Empty input is treated as
// PersonaLLMProviderCanned (the documented default) so cmd/server
// does not need to special-case "" at every call site.
func (p *PersonaLLMProvider) UnmarshalText(b []byte) error {
	raw := strings.TrimSpace(string(b))
	if raw == "" {
		*p = PersonaLLMProviderCanned
		return nil
	}
	candidate := PersonaLLMProvider(raw)
	if err := candidate.validate(); err != nil {
		return err
	}
	*p = candidate
	return nil
}

// validate confirms the receiver names one of the two documented
// values. Lives as a method on the value type so call sites that build
// an enum from a non-text source (a Go literal, a future YAML decoder
// that bypasses TextUnmarshaler) can still gate it through the same
// closed-set check.
func (p PersonaLLMProvider) validate() error {
	switch p {
	case PersonaLLMProviderCanned, PersonaLLMProviderOpenRouter:
		return nil
	}
	return fmt.Errorf("%w (got %q)", ErrPersonaLLMProviderUnknown, string(p))
}

// String returns the lowercase token form (the same string the env var
// carries) so the boot-log audit line and any future metric label stay
// stable across refactors.
func (p PersonaLLMProvider) String() string { return string(p) }

// ReadPersonaLLMProvider parses PERSONA_LLM_PROVIDER from the supplied
// getenv. A nil getenv (test ergonomics) and an empty value both
// resolve to PersonaLLMProviderCanned. Any other unknown value is
// rejected via UnmarshalText so a typo cannot silently degrade to the
// default.
func ReadPersonaLLMProvider(getenv func(string) string) (PersonaLLMProvider, error) {
	if getenv == nil {
		return PersonaLLMProviderCanned, nil
	}
	var provider PersonaLLMProvider
	if err := provider.UnmarshalText([]byte(getenv(envPersonaLLMProvider))); err != nil {
		return PersonaLLMProviderCanned, err
	}
	return provider, nil
}

// LogPersonaLLMProviderBoot emits the structured boot-log audit line
// cmd/server uses to surface the selected persona impl on every start.
// The attribute key (persona.llm.provider) is the contract operator
// dashboards / log alerts watch — keep it stable.
//
// Lives in its own helper so a unit test can drive it against a
// JSON-capturing slog handler without spinning up the HTTP listener.
func LogPersonaLLMProviderBoot(logger *slog.Logger, provider PersonaLLMProvider) {
	if logger == nil {
		logger = slog.Default()
	}
	logger.Info(
		"crm: persona LLM provider selected",
		slog.String("persona.llm.provider", provider.String()),
	)
}

// PersonaLLMRefusedWithoutKey enforces the SIN-63826 hard-refuse-at-
// parse gate: when PERSONA_LLM_PROVIDER=openrouter AND
// OPENROUTER_API_KEY is empty/whitespace, boot fails closed with
// ErrPersonaLLMOpenRouterKeyMissing. Other providers (canned, unset)
// bypass the check.
//
// Call this from cmd/server BEFORE the HTTP listener binds so a
// misconfigured deploy aborts on startup with a clear error rather
// than failing on every persona reply at runtime.
func PersonaLLMRefusedWithoutKey(getenv func(string) string) error {
	if getenv == nil {
		return nil
	}
	provider, err := ReadPersonaLLMProvider(getenv)
	if err != nil {
		return err
	}
	if provider != PersonaLLMProviderOpenRouter {
		return nil
	}
	if strings.TrimSpace(getenv(envOpenRouterAPIKey)) == "" {
		return ErrPersonaLLMOpenRouterKeyMissing
	}
	return nil
}
