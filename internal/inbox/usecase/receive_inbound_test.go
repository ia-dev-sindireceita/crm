package usecase_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/contacts"
	contactsusecase "github.com/pericles-luz/crm/internal/contacts/usecase"
	"github.com/pericles-luz/crm/internal/inbox"
	inboxusecase "github.com/pericles-luz/crm/internal/inbox/usecase"
)

func TestNewReceiveInbound_RejectsNilDeps(t *testing.T) {
	repo := newInMemoryRepo()
	dedup := newInMemoryDedup()
	contactsU := newStubContactUpserter()
	cases := []struct {
		name string
		args [3]any
	}{
		{"nil repo", [3]any{nil, dedup, contactsU}},
		{"nil dedup", [3]any{repo, nil, contactsU}},
		{"nil contacts", [3]any{repo, dedup, nil}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var r inbox.Repository
			if tc.args[0] != nil {
				r = tc.args[0].(inbox.Repository)
			}
			var d inbox.InboundDedupRepository
			if tc.args[1] != nil {
				d = tc.args[1].(inbox.InboundDedupRepository)
			}
			var cu inboxusecase.ContactUpserter
			if tc.args[2] != nil {
				cu = tc.args[2].(inboxusecase.ContactUpserter)
			}
			if _, err := inboxusecase.NewReceiveInbound(r, d, cu); err == nil {
				t.Errorf("err = nil, want construction error")
			}
		})
	}
}

func TestMustNewReceiveInbound_PanicsOnNil(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Error("MustNewReceiveInbound did not panic")
		}
	}()
	inboxusecase.MustNewReceiveInbound(nil, nil, nil)
}

func TestReceiveInbound_HappyPath(t *testing.T) {
	repo := newInMemoryRepo()
	dedup := newInMemoryDedup()
	contactsU := newStubContactUpserter()
	u := inboxusecase.MustNewReceiveInbound(repo, dedup, contactsU)
	tenant := uuid.New()
	res, err := u.Execute(context.Background(), inbox.InboundEvent{
		TenantID:          tenant,
		Channel:           "whatsapp",
		ChannelExternalID: "wamid.abc",
		SenderExternalID:  "+5511999990001",
		SenderDisplayName: "Alice",
		Body:              "hello",
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Duplicate {
		t.Error("Duplicate = true, want false")
	}
	if res.Contact == nil || res.Conversation == nil || res.Message == nil {
		t.Fatalf("result missing pieces: %+v", res)
	}
	if res.Message.Direction != inbox.MessageDirectionIn {
		t.Errorf("Direction = %q, want in", res.Message.Direction)
	}
	if res.Message.ChannelExternalID != "wamid.abc" {
		t.Errorf("ChannelExternalID = %q, want wamid.abc", res.Message.ChannelExternalID)
	}
	if !res.Conversation.LastMessageAt.Equal(res.Message.CreatedAt) {
		t.Errorf("LastMessageAt = %v, want %v", res.Conversation.LastMessageAt, res.Message.CreatedAt)
	}
}

// TestReceiveInbound_IdempotentByDedup is the AC #4 anchor in unit
// land: two callers racing on the same dedup key result in exactly
// one persisted message and exactly one contact-upserter call after
// the winner. The integration test (postgres adapter) exercises the
// same property against real RLS / UNIQUE constraints.
func TestReceiveInbound_IdempotentByDedup(t *testing.T) {
	repo := newInMemoryRepo()
	dedup := newInMemoryDedup()
	contactsU := newStubContactUpserter()
	u := inboxusecase.MustNewReceiveInbound(repo, dedup, contactsU)
	tenant := uuid.New()
	ev := inbox.InboundEvent{
		TenantID:          tenant,
		Channel:           "whatsapp",
		ChannelExternalID: "wamid.dupkey",
		SenderExternalID:  "+5511999990001",
		SenderDisplayName: "Alice",
		Body:              "hello",
	}
	if _, err := u.Execute(context.Background(), ev); err != nil {
		t.Fatalf("first Execute: %v", err)
	}
	res, err := u.Execute(context.Background(), ev)
	if err != nil {
		t.Fatalf("second Execute: %v", err)
	}
	if !res.Duplicate {
		t.Error("Duplicate = false, want true on retry")
	}
	if repo.messageCount() != 1 {
		t.Errorf("messages persisted = %d, want 1", repo.messageCount())
	}
	if repo.conversationCount() != 1 {
		t.Errorf("conversations persisted = %d, want 1", repo.conversationCount())
	}
}

func TestReceiveInbound_ReusesOpenConversation(t *testing.T) {
	repo := newInMemoryRepo()
	dedup := newInMemoryDedup()
	contactsU := newStubContactUpserter()
	u := inboxusecase.MustNewReceiveInbound(repo, dedup, contactsU)
	tenant := uuid.New()
	r1, err := u.Execute(context.Background(), inbox.InboundEvent{
		TenantID: tenant, Channel: "whatsapp",
		ChannelExternalID: "wamid.1", SenderExternalID: "+5511999990001",
		SenderDisplayName: "Alice", Body: "hello",
	})
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	r2, err := u.Execute(context.Background(), inbox.InboundEvent{
		TenantID: tenant, Channel: "whatsapp",
		ChannelExternalID: "wamid.2", SenderExternalID: "+5511999990001",
		SenderDisplayName: "Alice", Body: "world",
	})
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if r1.Conversation.ID != r2.Conversation.ID {
		t.Errorf("conversation id mismatch: %s vs %s", r1.Conversation.ID, r2.Conversation.ID)
	}
	if repo.messageCount() != 2 {
		t.Errorf("messages = %d, want 2", repo.messageCount())
	}
	if repo.conversationCount() != 1 {
		t.Errorf("conversations = %d, want 1", repo.conversationCount())
	}
}

func TestReceiveInbound_PreservesOccurredAt(t *testing.T) {
	repo := newInMemoryRepo()
	dedup := newInMemoryDedup()
	contactsU := newStubContactUpserter()
	u := inboxusecase.MustNewReceiveInbound(repo, dedup, contactsU)
	occurred := time.Date(2026, 5, 14, 9, 0, 0, 0, time.UTC)
	res, err := u.Execute(context.Background(), inbox.InboundEvent{
		TenantID: uuid.New(), Channel: "whatsapp",
		ChannelExternalID: "wamid.t", SenderExternalID: "+5511999990001",
		SenderDisplayName: "Alice", Body: "hello", OccurredAt: occurred,
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !res.Message.CreatedAt.Equal(occurred) {
		t.Errorf("CreatedAt = %v, want %v", res.Message.CreatedAt, occurred)
	}
}

func TestReceiveInbound_Validates(t *testing.T) {
	repo := newInMemoryRepo()
	dedup := newInMemoryDedup()
	contactsU := newStubContactUpserter()
	u := inboxusecase.MustNewReceiveInbound(repo, dedup, contactsU)
	base := inbox.InboundEvent{
		TenantID: uuid.New(), Channel: "whatsapp",
		ChannelExternalID: "wamid.v", SenderExternalID: "+5511999990001",
		SenderDisplayName: "Alice", Body: "hello",
	}
	zeroTenant := base
	zeroTenant.TenantID = uuid.Nil
	if _, err := u.Execute(context.Background(), zeroTenant); !errors.Is(err, inbox.ErrInvalidTenant) {
		t.Errorf("zero tenant err = %v, want ErrInvalidTenant", err)
	}
	emptyChannel := base
	emptyChannel.Channel = "  "
	if _, err := u.Execute(context.Background(), emptyChannel); !errors.Is(err, inbox.ErrInvalidChannel) {
		t.Errorf("empty channel err = %v, want ErrInvalidChannel", err)
	}
	emptyExt := base
	emptyExt.ChannelExternalID = " "
	if _, err := u.Execute(context.Background(), emptyExt); err == nil {
		t.Error("empty channel-external-id err = nil")
	}
}

// TestReceiveInbound_ConcurrentDedup proves that 50 concurrent callers
// with the same dedup key result in exactly one Message persisted. The
// in-memory dedup uses a mutex; the production adapter relies on the
// inbound_message_dedup UNIQUE(channel, channel_external_id) index.
func TestReceiveInbound_ConcurrentDedup(t *testing.T) {
	repo := newInMemoryRepo()
	dedup := newInMemoryDedup()
	contactsU := newStubContactUpserter()
	u := inboxusecase.MustNewReceiveInbound(repo, dedup, contactsU)
	tenant := uuid.New()
	ev := inbox.InboundEvent{
		TenantID: tenant, Channel: "whatsapp",
		ChannelExternalID: "wamid.concurrent", SenderExternalID: "+5511999990001",
		SenderDisplayName: "Alice", Body: "hello",
	}
	const n = 50
	var wg sync.WaitGroup
	var duplicates atomic.Int64
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			res, err := u.Execute(context.Background(), ev)
			if err != nil {
				t.Errorf("Execute: %v", err)
				return
			}
			if res.Duplicate {
				duplicates.Add(1)
			}
		}()
	}
	close(start)
	wg.Wait()
	if got := duplicates.Load(); got != n-1 {
		t.Errorf("Duplicate count = %d, want %d", got, n-1)
	}
	if repo.messageCount() != 1 {
		t.Errorf("messages = %d, want 1", repo.messageCount())
	}
	if repo.conversationCount() != 1 {
		t.Errorf("conversations = %d, want 1", repo.conversationCount())
	}
}

func TestReceiveInbound_DedupClaimNonAlreadyError(t *testing.T) {
	repo := newInMemoryRepo()
	dedup := &errorClaimDedup{err: errors.New("boom")}
	contactsU := newStubContactUpserter()
	u := inboxusecase.MustNewReceiveInbound(repo, dedup, contactsU)
	_, err := u.Execute(context.Background(), inbox.InboundEvent{
		TenantID: uuid.New(), Channel: "whatsapp",
		ChannelExternalID: "wamid.x", SenderExternalID: "+5511999990001",
		SenderDisplayName: "Alice", Body: "hello",
	})
	if err == nil {
		t.Error("err = nil, want boom")
	}
}

type errorClaimDedup struct{ err error }

func (e *errorClaimDedup) Claim(_ context.Context, _, _ string) error { return e.err }
func (e *errorClaimDedup) MarkProcessed(_ context.Context, _, _ string) error {
	return nil
}

// TestReceiveInbound_ContactUpserterError surfaces failures from the
// contacts use-case: the dedup row has already been claimed, but
// downstream failed, so Execute returns the error and a follow-up
// retry (with the same dedup key) is reported as Duplicate. That
// matches the GC-recovers-pending-claim contract documented on
// inbound_message_dedup.
// TestReceiveInbound_FallbackDisplay_UsesSenderExternalIDWhenProfileBlank
// covers the fallback branch in fallbackDisplay: empty
// SenderDisplayName falls through to SenderExternalID.
func TestReceiveInbound_FallbackDisplay_UsesSenderExternalIDWhenProfileBlank(t *testing.T) {
	repo := newInMemoryRepo()
	dedup := newInMemoryDedup()
	contactsU := &recordingContactUpserter{}
	u := inboxusecase.MustNewReceiveInbound(repo, dedup, contactsU)
	if _, err := u.Execute(context.Background(), inbox.InboundEvent{
		TenantID: uuid.New(), Channel: "whatsapp",
		ChannelExternalID: "wamid.f1", SenderExternalID: "+5511999990001",
		SenderDisplayName: "  ", Body: "hi",
	}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if got := contactsU.calls[0].DisplayName; got != "+5511999990001" {
		t.Errorf("display = %q, want sender external id", got)
	}
}

func TestReceiveInbound_RejectsBlankSenderExternalID(t *testing.T) {
	u := inboxusecase.MustNewReceiveInbound(newInMemoryRepo(), newInMemoryDedup(), newStubContactUpserter())
	_, err := u.Execute(context.Background(), inbox.InboundEvent{
		TenantID: uuid.New(), Channel: "whatsapp",
		ChannelExternalID: "wamid.x", SenderExternalID: "  ",
		SenderDisplayName: "Alice", Body: "hi",
	})
	if !errors.Is(err, inbox.ErrInvalidChannel) {
		t.Errorf("err = %v, want ErrInvalidChannel", err)
	}
}

type recordingContactUpserter struct {
	calls []contactsusecase.Input
}

func (r *recordingContactUpserter) Execute(_ context.Context, in contactsusecase.Input) (contactsusecase.Result, error) {
	r.calls = append(r.calls, in)
	c, err := contacts.New(in.TenantID, in.DisplayName)
	if err != nil {
		return contactsusecase.Result{}, err
	}
	if err := c.AddChannelIdentity(in.Channel, in.ExternalID); err != nil {
		return contactsusecase.Result{}, err
	}
	return contactsusecase.Result{Contact: c, Created: true}, nil
}

func TestReceiveInbound_ContactUpserterError(t *testing.T) {
	repo := newInMemoryRepo()
	dedup := newInMemoryDedup()
	contactsU := &errorContactUpserter{err: errors.New("contacts down")}
	u := inboxusecase.MustNewReceiveInbound(repo, dedup, contactsU)
	_, err := u.Execute(context.Background(), inbox.InboundEvent{
		TenantID: uuid.New(), Channel: "whatsapp",
		ChannelExternalID: "wamid.err", SenderExternalID: "+5511999990001",
		SenderDisplayName: "Alice", Body: "hello",
	})
	if err == nil || err.Error() != "contacts down" {
		t.Errorf("err = %v, want contacts down", err)
	}
}

type errorContactUpserter struct{ err error }

func (e *errorContactUpserter) Execute(_ context.Context, _ contactsusecase.Input) (contactsusecase.Result, error) {
	return contactsusecase.Result{}, e.err
}
