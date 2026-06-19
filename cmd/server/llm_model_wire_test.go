package main

// SIN-65244 — table-driven unit tests for the unified LLM model
// resolution helper. The pure resolveLLMModel core is exercised across
// the full precedence lattice (default / central / override / blank
// override falls through), and the two env-reading wrappers are pinned
// to the env vars they read so a future rename surfaces here.

import "testing"

func TestResolveLLMModel(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		override string
		central  string
		want     string
	}{
		{name: "both blank falls to hardcoded default", override: "", central: "", want: defaultLLMModel},
		{name: "central only", override: "", central: "x-ai/grok", want: "x-ai/grok"},
		{name: "override wins over central", override: "anthropic/claude-haiku-4.5", central: "x-ai/grok", want: "anthropic/claude-haiku-4.5"},
		{name: "override only", override: "anthropic/claude-haiku-4.5", central: "", want: "anthropic/claude-haiku-4.5"},
		{name: "whitespace override falls through to central", override: "   ", central: "x-ai/grok", want: "x-ai/grok"},
		{name: "whitespace override and blank central falls to default", override: "  \t ", central: "", want: defaultLLMModel},
		{name: "whitespace central falls to default", override: "", central: "  ", want: defaultLLMModel},
		{name: "values are trimmed", override: "  anthropic/claude-haiku-4.5\n", central: "", want: "anthropic/claude-haiku-4.5"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := resolveLLMModel(tc.override, tc.central); got != tc.want {
				t.Fatalf("resolveLLMModel(%q, %q) = %q; want %q", tc.override, tc.central, got, tc.want)
			}
		})
	}
}

func TestReadAIAssistModel(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		getenv func(string) string
		want   string
	}{
		{name: "nil getenv is default", getenv: nil, want: defaultLLMModel},
		{name: "empty env is default", getenv: func(string) string { return "" }, want: defaultLLMModel},
		{
			name: "central applies when no point override",
			getenv: func(k string) string {
				if k == envOpenRouterModel {
					return "x-ai/grok"
				}
				return ""
			},
			want: "x-ai/grok",
		},
		{
			name: "point override wins over central",
			getenv: func(k string) string {
				switch k {
				case envAIAssistLLMModel:
					return "anthropic/claude-haiku-4.5"
				case envOpenRouterModel:
					return "x-ai/grok"
				}
				return ""
			},
			want: "anthropic/claude-haiku-4.5",
		},
		{
			name: "persona override does NOT leak into ai-assist",
			getenv: func(k string) string {
				if k == envPersonaLLMModel {
					return "persona/only"
				}
				return ""
			},
			want: defaultLLMModel,
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := ReadAIAssistModel(tc.getenv); got != tc.want {
				t.Fatalf("ReadAIAssistModel = %q; want %q", got, tc.want)
			}
		})
	}
}

func TestReadPersonaModel(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		getenv func(string) string
		want   string
	}{
		{name: "nil getenv is default", getenv: nil, want: defaultLLMModel},
		{name: "empty env is default", getenv: func(string) string { return "" }, want: defaultLLMModel},
		{
			name: "central applies when no point override",
			getenv: func(k string) string {
				if k == envOpenRouterModel {
					return "x-ai/grok"
				}
				return ""
			},
			want: "x-ai/grok",
		},
		{
			name: "point override wins over central",
			getenv: func(k string) string {
				switch k {
				case envPersonaLLMModel:
					return "anthropic/claude-haiku-4.5"
				case envOpenRouterModel:
					return "x-ai/grok"
				}
				return ""
			},
			want: "anthropic/claude-haiku-4.5",
		},
		{
			name: "ai-assist override does NOT leak into persona",
			getenv: func(k string) string {
				if k == envAIAssistLLMModel {
					return "aiassist/only"
				}
				return ""
			},
			want: defaultLLMModel,
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := ReadPersonaModel(tc.getenv); got != tc.want {
				t.Fatalf("ReadPersonaModel = %q; want %q", got, tc.want)
			}
		})
	}
}
