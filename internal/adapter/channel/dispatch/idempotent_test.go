package dispatch

import (
	"context"
	"errors"
	"runtime"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/inbox"
)

func TestIdempotent_FirstSend_DelegatesAndRecords(t *testing.T) {
	t.Parallel()
	inner := newRecordingChannel("wamid.1")
	d := NewIdempotent(inner, NewMemoryLedger())
	m := newOutboundMessage(uuid.New(), "whatsapp", "key-1")

	got, err := d.SendMessage(context.Background(), m)
	if err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	if got != "wamid.1" {
		t.Errorf("wamid = %q, want wamid.1", got)
	}
	if inner.callCount() != 1 {
		t.Errorf("inner calls = %d, want 1", inner.callCount())
	}
}

func TestIdempotent_DoubleSubmit_SingleCarrierCall(t *testing.T) {
	t.Parallel()
	inner := newRecordingChannel("wamid.dedup")
	d := NewIdempotent(inner, NewMemoryLedger())
	tenant := uuid.New()
	// Same operator, same compose token, submitted twice.
	m1 := newOutboundMessage(tenant, "whatsapp", "compose-abc")
	m2 := newOutboundMessage(tenant, "whatsapp", "compose-abc")

	w1, err := d.SendMessage(context.Background(), m1)
	if err != nil {
		t.Fatalf("first send: %v", err)
	}
	w2, err := d.SendMessage(context.Background(), m2)
	if err != nil {
		t.Fatalf("second send: %v", err)
	}
	if w1 != "wamid.dedup" || w2 != "wamid.dedup" {
		t.Errorf("wamids = (%q,%q), want both wamid.dedup", w1, w2)
	}
	if inner.callCount() != 1 {
		t.Fatalf("carrier calls = %d, want 1 (double-submit must dedup)", inner.callCount())
	}
}

func TestIdempotent_DistinctKeys_BothSend(t *testing.T) {
	t.Parallel()
	inner := newRecordingChannel("wamid.x")
	d := NewIdempotent(inner, NewMemoryLedger())
	tenant := uuid.New()
	if _, err := d.SendMessage(context.Background(), newOutboundMessage(tenant, "whatsapp", "k1")); err != nil {
		t.Fatal(err)
	}
	if _, err := d.SendMessage(context.Background(), newOutboundMessage(tenant, "whatsapp", "k2")); err != nil {
		t.Fatal(err)
	}
	if inner.callCount() != 2 {
		t.Errorf("carrier calls = %d, want 2 (distinct keys are distinct sends)", inner.callCount())
	}
}

func TestIdempotent_SameKeyDifferentTenant_BothSend(t *testing.T) {
	t.Parallel()
	inner := newRecordingChannel("wamid.x")
	d := NewIdempotent(inner, NewMemoryLedger())
	if _, err := d.SendMessage(context.Background(), newOutboundMessage(uuid.New(), "whatsapp", "same")); err != nil {
		t.Fatal(err)
	}
	if _, err := d.SendMessage(context.Background(), newOutboundMessage(uuid.New(), "whatsapp", "same")); err != nil {
		t.Fatal(err)
	}
	if inner.callCount() != 2 {
		t.Errorf("carrier calls = %d, want 2 (key is tenant-scoped)", inner.callCount())
	}
}

func TestIdempotent_EmptyKey_Passthrough(t *testing.T) {
	t.Parallel()
	inner := newRecordingChannel("wamid.x")
	d := NewIdempotent(inner, NewMemoryLedger())
	tenant := uuid.New()
	// No idempotency key → every send hits the carrier (pre-dispatcher behaviour).
	for i := 0; i < 3; i++ {
		if _, err := d.SendMessage(context.Background(), newOutboundMessage(tenant, "whatsapp", "")); err != nil {
			t.Fatal(err)
		}
	}
	if inner.callCount() != 3 {
		t.Errorf("carrier calls = %d, want 3 (empty key disables dedup)", inner.callCount())
	}
}

func TestIdempotent_NilLedger_ReturnsInner(t *testing.T) {
	t.Parallel()
	inner := newRecordingChannel("wamid.x")
	d := NewIdempotent(inner, nil)
	if _, err := d.SendMessage(context.Background(), newOutboundMessage(uuid.New(), "whatsapp", "k")); err != nil {
		t.Fatal(err)
	}
	if inner.callCount() != 1 {
		t.Errorf("inner calls = %d, want 1", inner.callCount())
	}
}

func TestIdempotent_FailedSend_ReleasesForRetry(t *testing.T) {
	t.Parallel()
	inner := newRecordingChannel("wamid.ok")
	inner.err = inbox.ErrChannelTransient // first attempt fails
	d := NewIdempotent(inner, NewMemoryLedger())
	tenant := uuid.New()
	m := newOutboundMessage(tenant, "whatsapp", "retryable")

	if _, err := d.SendMessage(context.Background(), m); !errors.Is(err, inbox.ErrChannelTransient) {
		t.Fatalf("first send err = %v, want transient", err)
	}
	// Failure released the claim: a retry with the same key must reach the
	// carrier again (not be swallowed as a duplicate).
	inner.mu.Lock()
	inner.err = nil // carrier now healthy
	inner.mu.Unlock()
	got, err := d.SendMessage(context.Background(), m)
	if err != nil {
		t.Fatalf("retry err = %v, want nil", err)
	}
	if got != "wamid.ok" {
		t.Errorf("retry wamid = %q, want wamid.ok", got)
	}
	if inner.callCount() != 2 {
		t.Errorf("carrier calls = %d, want 2 (failed send must be retryable)", inner.callCount())
	}
}

func TestIdempotent_ConcurrentInFlight_SecondIsTransient(t *testing.T) {
	t.Parallel()
	inner := newRecordingChannel("wamid.race")
	inner.block = make(chan struct{}) // hold the first send in-flight
	d := NewIdempotent(inner, NewMemoryLedger())
	tenant := uuid.New()
	m := newOutboundMessage(tenant, "whatsapp", "race-key")

	var wg sync.WaitGroup
	wg.Add(1)
	started := make(chan struct{})
	var firstErr error
	var firstWAMID string
	go func() {
		defer wg.Done()
		close(started)
		firstWAMID, firstErr = d.SendMessage(context.Background(), m)
	}()

	<-started
	// Spin until the first send has entered the carrier (claim taken).
	for inner.callCount() == 0 {
		runtime.Gosched()
	}
	// Second submit while the first is still in-flight → transient, no send.
	_, err := d.SendMessage(context.Background(), m)
	if !errors.Is(err, inbox.ErrChannelTransient) {
		t.Errorf("in-flight duplicate err = %v, want transient", err)
	}
	if inner.callCount() != 1 {
		t.Errorf("carrier calls = %d, want 1 (duplicate must not send while in-flight)", inner.callCount())
	}

	close(inner.block) // release the first send
	wg.Wait()
	if firstErr != nil || firstWAMID != "wamid.race" {
		t.Errorf("first send = (%q,%v), want (wamid.race,nil)", firstWAMID, firstErr)
	}
}

func TestMemoryLedger_ClaimCompleteRelease(t *testing.T) {
	t.Parallel()
	l := NewMemoryLedger()
	ctx := context.Background()
	tenant := uuid.New()

	prior, claimed, err := l.Claim(ctx, tenant, "k")
	if err != nil || !claimed {
		t.Fatalf("first claim = (%+v,%v,%v), want claimed", prior, claimed, err)
	}
	// Second claim before completion: not claimed, in-flight (Done=false).
	prior, claimed, err = l.Claim(ctx, tenant, "k")
	if err != nil || claimed || prior.Done {
		t.Fatalf("second claim = (%+v,%v,%v), want not-claimed in-flight", prior, claimed, err)
	}
	if err := l.Complete(ctx, tenant, "k", "wamid.7"); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	prior, claimed, err = l.Claim(ctx, tenant, "k")
	if err != nil || claimed || !prior.Done || prior.ChannelExternalID != "wamid.7" {
		t.Fatalf("post-complete claim = (%+v,%v,%v), want done wamid.7", prior, claimed, err)
	}
	// Release clears the entry so the key can be claimed fresh.
	if err := l.Release(ctx, tenant, "k"); err != nil {
		t.Fatalf("Release: %v", err)
	}
	_, claimed, err = l.Claim(ctx, tenant, "k")
	if err != nil || !claimed {
		t.Fatalf("post-release claim = (%v,%v), want fresh claim", claimed, err)
	}
}
