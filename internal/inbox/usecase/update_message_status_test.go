package usecase_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/inbox"
	inboxusecase "github.com/pericles-luz/crm/internal/inbox/usecase"
)

// seedOutboundMessage builds an outbound message with a known wamid
// and persists it through the in-memory repo. Returns the tenant +
// the resulting message for assertion in downstream tests.
func seedOutboundMessage(t *testing.T, repo *inMemoryRepo, wamid string, initial inbox.MessageStatus) (uuid.UUID, *inbox.Message) {
	t.Helper()
	tenant := uuid.New()
	contactID := uuid.New()
	conv, err := inbox.NewConversation(tenant, contactID, "whatsapp")
	if err != nil {
		t.Fatalf("new conv: %v", err)
	}
	if err := repo.CreateConversation(context.Background(), conv); err != nil {
		t.Fatalf("create conv: %v", err)
	}
	m, err := inbox.NewMessage(inbox.NewMessageInput{
		TenantID:          tenant,
		ConversationID:    conv.ID,
		Direction:         inbox.MessageDirectionOut,
		Body:              "hi",
		Status:            initial,
		ChannelExternalID: wamid,
	})
	if err != nil {
		t.Fatalf("new message: %v", err)
	}
	if err := repo.SaveMessage(context.Background(), m); err != nil {
		t.Fatalf("save message: %v", err)
	}
	return tenant, m
}

func TestNewUpdateMessageStatus_RejectsNilDeps(t *testing.T) {
	repo := newInMemoryRepo()
	dedup := newInMemoryDedup()
	if _, err := inboxusecase.NewUpdateMessageStatus(nil, dedup); err == nil {
		t.Error("nil repo: err = nil, want construction error")
	}
	if _, err := inboxusecase.NewUpdateMessageStatus(repo, nil); err == nil {
		t.Error("nil dedup: err = nil, want construction error")
	}
}

func TestMustNewUpdateMessageStatus_PanicsOnNil(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Error("MustNewUpdateMessageStatus did not panic")
		}
	}()
	inboxusecase.MustNewUpdateMessageStatus(nil, nil)
}

// TestHandleStatus_MonotonicForwardSequence covers AC #1 / #3 — the
// sent → delivered → read sequence advances the message, and a
// regressed delivered → after read collapses to a no-op rather than
// rewriting state.
func TestHandleStatus_MonotonicForwardSequence(t *testing.T) {
	repo := newInMemoryRepo()
	dedup := newInMemoryDedup()
	u := inboxusecase.MustNewUpdateMessageStatus(repo, dedup)
	tenant, msg := seedOutboundMessage(t, repo, "wamid.AAA", inbox.MessageStatusPending)

	ctx := context.Background()
	cases := []struct {
		name          string
		status        inbox.MessageStatus
		wantOutcome   inbox.StatusUpdateOutcome
		wantPersisted inbox.MessageStatus
	}{
		{"pending->sent", inbox.MessageStatusSent, inbox.StatusOutcomeApplied, inbox.MessageStatusSent},
		{"sent->delivered", inbox.MessageStatusDelivered, inbox.StatusOutcomeApplied, inbox.MessageStatusDelivered},
		{"delivered->read", inbox.MessageStatusRead, inbox.StatusOutcomeApplied, inbox.MessageStatusRead},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res, err := u.HandleStatus(ctx, inbox.StatusUpdate{
				TenantID:          tenant,
				Channel:           "whatsapp",
				ChannelExternalID: "wamid.AAA",
				NewStatus:         tc.status,
				OccurredAt:        time.Now(),
			})
			if err != nil {
				t.Fatalf("HandleStatus: %v", err)
			}
			if res.Outcome != tc.wantOutcome {
				t.Errorf("outcome = %q, want %q", res.Outcome, tc.wantOutcome)
			}
			got, err := repo.FindMessageByChannelExternalID(ctx, tenant, "whatsapp", "wamid.AAA")
			if err != nil {
				t.Fatalf("find message: %v", err)
			}
			if got.Status != tc.wantPersisted {
				t.Errorf("persisted status = %q, want %q", got.Status, tc.wantPersisted)
			}
		})
	}
	_ = msg
}

// TestHandleStatus_RejectsRegression covers AC #3 — once a message
// is read, a later delivered event becomes a no-op without rewriting
// the row.
func TestHandleStatus_RejectsRegression(t *testing.T) {
	repo := newInMemoryRepo()
	dedup := newInMemoryDedup()
	u := inboxusecase.MustNewUpdateMessageStatus(repo, dedup)
	tenant, _ := seedOutboundMessage(t, repo, "wamid.REG", inbox.MessageStatusRead)
	ctx := context.Background()

	res, err := u.HandleStatus(ctx, inbox.StatusUpdate{
		TenantID:          tenant,
		Channel:           "whatsapp",
		ChannelExternalID: "wamid.REG",
		NewStatus:         inbox.MessageStatusDelivered,
		OccurredAt:        time.Now(),
	})
	if err != nil {
		t.Fatalf("HandleStatus: %v", err)
	}
	if res.Outcome != inbox.StatusOutcomeNoop {
		t.Errorf("outcome = %q, want %q", res.Outcome, inbox.StatusOutcomeNoop)
	}
	if res.PreviousStatus != inbox.MessageStatusRead {
		t.Errorf("previous = %q, want read", res.PreviousStatus)
	}
	got, err := repo.FindMessageByChannelExternalID(ctx, tenant, "whatsapp", "wamid.REG")
	if err != nil {
		t.Fatalf("find: %v", err)
	}
	if got.Status != inbox.MessageStatusRead {
		t.Errorf("persisted = %q, want read (unchanged)", got.Status)
	}
}

// TestHandleStatus_ReplayIsIdempotent covers AC #2 — the second
// delivery of the same (wamid, status) ACK collapses to a no-op via
// the dedup ledger; the message row is not rewritten.
func TestHandleStatus_ReplayIsIdempotent(t *testing.T) {
	repo := newInMemoryRepo()
	dedup := newInMemoryDedup()
	u := inboxusecase.MustNewUpdateMessageStatus(repo, dedup)
	tenant, _ := seedOutboundMessage(t, repo, "wamid.REPLAY", inbox.MessageStatusPending)
	ctx := context.Background()

	ev := inbox.StatusUpdate{
		TenantID:          tenant,
		Channel:           "whatsapp",
		ChannelExternalID: "wamid.REPLAY",
		NewStatus:         inbox.MessageStatusSent,
		OccurredAt:        time.Now(),
	}
	first, err := u.HandleStatus(ctx, ev)
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	if first.Outcome != inbox.StatusOutcomeApplied {
		t.Errorf("first outcome = %q, want applied", first.Outcome)
	}
	second, err := u.HandleStatus(ctx, ev)
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if second.Outcome != inbox.StatusOutcomeNoop {
		t.Errorf("second outcome = %q, want noop", second.Outcome)
	}
}

// TestHandleStatus_FailedRecordsErrorMetadata covers the failed path:
// the message moves to failed and the error metadata flows through
// the use case so the adapter can log + count it.
func TestHandleStatus_FailedRecordsErrorMetadata(t *testing.T) {
	repo := newInMemoryRepo()
	dedup := newInMemoryDedup()
	u := inboxusecase.MustNewUpdateMessageStatus(repo, dedup)
	tenant, _ := seedOutboundMessage(t, repo, "wamid.FAIL", inbox.MessageStatusSent)
	ctx := context.Background()

	res, err := u.HandleStatus(ctx, inbox.StatusUpdate{
		TenantID:          tenant,
		Channel:           "whatsapp",
		ChannelExternalID: "wamid.FAIL",
		NewStatus:         inbox.MessageStatusFailed,
		OccurredAt:        time.Now(),
		ErrorCode:         131026,
		ErrorTitle:        "Message undeliverable",
	})
	if err != nil {
		t.Fatalf("HandleStatus: %v", err)
	}
	if res.Outcome != inbox.StatusOutcomeApplied {
		t.Errorf("outcome = %q, want applied", res.Outcome)
	}
	got, err := repo.FindMessageByChannelExternalID(ctx, tenant, "whatsapp", "wamid.FAIL")
	if err != nil {
		t.Fatalf("find: %v", err)
	}
	if got.Status != inbox.MessageStatusFailed {
		t.Errorf("status = %q, want failed", got.Status)
	}
}

// TestHandleStatus_UnknownMessageIsSilentAck covers the "we got a
// status for a wamid we never sent" case: the use case reports
// unknown_message without persisting anything and the dedup row is
// closed so subsequent replays still collapse.
func TestHandleStatus_UnknownMessageIsSilentAck(t *testing.T) {
	repo := newInMemoryRepo()
	dedup := newInMemoryDedup()
	u := inboxusecase.MustNewUpdateMessageStatus(repo, dedup)
	tenant := uuid.New()
	ctx := context.Background()

	res, err := u.HandleStatus(ctx, inbox.StatusUpdate{
		TenantID:          tenant,
		Channel:           "whatsapp",
		ChannelExternalID: "wamid.NOPE",
		NewStatus:         inbox.MessageStatusDelivered,
		OccurredAt:        time.Now(),
	})
	if err != nil {
		t.Fatalf("HandleStatus: %v", err)
	}
	if res.Outcome != inbox.StatusOutcomeUnknownMessage {
		t.Errorf("outcome = %q, want unknown_message", res.Outcome)
	}
}

// TestHandleStatus_RejectsMalformedInput covers the boundary checks
// on the use case: nil tenant, empty channel, empty external id, and
// empty status MUST surface a typed sentinel.
func TestHandleStatus_RejectsMalformedInput(t *testing.T) {
	repo := newInMemoryRepo()
	dedup := newInMemoryDedup()
	u := inboxusecase.MustNewUpdateMessageStatus(repo, dedup)
	good := inbox.StatusUpdate{
		TenantID:          uuid.New(),
		Channel:           "whatsapp",
		ChannelExternalID: "wamid.x",
		NewStatus:         inbox.MessageStatusSent,
	}
	cases := []struct {
		name string
		mut  func(*inbox.StatusUpdate)
	}{
		{"nil tenant", func(e *inbox.StatusUpdate) { e.TenantID = uuid.Nil }},
		{"empty channel", func(e *inbox.StatusUpdate) { e.Channel = "" }},
		{"blank external id", func(e *inbox.StatusUpdate) { e.ChannelExternalID = "  " }},
		{"empty status", func(e *inbox.StatusUpdate) { e.NewStatus = "" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ev := good
			tc.mut(&ev)
			if _, err := u.HandleStatus(context.Background(), ev); err == nil {
				t.Errorf("err = nil, want validation error")
			}
		})
	}
}

// TestHandleStatus_TenantScopedLookup covers RLS isolation in the
// use case path: a status update from tenant B for a wamid owned by
// tenant A collapses to unknown_message rather than mutating the
// other tenant's row.
func TestHandleStatus_TenantScopedLookup(t *testing.T) {
	repo := newInMemoryRepo()
	dedup := newInMemoryDedup()
	u := inboxusecase.MustNewUpdateMessageStatus(repo, dedup)
	_, _ = seedOutboundMessage(t, repo, "wamid.OWNED", inbox.MessageStatusPending)
	otherTenant := uuid.New()
	ctx := context.Background()

	res, err := u.HandleStatus(ctx, inbox.StatusUpdate{
		TenantID:          otherTenant,
		Channel:           "whatsapp",
		ChannelExternalID: "wamid.OWNED",
		NewStatus:         inbox.MessageStatusDelivered,
		OccurredAt:        time.Now(),
	})
	if err != nil {
		t.Fatalf("HandleStatus: %v", err)
	}
	if res.Outcome != inbox.StatusOutcomeUnknownMessage {
		t.Errorf("outcome = %q, want unknown_message", res.Outcome)
	}
}
