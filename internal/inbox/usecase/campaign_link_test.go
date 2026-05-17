package usecase_test

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/campaigns"
	"github.com/pericles-luz/crm/internal/inbox"
	inboxusecase "github.com/pericles-luz/crm/internal/inbox/usecase"
)

func TestExtractClickID_HappyPath(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		body string
		want string
	}{
		{name: "empty", body: "", want: ""},
		{name: "no marker", body: "Olá, vim falar com vocês.", want: ""},
		{name: "uuid marker", body: "Olá [crm:c0a5e0b7-3d3e-4f4a-9aa8-44e2f8b9b1f3] vim do site", want: "c0a5e0b7-3d3e-4f4a-9aa8-44e2f8b9b1f3"},
		{name: "short token", body: "x [crm:abc12345] y", want: "abc12345"},
		{name: "too short ignored", body: "[crm:abc] x", want: ""},
		{name: "no closing bracket", body: "[crm:c0a5e0b7-3d3e-4f4a-9aa8-44e2f8b9b1f3", want: ""},
		{name: "whitespace inside ignored", body: "[crm:abc 123]", want: ""},
		{name: "first match wins", body: "[crm:firstoken12] then [crm:secondtoken12]", want: "firstoken12"},
		{name: "case preserved", body: "[crm:ABC12345xyz]", want: "ABC12345xyz"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := inboxusecase.ExtractClickID(tc.body); got != tc.want {
				t.Fatalf("ExtractClickID(%q) = %q, want %q", tc.body, got, tc.want)
			}
		})
	}
}

// recordingLinker spies on LinkContactToCampaign calls. Lives in this
// test file because it is shared by the unit assertions below; not
// exposed from the production package.
type recordingLinker struct {
	mu    sync.Mutex
	calls []linkCall
	err   error
}

type linkCall struct {
	tenant    uuid.UUID
	clickID   string
	contactID uuid.UUID
}

func (r *recordingLinker) LinkContactToCampaign(_ context.Context, tenantID uuid.UUID, clickID string, contactID uuid.UUID) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, linkCall{tenant: tenantID, clickID: clickID, contactID: contactID})
	return r.err
}

func (r *recordingLinker) snapshot() []linkCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]linkCall, len(r.calls))
	copy(out, r.calls)
	return out
}

func newReceiveInboundForCampaignTest(t *testing.T) (*inboxusecase.ReceiveInbound, uuid.UUID) {
	t.Helper()
	repo := newInMemoryRepo()
	dedup := newInMemoryDedup()
	contactsU := newStubContactUpserter()
	uc := inboxusecase.MustNewReceiveInbound(repo, dedup, contactsU)
	uc.SetCampaignLinkerLogger(slog.New(slog.NewTextHandler(io.Discard, nil)))
	return uc, uuid.New()
}

func inboundEventBody(tenant uuid.UUID, body string) inbox.InboundEvent {
	return inbox.InboundEvent{
		TenantID:          tenant,
		Channel:           "whatsapp",
		ChannelExternalID: "wamid." + uuid.NewString(),
		SenderExternalID:  "+5511999990001",
		SenderDisplayName: "Alice",
		Body:              body,
	}
}

func TestReceiveInbound_LinksContactWhenBodyHasMarker(t *testing.T) {
	t.Parallel()
	uc, tenant := newReceiveInboundForCampaignTest(t)
	linker := &recordingLinker{}
	uc.SetCampaignLinker(linker)

	res, err := uc.Execute(context.Background(), inboundEventBody(tenant, "Hello [crm:c0a5e0b7-3d3e-4f4a-9aa8-44e2f8b9b1f3]"))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Duplicate {
		t.Fatal("Duplicate = true, want false")
	}
	calls := linker.snapshot()
	if len(calls) != 1 {
		t.Fatalf("link calls = %d, want 1", len(calls))
	}
	if calls[0].tenant != tenant {
		t.Errorf("tenant = %s, want %s", calls[0].tenant, tenant)
	}
	if calls[0].clickID != "c0a5e0b7-3d3e-4f4a-9aa8-44e2f8b9b1f3" {
		t.Errorf("clickID = %q, want %q", calls[0].clickID, "c0a5e0b7-3d3e-4f4a-9aa8-44e2f8b9b1f3")
	}
	if calls[0].contactID != res.Contact.ID {
		t.Errorf("contactID = %s, want %s", calls[0].contactID, res.Contact.ID)
	}
}

func TestReceiveInbound_NoLinkerNoCall(t *testing.T) {
	t.Parallel()
	uc, tenant := newReceiveInboundForCampaignTest(t)
	// Intentionally do NOT wire a linker — the hook must be a silent no-op.
	if _, err := uc.Execute(context.Background(), inboundEventBody(tenant, "Hello [crm:abc12345xyz]")); err != nil {
		t.Fatalf("Execute: %v", err)
	}
}

func TestReceiveInbound_NoMarkerNoCall(t *testing.T) {
	t.Parallel()
	uc, tenant := newReceiveInboundForCampaignTest(t)
	linker := &recordingLinker{}
	uc.SetCampaignLinker(linker)

	if _, err := uc.Execute(context.Background(), inboundEventBody(tenant, "Just a normal greeting")); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if got := len(linker.snapshot()); got != 0 {
		t.Errorf("link calls = %d, want 0 (no marker)", got)
	}
}

func TestReceiveInbound_LinkerNotFoundSoftFails(t *testing.T) {
	t.Parallel()
	uc, tenant := newReceiveInboundForCampaignTest(t)
	linker := &recordingLinker{err: campaigns.ErrNotFound}
	uc.SetCampaignLinker(linker)

	if _, err := uc.Execute(context.Background(), inboundEventBody(tenant, "[crm:c0a5e0b7-3d3e-4f4a-9aa8-44e2f8b9b1f3]")); err != nil {
		t.Fatalf("Execute (linker ErrNotFound must not fail): %v", err)
	}
}

func TestReceiveInbound_LinkerOpaqueErrSoftFails(t *testing.T) {
	t.Parallel()
	uc, tenant := newReceiveInboundForCampaignTest(t)
	linker := &recordingLinker{err: errBoomLinker}
	uc.SetCampaignLinker(linker)

	if _, err := uc.Execute(context.Background(), inboundEventBody(tenant, "[crm:c0a5e0b7-3d3e-4f4a-9aa8-44e2f8b9b1f3]")); err != nil {
		t.Fatalf("Execute (linker generic err must not fail): %v", err)
	}
}

func TestSetCampaignLinkerNilIsNoop(t *testing.T) {
	t.Parallel()
	uc, tenant := newReceiveInboundForCampaignTest(t)
	uc.SetCampaignLinker(nil)
	if _, err := uc.Execute(context.Background(), inboundEventBody(tenant, "[crm:c0a5e0b7-3d3e-4f4a-9aa8-44e2f8b9b1f3]")); err != nil {
		t.Fatalf("Execute with nil linker: %v", err)
	}
}

func TestSetCampaignLinkerLoggerNilFallsBackToDefault(t *testing.T) {
	t.Parallel()
	repo := newInMemoryRepo()
	dedup := newInMemoryDedup()
	contactsU := newStubContactUpserter()
	uc := inboxusecase.MustNewReceiveInbound(repo, dedup, contactsU)
	// No SetCampaignLinkerLogger call — campaignLogger field stays nil
	// so the hook falls back to slog.Default().
	linker := &recordingLinker{err: errBoomLinker}
	uc.SetCampaignLinker(linker)
	if _, err := uc.Execute(context.Background(), inboundEventBody(uuid.New(), "[crm:c0a5e0b7-3d3e-4f4a-9aa8-44e2f8b9b1f3]")); err != nil {
		t.Fatalf("Execute: %v", err)
	}
}

var errBoomLinker = stringError("boom")

type stringError string

func (s stringError) Error() string { return string(s) }
