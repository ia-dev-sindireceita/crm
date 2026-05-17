package nats_test

// Embedded-NATS coverage for the SIN-62934 wallet.balance.depleted
// producer. The unit tests in wallet_depleted_publisher_test.go pin
// the wire shape against a fake target; this file pushes a real
// JetStream message through the broker so an adapter regression
// (header layout, dedup behaviour, subject typo) trips a clear failure
// against the real SDK.
//
// Pairs with the consumer-side integration test at
// internal/worker/wallet_alerter/integration_test.go which exercises
// the same broker → worker → mock-Slack pipeline; together they prove
// the producer → consumer contract holds end-to-end.

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	natsgo "github.com/nats-io/nats.go"

	natsadapter "github.com/pericles-luz/crm/internal/adapter/messaging/nats"
	"github.com/pericles-luz/crm/internal/wallet"
	"github.com/pericles-luz/crm/internal/worker/wallet_alerter"
)

func TestSDK_Embedded_PublishMsgID_RejectsEmptySubject(t *testing.T) {
	url := runEmbedded(t)
	a := connect(t, url)
	if err := a.PublishMsgID(context.Background(), "", "id-1", []byte("x")); err == nil {
		t.Error("expected error on empty subject")
	}
}

func TestSDK_Embedded_PublishMsgID_NoMsgID_StillPublishes(t *testing.T) {
	// Empty msgID is the fallback path — no header, broker assigns a
	// fresh sequence. Used by callers that want at-least-once without
	// content-based dedup.
	url := runEmbedded(t)
	a := connect(t, url)
	if err := a.EnsureStream("WMID1", []string{"wmid1.>"}); err != nil {
		t.Fatalf("EnsureStream: %v", err)
	}
	if err := a.PublishMsgID(context.Background(), "wmid1.s", "", []byte("hello")); err != nil {
		t.Fatalf("PublishMsgID: %v", err)
	}
}

func TestWalletDepletedPublisher_EmbeddedNATS_RoundTrip(t *testing.T) {
	url := runEmbedded(t)
	a := connect(t, url)
	if err := a.EnsureStream(wallet_alerter.StreamName, []string{wallet_alerter.Subject}); err != nil {
		t.Fatalf("EnsureStream: %v", err)
	}

	// Subscribe a thin observer that captures the raw delivery so we can
	// inspect the Nats-Msg-Id header AND the decoded payload — both
	// matter for the at-most-once contract with the consumer.
	type captured struct {
		header natsgo.Header
		body   []byte
	}
	var (
		mu  sync.Mutex
		got []captured
	)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	nc, err := natsgo.Connect(url)
	if err != nil {
		t.Fatalf("nats.Connect: %v", err)
	}
	t.Cleanup(nc.Close)
	js, err := nc.JetStream()
	if err != nil {
		t.Fatalf("JetStream: %v", err)
	}
	sub, err := js.QueueSubscribe(wallet_alerter.Subject, "wd-test", func(m *natsgo.Msg) {
		hdr := natsgo.Header{}
		for k, v := range m.Header {
			hdr[k] = append([]string(nil), v...)
		}
		mu.Lock()
		got = append(got, captured{header: hdr, body: append([]byte(nil), m.Data...)})
		mu.Unlock()
		_ = m.Ack()
	}, natsgo.Durable("wd-test"), natsgo.ManualAck(), natsgo.DeliverAll())
	if err != nil {
		t.Fatalf("QueueSubscribe: %v", err)
	}
	t.Cleanup(func() { _ = sub.Unsubscribe() })

	pub, err := natsadapter.NewWalletDepletedPublisher(a)
	if err != nil {
		t.Fatalf("NewWalletDepletedPublisher: %v", err)
	}

	tid := uuid.MustParse("aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee")
	occurred := time.Date(2026, 5, 16, 19, 42, 0, 0, time.UTC)
	evt := wallet.BalanceDepletedEvent{
		TenantID:         tid,
		PolicyScope:      "tenant:default",
		LastChargeTokens: 123,
		OccurredAt:       occurred,
	}
	if err := pub.PublishBalanceDepleted(ctx, evt); err != nil {
		t.Fatalf("PublishBalanceDepleted: %v", err)
	}

	deadline := time.Now().Add(3 * time.Second)
	for {
		mu.Lock()
		n := len(got)
		mu.Unlock()
		if n >= 1 || time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1 {
		t.Fatalf("delivery count = %d, want 1", len(got))
	}
	if msgID := got[0].header.Get(natsgo.MsgIdHdr); msgID == "" {
		t.Errorf("Nats-Msg-Id header missing; producer must set it for JetStream dedup")
	}
	var wire wallet_alerter.Event
	if err := json.Unmarshal(got[0].body, &wire); err != nil {
		t.Fatalf("decode wire body: %v", err)
	}
	if wire.TenantID != tid.String() {
		t.Errorf("TenantID = %q, want %q", wire.TenantID, tid.String())
	}
	if wire.LastChargeTokens != 123 {
		t.Errorf("LastChargeTokens = %d, want 123", wire.LastChargeTokens)
	}
}

func TestWalletDepletedPublisher_EmbeddedNATS_DedupesDuplicates(t *testing.T) {
	// Two publishes with the same (tenant_id, occurred_at) MUST collapse
	// to one stored message because the adapter sets a deterministic
	// Nats-Msg-Id and EnsureStream pins a 1h Duplicates window. This
	// the producer-side half of the at-most-once contract.
	url := runEmbedded(t)
	a := connect(t, url)
	if err := a.EnsureStream(wallet_alerter.StreamName, []string{wallet_alerter.Subject}); err != nil {
		t.Fatalf("EnsureStream: %v", err)
	}

	pub, err := natsadapter.NewWalletDepletedPublisher(a)
	if err != nil {
		t.Fatalf("NewWalletDepletedPublisher: %v", err)
	}
	evt := wallet.BalanceDepletedEvent{
		TenantID:         uuid.New(),
		PolicyScope:      "tenant:default",
		LastChargeTokens: 1,
		OccurredAt:       time.Date(2026, 5, 16, 12, 0, 0, 0, time.UTC),
	}

	for i := 0; i < 3; i++ {
		if err := pub.PublishBalanceDepleted(context.Background(), evt); err != nil {
			t.Fatalf("PublishBalanceDepleted #%d: %v", i, err)
		}
	}

	nc, err := natsgo.Connect(url)
	if err != nil {
		t.Fatalf("nats.Connect: %v", err)
	}
	t.Cleanup(nc.Close)
	js, err := nc.JetStream()
	if err != nil {
		t.Fatalf("JetStream: %v", err)
	}
	info, err := js.StreamInfo(wallet_alerter.StreamName)
	if err != nil {
		t.Fatalf("StreamInfo: %v", err)
	}
	if info.State.Msgs != 1 {
		t.Errorf("stream Msgs = %d, want 1 (broker must dedup identical msg-id within Duplicates window)", info.State.Msgs)
	}
}
