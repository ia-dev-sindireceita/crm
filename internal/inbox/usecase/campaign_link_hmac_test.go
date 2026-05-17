package usecase_test

// SIN-62982 — inbox-side HMAC marker verification tests. The marker
// primitive is covered in internal/campaigns/marker_test.go and the
// handler-side emission in internal/web/public/campaign; this file
// pins the inbox-side `linkContactToCampaign` hook's verify behaviour.

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/campaigns"
	inboxusecase "github.com/pericles-luz/crm/internal/inbox/usecase"
)

// withCampaignMarkerKey builds a ReceiveInbound wired with a marker key
// + a recording linker so tests can assert which (or no) link call
// happened. allowLegacy mirrors the production wire flag.
func withCampaignMarkerKey(t *testing.T, key campaigns.MarkerKey, allowLegacy bool, linker *recordingLinker) (*inboxusecase.ReceiveInbound, uuid.UUID) {
	t.Helper()
	repo := newInMemoryRepo()
	dedup := newInMemoryDedup()
	contactsU := newStubContactUpserter()
	uc := inboxusecase.MustNewReceiveInbound(repo, dedup, contactsU)
	uc.SetCampaignLinkerLogger(slog.New(slog.NewTextHandler(io.Discard, nil)))
	uc.SetCampaignLinker(linker)
	uc.SetCampaignMarkerKey(key)
	uc.SetCampaignMarkerAllowLegacy(allowLegacy)
	return uc, uuid.New()
}

func TestReceiveInbound_AcceptsSignedMarkerWhenHMACValid(t *testing.T) {
	t.Parallel()
	key := campaigns.MarkerKey("0123456789abcdef0123456789abcdef")
	linker := &recordingLinker{}
	uc, tenant := withCampaignMarkerKey(t, key, false, linker)

	clickID := "c0a5e0b7-3d3e-4f4a-9aa8-44e2f8b9b1f3"
	token := campaigns.BuildClickToken(key, tenant, clickID)
	body := "Hi [crm:" + token + "]"
	if _, err := uc.Execute(context.Background(), inboundEventBody(tenant, body)); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	calls := linker.snapshot()
	if len(calls) != 1 {
		t.Fatalf("link calls = %d, want 1 (signed marker valid)", len(calls))
	}
	if calls[0].clickID != clickID {
		t.Errorf("clickID = %q, want %q", calls[0].clickID, clickID)
	}
}

func TestReceiveInbound_RejectsForgedHMAC(t *testing.T) {
	t.Parallel()
	key := campaigns.MarkerKey("0123456789abcdef0123456789abcdef")
	linker := &recordingLinker{}
	uc, tenant := withCampaignMarkerKey(t, key, false, linker)

	clickID := "c0a5e0b7-3d3e-4f4a-9aa8-44e2f8b9b1f3"
	// 8 lowercase hex chars but not the real digest.
	body := "Hi [crm:" + clickID + ".deadbeef]"
	if _, err := uc.Execute(context.Background(), inboundEventBody(tenant, body)); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if got := len(linker.snapshot()); got != 0 {
		t.Errorf("link calls = %d, want 0 (forged HMAC must NOT link)", got)
	}
}

func TestReceiveInbound_RejectsCrossTenantReplay(t *testing.T) {
	t.Parallel()
	key := campaigns.MarkerKey("0123456789abcdef0123456789abcdef")
	linker := &recordingLinker{}

	tenantA := uuid.New()
	tenantB := uuid.New()
	clickID := "c0a5e0b7-3d3e-4f4a-9aa8-44e2f8b9b1f3"
	tokenForA := campaigns.BuildClickToken(key, tenantA, clickID)

	// Build a receiver scoped to tenantB but replay the marker minted
	// for tenantA. The HMAC depends on tenant_id so verification must
	// fail and the link call must NOT fire.
	repo := newInMemoryRepo()
	dedup := newInMemoryDedup()
	contactsU := newStubContactUpserter()
	uc := inboxusecase.MustNewReceiveInbound(repo, dedup, contactsU)
	uc.SetCampaignLinkerLogger(slog.New(slog.NewTextHandler(io.Discard, nil)))
	uc.SetCampaignLinker(linker)
	uc.SetCampaignMarkerKey(key)
	uc.SetCampaignMarkerAllowLegacy(false)

	body := "Hi [crm:" + tokenForA + "]"
	if _, err := uc.Execute(context.Background(), inboundEventBody(tenantB, body)); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if got := len(linker.snapshot()); got != 0 {
		t.Errorf("link calls = %d, want 0 (tenantA marker replayed against tenantB must NOT link)", got)
	}
}

func TestReceiveInbound_RejectsUnsignedMarkerWhenLegacyDisabled(t *testing.T) {
	t.Parallel()
	key := campaigns.MarkerKey("0123456789abcdef0123456789abcdef")
	linker := &recordingLinker{}
	uc, tenant := withCampaignMarkerKey(t, key, false, linker)

	body := "Hi [crm:c0a5e0b7-3d3e-4f4a-9aa8-44e2f8b9b1f3]"
	if _, err := uc.Execute(context.Background(), inboundEventBody(tenant, body)); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if got := len(linker.snapshot()); got != 0 {
		t.Errorf("link calls = %d, want 0 (legacy marker rejected when allowLegacy=false)", got)
	}
}

func TestReceiveInbound_AcceptsUnsignedMarkerDuringCompatWindow(t *testing.T) {
	t.Parallel()
	key := campaigns.MarkerKey("0123456789abcdef0123456789abcdef")
	linker := &recordingLinker{}
	uc, tenant := withCampaignMarkerKey(t, key, true, linker)

	clickID := "c0a5e0b7-3d3e-4f4a-9aa8-44e2f8b9b1f3"
	body := "Hi [crm:" + clickID + "]"
	if _, err := uc.Execute(context.Background(), inboundEventBody(tenant, body)); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	calls := linker.snapshot()
	if len(calls) != 1 {
		t.Fatalf("link calls = %d, want 1 (legacy marker allowed in compat window)", len(calls))
	}
	if calls[0].clickID != clickID {
		t.Errorf("clickID = %q, want %q", calls[0].clickID, clickID)
	}
}

func TestReceiveInbound_RejectsSignedMarkerWhenWireHasNoKey(t *testing.T) {
	t.Parallel()
	linker := &recordingLinker{}
	// Wire has no key but the marker carries a suffix from a sibling
	// process that DOES have one. Fail closed.
	uc, tenant := withCampaignMarkerKey(t, nil, true, linker)

	body := "Hi [crm:c0a5e0b7-3d3e-4f4a-9aa8-44e2f8b9b1f3.deadbeef]"
	if _, err := uc.Execute(context.Background(), inboundEventBody(tenant, body)); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if got := len(linker.snapshot()); got != 0 {
		t.Errorf("link calls = %d, want 0 (suffixed marker with no key must fail closed)", got)
	}
}

func TestReceiveInbound_DefaultsAllowLegacyTrue(t *testing.T) {
	t.Parallel()
	// Constructor default for the compat window: allowLegacy=true.
	// Without SetCampaignMarkerAllowLegacy a legacy marker still links.
	repo := newInMemoryRepo()
	dedup := newInMemoryDedup()
	contactsU := newStubContactUpserter()
	uc := inboxusecase.MustNewReceiveInbound(repo, dedup, contactsU)
	uc.SetCampaignLinkerLogger(slog.New(slog.NewTextHandler(io.Discard, nil)))
	linker := &recordingLinker{}
	uc.SetCampaignLinker(linker)
	// no SetCampaignMarkerKey / SetCampaignMarkerAllowLegacy calls.

	tenant := uuid.New()
	if _, err := uc.Execute(context.Background(), inboundEventBody(tenant, "Hi [crm:c0a5e0b7-3d3e-4f4a-9aa8-44e2f8b9b1f3]")); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if got := len(linker.snapshot()); got != 1 {
		t.Errorf("link calls = %d, want 1 (default compat-window allowLegacy=true)", got)
	}
}
