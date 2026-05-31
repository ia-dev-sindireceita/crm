package main

// SIN-63826 — table-driven tests for buildPersonaLLM. Verifies the
// selector dispatches to the canned default, constructs an openrouter
// persona when the env says so, and fails loud when the env disagrees
// with itself (e.g. openrouter selected but key absent).

import (
	"errors"
	"strings"
	"testing"

	"github.com/pericles-luz/crm/internal/adapter/channels/llmcustomer"
	"github.com/pericles-luz/crm/internal/adapter/channels/llmcustomer/canned"
	openrouterpersona "github.com/pericles-luz/crm/internal/adapter/channels/llmcustomer/openrouter"
)

func TestBuildPersonaLLM_NilGetenvIsCanned(t *testing.T) {
	t.Parallel()
	got, err := buildPersonaLLM(nil)
	if err != nil {
		t.Fatalf("buildPersonaLLM(nil): unexpected error: %v", err)
	}
	if _, ok := got.(*canned.LLM); !ok {
		t.Fatalf("got %T, want *canned.LLM", got)
	}
}

func TestBuildPersonaLLM_EmptyEnvIsCanned(t *testing.T) {
	t.Parallel()
	got, err := buildPersonaLLM(func(string) string { return "" })
	if err != nil {
		t.Fatalf("buildPersonaLLM: unexpected error: %v", err)
	}
	if _, ok := got.(*canned.LLM); !ok {
		t.Fatalf("got %T, want *canned.LLM", got)
	}
}

func TestBuildPersonaLLM_ExplicitCanned(t *testing.T) {
	t.Parallel()
	getenv := func(k string) string {
		if k == envPersonaLLMProvider {
			return "canned"
		}
		return ""
	}
	got, err := buildPersonaLLM(getenv)
	if err != nil {
		t.Fatalf("buildPersonaLLM: unexpected error: %v", err)
	}
	if _, ok := got.(*canned.LLM); !ok {
		t.Fatalf("got %T, want *canned.LLM", got)
	}
}

func TestBuildPersonaLLM_OpenRouterConstructsPersona(t *testing.T) {
	t.Parallel()
	getenv := func(k string) string {
		switch k {
		case envPersonaLLMProvider:
			return "openrouter"
		case envOpenRouterAPIKey:
			return "sk-test-fixture"
		case envPersonaLLMModel:
			return "anthropic/claude-haiku-4.5"
		}
		return ""
	}
	got, err := buildPersonaLLM(getenv)
	if err != nil {
		t.Fatalf("buildPersonaLLM: unexpected error: %v", err)
	}
	if _, ok := got.(*openrouterpersona.Persona); !ok {
		t.Fatalf("got %T, want *openrouterpersona.Persona", got)
	}
}

func TestBuildPersonaLLM_OpenRouterDefaultsModelWhenUnset(t *testing.T) {
	t.Parallel()
	getenv := func(k string) string {
		switch k {
		case envPersonaLLMProvider:
			return "openrouter"
		case envOpenRouterAPIKey:
			return "sk-test"
		}
		return ""
	}
	got, err := buildPersonaLLM(getenv)
	if err != nil {
		t.Fatalf("buildPersonaLLM: unexpected error: %v", err)
	}
	// Defensive: the returned impl still satisfies the port (and the
	// underlying default model is exercised by the openrouter package
	// tests).
	var _ llmcustomer.PersonaLLM = got
}

func TestBuildPersonaLLM_OpenRouterWithoutKeyFails(t *testing.T) {
	t.Parallel()
	// PersonaLLMRefusedWithoutKey is the primary gate at runWith, but
	// buildPersonaLLM is also called directly from the test path and
	// must fail loud rather than silently constructing a broken impl.
	getenv := func(k string) string {
		if k == envPersonaLLMProvider {
			return "openrouter"
		}
		return ""
	}
	_, err := buildPersonaLLM(getenv)
	if err == nil {
		t.Fatal("expected error when openrouter selected without OPENROUTER_API_KEY, got nil")
	}
	if !strings.Contains(err.Error(), "APIKey") {
		t.Fatalf("err = %q; want it to mention APIKey", err.Error())
	}
}

func TestBuildPersonaLLM_TypoOnProviderPropagatesParseError(t *testing.T) {
	t.Parallel()
	getenv := func(k string) string {
		if k == envPersonaLLMProvider {
			return "fake"
		}
		return ""
	}
	_, err := buildPersonaLLM(getenv)
	if err == nil {
		t.Fatal("expected parse error, got nil")
	}
	if !errors.Is(err, ErrPersonaLLMProviderUnknown) {
		t.Fatalf("err = %v; want ErrPersonaLLMProviderUnknown", err)
	}
}
