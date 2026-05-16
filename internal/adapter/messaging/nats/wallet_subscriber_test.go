package nats_test

// SIN-62881 / Fase 2.5 C6 — embedded-JetStream integration tests for
// the wallet allocator subscriber. Reuses the runEmbedded helper from
// sdk_embed_test.go (same package nats_test, same test binary, no
// duplicated server bootstrap).

import (
	"context"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"

	natsadapter "github.com/pericles-luz/crm/internal/adapter/messaging/nats"
	walletworker "github.com/pericles-luz/crm/internal/worker/wallet"
)

const (
	walletTestStream = "WALLET_ALLOC_TEST"
)

func newJSContext(t *testing.T, url string) natsgo.JetStreamContext {
	t.Helper()
	conn, err := natsgo.Connect(url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(conn.Close)
	js, err := conn.JetStream()
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	// Each test gets its own stream by virtue of t.TempDir + a fresh
	// embedded server. Configure 1h Duplicates window so the dedup
	// invariant matches the publisher contract.
	if _, err := js.AddStream(&natsgo.StreamConfig{
		Name:       walletTestStream,
		Subjects:   []string{walletworker.SubjectSubscriptionRenewed},
		Storage:    natsgo.MemoryStorage,
		Retention:  natsgo.WorkQueuePolicy,
		Duplicates: time.Hour,
	}); err != nil {
		t.Fatalf("add stream: %v", err)
	}
	return js
}

func publish(t *testing.T, js natsgo.JetStreamContext, msgID string, body []byte) {
	t.Helper()
	m := &natsgo.Msg{
		Subject: walletworker.SubjectSubscriptionRenewed,
		Data:    body,
		Header:  natsgo.Header{},
	}
	m.Header.Set("Nats-Msg-Id", msgID)
	if _, err := js.PublishMsg(m); err != nil {
		t.Fatalf("publish: %v", err)
	}
}

func TestWalletSubscriber_DeliversAndAcks(t *testing.T) {
	url := runEmbedded(t)
	js := newJSContext(t, url)

	sub, err := natsadapter.NewWalletSubscriber(natsadapter.WalletSubscriberConfig{
		JS:         js,
		Stream:     walletTestStream,
		Durable:    "wallet-test-deliver",
		AckWait:    500 * time.Millisecond,
		MaxDeliver: 3,
		BackOff:    []time.Duration{50 * time.Millisecond, 50 * time.Millisecond},
		BufferSize: 4,
	})
	if err != nil {
		t.Fatalf("NewWalletSubscriber: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	deliveries, errs, err := sub.Subscribe(ctx)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	publish(t, js, "msg-1", []byte(`{"hello":"world"}`))

	select {
	case d := <-deliveries:
		if got := d.MsgID(); got != "msg-1" {
			t.Errorf("msgID = %q, want %q", got, "msg-1")
		}
		if string(d.Data()) != `{"hello":"world"}` {
			t.Errorf("data = %q, want %q", d.Data(), `{"hello":"world"}`)
		}
		if err := d.Ack(ctx); err != nil {
			t.Errorf("Ack: %v", err)
		}
	case e := <-errs:
		t.Fatalf("errs: %v", e)
	case <-time.After(3 * time.Second):
		t.Fatal("no delivery within 3s")
	}
}

func TestWalletSubscriber_NakTriggersRedelivery(t *testing.T) {
	url := runEmbedded(t)
	js := newJSContext(t, url)

	sub, err := natsadapter.NewWalletSubscriber(natsadapter.WalletSubscriberConfig{
		JS:         js,
		Stream:     walletTestStream,
		Durable:    "wallet-test-nak",
		AckWait:    500 * time.Millisecond,
		MaxDeliver: 4,
		// Zero out backoff so the redelivery is fast.
		BackOff: []time.Duration{50 * time.Millisecond, 50 * time.Millisecond},
	})
	if err != nil {
		t.Fatalf("NewWalletSubscriber: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	deliveries, _, err := sub.Subscribe(ctx)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	publish(t, js, "msg-nak", []byte(`payload`))

	first := waitForDelivery(t, deliveries, 3*time.Second)
	if err := first.Nak(ctx, 10*time.Millisecond); err != nil {
		t.Fatalf("Nak: %v", err)
	}
	second := waitForDelivery(t, deliveries, 3*time.Second)
	if got := second.MsgID(); got != "msg-nak" {
		t.Errorf("redelivered msgID = %q, want %q", got, "msg-nak")
	}
	if err := second.Ack(ctx); err != nil {
		t.Errorf("Ack: %v", err)
	}
}

func TestWalletSubscriber_DuplicateMsgID_DedupedByServer(t *testing.T) {
	url := runEmbedded(t)
	js := newJSContext(t, url)

	sub, err := natsadapter.NewWalletSubscriber(natsadapter.WalletSubscriberConfig{
		JS:         js,
		Stream:     walletTestStream,
		Durable:    "wallet-test-dup",
		AckWait:    500 * time.Millisecond,
		MaxDeliver: 3,
		BackOff:    []time.Duration{50 * time.Millisecond, 50 * time.Millisecond},
		BufferSize: 4,
	})
	if err != nil {
		t.Fatalf("NewWalletSubscriber: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	deliveries, _, err := sub.Subscribe(ctx)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	publish(t, js, "msg-dup", []byte(`one`))
	publish(t, js, "msg-dup", []byte(`one`))

	first := waitForDelivery(t, deliveries, 3*time.Second)
	if err := first.Ack(ctx); err != nil {
		t.Fatalf("Ack: %v", err)
	}

	select {
	case d := <-deliveries:
		t.Fatalf("got duplicate delivery msgID=%q (server-side dedup failed)", d.MsgID())
	case <-time.After(750 * time.Millisecond):
		// Expected: 1h Duplicates window dropped the second publish.
	}
}

func TestWalletSubscriber_CtxCancelClosesChannel(t *testing.T) {
	url := runEmbedded(t)
	js := newJSContext(t, url)

	sub, err := natsadapter.NewWalletSubscriber(natsadapter.WalletSubscriberConfig{
		JS:      js,
		Stream:  walletTestStream,
		Durable: "wallet-test-cancel",
	})
	if err != nil {
		t.Fatalf("NewWalletSubscriber: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	deliveries, _, err := sub.Subscribe(ctx)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	cancel()
	select {
	case _, ok := <-deliveries:
		if ok {
			t.Fatal("deliveries channel should close after ctx cancel")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("deliveries channel did not close within 2s")
	}
}

func TestNewWalletSubscriber_ValidationErrors(t *testing.T) {
	if _, err := natsadapter.NewWalletSubscriber(natsadapter.WalletSubscriberConfig{}); err == nil {
		t.Fatal("expected error on zero config (missing JS)")
	}

	url := runEmbedded(t)
	js := newJSContext(t, url)

	if _, err := natsadapter.NewWalletSubscriber(natsadapter.WalletSubscriberConfig{
		JS:         js,
		MaxDeliver: 2,
		BackOff:    []time.Duration{time.Second, time.Second, time.Second}, // len == 3 > MaxDeliver
	}); err == nil {
		t.Fatal("expected error when MaxDeliver <= len(BackOff)")
	}
}

func TestNewWalletSubscriber_DefaultsApplied(t *testing.T) {
	url := runEmbedded(t)
	js := newJSContext(t, url)
	s, err := natsadapter.NewWalletSubscriber(natsadapter.WalletSubscriberConfig{JS: js})
	if err != nil {
		t.Fatalf("NewWalletSubscriber: %v", err)
	}
	if s == nil {
		t.Fatal("nil subscriber")
	}
}

func waitForDelivery(t *testing.T, ch <-chan walletworker.Delivery, timeout time.Duration) walletworker.Delivery {
	t.Helper()
	select {
	case d, ok := <-ch:
		if !ok {
			t.Fatal("deliveries channel closed")
		}
		return d
	case <-time.After(timeout):
		t.Fatalf("no delivery within %s", timeout)
	}
	return nil
}
