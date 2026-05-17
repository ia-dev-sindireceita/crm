package nats_test

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	natsadapter "github.com/pericles-luz/crm/internal/adapter/messaging/nats"
	"github.com/pericles-luz/crm/internal/wallet"
	"github.com/pericles-luz/crm/internal/worker/wallet_alerter"
)

// fakePublishTarget records every PublishMsgID call so the unit tests
// can pin the wire shape without standing up an embedded JetStream
// server (the integration coverage at
// internal/worker/wallet_alerter/integration_test.go exercises the
// real broker end-to-end).
type fakePublishTarget struct {
	mu      sync.Mutex
	calls   []publishCall
	failNth int // 1-indexed; 0 disables the failure injection.
	err     error
}

type publishCall struct {
	subject string
	msgID   string
	body    []byte
}

func (f *fakePublishTarget) PublishMsgID(_ context.Context, subject, msgID string, body []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, publishCall{subject: subject, msgID: msgID, body: append([]byte(nil), body...)})
	if f.failNth > 0 && len(f.calls) == f.failNth {
		return f.err
	}
	return nil
}

func (f *fakePublishTarget) snapshot() []publishCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]publishCall, len(f.calls))
	copy(out, f.calls)
	return out
}

func TestNewWalletDepletedPublisher_RejectsNilTarget(t *testing.T) {
	t.Parallel()
	if _, err := natsadapter.NewWalletDepletedPublisher(nil); err == nil {
		t.Fatal("NewWalletDepletedPublisher(nil): want error, got nil")
	}
}

func TestWalletDepletedPublisher_PublishesWireEnvelope(t *testing.T) {
	t.Parallel()
	target := &fakePublishTarget{}
	pub, err := natsadapter.NewWalletDepletedPublisher(target)
	if err != nil {
		t.Fatalf("NewWalletDepletedPublisher: %v", err)
	}

	tid := uuid.MustParse("11111111-2222-3333-4444-555555555555")
	occurred := time.Date(2026, 5, 16, 19, 42, 0, 0, time.UTC)
	evt := wallet.BalanceDepletedEvent{
		TenantID:         tid,
		PolicyScope:      "tenant:default",
		LastChargeTokens: 7777,
		OccurredAt:       occurred,
	}
	if err := pub.PublishBalanceDepleted(context.Background(), evt); err != nil {
		t.Fatalf("PublishBalanceDepleted: %v", err)
	}

	got := target.snapshot()
	if len(got) != 1 {
		t.Fatalf("PublishMsgID call count = %d, want 1", len(got))
	}
	if got[0].subject != wallet_alerter.Subject {
		t.Errorf("subject = %q, want %q (consumer pins the unsuffixed subject)", got[0].subject, wallet_alerter.Subject)
	}
	wantMsgID := tid.String() + ":" + strconv.FormatInt(occurred.UTC().UnixNano(), 10)
	if got[0].msgID != wantMsgID {
		t.Errorf("msgID = %q, want %q", got[0].msgID, wantMsgID)
	}

	// Decode through wallet_alerter.Event — the consumer is the source
	// of truth for the wire format.
	var wire wallet_alerter.Event
	if err := json.Unmarshal(got[0].body, &wire); err != nil {
		t.Fatalf("wire body must decode via wallet_alerter.Event: %v\nbody=%s", err, got[0].body)
	}
	if wire.TenantID != tid.String() {
		t.Errorf("wire TenantID = %q, want %q", wire.TenantID, tid.String())
	}
	if wire.PolicyScope != "tenant:default" {
		t.Errorf("wire PolicyScope = %q, want tenant:default", wire.PolicyScope)
	}
	if wire.LastChargeTokens != 7777 {
		t.Errorf("wire LastChargeTokens = %d, want 7777", wire.LastChargeTokens)
	}
	if !wire.OccurredAt.Equal(occurred) {
		t.Errorf("wire OccurredAt = %s, want %s", wire.OccurredAt, occurred)
	}
}

func TestWalletDepletedPublisher_NormalisesOccurredAtToUTC(t *testing.T) {
	t.Parallel()
	target := &fakePublishTarget{}
	pub, _ := natsadapter.NewWalletDepletedPublisher(target)

	// Caller passes a non-UTC OccurredAt; the adapter MUST normalise to
	// UTC on the wire so the consumer's RFC3339-formatted golden stays
	// stable across deploy timezones.
	loc, err := time.LoadLocation("America/Sao_Paulo")
	if err != nil {
		t.Skipf("LoadLocation America/Sao_Paulo: %v", err)
	}
	local := time.Date(2026, 5, 16, 16, 42, 0, 0, loc) // == 19:42 UTC
	evt := wallet.BalanceDepletedEvent{
		TenantID:    uuid.New(),
		PolicyScope: "tenant:default",
		OccurredAt:  local,
	}
	if err := pub.PublishBalanceDepleted(context.Background(), evt); err != nil {
		t.Fatalf("PublishBalanceDepleted: %v", err)
	}

	var wire wallet_alerter.Event
	if err := json.Unmarshal(target.snapshot()[0].body, &wire); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !wire.OccurredAt.Equal(local) {
		t.Errorf("wire OccurredAt = %s, want %s (same instant)", wire.OccurredAt, local)
	}
	if wire.OccurredAt.Location() != time.UTC {
		t.Errorf("wire OccurredAt loc = %s, want UTC", wire.OccurredAt.Location())
	}
}

func TestWalletDepletedPublisher_TargetErrorWraps(t *testing.T) {
	t.Parallel()
	boom := errors.New("nats: connection refused")
	target := &fakePublishTarget{failNth: 1, err: boom}
	pub, _ := natsadapter.NewWalletDepletedPublisher(target)

	err := pub.PublishBalanceDepleted(context.Background(), wallet.BalanceDepletedEvent{
		TenantID:    uuid.New(),
		PolicyScope: "tenant:default",
		OccurredAt:  time.Unix(1_700_000_000, 0).UTC(),
	})
	if err == nil {
		t.Fatal("PublishBalanceDepleted: want error from target, got nil")
	}
	if !errors.Is(err, boom) {
		t.Errorf("error chain missing target error: %v", err)
	}
}

func TestWalletDepletedPublisher_SatisfiesPort(t *testing.T) {
	t.Parallel()
	target := &fakePublishTarget{}
	pub, err := natsadapter.NewWalletDepletedPublisher(target)
	if err != nil {
		t.Fatalf("NewWalletDepletedPublisher: %v", err)
	}
	var _ wallet.BalanceDepletedPublisher = pub
}
