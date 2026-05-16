package wallet_alerter_test

// Integration coverage for the SIN-62905 acceptance criteria. The test
// stands up:
//
//   - An in-process JetStream NATS server (the same embed used by the
//     internal/adapter/messaging/nats package suite).
//   - A net/http/httptest mock for the Slack incoming webhook.
//
// We then drive the worker against the real SDK adapter and the real
// Slack-webhook adapter (internal/adapter/notify/slack) — only the
// network is faked. This exercises the entire pipeline that ships:
// NATS Publish → JetStream durable → SDK adapter Subscribe → worker
// Handle → Slack adapter POST.

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	natsserver "github.com/nats-io/nats-server/v2/server"

	natsadapter "github.com/pericles-luz/crm/internal/adapter/messaging/nats"
	slacknotify "github.com/pericles-luz/crm/internal/adapter/notify/slack"
	"github.com/pericles-luz/crm/internal/worker/wallet_alerter"
)

// runEmbeddedNATS boots a JetStream-enabled nats-server bound to a free
// loopback port and returns its client URL. The store lives under
// t.TempDir so concurrent tests do not collide.
func runEmbeddedNATS(t *testing.T) string {
	t.Helper()
	port := pickFreePort(t)
	opts := &natsserver.Options{
		Host:      "127.0.0.1",
		Port:      port,
		NoLog:     true,
		NoSigs:    true,
		JetStream: true,
		StoreDir:  t.TempDir(),
	}
	s, err := natsserver.NewServer(opts)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	t.Cleanup(func() {
		s.Shutdown()
		s.WaitForShutdown()
	})
	go s.Start()
	if !s.ReadyForConnections(5 * time.Second) {
		t.Fatal("nats-server not ready in time")
	}
	return s.ClientURL()
}

func pickFreePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func connectSDK(t *testing.T, url string) *natsadapter.SDKAdapter {
	t.Helper()
	a, err := natsadapter.Connect(context.Background(), natsadapter.SDKConfig{
		URL:            url,
		Name:           t.Name(),
		ConnectTimeout: 2 * time.Second,
		ReconnectWait:  100 * time.Millisecond,
		MaxReconnects:  0,
		Insecure:       true, // embedded plaintext loopback
	})
	if err != nil {
		t.Fatalf("nats Connect: %v", err)
	}
	t.Cleanup(a.Close)
	return a
}

// natsAdapterShim adapts *natsadapter.SDKAdapter to the worker's
// Subscriber port. Same pattern as cmd/mediascan-worker's shim — the
// SDK adapter's Subscribe returns *natsgo.Subscription / SDK-typed
// Delivery, so we re-wrap to the package-local Subscription / Delivery
// interfaces here.
type natsAdapterShim struct{ a *natsadapter.SDKAdapter }

func (n *natsAdapterShim) EnsureStream(name string, subjects []string) error {
	return n.a.EnsureStream(name, subjects)
}

func (n *natsAdapterShim) Subscribe(
	ctx context.Context,
	subject, queue, durable string,
	ackWait time.Duration,
	handler wallet_alerter.HandlerFunc,
) (wallet_alerter.Subscription, error) {
	return n.a.Subscribe(ctx, subject, queue, durable, ackWait,
		func(c context.Context, d *natsadapter.Delivery) error {
			return handler(c, d)
		},
	)
}

func (n *natsAdapterShim) Drain() error { return n.a.Drain() }

// captured is the parsed Slack POST body the mock server records.
type captured struct {
	Text string `json:"text"`
}

// startSlackMock boots an httptest server that records every POST it
// receives. Returns the URL and a snapshot accessor.
func startSlackMock(t *testing.T) (string, func() []captured) {
	t.Helper()
	var (
		mu   sync.Mutex
		recs []captured
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		var c captured
		if err := json.Unmarshal(body, &c); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		mu.Lock()
		recs = append(recs, c)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	return srv.URL, func() []captured {
		mu.Lock()
		defer mu.Unlock()
		out := make([]captured, len(recs))
		copy(out, recs)
		return out
	}
}

// ---------------------------------------------------------------------
// AC #1 — Integration test with NATS jetstream embed + mock Slack:
// publish wallet.balance.depleted → POST recebido no mock com body correto.
// ---------------------------------------------------------------------

func TestIntegration_PublishedEvent_PostsToSlack(t *testing.T) {
	url := runEmbeddedNATS(t)
	sdk := connectSDK(t, url)

	slackURL, snapshot := startSlackMock(t)
	notifier := slacknotify.New(slackURL)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	runDone := make(chan error, 1)
	go func() {
		runDone <- wallet_alerter.Run(ctx, &natsAdapterShim{a: sdk}, wallet_alerter.RunConfig{
			Notifier: notifier,
			Logger:   silentLogger(),
			AckWait:  500 * time.Millisecond,
		})
	}()

	// Publish a single event. The Run goroutine ensures the stream
	// before the subscription returns, so a Publish issued AFTER the
	// stream exists is what we need — wait for that condition.
	waitForStream(t, sdk, wallet_alerter.StreamName)

	body := []byte(`{
		"tenant_id": "tenant-abc",
		"policy_scope": "tenant:default",
		"last_charge_tokens": 7777,
		"occurred_at": "2026-05-16T19:42:00Z"
	}`)
	if err := sdk.Publish(context.Background(), wallet_alerter.Subject, body); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	waitForPOSTs(t, snapshot, 1, 3*time.Second)
	got := snapshot()
	if len(got) != 1 {
		t.Fatalf("Slack POST count = %d, want 1", len(got))
	}
	const want = ":warning: Wallet zerada em tenant `tenant-abc` (escopo `tenant:default`). Último débito: 7777 tokens em 2026-05-16T19:42:00Z."
	if got[0].Text != want {
		t.Errorf("Slack body mismatch:\n got: %s\nwant: %s", got[0].Text, want)
	}

	cancel()
	if err := <-runDone; err != nil {
		t.Errorf("Run returned: %v", err)
	}
}

// ---------------------------------------------------------------------
// AC #2 — Test de dedup: 2 events com mesma chave em <1h → 1 POST.
// ---------------------------------------------------------------------

func TestIntegration_DuplicateEvents_PostOnce(t *testing.T) {
	url := runEmbeddedNATS(t)
	sdk := connectSDK(t, url)

	slackURL, snapshot := startSlackMock(t)
	notifier := slacknotify.New(slackURL)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	runDone := make(chan error, 1)
	go func() {
		runDone <- wallet_alerter.Run(ctx, &natsAdapterShim{a: sdk}, wallet_alerter.RunConfig{
			Notifier: notifier,
			Logger:   silentLogger(),
			AckWait:  500 * time.Millisecond,
			DedupTTL: time.Hour,
		})
	}()

	waitForStream(t, sdk, wallet_alerter.StreamName)

	body := []byte(`{
		"tenant_id": "tenant-dup",
		"policy_scope": "tenant:default",
		"last_charge_tokens": 1,
		"occurred_at": "2026-05-16T19:42:00Z"
	}`)

	for i := 0; i < 3; i++ {
		if err := sdk.Publish(context.Background(), wallet_alerter.Subject, body); err != nil {
			t.Fatalf("Publish #%d: %v", i, err)
		}
	}

	// Give the worker a chance to process all three deliveries. If
	// dedup is broken we'd see 3 POSTs; if it works we see exactly 1.
	// Use a small settling window rather than a strict count-equals
	// because the broker may batch deliveries.
	time.Sleep(750 * time.Millisecond)
	got := snapshot()
	if len(got) != 1 {
		t.Errorf("Slack POST count = %d, want 1 (dedup must collapse identical events)", len(got))
	}

	cancel()
	if err := <-runDone; err != nil {
		t.Errorf("Run returned: %v", err)
	}
}

// ---------------------------------------------------------------------
// AC #3 — SLACK_ALERTS_WEBHOOK_URL ausente → worker loga warning e segue.
// ---------------------------------------------------------------------

func TestIntegration_EmptyWebhookURL_WorkerStaysUp(t *testing.T) {
	url := runEmbeddedNATS(t)
	sdk := connectSDK(t, url)

	// Empty URL: the Slack adapter degrades to a silent no-op. The
	// worker MUST still boot, consume the event, and ack — exercising
	// the AC "worker loga warning e segue (não crash; alerta apenas
	// degrada)".
	notifier := slacknotify.New("")

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	// Count Handle invocations indirectly: a no-op notifier returns nil
	// from Notify, so the worker will ack the delivery and we can
	// verify the message was consumed by polling the JetStream
	// pending-count via a second subscriber after the test. For
	// simplicity we instead wrap the notifier in a counter that
	// records the would-have-been Slack POST count without actually
	// faking a server.
	wrapped := &countingNotifier{inner: notifier}

	runDone := make(chan error, 1)
	go func() {
		runDone <- wallet_alerter.Run(ctx, &natsAdapterShim{a: sdk}, wallet_alerter.RunConfig{
			Notifier:       wrapped,
			NotifyDegraded: true, // mirrors the cmd boot path
			Logger:         silentLogger(),
			AckWait:        500 * time.Millisecond,
		})
	}()

	waitForStream(t, sdk, wallet_alerter.StreamName)

	body := []byte(`{
		"tenant_id": "tenant-degraded",
		"policy_scope": "tenant:default",
		"last_charge_tokens": 1,
		"occurred_at": "2026-05-16T19:42:00Z"
	}`)
	if err := sdk.Publish(context.Background(), wallet_alerter.Subject, body); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for wrapped.Count() == 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if got := wrapped.Count(); got != 1 {
		t.Errorf("Notify invocations = %d, want 1 (worker must process the event even with no webhook)", got)
	}

	cancel()
	if err := <-runDone; err != nil {
		t.Errorf("Run returned: %v", err)
	}
}

// countingNotifier records Notify invocations while delegating to an
// inner Notifier. Used by the degraded-URL test to prove the worker
// dispatched (the inner adapter's no-op branch is what handles the
// empty-URL case).
type countingNotifier struct {
	inner wallet_alerter.Notifier
	n     atomic.Int32
}

func (c *countingNotifier) Notify(ctx context.Context, msg string) error {
	c.n.Add(1)
	return c.inner.Notify(ctx, msg)
}

func (c *countingNotifier) Count() int { return int(c.n.Load()) }

// ---------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------

func waitForStream(t *testing.T, sdk *natsadapter.SDKAdapter, name string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		// Publish a no-op probe; if the stream is up the probe succeeds
		// AND the worker's durable consumer is already subscribed (the
		// embedded server creates both eagerly under WorkQueuePolicy).
		// We avoid touching nats.go directly here by issuing the same
		// EnsureStream call the worker already performed — it is
		// idempotent, so a second call against an existing stream
		// returns nil immediately.
		if err := sdk.EnsureStream(name, []string{wallet_alerter.Subject}); err == nil {
			// Give the worker a brief window to register the durable
			// consumer so the first Publish lands on a live binding.
			time.Sleep(50 * time.Millisecond)
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("stream %q never became ready", name)
}

func waitForPOSTs(t *testing.T, snapshot func() []captured, want int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if len(snapshot()) >= want {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
}
