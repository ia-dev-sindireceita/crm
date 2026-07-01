package usecase_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/inbox"
	inboxusecase "github.com/pericles-luz/crm/internal/inbox/usecase"
)

// fakeChannelResolver records the resolve call and returns a canned
// instance id (or error). It satisfies inboxusecase.ChannelResolver.
type fakeChannelResolver struct {
	id          uuid.UUID
	err         error
	gotTenant   uuid.UUID
	gotChannel  string
	gotExternal string
	calls       int
}

func (f *fakeChannelResolver) ResolveChannelID(_ context.Context, tenantID uuid.UUID, channelKey, externalID string) (uuid.UUID, error) {
	f.calls++
	f.gotTenant = tenantID
	f.gotChannel = channelKey
	f.gotExternal = externalID
	return f.id, f.err
}

func discardResolverLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// TestReceiveInbound_RoutesNewConversationToChannelInstance is the P4
// routing anchor: a new conversation references the resolved channel
// instance (channel_id), and the resolver is called with the inbound
// identity (carrier + destination address).
func TestReceiveInbound_RoutesNewConversationToChannelInstance(t *testing.T) {
	repo := newInMemoryRepo()
	dedup := newInMemoryDedup()
	contactsU := newStubContactUpserter()
	u := inboxusecase.MustNewReceiveInbound(repo, dedup, contactsU)

	instance := uuid.New()
	resolver := &fakeChannelResolver{id: instance}
	u.SetChannelResolver(resolver)
	u.SetChannelResolverLogger(discardResolverLogger())

	tenant := uuid.New()
	res, err := u.Execute(context.Background(), inbox.InboundEvent{
		TenantID:              tenant,
		Channel:               "whatsapp",
		ChannelExternalID:     "wamid.route1",
		SenderExternalID:      "+5511999990001",
		DestinationExternalID: "+5511333330000",
		Body:                  "olá",
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Conversation.ChannelID == nil || *res.Conversation.ChannelID != instance {
		t.Errorf("conversation ChannelID = %v, want %v", res.Conversation.ChannelID, instance)
	}
	// The resolver saw the normalised carrier + the destination address.
	if resolver.gotTenant != tenant || resolver.gotChannel != "whatsapp" || resolver.gotExternal != "+5511333330000" {
		t.Errorf("resolver call = (%v, %q, %q), want (%v, whatsapp, +5511333330000)",
			resolver.gotTenant, resolver.gotChannel, resolver.gotExternal, tenant)
	}
}

// TestReceiveInbound_RoutingUnresolvedLeavesChannelNil: a resolver that
// finds no instance (uuid.Nil) leaves the conversation unrouted rather
// than failing the delivery.
func TestReceiveInbound_RoutingUnresolvedLeavesChannelNil(t *testing.T) {
	repo := newInMemoryRepo()
	u := inboxusecase.MustNewReceiveInbound(repo, newInMemoryDedup(), newStubContactUpserter())
	u.SetChannelResolver(&fakeChannelResolver{id: uuid.Nil})
	u.SetChannelResolverLogger(discardResolverLogger())

	res, err := u.Execute(context.Background(), inbox.InboundEvent{
		TenantID:          uuid.New(),
		Channel:           "whatsapp",
		ChannelExternalID: "wamid.route2",
		SenderExternalID:  "+5511999990002",
		Body:              "oi",
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Conversation.ChannelID != nil {
		t.Errorf("ChannelID = %v, want nil (unresolved)", res.Conversation.ChannelID)
	}
}

// TestReceiveInbound_RoutingErrorIsSoftFail: a resolver error is logged
// and skipped — the message is still persisted and the conversation is
// created (unrouted), never dropped.
func TestReceiveInbound_RoutingErrorIsSoftFail(t *testing.T) {
	repo := newInMemoryRepo()
	u := inboxusecase.MustNewReceiveInbound(repo, newInMemoryDedup(), newStubContactUpserter())
	u.SetChannelResolver(&fakeChannelResolver{err: errors.New("resolve boom")})
	u.SetChannelResolverLogger(discardResolverLogger())

	res, err := u.Execute(context.Background(), inbox.InboundEvent{
		TenantID:          uuid.New(),
		Channel:           "whatsapp",
		ChannelExternalID: "wamid.route3",
		SenderExternalID:  "+5511999990003",
		Body:              "e aí",
	})
	if err != nil {
		t.Fatalf("Execute soft-fail broke: %v", err)
	}
	if res.Message == nil {
		t.Fatal("message not persisted on routing error")
	}
	if res.Conversation.ChannelID != nil {
		t.Errorf("ChannelID = %v, want nil on routing error", res.Conversation.ChannelID)
	}
}

// TestReceiveInbound_NoResolverIsPreP4NoOp: without a resolver wired the
// conversation is created with a nil channel_id (backwards-compatible).
func TestReceiveInbound_NoResolverIsPreP4NoOp(t *testing.T) {
	repo := newInMemoryRepo()
	u := inboxusecase.MustNewReceiveInbound(repo, newInMemoryDedup(), newStubContactUpserter())

	res, err := u.Execute(context.Background(), inbox.InboundEvent{
		TenantID:          uuid.New(),
		Channel:           "whatsapp",
		ChannelExternalID: "wamid.route4",
		SenderExternalID:  "+5511999990004",
		Body:              "hey",
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Conversation.ChannelID != nil {
		t.Errorf("ChannelID = %v, want nil with no resolver wired", res.Conversation.ChannelID)
	}
}

// TestSetChannelResolverNilIsNoop: SetChannelResolver(nil) leaves routing
// disabled without panicking.
func TestSetChannelResolverNilIsNoop(t *testing.T) {
	u := inboxusecase.MustNewReceiveInbound(newInMemoryRepo(), newInMemoryDedup(), newStubContactUpserter())
	u.SetChannelResolver(nil)
	if _, err := u.Execute(context.Background(), inbox.InboundEvent{
		TenantID:          uuid.New(),
		Channel:           "whatsapp",
		ChannelExternalID: "wamid.route5",
		SenderExternalID:  "+5511999990005",
		Body:              "test",
	}); err != nil {
		t.Fatalf("Execute after SetChannelResolver(nil): %v", err)
	}
}
