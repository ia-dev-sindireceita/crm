// Tests for the funnel-engine-worker entrypoint. Like
// cmd/wallet-alerter-worker, the heavy integration coverage lives in
// internal/worker/funnel_engine and internal/funnel/engine; the tests
// here pin the env-parsing, validation, and wiring-shape contracts so
// a deploy mistake fails at startup with a message that names the env
// knob.

package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	natsadapter "github.com/pericles-luz/crm/internal/adapter/messaging/nats"
	"github.com/pericles-luz/crm/internal/funnel/engine"
	"github.com/pericles-luz/crm/internal/worker/funnel_engine"
)

// clearWorkerEnv resets every env knob loadConfig reads.
func clearWorkerEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		"NATS_URL", "NATS_NAME", "NATS_CONNECT_TIMEOUT",
		"NATS_TOKEN", "NATS_NKEY_FILE", "NATS_CREDS_FILE",
		"NATS_TLS_CA", "NATS_TLS_CERT", "NATS_TLS_KEY",
		"NATS_INSECURE",
		"FUNNEL_ENGINE_METRICS_ADDR", "FUNNEL_ENGINE_ACK_WAIT",
		"DATABASE_URL",
	} {
		t.Setenv(k, "")
	}
}

func TestLoadConfig_RejectsMissingNATSURL(t *testing.T) {
	clearWorkerEnv(t)
	t.Setenv("DATABASE_URL", "postgres://x")
	_, err := loadConfig()
	if err == nil || !strings.Contains(err.Error(), "NATS_URL") {
		t.Fatalf("loadConfig: want NATS_URL error, got %v", err)
	}
}

func TestLoadConfig_RejectsMissingDatabaseURL(t *testing.T) {
	clearWorkerEnv(t)
	t.Setenv("NATS_URL", "nats://nats:4222")
	t.Setenv("NATS_INSECURE", "1")
	_, err := loadConfig()
	if err == nil || !strings.Contains(err.Error(), "DATABASE_URL") {
		t.Fatalf("loadConfig: want DATABASE_URL error, got %v", err)
	}
}

func TestLoadConfig_AppliesDefaults(t *testing.T) {
	clearWorkerEnv(t)
	t.Setenv("NATS_URL", "nats://nats:4222")
	t.Setenv("NATS_INSECURE", "1")
	t.Setenv("DATABASE_URL", "postgres://x")
	c, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if c.natsName != "crm-funnel-engine-worker" {
		t.Errorf("natsName default = %q", c.natsName)
	}
	if c.natsConnectTimeout != 10*time.Second {
		t.Errorf("natsConnectTimeout default = %v", c.natsConnectTimeout)
	}
	if c.metricsAddr != ":9402" {
		t.Errorf("metricsAddr default = %q", c.metricsAddr)
	}
	if c.ackWait != funnel_engine.DefaultAckWait {
		t.Errorf("ackWait default = %v, want %v", c.ackWait, funnel_engine.DefaultAckWait)
	}
}

func TestLoadConfig_HonorsOverrides(t *testing.T) {
	clearWorkerEnv(t)
	t.Setenv("NATS_URL", "tls://nats.example:4222")
	t.Setenv("NATS_NAME", "fe-worker")
	t.Setenv("NATS_CONNECT_TIMEOUT", "20s")
	t.Setenv("NATS_CREDS_FILE", "/etc/worker.creds")
	t.Setenv("NATS_TLS_CA", "/etc/ca.pem")
	t.Setenv("FUNNEL_ENGINE_METRICS_ADDR", ":9099")
	t.Setenv("FUNNEL_ENGINE_ACK_WAIT", "60s")
	t.Setenv("DATABASE_URL", "postgres://x")
	c, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if c.natsName != "fe-worker" {
		t.Errorf("natsName = %q", c.natsName)
	}
	if c.metricsAddr != ":9099" {
		t.Errorf("metricsAddr = %q", c.metricsAddr)
	}
	if c.ackWait != 60*time.Second {
		t.Errorf("ackWait = %v", c.ackWait)
	}
}

func TestLoadConfig_RejectsBadAckWait(t *testing.T) {
	clearWorkerEnv(t)
	t.Setenv("NATS_URL", "nats://nats:4222")
	t.Setenv("NATS_INSECURE", "1")
	t.Setenv("DATABASE_URL", "postgres://x")
	t.Setenv("FUNNEL_ENGINE_ACK_WAIT", "garbage")
	_, err := loadConfig()
	if err == nil || !strings.Contains(err.Error(), "FUNNEL_ENGINE_ACK_WAIT") {
		t.Fatalf("expected FUNNEL_ENGINE_ACK_WAIT error, got %v", err)
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
		t.Fatalf("Insecure=true bypass: %v", err)
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
	if err := validateNATSSecurity(c); err == nil {
		t.Fatal("expected error on multiple auth methods")
	}
}

func TestValidateNATSSecurity_RejectsTLSWithoutCA(t *testing.T) {
	t.Parallel()
	c := config{natsURL: "tls://nats:4222", natsToken: "tok"}
	if err := validateNATSSecurity(c); err == nil {
		t.Fatal("expected error on tls:// without NATS_TLS_CA")
	}
}

func TestValidateNATSSecurity_RejectsPlaintextWithoutInsecure(t *testing.T) {
	t.Parallel()
	c := config{natsURL: "nats://nats:4222", natsToken: "tok"}
	err := validateNATSSecurity(c)
	if err == nil || !strings.Contains(err.Error(), "NATS_INSECURE") {
		t.Fatalf("plaintext-without-insecure: %v", err)
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
	if err := validateNATSSecurity(c); err == nil {
		t.Fatal("expected error on half mTLS pair")
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
		natsName:           "fe",
		natsConnectTimeout: 7 * time.Second,
		natsCredsFile:      "/etc/x.creds",
		natsTLSCAFile:      "/etc/ca.pem",
		natsTLSCertFile:    "/etc/c.pem",
		natsTLSKeyFile:     "/etc/k.pem",
	}
	out := buildNATSConfig(cfg)
	want := natsadapter.SDKConfig{
		URL:            "tls://nats:4222",
		Name:           "fe",
		ConnectTimeout: 7 * time.Second,
		MaxReconnects:  -1,
		CredsFile:      "/etc/x.creds",
		TLSCAFile:      "/etc/ca.pem",
		TLSCertFile:    "/etc/c.pem",
		TLSKeyFile:     "/etc/k.pem",
	}
	if out != want {
		t.Errorf("buildNATSConfig = %+v\nwant %+v", out, want)
	}
}

func TestEnvBool_TruthyValues(t *testing.T) {
	// Cannot t.Parallel — t.Setenv requires a non-parallel scope.
	cases := map[string]bool{
		"1": true, "true": true, "TRUE": true, "yes": true, "on": true, " 1 ": true,
		"0": false, "false": false, "": false, "no": false,
	}
	for in, want := range cases {
		t.Setenv("FUNNEL_ENGINE_TEST_BOOL", in)
		if got := envBool("FUNNEL_ENGINE_TEST_BOOL"); got != want {
			t.Errorf("envBool(%q) = %v, want %v", in, got, want)
		}
	}
}

// TestRun_StopsOnContextCancel exercises the cmd-level Run wrapper via
// the runner indirection, so we don't need real NATS for the smoke.
// Mirrors cmd/wallet-alerter-worker/main_test.go.
func TestRun_StopsOnContextCancel(t *testing.T) {
	t.Parallel()
	orig := runner
	t.Cleanup(func() { runner = orig })
	runner = func(ctx context.Context, _ funnel_engine.Subscriber, _ funnel_engine.RunConfig) error {
		<-ctx.Done()
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, nil, funnel_engine.RunConfig{
			Engine:  nil,
			Logger:  slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError})),
			AckWait: time.Second,
		})
	}()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run returned err %v after cancel", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not honour ctx cancel")
	}
}

func TestRun_PropagatesError(t *testing.T) {
	t.Parallel()
	orig := runner
	t.Cleanup(func() { runner = orig })
	sentinel := errors.New("subscriber boom")
	runner = func(_ context.Context, _ funnel_engine.Subscriber, _ funnel_engine.RunConfig) error {
		return sentinel
	}
	err := Run(context.Background(), nil, funnel_engine.RunConfig{
		Engine:  nil,
		Logger:  slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError})),
		AckWait: time.Second,
	})
	if !errors.Is(err, sentinel) {
		t.Errorf("Run did not propagate: got %v", err)
	}
}

func TestMetricsMux_ServesMetricsAndHealth(t *testing.T) {
	t.Parallel()
	reg := prometheus.NewRegistry()
	engineMetrics := engine.NewMetrics(reg)
	// Touch the histograms so /metrics shows them.
	engineMetrics.Latency.Observe(0.001)
	handler := metricsMux(promhttpHandler(reg))
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatalf("/healthz: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("/healthz status = %d", resp.StatusCode)
	}
	healthBody, _ := io.ReadAll(resp.Body)
	if string(healthBody) != "ok" {
		t.Errorf("/healthz body = %q, want ok", string(healthBody))
	}

	resp2, err := http.Get(srv.URL + "/metrics")
	if err != nil {
		t.Fatalf("/metrics: %v", err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Errorf("/metrics status = %d", resp2.StatusCode)
	}
	body, _ := io.ReadAll(resp2.Body)
	if !strings.Contains(string(body), "funnel_evaluation_latency_seconds") {
		t.Errorf("/metrics output missing engine histogram; body=%s", string(body))
	}
}

func promhttpHandler(reg *prometheus.Registry) http.Handler {
	return promhttp.HandlerFor(reg, promhttp.HandlerOpts{
		Registry:          reg,
		EnableOpenMetrics: false,
	})
}

// TestSlogFunnelPublisher_PublishLogs ensures the placeholder
// funnel.EventPublisher used inside the worker process emits one log
// line per Publish call and never returns error.
func TestSlogFunnelPublisher_PublishLogs(t *testing.T) {
	t.Parallel()
	var buf strings.Builder
	logger := slog.New(slog.NewTextHandler(noOpWriter{buf: &buf}, &slog.HandlerOptions{Level: slog.LevelInfo}))
	p := slogFunnelPublisher{logger: logger}
	if err := p.Publish(context.Background(), "funnel.conversation_moved", struct{ X int }{X: 1}); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if !strings.Contains(buf.String(), "funnel.conversation_moved") {
		t.Errorf("log output missing event name: %q", buf.String())
	}
}

type noOpWriter struct{ buf *strings.Builder }

func (w noOpWriter) Write(p []byte) (int, error) { return w.buf.Write(p) }

func TestEnvOr_FallbackAndOverride(t *testing.T) {
	t.Setenv("FUNNEL_ENGINE_TEST_KEY", "")
	if got := envOr("FUNNEL_ENGINE_TEST_KEY", "fallback"); got != "fallback" {
		t.Errorf("envOr empty = %q, want fallback", got)
	}
	t.Setenv("FUNNEL_ENGINE_TEST_KEY", "x")
	if got := envOr("FUNNEL_ENGINE_TEST_KEY", "fallback"); got != "x" {
		t.Errorf("envOr set = %q, want x", got)
	}
}

func TestRun_DefaultsMissingValuesToZero(t *testing.T) {
	// Compile-time fence on the runner type so a future refactor that
	// loses the indirection alerts here.
	t.Parallel()
	var _ func(context.Context, funnel_engine.Subscriber, funnel_engine.RunConfig) error = runner
}

// -----------------------------------------------------------------------------
// run() error paths — exercise the loadConfig / pg / nats failure surface
// so the entrypoint's wrapping is pinned. The happy path is covered by
// the worker package's integration tests against embedded NATS + testpg.
// -----------------------------------------------------------------------------

func TestRunFunc_PropagatesLoadConfigError(t *testing.T) {
	clearWorkerEnv(t)
	logger := slog.New(slog.DiscardHandler)
	err := run(logger)
	if err == nil || !strings.Contains(err.Error(), "config") {
		t.Fatalf("expected config error, got %v", err)
	}
}

func TestRunFunc_PropagatesPostgresConnectError(t *testing.T) {
	clearWorkerEnv(t)
	// Pass validation but fail on the pgxpool dial. The runtime DSN
	// points at an unroutable address with a fast deadline so the
	// failure is deterministic.
	t.Setenv("NATS_URL", "tls://nonexistent.invalid:4222")
	t.Setenv("NATS_TLS_CA", "/nonexistent/ca.pem")
	t.Setenv("NATS_CREDS_FILE", "/nonexistent/worker.creds")
	t.Setenv("NATS_CONNECT_TIMEOUT", "100ms")
	t.Setenv("DATABASE_URL", "postgres://nobody:none@127.0.0.1:1/none?sslmode=disable&connect_timeout=1")

	logger := slog.New(slog.DiscardHandler)
	err := run(logger)
	if err == nil {
		t.Fatal("expected error from run()")
	}
	// Either "pg: connect" or a downstream wrap is acceptable — we
	// just want to be sure the boot bailed.
	if !strings.Contains(err.Error(), "pg:") && !strings.Contains(err.Error(), "nats.Connect") {
		t.Fatalf("expected pg: or nats.Connect error, got %v", err)
	}
}

// -----------------------------------------------------------------------------
// assembleEngine — pure adapter-assembly path. Nil pool is the only
// public failure mode (every adapter under assembleEngine rejects nil
// pools with postgres.ErrNilPool); we exercise it here so the wrap
// label is pinned for operators.
// -----------------------------------------------------------------------------

func TestAssembleEngine_RejectsNilPool(t *testing.T) {
	t.Parallel()
	reg := prometheus.NewRegistry()
	logger := slog.New(slog.DiscardHandler)
	if _, err := assembleEngine(nil, reg, logger); err == nil {
		t.Fatal("expected error on nil pool, got nil")
	}
}

// -----------------------------------------------------------------------------
// natsAdapterShim — smoke against an embedded JetStream server so the
// three thin pass-throughs (EnsureStream / Subscribe / Drain) are
// exercised. Integration coverage for the SDKAdapter lives in
// internal/adapter/messaging/nats; the test here only proves the shim
// hands the calls through.
// -----------------------------------------------------------------------------

func TestNatsAdapterShim_SatisfiesSubscriber(t *testing.T) {
	t.Parallel()
	var _ funnel_engine.Subscriber = (*natsAdapterShim)(nil)
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

	if err := shim.EnsureStream("FUNNEL_ENGINE_SHIM", []string{"funnel.shim.test"}); err != nil {
		t.Fatalf("EnsureStream: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sub, err := shim.Subscribe(ctx, "funnel.shim.test", "funnel-shim-q", "funnel-shim-d", 500*time.Millisecond,
		func(context.Context, funnel_engine.Delivery) error { return nil },
	)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	t.Cleanup(func() { _ = sub.Drain() })

	if err := shim.Drain(); err != nil {
		t.Fatalf("Drain: %v", err)
	}
}
