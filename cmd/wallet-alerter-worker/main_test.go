// Tests for the wallet-alerter-worker entrypoint. Like
// cmd/mediascan-worker, the heavy integration coverage lives in
// internal/worker/wallet_alerter (integration_test.go drives the real
// SDK adapter + the real Slack-webhook adapter against an embedded
// JetStream); the tests here pin the env-parsing, validation, and
// wiring-shape contracts so a deploy mistake fails at startup with a
// message that names the env knob.

package main

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	natsadapter "github.com/pericles-luz/crm/internal/adapter/messaging/nats"
	"github.com/pericles-luz/crm/internal/worker/wallet_alerter"
)

// clearWorkerEnv resets every env knob loadConfig reads so the test
// starts from a known baseline.
func clearWorkerEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		"NATS_URL", "NATS_NAME", "NATS_CONNECT_TIMEOUT",
		"NATS_TOKEN", "NATS_NKEY_FILE", "NATS_CREDS_FILE",
		"NATS_TLS_CA", "NATS_TLS_CERT", "NATS_TLS_KEY",
		"NATS_INSECURE",
		"SLACK_ALERTS_WEBHOOK_URL",
		"WALLET_ALERTER_DEDUP_TTL", "WALLET_ALERTER_ACK_WAIT",
	} {
		t.Setenv(k, "")
	}
}

func TestLoadConfig_RejectsMissingNATSURL(t *testing.T) {
	clearWorkerEnv(t)
	_, err := loadConfig()
	if err == nil {
		t.Fatal("expected error on missing NATS_URL")
	}
	if !strings.Contains(err.Error(), "NATS_URL") {
		t.Errorf("error %q should mention NATS_URL", err.Error())
	}
}

func TestLoadConfig_AppliesDefaults(t *testing.T) {
	clearWorkerEnv(t)
	t.Setenv("NATS_URL", "nats://nats:4222")
	t.Setenv("NATS_INSECURE", "1")

	c, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if c.natsName != "crm-wallet-alerter-worker" {
		t.Errorf("natsName default = %q, want crm-wallet-alerter-worker", c.natsName)
	}
	if c.natsConnectTimeout != 10*time.Second {
		t.Errorf("natsConnectTimeout default = %v, want 10s", c.natsConnectTimeout)
	}
	if c.dedupTTL != wallet_alerter.DefaultDedupTTL {
		t.Errorf("dedupTTL default = %v, want %v", c.dedupTTL, wallet_alerter.DefaultDedupTTL)
	}
	if c.ackWait != wallet_alerter.DefaultAckWait {
		t.Errorf("ackWait default = %v, want %v", c.ackWait, wallet_alerter.DefaultAckWait)
	}
	if c.slackWebhookURL != "" {
		t.Errorf("slackWebhookURL = %q, want empty (degraded mode)", c.slackWebhookURL)
	}
}

func TestLoadConfig_HonorsOverrides(t *testing.T) {
	clearWorkerEnv(t)
	t.Setenv("NATS_URL", "tls://nats.example:4222")
	t.Setenv("NATS_NAME", "my-worker")
	t.Setenv("NATS_CONNECT_TIMEOUT", "20s")
	t.Setenv("NATS_CREDS_FILE", "/etc/worker.creds")
	t.Setenv("NATS_TLS_CA", "/etc/ca.pem")
	t.Setenv("SLACK_ALERTS_WEBHOOK_URL", "https://hooks.slack.com/services/T/B/X")
	t.Setenv("WALLET_ALERTER_DEDUP_TTL", "30m")
	t.Setenv("WALLET_ALERTER_ACK_WAIT", "5s")

	c, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if c.natsName != "my-worker" {
		t.Errorf("natsName = %q", c.natsName)
	}
	if c.natsConnectTimeout != 20*time.Second {
		t.Errorf("natsConnectTimeout = %v", c.natsConnectTimeout)
	}
	if c.natsCredsFile != "/etc/worker.creds" {
		t.Errorf("natsCredsFile = %q", c.natsCredsFile)
	}
	if c.dedupTTL != 30*time.Minute {
		t.Errorf("dedupTTL = %v", c.dedupTTL)
	}
	if c.ackWait != 5*time.Second {
		t.Errorf("ackWait = %v", c.ackWait)
	}
	if c.slackWebhookURL == "" {
		t.Error("slackWebhookURL should be set")
	}
}

func TestLoadConfig_RejectsBadConnectTimeout(t *testing.T) {
	clearWorkerEnv(t)
	t.Setenv("NATS_URL", "nats://nats:4222")
	t.Setenv("NATS_INSECURE", "1")
	t.Setenv("NATS_CONNECT_TIMEOUT", "-1s")
	_, err := loadConfig()
	if err == nil || !strings.Contains(err.Error(), "NATS_CONNECT_TIMEOUT") {
		t.Fatalf("expected NATS_CONNECT_TIMEOUT error, got %v", err)
	}
}

func TestLoadConfig_RejectsBadDedupTTL(t *testing.T) {
	clearWorkerEnv(t)
	t.Setenv("NATS_URL", "nats://nats:4222")
	t.Setenv("NATS_INSECURE", "1")
	t.Setenv("WALLET_ALERTER_DEDUP_TTL", "garbage")
	_, err := loadConfig()
	if err == nil || !strings.Contains(err.Error(), "WALLET_ALERTER_DEDUP_TTL") {
		t.Fatalf("expected WALLET_ALERTER_DEDUP_TTL error, got %v", err)
	}
}

func TestLoadConfig_RejectsBadAckWait(t *testing.T) {
	clearWorkerEnv(t)
	t.Setenv("NATS_URL", "nats://nats:4222")
	t.Setenv("NATS_INSECURE", "1")
	t.Setenv("WALLET_ALERTER_ACK_WAIT", "0")
	_, err := loadConfig()
	if err == nil || !strings.Contains(err.Error(), "WALLET_ALERTER_ACK_WAIT") {
		t.Fatalf("expected WALLET_ALERTER_ACK_WAIT error, got %v", err)
	}
}

func TestLoadConfig_PropagatesValidateNATSSecurityError(t *testing.T) {
	clearWorkerEnv(t)
	t.Setenv("NATS_URL", "nats://nats:4222")
	// Plaintext URL without NATS_INSECURE — security validator fires.
	_, err := loadConfig()
	if err == nil || !strings.Contains(err.Error(), "NATS_INSECURE") {
		t.Fatalf("expected NATS_INSECURE error, got %v", err)
	}
}

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
	c := config{natsURL: "nats://nats:4222", natsInsecure: true}
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
	c := config{natsURL: "tls://nats:4222", natsToken: "tok"}
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
	c := config{natsURL: "nats://nats:4222", natsToken: "tok"}
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
		natsCredsFile:   "/etc/worker.creds",
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

func TestBuildNATSConfig_TranslatesEnv(t *testing.T) {
	t.Parallel()
	cfg := config{
		natsURL:            "tls://nats:4222",
		natsName:           "wlk",
		natsConnectTimeout: 7 * time.Second,
		natsCredsFile:      "/etc/x.creds",
		natsNKeyFile:       "",
		natsToken:          "",
		natsTLSCAFile:      "/etc/ca.pem",
		natsTLSCertFile:    "/etc/c.pem",
		natsTLSKeyFile:     "/etc/k.pem",
		natsInsecure:       false,
	}
	out := buildNATSConfig(cfg)
	if out.URL != "tls://nats:4222" || out.Name != "wlk" || out.ConnectTimeout != 7*time.Second {
		t.Errorf("URL/Name/Timeout = %+v", out)
	}
	if out.MaxReconnects != -1 {
		t.Errorf("MaxReconnects = %d, want -1", out.MaxReconnects)
	}
	if out.CredsFile != "/etc/x.creds" || out.TLSCAFile != "/etc/ca.pem" || out.TLSCertFile != "/etc/c.pem" || out.TLSKeyFile != "/etc/k.pem" {
		t.Errorf("creds/TLS mismatch = %+v", out)
	}
}

func TestEnvBool_TruthyValues(t *testing.T) {
	cases := map[string]bool{
		"1": true, "true": true, "TRUE": true, "yes": true, "on": true, " 1 ": true,
		"0": false, "false": false, "": false, "no": false,
	}
	for in, want := range cases {
		in, want := in, want
		t.Run(in, func(t *testing.T) {
			t.Setenv("WALLET_ALERTER_TEST_KNOB", in)
			if got := envBool("WALLET_ALERTER_TEST_KNOB"); got != want {
				t.Errorf("envBool(%q) = %v, want %v", in, got, want)
			}
		})
	}
}

func TestEnvOr_FallbackAndOverride(t *testing.T) {
	clearWorkerEnv(t)
	if got := envOr("NATS_NAME", "fallback"); got != "fallback" {
		t.Errorf("envOr without env = %q, want fallback", got)
	}
	t.Setenv("NATS_NAME", "override")
	if got := envOr("NATS_NAME", "fallback"); got != "override" {
		t.Errorf("envOr with env = %q, want override", got)
	}
}

// -----------------------------------------------------------------------------
// walletRunConfig + Run wiring
// -----------------------------------------------------------------------------

type stubNotifier struct{}

func (stubNotifier) Notify(_ context.Context, _ string) error { return nil }

func TestWalletRunConfig_PropagatesFlagsAndDegradedPosture(t *testing.T) {
	t.Parallel()
	logger := slog.New(slog.DiscardHandler)
	notifier := stubNotifier{}

	t.Run("configured", func(t *testing.T) {
		t.Parallel()
		cfg := config{
			slackWebhookURL: "https://hooks.slack.com/services/T/B/X",
			dedupTTL:        45 * time.Minute,
			ackWait:         8 * time.Second,
		}
		got := walletRunConfig(cfg, notifier, logger)
		if got.NotifyDegraded {
			t.Error("NotifyDegraded should be false when webhook URL is set")
		}
		if got.DedupTTL != 45*time.Minute || got.AckWait != 8*time.Second {
			t.Errorf("DedupTTL/AckWait mismatch: %+v", got)
		}
		if got.Notifier == nil || got.Logger == nil {
			t.Errorf("Notifier/Logger must be propagated: %+v", got)
		}
	})

	t.Run("degraded", func(t *testing.T) {
		t.Parallel()
		cfg := config{
			slackWebhookURL: "",
			dedupTTL:        wallet_alerter.DefaultDedupTTL,
			ackWait:         wallet_alerter.DefaultAckWait,
		}
		got := walletRunConfig(cfg, notifier, logger)
		if !got.NotifyDegraded {
			t.Error("NotifyDegraded should be true when webhook URL is empty")
		}
	})
}

// fakeSubscriber records the wiring contract Run hands to
// wallet_alerter.Run. Used together with a runner override so the test
// can assert what flows through walletRunConfig without standing up
// either a real or embedded NATS server.
type fakeSubscriber struct{}

func (fakeSubscriber) EnsureStream(string, []string) error { return nil }
func (fakeSubscriber) Subscribe(context.Context, string, string, string, time.Duration, wallet_alerter.HandlerFunc) (wallet_alerter.Subscription, error) {
	return nil, errors.New("not used in this test")
}
func (fakeSubscriber) Drain() error { return nil }

func TestRun_DelegatesToRunner(t *testing.T) {
	t.Parallel()
	saved := runner
	t.Cleanup(func() { runner = saved })

	var gotCfg wallet_alerter.RunConfig
	var gotSub wallet_alerter.Subscriber
	called := false
	runner = func(_ context.Context, sub wallet_alerter.Subscriber, cfg wallet_alerter.RunConfig) error {
		called = true
		gotSub = sub
		gotCfg = cfg
		return nil
	}

	cfg := wallet_alerter.RunConfig{
		Notifier:       stubNotifier{},
		NotifyDegraded: true,
		Logger:         slog.New(slog.DiscardHandler),
		DedupTTL:       2 * time.Minute,
		AckWait:        4 * time.Second,
	}
	sub := fakeSubscriber{}
	if err := Run(context.Background(), sub, cfg); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !called {
		t.Fatal("runner was not invoked")
	}
	if _, ok := gotSub.(fakeSubscriber); !ok {
		t.Errorf("runner saw subscriber %T, want fakeSubscriber", gotSub)
	}
	if gotCfg.DedupTTL != 2*time.Minute || gotCfg.AckWait != 4*time.Second || !gotCfg.NotifyDegraded {
		t.Errorf("runner saw cfg %+v", gotCfg)
	}
}

func TestRun_PropagatesRunnerError(t *testing.T) {
	t.Parallel()
	saved := runner
	t.Cleanup(func() { runner = saved })

	wantErr := errors.New("boom")
	runner = func(context.Context, wallet_alerter.Subscriber, wallet_alerter.RunConfig) error {
		return wantErr
	}

	got := Run(context.Background(), fakeSubscriber{}, wallet_alerter.RunConfig{
		Notifier: stubNotifier{},
		Logger:   slog.New(slog.DiscardHandler),
	})
	if !errors.Is(got, wantErr) {
		t.Fatalf("Run returned %v, want %v", got, wantErr)
	}
}

// -----------------------------------------------------------------------------
// natsAdapterShim — smoke against an embedded JetStream server so the
// three thin pass-throughs (EnsureStream / Subscribe / Drain) are
// exercised. Integration coverage for the underlying SDKAdapter lives in
// internal/adapter/messaging/nats and internal/worker/wallet_alerter;
// the test here only proves the shim hands the calls through.
// -----------------------------------------------------------------------------

func TestNatsAdapterShim_SatisfiesSubscriber(t *testing.T) {
	t.Parallel()
	// Compile-time fence already enforces this; the runtime check makes
	// the contract visible in test output too.
	var _ wallet_alerter.Subscriber = (*natsAdapterShim)(nil)
}

func TestNatsAdapterShim_PassesThroughToSDK(t *testing.T) {
	url := runEmbeddedNATSForShim(t)

	sdk, err := natsadapter.Connect(context.Background(), natsadapter.SDKConfig{
		URL:            url,
		Name:           t.Name(),
		ConnectTimeout: 2 * time.Second,
		ReconnectWait:  100 * time.Millisecond,
		MaxReconnects:  0,
		Insecure:       true,
	})
	if err != nil {
		t.Fatalf("nats Connect: %v", err)
	}
	t.Cleanup(sdk.Close)

	shim := &natsAdapterShim{a: sdk}

	if err := shim.EnsureStream("WALLET_SHIM", []string{"wallet.shim.test"}); err != nil {
		t.Fatalf("EnsureStream: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sub, err := shim.Subscribe(ctx, "wallet.shim.test", "wallet-shim-q", "wallet-shim-d", 500*time.Millisecond,
		func(context.Context, wallet_alerter.Delivery) error { return nil },
	)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	t.Cleanup(func() { _ = sub.Drain() })

	if err := shim.Drain(); err != nil {
		t.Fatalf("Drain: %v", err)
	}
}

// -----------------------------------------------------------------------------
// run() error paths — these do NOT dial NATS; they exercise the
// loadConfig / Connect failure surface so the entrypoint's wrapping is
// pinned. A real subscribe path is covered by the worker package's
// integration tests.
// -----------------------------------------------------------------------------

func TestRunFunc_PropagatesLoadConfigError(t *testing.T) {
	clearWorkerEnv(t)
	logger := slog.New(slog.DiscardHandler)
	err := run(logger)
	if err == nil || !strings.Contains(err.Error(), "config") {
		t.Fatalf("expected config error, got %v", err)
	}
}

func TestRunFunc_PropagatesNATSConnectError(t *testing.T) {
	clearWorkerEnv(t)
	// Validation accepts wss:// + creds + CA, but Connect will fail
	// because the file paths do not exist on disk. We assert that the
	// failure is wrapped with the "nats.Connect" prefix the operator
	// expects.
	t.Setenv("NATS_URL", "tls://nonexistent.invalid:4222")
	t.Setenv("NATS_TLS_CA", "/nonexistent/ca.pem")
	t.Setenv("NATS_CREDS_FILE", "/nonexistent/worker.creds")
	t.Setenv("NATS_CONNECT_TIMEOUT", "100ms")

	logger := slog.New(slog.DiscardHandler)
	err := run(logger)
	if err == nil || !strings.Contains(err.Error(), "nats.Connect") {
		t.Fatalf("expected nats.Connect error wrapping, got %v", err)
	}
}
