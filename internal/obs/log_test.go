package obs_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"github.com/pericles-luz/crm/internal/obs"
)

// decode reads the last JSON object emitted to buf. The slog JSON
// handler writes one record per line.
func decode(t *testing.T, buf *bytes.Buffer) map[string]any {
	t.Helper()
	out := buf.String()
	out = strings.TrimSpace(out)
	last := out
	if i := strings.LastIndexByte(out, '\n'); i >= 0 {
		last = strings.TrimSpace(out[i+1:])
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(last), &got); err != nil {
		t.Fatalf("decode last log line %q: %v", last, err)
	}
	return got
}

func TestNewJSONLogger_EmitsContextAttrs(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	l := obs.NewJSONLogger(&buf, slog.LevelDebug)

	ctx := obs.WithRequestID(
		obs.WithTenantID(
			obs.WithUserID(context.Background(), "user-123"),
			"tenant-abc"),
		"req-xyz")

	l.InfoContext(ctx, "hello")

	got := decode(t, &buf)
	if got["msg"] != "hello" {
		t.Errorf("msg: got %v, want hello", got["msg"])
	}
	if got["tenant_id"] != "tenant-abc" {
		t.Errorf("tenant_id: got %v", got["tenant_id"])
	}
	if got["request_id"] != "req-xyz" {
		t.Errorf("request_id: got %v", got["request_id"])
	}
	if got["user_id"] != "user-123" {
		t.Errorf("user_id: got %v", got["user_id"])
	}
}

func TestNewJSONLogger_DropsEmptyAttrs(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	l := obs.NewJSONLogger(&buf, slog.LevelDebug)

	l.InfoContext(context.Background(), "no attrs")

	got := decode(t, &buf)
	for _, k := range []string{"tenant_id", "request_id", "user_id"} {
		if _, ok := got[k]; ok {
			t.Errorf("expected %q to be omitted, got %v", k, got[k])
		}
	}
}

func TestWithFamily_EmptyValueIsNoop(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	if obs.WithTenantID(ctx, "") != ctx {
		t.Error("WithTenantID(\"\") should return same ctx")
	}
	if obs.WithRequestID(ctx, "") != ctx {
		t.Error("WithRequestID(\"\") should return same ctx")
	}
	if obs.WithUserID(ctx, "") != ctx {
		t.Error("WithUserID(\"\") should return same ctx")
	}
}

func TestNewJSONLogger_NilWriter_DiscardsSafely(t *testing.T) {
	t.Parallel()
	l := obs.NewJSONLogger(nil, slog.LevelInfo)
	// Should not panic.
	l.Info("dropped")
}

func TestFromContext_NilContext_ReturnsDefault(t *testing.T) {
	t.Parallel()
	// FromContext must accept a nil ctx without panicking — adapters
	// do call it from goroutines that haven't been handed a request
	// context (logging shims, deferred cleanups). Use a typed nil so
	// staticcheck SA1012 doesn't flag the explicit literal; the
	// runtime sees a (context.Context)(nil) regardless.
	var ctx context.Context
	if got := obs.FromContext(ctx); got == nil {
		t.Fatal("FromContext(nil) returned nil")
	}
}

func TestFromContext_BareContext_ReturnsDefault(t *testing.T) {
	t.Parallel()
	got := obs.FromContext(context.Background())
	if got != slog.Default() {
		t.Errorf("FromContext(empty ctx) should return slog.Default(); got %p vs %p", got, slog.Default())
	}
}

func TestFromContext_BindsAttrsAtCallTime(t *testing.T) {
	// Cannot run in parallel because we mutate slog.Default().
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(obs.NewJSONLogger(&buf, slog.LevelInfo))
	t.Cleanup(func() { slog.SetDefault(prev) })

	ctx := obs.WithTenantID(context.Background(), "t-1")
	ctx = obs.WithRequestID(ctx, "r-1")
	ctx = obs.WithUserID(ctx, "u-1")
	obs.FromContext(ctx).Info("ev")

	got := decode(t, &buf)
	if got["tenant_id"] != "t-1" || got["request_id"] != "r-1" || got["user_id"] != "u-1" {
		t.Errorf("attrs not bound: %v", got)
	}
}

func TestHandler_WithAttrs_PreservesContextLifting(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	l := obs.NewJSONLogger(&buf, slog.LevelInfo).With("svc", "crm")

	ctx := obs.WithTenantID(context.Background(), "t-9")
	l.InfoContext(ctx, "ev")

	got := decode(t, &buf)
	if got["svc"] != "crm" {
		t.Errorf("With(svc=crm) lost: %v", got)
	}
	if got["tenant_id"] != "t-9" {
		t.Errorf("ctx tenant_id lost after With: %v", got)
	}
}

func TestHandler_WithGroup_PreservesContextLifting(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	l := obs.NewJSONLogger(&buf, slog.LevelInfo).WithGroup("http")

	ctx := obs.WithRequestID(context.Background(), "r-grp")
	l.InfoContext(ctx, "ev", "method", "GET")

	got := decode(t, &buf)
	// When WithGroup has been called, AddAttrs in our handler land
	// inside the group along with the explicit args. That keeps
	// behaviour consistent with stdlib slog: callers that group
	// http-related fields get every per-record attr inside that
	// group, including the ctx-lifted ids.
	httpGroup, ok := got["http"].(map[string]any)
	if !ok {
		t.Fatalf("http group missing: %v", got)
	}
	if httpGroup["method"] != "GET" {
		t.Errorf("group method missing: %v", got)
	}
	if httpGroup["request_id"] != "r-grp" {
		t.Errorf("ctx request_id should land inside http group: %v", got)
	}
}

func TestRedactedEmail(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"":                "[redacted]",
		"no-at-symbol":    "[redacted]",
		"alice@inbox.com": "a***@inbox.com",
		"@bare-domain.io": "[redacted]@bare-domain.io",
		"x@y":             "x***@y",
	}
	for input, want := range cases {
		got := obs.RedactedEmail(input)
		if got != want {
			t.Errorf("RedactedEmail(%q): got %q, want %q", input, got, want)
		}
	}
}
