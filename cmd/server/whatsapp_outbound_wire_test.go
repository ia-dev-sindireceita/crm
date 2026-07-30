package main

// SIN-68302 — unit tests for the WhatsApp outbound dispatcher wiring.
//
// No DB (test pyramid, AC4): storage ports are in-memory fakes and the
// Graph HTTP path itself is already covered by
// internal/adapter/channel/whatsapp/sender_test.go — we do not re-test the
// real Graph call here. These tests prove:
//   - channelRoutingOutbound routes by conversation channel (WhatsApp ->
//     Graph sender, other channels -> primary; no cross-channel leak);
//   - a real inbox/usecase.SendOutbound wired to the router invokes the
//     WhatsApp OutboundChannel on a reply to a WhatsApp conversation;
//   - the deny-by-default gates (token / flag) collapse to no outbound;
//   - the conversation -> recipient contact lookup resolves the whatsapp
//     identity.

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	channelswa "github.com/pericles-luz/crm/internal/adapter/channels/whatsapp"
	pgpool "github.com/pericles-luz/crm/internal/adapter/db/postgres"
	"github.com/pericles-luz/crm/internal/contacts"
	"github.com/pericles-luz/crm/internal/inbox"
	inboxusecase "github.com/pericles-luz/crm/internal/inbox/usecase"
	"github.com/prometheus/client_golang/prometheus"
)

// --- fakes -----------------------------------------------------------------
//
// recordingOutbound (a shared fake inbox.OutboundChannel with .called /
// .last / .id / .err) is defined in wa_session_wire_test.go; we reuse it.

// fakeSendRepo is a minimal inbox.Repository for the SendOutbound path. It
// stores conversations by id and captures saved/updated messages. Only the
// methods SendOutbound.Execute touches are implemented; the rest embed the
// interface and panic if ever called (they are not on this path).
type fakeSendRepo struct {
	inbox.Repository
	convs   map[uuid.UUID]*inbox.Conversation
	saved   []*inbox.Message
	updated []*inbox.Message
}

func newFakeSendRepo() *fakeSendRepo {
	return &fakeSendRepo{convs: map[uuid.UUID]*inbox.Conversation{}}
}

func (f *fakeSendRepo) seed(c *inbox.Conversation) { f.convs[c.ID] = c }

func (f *fakeSendRepo) GetConversation(_ context.Context, _ uuid.UUID, id uuid.UUID) (*inbox.Conversation, error) {
	c, ok := f.convs[id]
	if !ok {
		return nil, inbox.ErrNotFound
	}
	return c, nil
}

func (f *fakeSendRepo) SaveMessage(_ context.Context, m *inbox.Message) error {
	f.saved = append(f.saved, m)
	return nil
}

func (f *fakeSendRepo) UpdateMessage(_ context.Context, m *inbox.Message) error {
	f.updated = append(f.updated, m)
	return nil
}

// --- channelRoutingOutbound ------------------------------------------------

func TestChannelRoutingOutbound_RoutesWhatsAppToGraphSender(t *testing.T) {
	wa := &recordingOutbound{id: "wamid.wa"}
	primary := &recordingOutbound{id: "other"}
	r := channelRoutingOutbound{whatsapp: wa, primary: primary}

	extID, err := r.SendMessage(context.Background(), inbox.OutboundMessage{
		Channel: channelswa.Channel, // "whatsapp"
		Body:    "olá",
	})
	if err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	if extID != "wamid.wa" {
		t.Errorf("extID = %q, want wamid.wa", extID)
	}
	if wa.called != 1 {
		t.Errorf("whatsapp sender calls = %d, want 1", wa.called)
	}
	if primary.called != 0 {
		t.Errorf("primary leaked %d calls on whatsapp channel", primary.called)
	}
}

func TestChannelRoutingOutbound_RoutesOtherChannelToPrimary(t *testing.T) {
	wa := &recordingOutbound{id: "wamid.wa"}
	primary := &recordingOutbound{id: "mid.msgr"}
	r := channelRoutingOutbound{whatsapp: wa, primary: primary}

	extID, err := r.SendMessage(context.Background(), inbox.OutboundMessage{
		Channel: "messenger",
		Body:    "hi",
	})
	if err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	if extID != "mid.msgr" {
		t.Errorf("extID = %q, want mid.msgr", extID)
	}
	if primary.called != 1 {
		t.Errorf("primary calls = %d, want 1", primary.called)
	}
	if wa.called != 0 {
		t.Errorf("whatsapp sender leaked %d calls on messenger channel", wa.called)
	}
}

func TestChannelRoutingOutbound_NilWhatsAppFallsThroughToPrimary(t *testing.T) {
	primary := &recordingOutbound{id: "fallback"}
	r := channelRoutingOutbound{whatsapp: nil, primary: primary}

	extID, err := r.SendMessage(context.Background(), inbox.OutboundMessage{
		Channel: channelswa.Channel,
	})
	if err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	if extID != "fallback" {
		t.Errorf("extID = %q, want fallback", extID)
	}
	if primary.called != 1 {
		t.Errorf("primary calls = %d, want 1 (nil whatsapp must fall through)", primary.called)
	}
}

func TestChannelRoutingOutbound_NilPrimaryIsNotFound(t *testing.T) {
	r := channelRoutingOutbound{whatsapp: nil, primary: nil}
	_, err := r.SendMessage(context.Background(), inbox.OutboundMessage{Channel: "messenger"})
	if !errors.Is(err, inbox.ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestDisabledOutbound_ReturnsNotFound(t *testing.T) {
	_, err := disabledOutbound{}.SendMessage(context.Background(), inbox.OutboundMessage{Channel: "sms"})
	if !errors.Is(err, inbox.ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

// --- real SendOutbound over the router (AC3/AC4) ---------------------------

func TestSendOutbound_WhatsAppReply_InvokesGraphSender(t *testing.T) {
	repo := newFakeSendRepo()
	tenant := uuid.New()
	conv, err := inbox.NewConversation(tenant, uuid.New(), channelswa.Channel)
	if err != nil {
		t.Fatalf("NewConversation: %v", err)
	}
	repo.seed(conv)

	wa := &recordingOutbound{id: "wamid.sent"}
	primary := &recordingOutbound{id: "should-not-fire"}
	router := channelRoutingOutbound{whatsapp: wa, primary: primary}

	uc, err := inboxusecase.NewSendOutbound(repo, passthroughWalletDebitor{}, router)
	if err != nil {
		t.Fatalf("NewSendOutbound: %v", err)
	}
	res, err := uc.Execute(context.Background(), inboxusecase.SendOutboundInput{
		TenantID:       tenant,
		ConversationID: conv.ID,
		Body:           "resposta do atendente",
		ToExternalID:   "+5511999990001",
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	// The WhatsApp Graph sender — not the fallback — carried the reply.
	if wa.called != 1 {
		t.Fatalf("whatsapp sender calls = %d, want 1", wa.called)
	}
	if primary.called != 0 {
		t.Errorf("primary leaked %d calls on whatsapp reply", primary.called)
	}
	got := wa.last
	if got.Channel != channelswa.Channel {
		t.Errorf("routed channel = %q, want whatsapp", got.Channel)
	}
	if got.Body != "resposta do atendente" {
		t.Errorf("body = %q", got.Body)
	}
	if got.ToExternalID != "+5511999990001" {
		t.Errorf("to = %q", got.ToExternalID)
	}
	// The carrier wamid is persisted on the outbound row (sent state).
	if res.Message == nil || res.Message.Status != inbox.MessageStatusSent {
		t.Fatalf("message not marked sent: %+v", res.Message)
	}
	if res.Message.ChannelExternalID != "wamid.sent" {
		t.Errorf("ChannelExternalID = %q, want wamid.sent", res.Message.ChannelExternalID)
	}
}

func TestSendOutbound_NonWhatsAppReply_DoesNotHitGraphSender(t *testing.T) {
	repo := newFakeSendRepo()
	tenant := uuid.New()
	conv, err := inbox.NewConversation(tenant, uuid.New(), "messenger")
	if err != nil {
		t.Fatalf("NewConversation: %v", err)
	}
	repo.seed(conv)

	wa := &recordingOutbound{id: "wamid.should-not-fire"}
	primary := &recordingOutbound{id: "mid.msgr"}
	router := channelRoutingOutbound{whatsapp: wa, primary: primary}

	uc, err := inboxusecase.NewSendOutbound(repo, passthroughWalletDebitor{}, router)
	if err != nil {
		t.Fatalf("NewSendOutbound: %v", err)
	}
	if _, err := uc.Execute(context.Background(), inboxusecase.SendOutboundInput{
		TenantID:       tenant,
		ConversationID: conv.ID,
		Body:           "hi",
		ToExternalID:   "psid-123",
	}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if wa.called != 0 {
		t.Errorf("whatsapp sender fired %d times on a messenger reply (channel leak)", wa.called)
	}
	if primary.called != 1 {
		t.Errorf("primary calls = %d, want 1", primary.called)
	}
}

// --- passthrough wallet ----------------------------------------------------

func TestPassthroughWalletDebitor_RunsCharge(t *testing.T) {
	called := false
	err := passthroughWalletDebitor{}.Debit(context.Background(), uuid.New(), 0, func(context.Context) error {
		called = true
		return nil
	})
	if err != nil {
		t.Fatalf("Debit: %v", err)
	}
	if !called {
		t.Error("charge callback was not invoked")
	}
}

func TestPassthroughWalletDebitor_PropagatesChargeError(t *testing.T) {
	sentinel := errors.New("carrier down")
	err := passthroughWalletDebitor{}.Debit(context.Background(), uuid.New(), 3, func(context.Context) error {
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Errorf("err = %v, want carrier down", err)
	}
}

// --- contact lookup --------------------------------------------------------

type fakeConvResolver struct {
	conv *inbox.Conversation
	err  error
}

func (f fakeConvResolver) GetConversation(context.Context, uuid.UUID, uuid.UUID) (*inbox.Conversation, error) {
	return f.conv, f.err
}

type fakeContactFinder struct {
	contact *contacts.Contact
	err     error
}

func (f fakeContactFinder) FindByID(context.Context, uuid.UUID, uuid.UUID) (*contacts.Contact, error) {
	return f.contact, f.err
}

func TestWhatsAppOutboundContactLookup_ResolvesWhatsAppIdentity(t *testing.T) {
	tenant := uuid.New()
	contactID := uuid.New()
	conv, _ := inbox.NewConversation(tenant, contactID, channelswa.Channel)
	waID, _ := contacts.NewChannelIdentity(contacts.ChannelWhatsApp, "+5511988887777")
	emailID, _ := contacts.NewChannelIdentity("email", "x@example.com")
	c := contacts.Hydrate(contactID, tenant, "Alice", []contacts.ChannelIdentity{emailID, waID}, conv.CreatedAt, conv.CreatedAt)

	lookup := whatsappOutboundContactLookup(fakeConvResolver{conv: conv}, fakeContactFinder{contact: c})
	to, err := lookup(context.Background(), tenant, conv.ID)
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if to != "+5511988887777" {
		t.Errorf("to = %q, want the whatsapp E.164", to)
	}
}

func TestWhatsAppOutboundContactLookup_NoWhatsAppIdentity(t *testing.T) {
	tenant := uuid.New()
	contactID := uuid.New()
	conv, _ := inbox.NewConversation(tenant, contactID, channelswa.Channel)
	emailID, _ := contacts.NewChannelIdentity("email", "x@example.com")
	c := contacts.Hydrate(contactID, tenant, "Bob", []contacts.ChannelIdentity{emailID}, conv.CreatedAt, conv.CreatedAt)

	lookup := whatsappOutboundContactLookup(fakeConvResolver{conv: conv}, fakeContactFinder{contact: c})
	if _, err := lookup(context.Background(), tenant, conv.ID); err == nil {
		t.Error("expected error when contact has no whatsapp identity")
	}
}

func TestWhatsAppOutboundContactLookup_PropagatesErrors(t *testing.T) {
	tenant := uuid.New()
	sentinel := errors.New("rls hidden")
	lookup := whatsappOutboundContactLookup(fakeConvResolver{err: sentinel}, fakeContactFinder{})
	if _, err := lookup(context.Background(), tenant, uuid.New()); !errors.Is(err, sentinel) {
		t.Errorf("err = %v, want rls hidden", err)
	}
}

// --- tenant config lookup (AC2) --------------------------------------------

type fakeWAFlag struct {
	on  bool
	err error
}

func (f fakeWAFlag) Enabled(context.Context, uuid.UUID) (bool, error) { return f.on, f.err }

func TestWhatsAppTenantConfigLookup_ResolvesPhoneAndEnabled(t *testing.T) {
	phone := func(context.Context, uuid.UUID) (string, error) { return "PN-123", nil }
	lookup := whatsappTenantConfigLookup(phone, fakeWAFlag{on: true})

	cfg, err := lookup(context.Background(), uuid.New())
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if cfg.PhoneNumberID != "PN-123" {
		t.Errorf("PhoneNumberID = %q, want PN-123", cfg.PhoneNumberID)
	}
	if !cfg.Enabled {
		t.Error("Enabled = false, want true")
	}
}

func TestWhatsAppTenantConfigLookup_FlagOffDisablesTenant(t *testing.T) {
	phone := func(context.Context, uuid.UUID) (string, error) { return "PN-123", nil }
	lookup := whatsappTenantConfigLookup(phone, fakeWAFlag{on: false})

	cfg, err := lookup(context.Background(), uuid.New())
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if cfg.Enabled {
		t.Error("Enabled = true, want false (deny-by-default)")
	}
}

func TestWhatsAppTenantConfigLookup_NoAssociationYieldsEmptyPhone(t *testing.T) {
	// Empty phone id => Sender surfaces ErrChannelAuthFailed rather than
	// dialling a 404. The lookup itself must not error on the empty string.
	phone := func(context.Context, uuid.UUID) (string, error) { return "", nil }
	lookup := whatsappTenantConfigLookup(phone, fakeWAFlag{on: true})

	cfg, err := lookup(context.Background(), uuid.New())
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if cfg.PhoneNumberID != "" {
		t.Errorf("PhoneNumberID = %q, want empty", cfg.PhoneNumberID)
	}
}

func TestWhatsAppTenantConfigLookup_PropagatesErrors(t *testing.T) {
	phoneErr := errors.New("db down")
	lookup := whatsappTenantConfigLookup(
		func(context.Context, uuid.UUID) (string, error) { return "", phoneErr },
		fakeWAFlag{on: true},
	)
	if _, err := lookup(context.Background(), uuid.New()); err == nil || !errors.Is(err, phoneErr) {
		t.Errorf("phone err = %v, want wrapped db down", err)
	}

	flagErr := errors.New("flag store down")
	lookup2 := whatsappTenantConfigLookup(
		func(context.Context, uuid.UUID) (string, error) { return "PN-1", nil },
		fakeWAFlag{err: flagErr},
	)
	if _, err := lookup2(context.Background(), uuid.New()); !errors.Is(err, flagErr) {
		t.Errorf("flag err = %v, want flag store down", err)
	}
}

// --- gating ----------------------------------------------------------------

func envFrom(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestBuildWhatsAppOutboundSender_Gating(t *testing.T) {
	tests := []struct {
		name    string
		env     map[string]string
		wantNil bool
	}{
		{
			name:    "no token disables outbound",
			env:     map[string]string{channelswa.EnvWhatsAppEnabled: "1"},
			wantNil: true,
		},
		{
			name:    "flag off disables outbound",
			env:     map[string]string{envMetaGraphToken: "tkn"},
			wantNil: true,
		},
		{
			name:    "token + flag on returns a sender",
			env:     map[string]string{envMetaGraphToken: "tkn", channelswa.EnvWhatsAppEnabled: "1"},
			wantNil: false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// nil pool is safe: the TenantConfigLookup closure is not
			// invoked at construction, only on a live send.
			got := buildWhatsAppOutboundSender(nil, envFrom(tc.env), prometheus.NewRegistry())
			if tc.wantNil && got != nil {
				t.Errorf("sender = %v, want nil (gate closed)", got)
			}
			if !tc.wantNil && got == nil {
				t.Error("sender = nil, want non-nil (gate open)")
			}
		})
	}
}

func TestBuildInboxOutboundSendForView_DenyByDefault(t *testing.T) {
	tests := []struct {
		name string
		env  map[string]string
	}{
		{"bare deploy (no token, no flag)", map[string]string{}},
		{"token but flag off", map[string]string{envMetaGraphToken: "tkn"}},
		{"flag on but no token", map[string]string{channelswa.EnvWhatsAppEnabled: "1"}},
		{"token + flag but no DATABASE_URL", map[string]string{envMetaGraphToken: "tkn", channelswa.EnvWhatsAppEnabled: "1"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Ensure DSN is absent for the last case regardless of process env.
			env := envFrom(tc.env)
			send, cleanup, ok := buildInboxOutboundSendForView(context.Background(), env)
			if ok || send != nil {
				t.Errorf("ok=%v send=%v, want disabled (stub kept)", ok, send)
			}
			if cleanup == nil {
				t.Error("cleanup must be non-nil even when disabled")
			}
			cleanup() // must be a safe no-op
		})
	}
	// Guard: the DSN env constant is the one the gate reads.
	if pgpool.EnvDSN == "" {
		t.Error("pgpool.EnvDSN unexpectedly empty")
	}
}
