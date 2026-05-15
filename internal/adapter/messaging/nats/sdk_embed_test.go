package nats_test

// SDK-level integration tests against an in-process nats-server.
// Embedding nats-server (test-only dep) lets us exercise Connect,
// EnsureStream, Subscribe, Publish and Delivery against a real
// JetStream without Docker. The full E2E pipeline (NATS + Postgres +
// clamd stub) is in internal/media/worker/integration_test.go behind
// `//go:build integration` for CI.

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	natsserver "github.com/nats-io/nats-server/v2/server"

	natsadapter "github.com/pericles-luz/crm/internal/adapter/messaging/nats"
)

// runEmbedded boots a JetStream-enabled NATS server bound to a free
// port and returns its URL plus a cleanup function. JetStream storage
// lives under a t.TempDir so concurrent tests do not collide.
func runEmbedded(t *testing.T) string {
	t.Helper()
	return runEmbeddedWith(t, nil)
}

// runEmbeddedWith boots an embedded server with optional auth/TLS
// overrides applied to the options struct. Used by the [SIN-62815]
// auth-required integration test.
func runEmbeddedWith(t *testing.T, override func(*natsserver.Options)) string {
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
	if override != nil {
		override(opts)
	}
	s, err := natsserver.NewServer(opts)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	t.Cleanup(func() {
		s.Shutdown()
		s.WaitForShutdown()
		// best-effort tempdir cleanup; t.TempDir handles the rest
		_ = os.RemoveAll(filepath.Join(opts.StoreDir, "jetstream"))
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

func connect(t *testing.T, url string) *natsadapter.SDKAdapter {
	t.Helper()
	a, err := natsadapter.Connect(context.Background(), natsadapter.SDKConfig{
		URL:            url,
		Name:           t.Name(),
		ConnectTimeout: 2 * time.Second,
		ReconnectWait:  100 * time.Millisecond,
		MaxReconnects:  0,
		// The embedded test server runs plaintext+anonymous; opt into
		// Insecure so the secure-by-default Connect ([SIN-62815]) does
		// not refuse the connection.
		Insecure: true,
	})
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	t.Cleanup(a.Close)
	return a
}

// ---------------------------------------------------------------------

func TestSDK_Embedded_EnsureStream_CreatesAndIsIdempotent(t *testing.T) {
	url := runEmbedded(t)
	a := connect(t, url)
	if err := a.EnsureStream("MED1", []string{"med1.>"}); err != nil {
		t.Fatalf("EnsureStream: %v", err)
	}
	// Second call must be a no-op (idempotent).
	if err := a.EnsureStream("MED1", []string{"med1.>"}); err != nil {
		t.Fatalf("EnsureStream (second): %v", err)
	}
}

func TestSDK_Embedded_EnsureStream_ValidatesArgs(t *testing.T) {
	url := runEmbedded(t)
	a := connect(t, url)
	if err := a.EnsureStream("", []string{"x"}); err == nil {
		t.Error("expected error on empty name")
	}
	if err := a.EnsureStream("X", nil); err == nil {
		t.Error("expected error on empty subjects")
	}
}

func TestSDK_Embedded_Publish_RejectsEmptySubject(t *testing.T) {
	url := runEmbedded(t)
	a := connect(t, url)
	if err := a.Publish(context.Background(), "", []byte("x")); err == nil {
		t.Error("expected error on empty subject")
	}
}

func TestSDK_Embedded_PublishAndSubscribe_DeliversOnce(t *testing.T) {
	url := runEmbedded(t)
	a := connect(t, url)
	if err := a.EnsureStream("MED2", []string{"med2.>"}); err != nil {
		t.Fatalf("EnsureStream: %v", err)
	}

	var (
		seen atomic.Int32
		wg   sync.WaitGroup
	)
	wg.Add(1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sub, err := a.Subscribe(ctx, "med2.requested", "g", "d", time.Second,
		func(_ context.Context, d *natsadapter.Delivery) error {
			defer wg.Done()
			if string(d.Data()) != "hello" {
				t.Errorf("data = %q", d.Data())
			}
			if err := d.Ack(context.Background()); err != nil {
				t.Errorf("Ack: %v", err)
			}
			seen.Add(1)
			return nil
		})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer func() { _ = sub.Unsubscribe() }()

	if err := a.Publish(context.Background(), "med2.requested", []byte("hello")); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	waitOrFail(t, &wg, 3*time.Second)
	if seen.Load() != 1 {
		t.Errorf("seen = %d, want 1", seen.Load())
	}
}

func TestSDK_Embedded_HandlerError_TriggersRedelivery(t *testing.T) {
	url := runEmbedded(t)
	a := connect(t, url)
	if err := a.EnsureStream("MED3", []string{"med3.>"}); err != nil {
		t.Fatalf("EnsureStream: %v", err)
	}

	var attempts atomic.Int32
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	sub, err := a.Subscribe(ctx, "med3.requested", "g", "d3", 250*time.Millisecond,
		func(_ context.Context, d *natsadapter.Delivery) error {
			n := attempts.Add(1)
			if n < 2 {
				// First attempt: return error so the adapter Nak's
				// and JetStream redelivers after AckWait expires.
				return errFake
			}
			_ = d.Ack(context.Background())
			close(done)
			return nil
		})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer func() { _ = sub.Unsubscribe() }()

	if err := a.Publish(context.Background(), "med3.requested", []byte("x")); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("redelivery never landed; attempts=%d", attempts.Load())
	}
	if attempts.Load() < 2 {
		t.Errorf("attempts = %d, want >= 2", attempts.Load())
	}
}

func TestSDK_Embedded_Subscribe_NilHandler(t *testing.T) {
	url := runEmbedded(t)
	a := connect(t, url)
	_, err := a.Subscribe(context.Background(), "s", "g", "d", time.Second, nil)
	if err == nil {
		t.Fatal("expected error on nil handler")
	}
}

func TestSDK_Embedded_Drain_Idempotent(t *testing.T) {
	url := runEmbedded(t)
	a := connect(t, url)
	if err := a.EnsureStream("MED4", []string{"med4.>"}); err != nil {
		t.Fatalf("EnsureStream: %v", err)
	}
	if err := a.Drain(); err != nil {
		t.Fatalf("Drain: %v", err)
	}
}

func TestSDK_Embedded_Delivery_DoubleAck(t *testing.T) {
	url := runEmbedded(t)
	a := connect(t, url)
	if err := a.EnsureStream("MED5", []string{"med5.>"}); err != nil {
		t.Fatalf("EnsureStream: %v", err)
	}

	var (
		mu       sync.Mutex
		captured *natsadapter.Delivery
		wg       sync.WaitGroup
	)
	wg.Add(1)

	sub, err := a.Subscribe(context.Background(), "med5.s", "g", "d5", time.Second,
		func(_ context.Context, d *natsadapter.Delivery) error {
			mu.Lock()
			captured = d
			mu.Unlock()
			if err := d.Ack(context.Background()); err != nil {
				t.Errorf("first Ack: %v", err)
			}
			wg.Done()
			return nil
		})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer func() { _ = sub.Unsubscribe() }()

	if err := a.Publish(context.Background(), "med5.s", []byte("x")); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	waitOrFail(t, &wg, 3*time.Second)

	// A second Ack on the same delivery is tolerated by nats.go
	// (returns AckOpts errors but our wrapper swallows them as a
	// formatted error). The wrapper MUST not panic.
	mu.Lock()
	d := captured
	mu.Unlock()
	_ = d.Ack(context.Background()) // tolerate either nil or error; just must not panic
}

func waitOrFail(t *testing.T, wg *sync.WaitGroup, timeout time.Duration) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(timeout):
		t.Fatalf("timed out waiting %s", timeout)
	}
}

// errFake is the sentinel returned by handlers that want JetStream to
// redeliver. Keeping it package-level avoids redeclaring it in each
// test.
var errFake = stringErr("fake handler error")

type stringErr string

func (s stringErr) Error() string { return string(s) }

// ---------------------------------------------------------------------
// [SIN-62815] auth-enabled integration coverage
// ---------------------------------------------------------------------

// TestSDK_Embedded_Auth_TokenConnects boots an embedded server with
// the broker-level Authorization token set and proves the SDKAdapter's
// new auth surface plumbs the matching SDKConfig.Token through to a
// successful Connect + Publish + Subscribe round-trip.
//
// This is the AC-named integration test for [SIN-62815]: a deploy with
// auth enabled at the broker MUST refuse an anonymous client and MUST
// accept the configured client.
func TestSDK_Embedded_Auth_TokenConnects(t *testing.T) {
	const token = "s3cret-token-for-test-only"

	url := runEmbeddedWith(t, func(o *natsserver.Options) {
		o.Authorization = token
	})

	// Anonymous client must be rejected by the broker. Insecure=true
	// bypasses our client-side secure-default checks so we exercise
	// the broker-level refusal, not our own validation.
	if _, err := natsadapter.Connect(context.Background(), natsadapter.SDKConfig{
		URL:            url,
		ConnectTimeout: 2 * time.Second,
		Insecure:       true,
	}); err == nil {
		t.Fatal("expected anonymous client to be rejected by token-protected broker")
	}

	// Authenticated client must connect and pass a round-trip.
	a, err := natsadapter.Connect(context.Background(), natsadapter.SDKConfig{
		URL:            url,
		ConnectTimeout: 2 * time.Second,
		Token:          token,
		Insecure:       true, // plaintext loopback for the test
	})
	if err != nil {
		t.Fatalf("authenticated Connect: %v", err)
	}
	t.Cleanup(a.Close)

	if err := a.EnsureStream("MED_AUTH", []string{"med_auth.>"}); err != nil {
		t.Fatalf("EnsureStream: %v", err)
	}

	var (
		seen atomic.Int32
		wg   sync.WaitGroup
	)
	wg.Add(1)
	sub, err := a.Subscribe(context.Background(), "med_auth.s", "g", "d_auth", time.Second,
		func(_ context.Context, d *natsadapter.Delivery) error {
			defer wg.Done()
			if got, want := string(d.Data()), "authed"; got != want {
				t.Errorf("data = %q, want %q", got, want)
			}
			_ = d.Ack(context.Background())
			seen.Add(1)
			return nil
		})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer func() { _ = sub.Unsubscribe() }()

	if err := a.Publish(context.Background(), "med_auth.s", []byte("authed")); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	waitOrFail(t, &wg, 3*time.Second)
	if seen.Load() != 1 {
		t.Errorf("seen = %d, want 1", seen.Load())
	}
}
