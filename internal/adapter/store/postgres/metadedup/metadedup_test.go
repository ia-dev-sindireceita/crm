package metadedup_test

import (
	"context"
	"errors"
	"testing"

	"github.com/pericles-luz/crm/internal/adapter/channels/metashared"
	"github.com/pericles-luz/crm/internal/adapter/store/postgres/metadedup"
	"github.com/pericles-luz/crm/internal/inbox"
)

// fakeInboxStore exposes the same Claim/MarkProcessed surface as the
// real *pginbox.Store. It returns whichever error the test wires —
// crucially the inbox.ErrInboundAlreadyProcessed sentinel — so the
// bridge's error-remap logic is exercised without standing up
// Postgres. The end-to-end behaviour against a real cluster is
// covered separately in internal/adapter/db/postgres/metadedup_bridge_test.go.
type fakeInboxStore struct {
	claimErr     error
	markErr      error
	claimCalls   int
	markCalls    int
	lastChannel  string
	lastExternal string
}

func (f *fakeInboxStore) Claim(_ context.Context, channel, externalID string) error {
	f.claimCalls++
	f.lastChannel = channel
	f.lastExternal = externalID
	return f.claimErr
}

func (f *fakeInboxStore) MarkProcessed(_ context.Context, channel, externalID string) error {
	f.markCalls++
	f.lastChannel = channel
	f.lastExternal = externalID
	return f.markErr
}

func TestNew_RejectsNilInner(t *testing.T) {
	t.Parallel()
	if _, err := metadedup.New(nil); err == nil {
		t.Fatal("metadedup.New(nil) err = nil, want non-nil")
	}
}

func TestClaim_RemapsDuplicateSentinel(t *testing.T) {
	t.Parallel()
	inner := &fakeInboxStore{claimErr: inbox.ErrInboundAlreadyProcessed}
	bridge, err := metadedup.New(inner)
	if err != nil {
		t.Fatalf("metadedup.New: %v", err)
	}
	err = bridge.Claim(context.Background(), "whatsapp", "wamid.A")
	if !errors.Is(err, metashared.ErrAlreadyProcessed) {
		t.Fatalf("Claim err = %v, want metashared.ErrAlreadyProcessed", err)
	}
	if errors.Is(err, inbox.ErrInboundAlreadyProcessed) {
		t.Errorf("inner sentinel leaked through bridge")
	}
	if inner.claimCalls != 1 {
		t.Errorf("inner.Claim called %d times, want 1", inner.claimCalls)
	}
}

func TestClaim_PassesThroughOtherErrors(t *testing.T) {
	t.Parallel()
	infra := errors.New("connection refused")
	inner := &fakeInboxStore{claimErr: infra}
	bridge, _ := metadedup.New(inner)
	err := bridge.Claim(context.Background(), "whatsapp", "wamid.B")
	if !errors.Is(err, infra) {
		t.Fatalf("err = %v, want infra error passthrough", err)
	}
}

func TestClaim_SuccessReturnsNil(t *testing.T) {
	t.Parallel()
	inner := &fakeInboxStore{}
	bridge, _ := metadedup.New(inner)
	if err := bridge.Claim(context.Background(), "whatsapp", "wamid.C"); err != nil {
		t.Fatalf("Claim err = %v, want nil", err)
	}
	if inner.lastChannel != "whatsapp" || inner.lastExternal != "wamid.C" {
		t.Errorf("forwarded args = (%q,%q), want (whatsapp, wamid.C)", inner.lastChannel, inner.lastExternal)
	}
}

func TestMarkProcessed_PassesThroughInnerError(t *testing.T) {
	t.Parallel()
	inner := &fakeInboxStore{markErr: inbox.ErrNotFound}
	bridge, _ := metadedup.New(inner)
	err := bridge.MarkProcessed(context.Background(), "whatsapp", "wamid.D")
	if !errors.Is(err, inbox.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound passthrough", err)
	}
}

func TestMarkProcessed_ForwardsArgs(t *testing.T) {
	t.Parallel()
	inner := &fakeInboxStore{}
	bridge, _ := metadedup.New(inner)
	if err := bridge.MarkProcessed(context.Background(), "instagram", "mid.E"); err != nil {
		t.Fatalf("MarkProcessed: %v", err)
	}
	if inner.markCalls != 1 || inner.lastChannel != "instagram" || inner.lastExternal != "mid.E" {
		t.Errorf("forwarded args = (%q,%q) calls=%d", inner.lastChannel, inner.lastExternal, inner.markCalls)
	}
}
