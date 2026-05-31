package main

// SIN-63823 — table-driven unit tests for the INBOX_CHANNEL_PROVIDER
// enum, the env-driven parser, and the production-tier refuse gate.
//
// The gate fires BEFORE the HTTP listener binds so its behaviour
// matters at boot-log fidelity: a typo on a prod deploy must abort
// startup with the offending value in the error string, otherwise the
// operator has no signal that the binary chose the disabled default.

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

func TestInboxChannelProvider_UnmarshalText(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		input   string
		want    InboxChannelProvider
		wantErr bool
	}{
		{name: "empty string defaults to disabled", input: "", want: InboxChannelProviderDisabled},
		{name: "explicit disabled", input: "disabled", want: InboxChannelProviderDisabled},
		{name: "llmcustomer", input: "llmcustomer", want: InboxChannelProviderLLMCustomer},
		{name: "real", input: "real", want: InboxChannelProviderReal},
		{name: "leading + trailing whitespace trimmed", input: "  llmcustomer\n", want: InboxChannelProviderLLMCustomer},
		{name: "unknown token rejected", input: "fake", wantErr: true},
		{name: "case mismatch rejected", input: "Disabled", wantErr: true},
		{name: "empty after trim defaults to disabled", input: "   ", want: InboxChannelProviderDisabled},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var got InboxChannelProvider
			err := got.UnmarshalText([]byte(tc.input))
			if tc.wantErr {
				if err == nil {
					t.Fatalf("UnmarshalText(%q): expected error, got nil (parsed=%q)", tc.input, got)
				}
				if !errors.Is(err, ErrInboxChannelProviderUnknown) {
					t.Fatalf("UnmarshalText(%q): error not ErrInboxChannelProviderUnknown: %v", tc.input, err)
				}
				if !strings.Contains(err.Error(), tc.input) {
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

func TestInboxChannelProvider_Validate(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		value   InboxChannelProvider
		wantErr bool
	}{
		{name: "disabled valid", value: InboxChannelProviderDisabled},
		{name: "llmcustomer valid", value: InboxChannelProviderLLMCustomer},
		{name: "real valid", value: InboxChannelProviderReal},
		{name: "empty invalid", value: InboxChannelProvider(""), wantErr: true},
		{name: "unknown invalid", value: InboxChannelProvider("fake"), wantErr: true},
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
				if !errors.Is(err, ErrInboxChannelProviderUnknown) {
					t.Fatalf("validate(%q): error not ErrInboxChannelProviderUnknown: %v", tc.value, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("validate(%q): unexpected error: %v", tc.value, err)
			}
		})
	}
}

func TestInboxChannelProvider_String(t *testing.T) {
	t.Parallel()
	if got := InboxChannelProviderDisabled.String(); got != "disabled" {
		t.Fatalf("String(disabled): got %q want disabled", got)
	}
	if got := InboxChannelProviderLLMCustomer.String(); got != "llmcustomer" {
		t.Fatalf("String(llmcustomer): got %q want llmcustomer", got)
	}
	if got := InboxChannelProviderReal.String(); got != "real" {
		t.Fatalf("String(real): got %q want real", got)
	}
}

func TestReadInboxChannelProvider(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		getenv  func(string) string
		want    InboxChannelProvider
		wantErr bool
	}{
		{
			name:   "nil getenv defaults to disabled",
			getenv: nil,
			want:   InboxChannelProviderDisabled,
		},
		{
			name:   "unset defaults to disabled",
			getenv: func(string) string { return "" },
			want:   InboxChannelProviderDisabled,
		},
		{
			name: "explicit llmcustomer",
			getenv: func(k string) string {
				if k == envInboxChannelProvider {
					return "llmcustomer"
				}
				return ""
			},
			want: InboxChannelProviderLLMCustomer,
		},
		{
			name: "explicit real",
			getenv: func(k string) string {
				if k == envInboxChannelProvider {
					return "real"
				}
				return ""
			},
			want: InboxChannelProviderReal,
		},
		{
			name: "unknown value rejected",
			getenv: func(k string) string {
				if k == envInboxChannelProvider {
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
			got, err := ReadInboxChannelProvider(tc.getenv)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil (parsed=%q)", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("got %q want %q", got, tc.want)
			}
		})
	}
}

func TestInboxChannelProviderRefusedInProd(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		appEnv   string
		provider string
		wantErr  bool
	}{
		// AC: production + llmcustomer → hard refuse with offending APP_ENV in error.
		{name: "production + llmcustomer refuses", appEnv: "production", provider: "llmcustomer", wantErr: true},
		{name: "staging-prod + llmcustomer refuses", appEnv: "staging-prod", provider: "llmcustomer", wantErr: true},

		// AC: dev / regular staging boot fine on llmcustomer.
		{name: "dev + llmcustomer boots", appEnv: "dev", provider: "llmcustomer"},
		{name: "staging + llmcustomer boots", appEnv: "staging", provider: "llmcustomer"},
		{name: "empty APP_ENV + llmcustomer boots", appEnv: "", provider: "llmcustomer"},

		// Strict matching: typos must NOT engage the gate (same defensive
		// posture as LGPDMasterOpsRequired) so the operator hits the
		// next layer's failure mode instead of the wrong error.
		{name: "PRODUCTION upper-case bypasses gate", appEnv: "PRODUCTION", provider: "llmcustomer"},
		{name: "prod abbreviation bypasses gate", appEnv: "prod", provider: "llmcustomer"},

		// AC: production + disabled / real → boot fine. The gate is
		// scoped to the fake-customer adapter only.
		{name: "production + disabled boots", appEnv: "production", provider: "disabled"},
		{name: "production + real boots", appEnv: "production", provider: "real"},
		{name: "staging-prod + disabled boots", appEnv: "staging-prod", provider: "disabled"},

		// Unset provider must not trigger the gate (defaults to disabled).
		{name: "production + unset boots", appEnv: "production", provider: ""},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			getenv := func(k string) string {
				switch k {
				case envAppEnv:
					return tc.appEnv
				case envInboxChannelProvider:
					return tc.provider
				}
				return ""
			}
			err := InboxChannelProviderRefusedInProd(getenv)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected ErrInboxChannelProviderRefusedInProd, got nil")
				}
				if !errors.Is(err, ErrInboxChannelProviderRefusedInProd) {
					t.Fatalf("error is not ErrInboxChannelProviderRefusedInProd: %v", err)
				}
				if !strings.Contains(err.Error(), tc.appEnv) {
					t.Fatalf("error must include offending APP_ENV %q, got %q", tc.appEnv, err.Error())
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestInboxChannelProviderRefusedInProd_NilGetenv_NilError(t *testing.T) {
	t.Parallel()
	if err := InboxChannelProviderRefusedInProd(nil); err != nil {
		t.Fatalf("nil getenv: expected nil error, got %v", err)
	}
}

func TestLogInboxChannelProviderBoot(t *testing.T) {
	t.Parallel()
	cases := []InboxChannelProvider{
		InboxChannelProviderDisabled,
		InboxChannelProviderLLMCustomer,
		InboxChannelProviderReal,
	}
	for _, provider := range cases {
		provider := provider
		t.Run(string(provider), func(t *testing.T) {
			t.Parallel()
			var buf bytes.Buffer
			logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
			LogInboxChannelProviderBoot(logger, provider)

			var entry map[string]any
			if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &entry); err != nil {
				t.Fatalf("decode log entry: %v (raw=%q)", err, buf.String())
			}
			if entry["level"] != "INFO" {
				t.Fatalf("level = %v, want INFO", entry["level"])
			}
			got, ok := entry["inbox.channel.provider"]
			if !ok {
				t.Fatalf("entry missing inbox.channel.provider key; got %v", entry)
			}
			if got != string(provider) {
				t.Fatalf("inbox.channel.provider = %v, want %q", got, provider)
			}
		})
	}
}

func TestLogInboxChannelProviderBoot_NilLoggerFallsBackToDefault(t *testing.T) {
	// Intentionally NOT t.Parallel — this test calls slog.SetDefault
	// to capture the fallback writer, which is a process-wide global.
	// Restore on cleanup so the suite's other slog.Default callers stay
	// pointed at the original sink.
	var buf bytes.Buffer
	captured := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	prev := slog.Default()
	slog.SetDefault(captured)
	t.Cleanup(func() { slog.SetDefault(prev) })

	LogInboxChannelProviderBoot(nil, InboxChannelProviderLLMCustomer)

	var entry map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &entry); err != nil {
		t.Fatalf("decode log entry: %v (raw=%q)", err, buf.String())
	}
	if entry["inbox.channel.provider"] != "llmcustomer" {
		t.Fatalf("inbox.channel.provider = %v, want llmcustomer", entry["inbox.channel.provider"])
	}
}

// TestRunWith_RefusesInboxChannelProviderInProd pins the AC the issue
// names explicitly: cmd/server exits non-zero BEFORE the HTTP listener
// binds when APP_ENV=production and INBOX_CHANNEL_PROVIDER=llmcustomer.
// runWith returns synchronously with the sentinel error so the
// caller's listener never starts — the dial seam below would have
// fataled the test if runWith had progressed far enough to need it.
func TestRunWith_RefusesInboxChannelProviderInProd(t *testing.T) {
	t.Parallel()
	getenv := func(k string) string {
		switch k {
		case envAppEnv:
			return appEnvProduction
		case envInboxChannelProvider:
			return string(InboxChannelProviderLLMCustomer)
		case envMasterOpsDSN:
			// LGPDMasterOpsRequired runs ahead of the inbox gate
			// and would otherwise short-circuit the test on the
			// missing DSN. Hand it a non-empty value (never dialed
			// — the inbox gate aborts before any handler is built)
			// so the inbox refuse is what surfaces.
			return "postgres://example/master-ops"
		}
		return ""
	}
	dial := func(context.Context, string) (webhookPool, error) {
		t.Fatal("dial must not be called when the prod gate refuses boot")
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	err := runWith(ctx, "127.0.0.1:0", getenv, dial)
	if err == nil {
		t.Fatal("expected runWith to refuse production-tier llmcustomer; got nil")
	}
	if !errors.Is(err, ErrInboxChannelProviderRefusedInProd) {
		t.Fatalf("err = %v; want ErrInboxChannelProviderRefusedInProd", err)
	}
	if !strings.Contains(err.Error(), "inbox channel provider wire-up") {
		t.Fatalf("err = %v; want runWith wrapper prefix", err)
	}
}

// TestRunWith_RefusesInvalidInboxChannelProvider pins the parse path:
// a typo on the env var surfaces as a hard boot failure with the
// offending value in the error string so the operator never has to
// guess which fallback the binary chose.
func TestRunWith_RefusesInvalidInboxChannelProvider(t *testing.T) {
	t.Parallel()
	getenv := func(k string) string {
		if k == envInboxChannelProvider {
			return "fake"
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
		t.Fatal("expected runWith to reject INBOX_CHANNEL_PROVIDER=fake; got nil")
	}
	if !errors.Is(err, ErrInboxChannelProviderUnknown) {
		t.Fatalf("err = %v; want ErrInboxChannelProviderUnknown", err)
	}
}

func TestInboxChannelProviderRefusedInProd_PropagatesParseError(t *testing.T) {
	t.Parallel()
	// A typo in INBOX_CHANNEL_PROVIDER must surface as a parse error
	// from the boot gate so the operator sees the offending value even
	// when APP_ENV is dev/CI — otherwise a misconfigured deploy
	// silently falls back to the disabled default.
	getenv := func(k string) string {
		switch k {
		case envInboxChannelProvider:
			return "fake"
		}
		return ""
	}
	err := InboxChannelProviderRefusedInProd(getenv)
	if err == nil {
		t.Fatal("expected parse error, got nil")
	}
	if !errors.Is(err, ErrInboxChannelProviderUnknown) {
		t.Fatalf("expected ErrInboxChannelProviderUnknown, got %v", err)
	}
}
