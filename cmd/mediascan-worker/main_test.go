// Tests for the env-level NATS security validation in main.go.
// SDKConfig.validate covers the library-level rules — this file pins
// the operator-facing wording so deploy mistakes fail at startup with a
// message that names the env knob.

package main

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidateNATSSecurity_AcceptsCredsOverTLS(t *testing.T) {
	t.Parallel()
	c := config{
		natsURL:       "tls://nats.example:4222",
		natsCredsFile: "/etc/nats/worker.creds",
		natsTLSCAFile: "/etc/nats/ca.pem",
	}
	if err := validateNATSSecurity(c); err != nil {
		t.Fatalf("expected valid config; got %v", err)
	}
}

func TestValidateNATSSecurity_AcceptsInsecureBypass(t *testing.T) {
	t.Parallel()
	c := config{
		natsURL:      "nats://nats:4222",
		natsInsecure: true,
	}
	if err := validateNATSSecurity(c); err != nil {
		t.Fatalf("expected Insecure=true to bypass; got %v", err)
	}
}

func TestValidateNATSSecurity_RejectsMultipleAuthMethods(t *testing.T) {
	t.Parallel()
	c := config{
		natsURL:       "tls://nats:4222",
		natsTLSCAFile: "/etc/ca.pem",
		natsToken:     "tok",
		natsCredsFile: "/etc/worker.creds",
	}
	err := validateNATSSecurity(c)
	if err == nil {
		t.Fatal("expected error on multiple auth methods")
	}
	for _, want := range []string{"NATS_TOKEN", "NATS_CREDS_FILE"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q should mention %s", err.Error(), want)
		}
	}
}

func TestValidateNATSSecurity_RejectsTLSWithoutCA(t *testing.T) {
	t.Parallel()
	c := config{
		natsURL:   "tls://nats:4222",
		natsToken: "tok",
	}
	err := validateNATSSecurity(c)
	if err == nil {
		t.Fatal("expected error on tls:// without NATS_TLS_CA")
	}
	if !strings.Contains(err.Error(), "NATS_TLS_CA") {
		t.Errorf("error %q should mention NATS_TLS_CA", err.Error())
	}
}

func TestValidateNATSSecurity_RejectsPlaintextWithoutInsecure(t *testing.T) {
	t.Parallel()
	c := config{
		natsURL:   "nats://nats:4222",
		natsToken: "tok",
	}
	err := validateNATSSecurity(c)
	if err == nil {
		t.Fatal("expected error refusing plaintext URL")
	}
	if !strings.Contains(err.Error(), "NATS_INSECURE") {
		t.Errorf("error %q should mention NATS_INSECURE escape", err.Error())
	}
}

func TestValidateNATSSecurity_RejectsMTLSHalfPair(t *testing.T) {
	t.Parallel()
	c := config{
		natsURL:         "tls://nats:4222",
		natsTLSCAFile:   "/etc/ca.pem",
		natsTLSCertFile: "/etc/client.crt",
		// missing key
		natsCredsFile: "/etc/worker.creds",
	}
	err := validateNATSSecurity(c)
	if err == nil {
		t.Fatal("expected error on half mTLS pair")
	}
	if !strings.Contains(err.Error(), "NATS_TLS_CERT") || !strings.Contains(err.Error(), "NATS_TLS_KEY") {
		t.Errorf("error %q should mention both NATS_TLS_CERT and NATS_TLS_KEY", err.Error())
	}
}

func TestValidateNATSSecurity_AcceptsWSSWithCA(t *testing.T) {
	t.Parallel()
	c := config{
		natsURL:       "wss://nats.example:443",
		natsCredsFile: "/etc/worker.creds",
		natsTLSCAFile: "/etc/ca.pem",
	}
	if err := validateNATSSecurity(c); err != nil {
		t.Fatalf("expected wss:// with CA + creds to validate; got %v", err)
	}
}

func TestNATSAuthMode_ReportsMode(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		cfg  config
		want string
	}{
		{"creds", config{natsCredsFile: "/x"}, "creds-file"},
		{"nkey", config{natsNKeyFile: "/x"}, "nkey-file"},
		{"token", config{natsToken: "s"}, "token"},
		{"none", config{}, "none"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := natsAuthMode(tc.cfg); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// clearWorkerEnv resets every env knob loadConfig reads so the test
// starts from a known baseline. Each subtest sets only what it cares
// about and lets the helper drop everything else.
func clearWorkerEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		"NATS_URL", "POSTGRES_DSN", "CLAMD_ADDR",
		"MEDIA_STREAM_NAME", "MEDIA_DURABLE_NAME", "MEDIA_QUEUE_NAME",
		"BLOB_BASE_DIR", "WORKER_CONCURRENCY",
		"NATS_TOKEN", "NATS_NKEY_FILE", "NATS_CREDS_FILE",
		"NATS_TLS_CA", "NATS_TLS_CERT", "NATS_TLS_KEY",
		"NATS_INSECURE",
	} {
		t.Setenv(k, "")
	}
}

func TestLoadConfig_RejectsMissingRequired(t *testing.T) {
	clearWorkerEnv(t)
	_, err := loadConfig()
	if err == nil {
		t.Fatal("expected error on missing required env")
	}
	for _, want := range []string{"NATS_URL", "POSTGRES_DSN", "CLAMD_ADDR"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q should list missing %s", err.Error(), want)
		}
	}
}

func TestLoadConfig_SecureDeployWithCredsFile(t *testing.T) {
	clearWorkerEnv(t)
	t.Setenv("NATS_URL", "tls://nats.example:4222")
	t.Setenv("POSTGRES_DSN", "postgres://x@localhost/db")
	t.Setenv("CLAMD_ADDR", "clamav:3310")
	t.Setenv("NATS_CREDS_FILE", "/etc/nats/worker.creds")
	t.Setenv("NATS_TLS_CA", "/etc/nats/ca.pem")
	t.Setenv("WORKER_CONCURRENCY", "8")

	c, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if c.concurrency != 8 {
		t.Errorf("concurrency = %d, want 8", c.concurrency)
	}
	if c.natsCredsFile != "/etc/nats/worker.creds" {
		t.Errorf("natsCredsFile = %q", c.natsCredsFile)
	}
	if c.natsTLSCAFile != "/etc/nats/ca.pem" {
		t.Errorf("natsTLSCAFile = %q", c.natsTLSCAFile)
	}
	if c.natsInsecure {
		t.Error("natsInsecure should be false on a secure deploy")
	}
}

func TestLoadConfig_RejectsBadConcurrency(t *testing.T) {
	clearWorkerEnv(t)
	t.Setenv("NATS_URL", "nats://nats:4222")
	t.Setenv("POSTGRES_DSN", "postgres://x")
	t.Setenv("CLAMD_ADDR", "clamav:3310")
	t.Setenv("NATS_INSECURE", "1")
	t.Setenv("WORKER_CONCURRENCY", "-3")

	_, err := loadConfig()
	if err == nil {
		t.Fatal("expected error on negative concurrency")
	}
	if !strings.Contains(err.Error(), "WORKER_CONCURRENCY") {
		t.Errorf("error %q should name WORKER_CONCURRENCY", err.Error())
	}
}

func TestLoadConfig_PropagatesValidateNATSSecurityError(t *testing.T) {
	clearWorkerEnv(t)
	t.Setenv("NATS_URL", "nats://nats:4222")
	t.Setenv("POSTGRES_DSN", "postgres://x")
	t.Setenv("CLAMD_ADDR", "clamav:3310")
	// Plaintext URL without NATS_INSECURE — the security validator
	// fires from loadConfig.
	_, err := loadConfig()
	if err == nil {
		t.Fatal("expected validateNATSSecurity error")
	}
	if !strings.Contains(err.Error(), "NATS_INSECURE") {
		t.Errorf("error %q should mention NATS_INSECURE escape", err.Error())
	}
}

func TestEnvOr_FallbackAndOverride(t *testing.T) {
	clearWorkerEnv(t)
	if got := envOr("MEDIA_QUEUE_NAME", "fallback"); got != "fallback" {
		t.Errorf("envOr without env = %q, want fallback", got)
	}
	t.Setenv("MEDIA_QUEUE_NAME", "override")
	if got := envOr("MEDIA_QUEUE_NAME", "fallback"); got != "override" {
		t.Errorf("envOr with env = %q, want override", got)
	}
}

func TestLocalBlobs_OpenRejectsEscapes(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "ok.bin"), []byte("hi"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	l := &localBlobs{root: dir}

	rc, err := l.Open(context.Background(), "ok.bin")
	if err != nil {
		t.Fatalf("Open(clean): %v", err)
	}
	b, _ := io.ReadAll(rc)
	_ = rc.Close()
	if string(b) != "hi" {
		t.Errorf("got %q, want hi", string(b))
	}

	for _, bad := range []string{"../etc/passwd", "/etc/passwd"} {
		if _, err := l.Open(context.Background(), bad); err == nil {
			t.Errorf("Open(%q) should reject path escape", bad)
		}
	}
}

func TestLocalBlobs_OpenWithoutRoot(t *testing.T) {
	t.Parallel()
	l := &localBlobs{root: ""}
	if _, err := l.Open(context.Background(), "x"); err == nil {
		t.Error("expected error when root is empty")
	}
}

// Compile-time fence: localBlobs.Open returns an io.ReadCloser. If
// that contract changes the worker bootstrap will not type-check, so
// failing here makes the breakage visible at test time.
var _ = func() any {
	var l *localBlobs
	var _ func(context.Context, string) (io.ReadCloser, error) = l.Open
	return errors.New("ok")
}()

func TestEnvBool_TruthyValues(t *testing.T) {
	cases := map[string]bool{
		"1":     true,
		"true":  true,
		"TRUE":  true,
		"yes":   true,
		"on":    true,
		" 1 ":   true,
		"0":     false,
		"false": false,
		"":      false,
		"no":    false,
	}
	for in, want := range cases {
		in, want := in, want
		t.Run(in, func(t *testing.T) {
			// t.Setenv mutates process env, which forbids t.Parallel on
			// both this subtest and the parent. Keep this serial.
			t.Setenv("NATS_INSECURE_TEST_KNOB", in)
			if got := envBool("NATS_INSECURE_TEST_KNOB"); got != want {
				t.Errorf("envBool(%q) = %v, want %v", in, got, want)
			}
		})
	}
}
