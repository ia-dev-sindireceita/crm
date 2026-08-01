package main

// Coverage for the whatsapp+fake-customer merge in inbox_wire_real.go:
// combinedOutboundContactLookup's per-channel dispatch (pure, fake-backed,
// no DB) and the fake-customer-enabled assembly path booting cleanly
// (mirrors TestBuildInboxHandlerReal_Boots' unreachable-pool pattern — no
// live Postgres needed since every pg adapter/use-case constructor only
// wraps the pool at assembly time).

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/adapter/channels/llmcustomer"
	"github.com/pericles-luz/crm/internal/contacts"
	"github.com/pericles-luz/crm/internal/inbox"
)

// fakeConversationResolver / fakeContactIdentityFinder are minimal,
// in-memory stand-ins for the conversationResolver / contactIdentityFinder
// ports combinedOutboundContactLookup consumes.
type fakeConversationResolver struct {
	conv *inbox.Conversation
	err  error
}

func (f *fakeConversationResolver) GetConversation(_ context.Context, _, _ uuid.UUID) (*inbox.Conversation, error) {
	return f.conv, f.err
}

type fakeContactIdentityFinder struct {
	contact *contacts.Contact
	err     error
}

func (f *fakeContactIdentityFinder) FindByID(_ context.Context, _, _ uuid.UUID) (*contacts.Contact, error) {
	return f.contact, f.err
}

func TestCombinedOutboundContactLookup_FakellmChannel_ReturnsSyntheticID(t *testing.T) {
	t.Parallel()
	convs := &fakeConversationResolver{conv: &inbox.Conversation{
		Channel:   llmcustomer.ChannelName,
		ContactID: uuid.New(),
	}}
	// finder must NOT be consulted for the fakellm channel — a nil-err
	// fake with no contact set would panic Identities() if reached.
	finder := &fakeContactIdentityFinder{err: errors.New("must not be called")}

	lookup := combinedOutboundContactLookup(convs, finder)
	got, err := lookup(context.Background(), uuid.New(), uuid.New())
	if err != nil {
		t.Fatalf("lookup: unexpected error: %v", err)
	}
	if got != llmcustomer.SyntheticContactExternalID {
		t.Fatalf("lookup = %q, want %q", got, llmcustomer.SyntheticContactExternalID)
	}
}

func TestCombinedOutboundContactLookup_WhatsAppChannel_FallsThroughToIdentity(t *testing.T) {
	t.Parallel()
	contactID := uuid.New()
	convs := &fakeConversationResolver{conv: &inbox.Conversation{
		Channel:   "whatsapp",
		ContactID: contactID,
	}}
	c := contacts.Hydrate(contactID, uuid.New(), "Ana", []contacts.ChannelIdentity{
		{Channel: contacts.ChannelWhatsApp, ExternalID: "+5511999990000"},
	}, time.Now(), time.Now())
	finder := &fakeContactIdentityFinder{contact: c}

	lookup := combinedOutboundContactLookup(convs, finder)
	got, err := lookup(context.Background(), uuid.New(), uuid.New())
	if err != nil {
		t.Fatalf("lookup: unexpected error: %v", err)
	}
	if got != "+5511999990000" {
		t.Fatalf("lookup = %q, want +5511999990000", got)
	}
}

func TestCombinedOutboundContactLookup_WhatsAppChannel_NoIdentity_Errors(t *testing.T) {
	t.Parallel()
	contactID := uuid.New()
	convs := &fakeConversationResolver{conv: &inbox.Conversation{
		Channel:   "whatsapp",
		ContactID: contactID,
	}}
	c := contacts.Hydrate(contactID, uuid.New(), "Ana", nil, time.Now(), time.Now())
	finder := &fakeContactIdentityFinder{contact: c}

	lookup := combinedOutboundContactLookup(convs, finder)
	if _, err := lookup(context.Background(), uuid.New(), uuid.New()); err == nil {
		t.Fatal("lookup: expected error for a contact with no whatsapp identity")
	}
}

func TestCombinedOutboundContactLookup_PropagatesGetConversationError(t *testing.T) {
	t.Parallel()
	boom := errors.New("boom")
	convs := &fakeConversationResolver{err: boom}
	finder := &fakeContactIdentityFinder{}

	lookup := combinedOutboundContactLookup(convs, finder)
	if _, err := lookup(context.Background(), uuid.New(), uuid.New()); !errors.Is(err, boom) {
		t.Fatalf("lookup err = %v, want wrapping %v", err, boom)
	}
}

// TestAssembleInboxHandlerReal_FakeCustomerEnabled_Boots pins that turning
// on INBOX_FAKE_CUSTOMER_ENABLED no longer crashes the real-carrier wire
// (regression guard for the "duplicate metrics collector registration"
// class of bug fixed earlier this session) and that the fake-customer
// adapter's Stop() is reachable through the returned cleanup without
// panicking. PERSONA_LLM_PROVIDER is left unset so buildPersonaLLM falls
// back to the canned provider — no OPENROUTER_API_KEY needed.
func TestAssembleInboxHandlerReal_FakeCustomerEnabled_Boots(t *testing.T) {
	t.Parallel()
	pool := newUnreachablePool(t)
	mux, cleanup, err := assembleInboxHandlerRealFromPool(pool, nil, envOnly(map[string]string{
		envInboxFakeCustomerEnabled: "1",
	}))
	if err != nil {
		t.Fatalf("assembleInboxHandlerRealFromPool: %v", err)
	}
	if mux == nil {
		t.Fatalf("assembleInboxHandlerRealFromPool returned nil handler")
	}

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	res, err := http.Get(srv.URL + "/inbox")
	if err != nil {
		t.Fatalf("GET /inbox: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode == http.StatusNotFound {
		t.Fatalf("GET /inbox returned 404; real-carrier route not mounted")
	}

	// cleanup must call the fake adapter's Stop() and return without
	// panicking or hanging (Stop waits on in-flight goroutines, of which
	// there are none here).
	done := make(chan struct{})
	go func() { cleanup(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("cleanup did not return within 2s — fakeAdapter.Stop() may be stuck")
	}
}

func TestAssembleInboxHandlerReal_FakeCustomerDisabled_CleanupIsNoop(t *testing.T) {
	t.Parallel()
	pool := newUnreachablePool(t)
	_, cleanup, err := assembleInboxHandlerRealFromPool(pool, nil, envOnly(map[string]string{}))
	if err != nil {
		t.Fatalf("assembleInboxHandlerRealFromPool: %v", err)
	}
	// Must not panic even though no fake adapter was built.
	cleanup()
}
