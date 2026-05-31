package main

// SIN-63826 — table-driven unit tests for the PERSONA_LLM_PROVIDER
// enum, the env-driven parser, and the hard-refuse-at-parse gate that
// blocks openrouter without OPENROUTER_API_KEY.
//
// The gate fires BEFORE the HTTP listener binds so its behaviour
// matters at boot-log fidelity: a typo or missing secret on the env
// var must abort startup with the offending value in the error string,
// otherwise the operator has no signal that the binary chose the
// canned default or rejected the boot for a different reason.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"
)

func TestPersonaLLMProvider_UnmarshalText(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		input   string
		want    PersonaLLMProvider
		wantErr bool
	}{
		{name: "empty string defaults to canned", input: "", want: PersonaLLMProviderCanned},
		{name: "explicit canned", input: "canned", want: PersonaLLMProviderCanned},
		{name: "openrouter", input: "openrouter", want: PersonaLLMProviderOpenRouter},
		{name: "leading + trailing whitespace trimmed", input: "  openrouter\n", want: PersonaLLMProviderOpenRouter},
		{name: "unknown token rejected", input: "fake", wantErr: true},
		{name: "case mismatch rejected", input: "OpenRouter", wantErr: true},
		{name: "empty after trim defaults to canned", input: "   ", want: PersonaLLMProviderCanned},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var got PersonaLLMProvider
			err := got.UnmarshalText([]byte(tc.input))
			if tc.wantErr {
				if err == nil {
					t.Fatalf("UnmarshalText(%q): expected error, got nil (parsed=%q)", tc.input, got)
				}
				if !errors.Is(err, ErrPersonaLLMProviderUnknown) {
					t.Fatalf("UnmarshalText(%q): error not ErrPersonaLLMProviderUnknown: %v", tc.input, err)
				}
				if !strings.Contains(err.Error(), strings.TrimSpace(tc.input)) {
					t.Fatalf("UnmarshalText(%q): error must include offending input, got %q", tc.input, err.Error())
				}
				return
			}
			if err != nil {
				t.Fatalf("UnmarshalText(%q): unexpected error: %v", tc.input, err)
			}
			if got != tc.want {
				t.Fatalf("UnmarshalText(%q): got %q want %q", tc.input, got, tc.want)
			}
		})
	}
}

func TestPersonaLLMProvider_Validate(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		value   PersonaLLMProvider
		wantErr bool
	}{
		{name: "canned valid", value: PersonaLLMProviderCanned},
		{name: "openrouter valid", value: PersonaLLMProviderOpenRouter},
		{name: "empty invalid", value: PersonaLLMProvider(""), wantErr: true},
		{name: "unknown invalid", value: PersonaLLMProvider("fake"), wantErr: true},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := tc.value.validate()
			if tc.wantErr {
				if err == nil {
					t.Fatalf("validate(%q): expected error, got nil", tc.value)
				}
				if !errors.Is(err, ErrPersonaLLMProviderUnknown) {
					t.Fatalf("validate(%q): error not ErrPersonaLLMProviderUnknown: %v", tc.value, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("validate(%q): unexpected error: %v", tc.value, err)
			}
		})
	}
}

func TestPersonaLLMProvider_String(t *testing.T) {
	t.Parallel()
	if got := PersonaLLMProviderCanned.String(); got != "canned" {
		t.Fatalf("String(canned): got %q want canned", got)
	}
	if got := PersonaLLMProviderOpenRouter.String(); got != "openrouter" {
		t.Fatalf("String(openrouter): got %q want openrouter", got)
	}
}

func TestReadPersonaLLMProvider(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		getenv  func(string) string
		want    PersonaLLMProvider
		wantErr bool
	}{
		{
			name:   "nil getenv defaults to canned",
			getenv: nil,
			want:   PersonaLLMProviderCanned,
		},
		{
			name:   "empty env defaults to canned",
			getenv: func(string) string { return "" },
			want:   PersonaLLMProviderCanned,
		},
		{
			name: "openrouter explicit",
			getenv: func(k string) string {
				if k == envPersonaLLMProvider {
					return "openrouter"
				}
				return ""
			},
			want: PersonaLLMProviderOpenRouter,
		},
		{
			name: "unknown rejected",
			getenv: func(k string) string {
				if k == envPersonaLLMProvider {
					return "fake"
				}
				return ""
			},
			wantErr: true,
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := ReadPersonaLLMProvider(tc.getenv)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil (parsed=%q)", got)
				}
				if !errors.Is(err, ErrPersonaLLMProviderUnknown) {
					t.Fatalf("err = %v; want ErrPersonaLLMProviderUnknown", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestPersonaLLMRefusedWithoutKey(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		getenv  func(string) string
		wantErr error
	}{
		{
			name:   "nil getenv is a no-op",
			getenv: nil,
		},
		{
			name:   "canned without key is fine",
			getenv: func(string) string { return "" },
		},
		{
			name: "canned with key set is fine",
			getenv: func(k string) string {
				if k == envOpenRouterAPIKey {
					return "sk-test"
				}
				return ""
			},
		},
		{
			name: "openrouter with key set is fine",
			getenv: func(k string) string {
				switch k {
				case envPersonaLLMProvider:
					return "openrouter"
				case envOpenRouterAPIKey:
					return "sk-test"
				}
				return ""
			},
		},
		{
			name: "openrouter without key refused",
			getenv: func(k string) string {
				if k == envPersonaLLMProvider {
					return "openrouter"
				}
				return ""
			},
			wantErr: ErrPersonaLLMOpenRouterKeyMissing,
		},
		{
			name: "openrouter with whitespace-only key refused",
			getenv: func(k string) string {
				switch k {
				case envPersonaLLMProvider:
					return "openrouter"
				case envOpenRouterAPIKey:
					return "   \t  "
				}
				return ""
			},
			wantErr: ErrPersonaLLMOpenRouterKeyMissing,
		},
		{
			name: "typo on provider propagates parse error",
			getenv: func(k string) string {
				if k == envPersonaLLMProvider {
					return "fake"
				}
				return ""
			},
			wantErr: ErrPersonaLLMProviderUnknown,
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := PersonaLLMRefusedWithoutKey(tc.getenv)
			if tc.wantErr == nil {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected error %v, got nil", tc.wantErr)
			}
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v; want errors.Is(err, %v)", err, tc.wantErr)
			}
		})
	}
}

func TestLogPersonaLLMProviderBoot_EmitsAuditLine(t *testing.T) {
	cases := []PersonaLLMProvider{PersonaLLMProviderCanned, PersonaLLMProviderOpenRouter}
	for _, provider := range cases {
		provider := provider
		t.Run(provider.String(), func(t *testing.T) {
			t.Parallel()
			var buf bytes.Buffer
			logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
			LogPersonaLLMProviderBoot(logger, provider)

			var entry map[string]any
			if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &entry); err != nil {
				t.Fatalf("decode log entry: %v (raw=%q)", err, buf.String())
			}
			got, ok := entry["persona.llm.provider"]
			if !ok {
				t.Fatalf("entry missing persona.llm.provider key; got %v", entry)
			}
			if got != string(provider) {
				t.Fatalf("persona.llm.provider = %v, want %q", got, provider)
			}
		})
	}
}

func TestLogPersonaLLMProviderBoot_NilLoggerFallsBackToDefault(t *testing.T) {
	// Intentionally NOT t.Parallel — slog.SetDefault is a process-wide
	// global. Restore on cleanup so the suite's other slog.Default
	// callers stay pointed at the original sink.
	var buf bytes.Buffer
	captured := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	prev := slog.Default()
	slog.SetDefault(captured)
	t.Cleanup(func() { slog.SetDefault(prev) })

	LogPersonaLLMProviderBoot(nil, PersonaLLMProviderOpenRouter)

	var entry map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &entry); err != nil {
		t.Fatalf("decode log entry: %v (raw=%q)", err, buf.String())
	}
	if entry["persona.llm.provider"] != "openrouter" {
		t.Fatalf("persona.llm.provider = %v, want openrouter", entry["persona.llm.provider"])
	}
}

// TestRunWith_RefusesOpenRouterWithoutKey pins the boot-gate AC:
// cmd/server exits non-zero BEFORE the HTTP listener binds when
// PERSONA_LLM_PROVIDER=openrouter and OPENROUTER_API_KEY is unset.
// The dial seam below would have fataled the test if runWith had
// progressed far enough to need it.
func TestRunWith_RefusesOpenRouterWithoutKey(t *testing.T) {
	t.Parallel()
	getenv := func(k string) string {
		switch k {
		case envPersonaLLMProvider:
			return "openrouter"
		case envMasterOpsDSN:
			// LGPDMasterOpsRequired runs ahead of the persona gate;
			// hand it a non-empty value so the failure surfaces from
			// the persona gate and not the lgpd one.
			return "postgres://example/master-ops"
		}
		return ""
	}
	dial := func(context.Context, string) (webhookPool, error) {
		t.Fatal("dial must not be called when the persona gate refuses boot")
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	err := runWith(ctx, "127.0.0.1:0", getenv, dial)
	if err == nil {
		t.Fatal("expected runWith to refuse openrouter without OPENROUTER_API_KEY; got nil")
	}
	if !errors.Is(err, ErrPersonaLLMOpenRouterKeyMissing) {
		t.Fatalf("err = %v; want ErrPersonaLLMOpenRouterKeyMissing", err)
	}
	if !strings.Contains(err.Error(), "persona-llm wire-up") {
		t.Fatalf("err = %v; want runWith wrapper prefix", err)
	}
}

// TestRunWith_RefusesInvalidPersonaLLMProvider pins the parse path:
// a typo on the env var surfaces as a hard boot failure with the
// offending value in the error string so the operator never has to
// guess which fallback the binary chose.
func TestRunWith_RefusesInvalidPersonaLLMProvider(t *testing.T) {
	t.Parallel()
	getenv := func(k string) string {
		switch k {
		case envPersonaLLMProvider:
			return "fake"
		case envMasterOpsDSN:
			return "postgres://example/master-ops"
		}
		return ""
	}
	dial := func(context.Context, string) (webhookPool, error) {
		t.Fatal("dial must not be called when the parser rejects the env value")
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	err := runWith(ctx, "127.0.0.1:0", getenv, dial)
	if err == nil {
		t.Fatal("expected runWith to reject PERSONA_LLM_PROVIDER=fake; got nil")
	}
	if !errors.Is(err, ErrPersonaLLMProviderUnknown) {
		t.Fatalf("err = %v; want ErrPersonaLLMProviderUnknown", err)
	}
}
