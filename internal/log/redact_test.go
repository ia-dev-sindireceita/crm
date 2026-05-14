package log_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	ilog "github.com/pericles-luz/crm/internal/log"
)

func decodeLastLine(t *testing.T, buf *bytes.Buffer) map[string]any {
	t.Helper()
	out := strings.TrimSpace(buf.String())
	if i := strings.LastIndexByte(out, '\n'); i >= 0 {
		out = strings.TrimSpace(out[i+1:])
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(out), &m); err != nil {
		t.Fatalf("json decode %q: %v", out, err)
	}
	return m
}

// --- key allowlist ---

func TestRedact_SensitiveKeyIsRedacted(t *testing.T) {
	t.Parallel()
	cases := []string{
		"password", "passwd", "pwd",
		"token", "jwt", "refresh_token", "access_token",
		"api_key", "authorization",
		"cookie", "set_cookie",
		"secret", "recovery_code", "totp_secret", "otp_seed",
		"cpf", "cnpj",
		"phone", "telefone",
		"email", "e_mail", "mail",
	}
	for _, key := range cases {
		key := key
		t.Run(key, func(t *testing.T) {
			t.Parallel()
			var buf bytes.Buffer
			l := ilog.New(&buf, slog.LevelDebug)
			l.Info("ev", key, "supersecret")
			got := decodeLastLine(t, &buf)
			if got[key] != "[REDACTED]" {
				t.Errorf("key %q: want [REDACTED], got %v", key, got[key])
			}
		})
	}
}

func TestRedact_CaseInsensitiveKey(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	l := ilog.New(&buf, slog.LevelDebug)
	l.Info("ev", "Password", "hunter2", "EMAIL", "x@y.com")
	got := decodeLastLine(t, &buf)
	if got["Password"] != "[REDACTED]" {
		t.Errorf("Password: got %v", got["Password"])
	}
	if got["EMAIL"] != "[REDACTED]" {
		t.Errorf("EMAIL: got %v", got["EMAIL"])
	}
}

func TestRedact_NonSensitiveKeyPassesThrough(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	l := ilog.New(&buf, slog.LevelDebug)
	l.Info("ev", "username", "alice", "tenant_id", "t-1")
	got := decodeLastLine(t, &buf)
	if got["username"] != "alice" {
		t.Errorf("username: got %v", got["username"])
	}
	if got["tenant_id"] != "t-1" {
		t.Errorf("tenant_id: got %v", got["tenant_id"])
	}
}

// --- struct tag ---

type loginAttempt struct {
	Username string
	Password string `log:"redact"`
	TOTP     string `log:"redact"`
	IP       string
}

func TestRedact_StructTagRedactsField(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	l := ilog.New(&buf, slog.LevelDebug)
	a := loginAttempt{
		Username: "alice",
		Password: "correct horse battery staple",
		TOTP:     "123456",
		IP:       "1.2.3.4",
	}
	l.Info("attempt", "login", a)
	raw := buf.String()
	if strings.Contains(raw, "correct horse battery staple") {
		t.Errorf("raw password leaked in log: %s", raw)
	}
	if strings.Contains(raw, "123456") {
		t.Errorf("TOTP leaked in log: %s", raw)
	}
	if !strings.Contains(raw, "alice") {
		t.Errorf("username should be present: %s", raw)
	}
	if !strings.Contains(raw, "1.2.3.4") {
		t.Errorf("IP should be present: %s", raw)
	}
}

func TestRedact_StructWithCPFKey(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	l := ilog.New(&buf, slog.LevelDebug)
	l.Info("ev", "cpf", "123.456.789-00")
	got := decodeLastLine(t, &buf)
	if got["cpf"] != "[REDACTED]" {
		t.Errorf("cpf: got %v", got["cpf"])
	}
}

// --- WithAttrs and WithGroup ---

func TestRedact_WithAttrs_ScrubsAtBind(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	l := ilog.New(&buf, slog.LevelDebug).With("token", "tok-abc", "svc", "crm")
	l.Info("ev")
	got := decodeLastLine(t, &buf)
	if got["token"] != "[REDACTED]" {
		t.Errorf("token via WithAttrs: got %v", got["token"])
	}
	if got["svc"] != "crm" {
		t.Errorf("svc via WithAttrs: got %v", got["svc"])
	}
}

func TestRedact_NilWriter_Discards(t *testing.T) {
	t.Parallel()
	l := ilog.New(nil, slog.LevelInfo)
	l.Info("dropped")
}

// --- NewRawEvent / LogRawEvent ---

func TestRawEvent_DoesNotLeakPayload(t *testing.T) {
	t.Parallel()
	payload := []byte(`{"password":"supersecret","body":"raw content"}`)
	ev := ilog.NewRawEvent(payload)

	var buf bytes.Buffer
	l := ilog.New(&buf, slog.LevelError)
	ilog.LogRawEvent(context.Background(), l, "webhook received", ev)

	out := buf.String()
	if strings.Contains(out, "supersecret") {
		t.Errorf("raw payload leaked: %s", out)
	}
	if strings.Contains(out, "raw content") {
		t.Errorf("raw content leaked: %s", out)
	}
	if !strings.Contains(out, ev.ID) {
		t.Errorf("raw_event_id missing: %s", out)
	}
	if !strings.Contains(out, ev.PayloadSHA) {
		t.Errorf("payload_sha256 missing: %s", out)
	}
}

func TestRedact_WithGroup_ScrubsInsideGroup(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	l := ilog.New(&buf, slog.LevelDebug).WithGroup("req")
	l.Info("ev", "password", "s3cr3t", "method", "POST")
	out := buf.String()
	if strings.Contains(out, "s3cr3t") {
		t.Errorf("password leaked in group log: %s", out)
	}
	if !strings.Contains(out, "POST") {
		t.Errorf("method should be present: %s", out)
	}
}

func TestRedact_GroupAttr_ScrubsNestedKeys(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	l := ilog.New(&buf, slog.LevelDebug)
	l.Info("ev", slog.Group("creds", "token", "tok-xyz", "user", "alice"))
	out := buf.String()
	if strings.Contains(out, "tok-xyz") {
		t.Errorf("token in group leaked: %s", out)
	}
	if !strings.Contains(out, "alice") {
		t.Errorf("user in group should be present: %s", out)
	}
}

func TestRedact_StructPointer_Redacted(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	l := ilog.New(&buf, slog.LevelDebug)
	a := &loginAttempt{
		Username: "bob",
		Password: "password123",
		IP:       "5.6.7.8",
	}
	l.Info("attempt", "login", a)
	out := buf.String()
	if strings.Contains(out, "password123") {
		t.Errorf("password via pointer leaked: %s", out)
	}
}

func TestRedact_StructNoTagFields_PassesThrough(t *testing.T) {
	t.Parallel()
	type plain struct{ Name, Role string }
	var buf bytes.Buffer
	l := ilog.New(&buf, slog.LevelDebug)
	l.Info("ev", "obj", plain{Name: "alice", Role: "admin"})
	out := buf.String()
	if !strings.Contains(out, "alice") {
		t.Errorf("plain struct field should pass through: %s", out)
	}
}

func TestRawEvent_SHA256_IsStable(t *testing.T) {
	t.Parallel()
	payload := []byte("hello")
	ev1 := ilog.NewRawEvent(payload)
	ev2 := ilog.NewRawEvent(payload)
	// IDs differ (UUID), but SHA must be identical
	if ev1.PayloadSHA != ev2.PayloadSHA {
		t.Errorf("SHA mismatch: %v vs %v", ev1.PayloadSHA, ev2.PayloadSHA)
	}
	if ev1.ID == ev2.ID {
		t.Errorf("UUIDs should differ")
	}
}
