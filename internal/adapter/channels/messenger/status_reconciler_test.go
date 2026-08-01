package messenger_test

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/adapter/channels/messenger"
	"github.com/pericles-luz/crm/internal/contacts"
	"github.com/pericles-luz/crm/internal/inbox"
)

// fakeStatusUpdater mirrors the WhatsApp package's test double
// (internal/adapter/channels/whatsapp/status_reconciler_test.go) — a
// deterministic in-memory inbox.MessageStatusUpdater whose monotonic
// state machine matches the real use case closely enough that test
// expectations track production behaviour.
type fakeStatusUpdater struct {
	mu    sync.Mutex
	state map[string]inbox.MessageStatus
	known map[string]bool
	calls []inbox.StatusUpdate
	err   error
}

func newFakeStatusUpdater() *fakeStatusUpdater {
	return &fakeStatusUpdater{state: map[string]inbox.MessageStatus{}, known: map[string]bool{}}
}

func (f *fakeStatusUpdater) Seed(mid string, initial inbox.MessageStatus) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.known[mid] = true
	f.state[mid] = initial
}

func (f *fakeStatusUpdater) FailWith(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.err = err
}

func (f *fakeStatusUpdater) Calls() []inbox.StatusUpdate {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]inbox.StatusUpdate, len(f.calls))
	copy(out, f.calls)
	return out
}

var statusRankForTest = map[inbox.MessageStatus]int{
	inbox.MessageStatusPending:   0,
	inbox.MessageStatusSent:      1,
	inbox.MessageStatusDelivered: 2,
	inbox.MessageStatusRead:      3,
}

func (f *fakeStatusUpdater) HandleStatus(_ context.Context, ev inbox.StatusUpdate) (inbox.StatusUpdateResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return inbox.StatusUpdateResult{}, f.err
	}
	f.calls = append(f.calls, ev)
	if !f.known[ev.ChannelExternalID] {
		return inbox.StatusUpdateResult{Outcome: inbox.StatusOutcomeUnknownMessage, NewStatus: ev.NewStatus}, nil
	}
	prev := f.state[ev.ChannelExternalID]
	if statusRankForTest[prev] >= statusRankForTest[ev.NewStatus] {
		return inbox.StatusUpdateResult{Outcome: inbox.StatusOutcomeNoop, PreviousStatus: prev, NewStatus: prev}, nil
	}
	f.state[ev.ChannelExternalID] = ev.NewStatus
	return inbox.StatusUpdateResult{Outcome: inbox.StatusOutcomeApplied, PreviousStatus: prev, NewStatus: ev.NewStatus}, nil
}

// fakeContactLookup / fakeConversationLookup are minimal, in-memory
// stand-ins for the narrow read ports deliverRead consumes.
type fakeContactLookup struct {
	byPSID map[string]*contacts.Contact
	err    error
}

func newFakeContactLookup() *fakeContactLookup {
	return &fakeContactLookup{byPSID: map[string]*contacts.Contact{}}
}

func (f *fakeContactLookup) Register(psid string, c *contacts.Contact) { f.byPSID[psid] = c }

func (f *fakeContactLookup) FindByChannelIdentity(_ context.Context, _ uuid.UUID, _, externalID string) (*contacts.Contact, error) {
	if f.err != nil {
		return nil, f.err
	}
	c, ok := f.byPSID[externalID]
	if !ok {
		return nil, contacts.ErrNotFound
	}
	return c, nil
}

type fakeConversationLookup struct {
	conv     *inbox.Conversation
	messages []*inbox.Message
	convErr  error
	listErr  error
}

func newFakeConversationLookup() *fakeConversationLookup { return &fakeConversationLookup{} }

func (f *fakeConversationLookup) FindOpenConversation(_ context.Context, _, _ uuid.UUID, _ string) (*inbox.Conversation, error) {
	if f.convErr != nil {
		return nil, f.convErr
	}
	return f.conv, nil
}

func (f *fakeConversationLookup) ListMessages(_ context.Context, _, _ uuid.UUID) ([]*inbox.Message, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.messages, nil
}

// newAdapterWithStatus builds an Adapter wired for the status-parity
// tests: the base inbound path (fakeInbox/fakeResolver/fakeFlag/fakeClock,
// mirroring newAdapter in handler_test.go) plus the status updater and
// read-receipt lookup options under test.
func newAdapterWithStatus(t *testing.T, r *fakeResolver, f *fakeFlag, c *fakeClock, su *fakeStatusUpdater, cl *fakeContactLookup, convl *fakeConversationLookup) *messenger.Adapter {
	t.Helper()
	cfg := messenger.Config{
		AppSecret:      testAppSecret,
		VerifyToken:    testVerifyToken,
		MaxBodyBytes:   1 << 20,
		PastWindow:     24 * time.Hour,
		FutureSkew:     time.Minute,
		DeliverTimeout: time.Second,
	}
	opts := []messenger.Option{messenger.WithClock(c)}
	if su != nil {
		opts = append(opts, messenger.WithStatusUpdater(su))
	}
	if cl != nil || convl != nil {
		opts = append(opts, messenger.WithReadReceiptLookup(cl, convl))
	}
	a, err := messenger.New(cfg, newFakeInbox(), r, f, opts...)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return a
}

func deliveryEnvelope(pageID, senderPSID string, mids []string, watermarkMs, entryTimeMs int64) []byte {
	b, _ := json.Marshal(map[string]any{
		"object": "page",
		"entry": []any{
			map[string]any{
				"id":   pageID,
				"time": entryTimeMs,
				"messaging": []any{
					map[string]any{
						"sender":    map[string]any{"id": senderPSID},
						"recipient": map[string]any{"id": pageID},
						"timestamp": entryTimeMs,
						"delivery":  map[string]any{"mids": mids, "watermark": watermarkMs},
					},
				},
			},
		},
	})
	return b
}

func readEnvelope(pageID, senderPSID string, watermarkMs, entryTimeMs int64) []byte {
	b, _ := json.Marshal(map[string]any{
		"object": "page",
		"entry": []any{
			map[string]any{
				"id":   pageID,
				"time": entryTimeMs,
				"messaging": []any{
					map[string]any{
						"sender":    map[string]any{"id": senderPSID},
						"recipient": map[string]any{"id": pageID},
						"timestamp": entryTimeMs,
						"read":      map[string]any{"watermark": watermarkMs},
					},
				},
			},
		},
	})
	return b
}

func TestDeliverDelivery_AdvancesEachMID(t *testing.T) {
	t.Parallel()
	tenant := uuid.New()
	r := newFakeResolver()
	r.Register(testPageID, tenant)
	f := newFakeFlag(true)
	su := newFakeStatusUpdater()
	su.Seed("mid-1", inbox.MessageStatusSent)
	su.Seed("mid-2", inbox.MessageStatusSent)
	a := newAdapterWithStatus(t, r, f, newFakeClock(fixedNow), su, nil, nil)

	tsMs := fixedNow.Add(-10 * time.Second).UnixMilli()
	body := deliveryEnvelope(testPageID, testPSID, []string{"mid-1", "mid-2"}, tsMs, tsMs)
	rec := doPost(t, a, body, sign(t, body))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d", rec.Code)
	}

	calls := su.Calls()
	if len(calls) != 2 {
		t.Fatalf("HandleStatus calls=%d, want 2 (body=%s)", len(calls), rec.Body.String())
	}
	for _, c := range calls {
		if c.NewStatus != inbox.MessageStatusDelivered {
			t.Errorf("call for %q status=%q, want delivered", c.ChannelExternalID, c.NewStatus)
		}
		if c.Channel != messenger.Channel {
			t.Errorf("call channel=%q, want %q", c.Channel, messenger.Channel)
		}
	}
}

func TestDeliverDelivery_UnwiredStatusUpdater_DropsCleanly(t *testing.T) {
	t.Parallel()
	tenant := uuid.New()
	r := newFakeResolver()
	r.Register(testPageID, tenant)
	f := newFakeFlag(true)
	a := newAdapterWithStatus(t, r, f, newFakeClock(fixedNow), nil, nil, nil)

	tsMs := fixedNow.Add(-10 * time.Second).UnixMilli()
	body := deliveryEnvelope(testPageID, testPSID, []string{"mid-1"}, tsMs, tsMs)
	rec := doPost(t, a, body, sign(t, body))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200 even when unwired (anti-enumeration ack)", rec.Code)
	}
}

func TestDeliverRead_AdvancesOnlyQualifyingOutboundMessages(t *testing.T) {
	t.Parallel()
	tenant := uuid.New()
	contactID := uuid.New()
	convID := uuid.New()
	r := newFakeResolver()
	r.Register(testPageID, tenant)
	f := newFakeFlag(true)

	su := newFakeStatusUpdater()
	su.Seed("mid-before", inbox.MessageStatusSent)
	su.Seed("mid-after", inbox.MessageStatusSent)

	cl := newFakeContactLookup()
	cl.Register(testPSID, &contacts.Contact{ID: contactID, TenantID: tenant})

	watermark := fixedNow.Add(-5 * time.Minute)
	convl := newFakeConversationLookup()
	convl.conv = &inbox.Conversation{ID: convID, TenantID: tenant, ContactID: contactID, Channel: messenger.Channel}
	convl.messages = []*inbox.Message{
		{Direction: inbox.MessageDirectionOut, ChannelExternalID: "mid-before", CreatedAt: watermark.Add(-time.Minute)}, // before watermark: qualifies
		{Direction: inbox.MessageDirectionOut, ChannelExternalID: "mid-after", CreatedAt: watermark.Add(time.Minute)},   // after watermark: must NOT advance
		{Direction: inbox.MessageDirectionIn, ChannelExternalID: "mid-inbound", CreatedAt: watermark.Add(-time.Minute)}, // inbound: never advances
		{Direction: inbox.MessageDirectionOut, ChannelExternalID: "", CreatedAt: watermark.Add(-time.Minute)},           // no channel id: skipped
	}

	a := newAdapterWithStatus(t, r, f, newFakeClock(fixedNow), su, cl, convl)

	tsMs := fixedNow.Add(-10 * time.Second).UnixMilli()
	body := readEnvelope(testPageID, testPSID, watermark.UnixMilli(), tsMs)
	rec := doPost(t, a, body, sign(t, body))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d", rec.Code)
	}

	calls := su.Calls()
	if len(calls) != 1 {
		t.Fatalf("HandleStatus calls=%d, want 1 (calls=%+v)", len(calls), calls)
	}
	if calls[0].ChannelExternalID != "mid-before" {
		t.Errorf("advanced mid=%q, want mid-before", calls[0].ChannelExternalID)
	}
	if calls[0].NewStatus != inbox.MessageStatusRead {
		t.Errorf("status=%q, want read", calls[0].NewStatus)
	}
}

func TestDeliverRead_UnknownContact_DropsCleanly(t *testing.T) {
	t.Parallel()
	tenant := uuid.New()
	r := newFakeResolver()
	r.Register(testPageID, tenant)
	f := newFakeFlag(true)
	su := newFakeStatusUpdater()
	cl := newFakeContactLookup() // no contact registered
	convl := newFakeConversationLookup()
	a := newAdapterWithStatus(t, r, f, newFakeClock(fixedNow), su, cl, convl)

	tsMs := fixedNow.Add(-10 * time.Second).UnixMilli()
	body := readEnvelope(testPageID, testPSID, tsMs, tsMs)
	rec := doPost(t, a, body, sign(t, body))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d", rec.Code)
	}
	if calls := su.Calls(); len(calls) != 0 {
		t.Fatalf("HandleStatus calls=%d, want 0 for an unknown contact", len(calls))
	}
}

func TestDeliverRead_UnwiredLookups_DropsCleanly(t *testing.T) {
	t.Parallel()
	tenant := uuid.New()
	r := newFakeResolver()
	r.Register(testPageID, tenant)
	f := newFakeFlag(true)
	su := newFakeStatusUpdater()
	a := newAdapterWithStatus(t, r, f, newFakeClock(fixedNow), su, nil, nil)

	tsMs := fixedNow.Add(-10 * time.Second).UnixMilli()
	body := readEnvelope(testPageID, testPSID, tsMs, tsMs)
	rec := doPost(t, a, body, sign(t, body))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d", rec.Code)
	}
	if calls := su.Calls(); len(calls) != 0 {
		t.Fatalf("HandleStatus calls=%d, want 0 when read-receipt lookups are unwired", len(calls))
	}
}

func TestDeliverDelivery_ReplayIsMonotonicNoop(t *testing.T) {
	t.Parallel()
	tenant := uuid.New()
	r := newFakeResolver()
	r.Register(testPageID, tenant)
	f := newFakeFlag(true)
	su := newFakeStatusUpdater()
	su.Seed("mid-1", inbox.MessageStatusRead) // already read
	a := newAdapterWithStatus(t, r, f, newFakeClock(fixedNow), su, nil, nil)

	tsMs := fixedNow.Add(-10 * time.Second).UnixMilli()
	body := deliveryEnvelope(testPageID, testPSID, []string{"mid-1"}, tsMs, tsMs)
	rec := doPost(t, a, body, sign(t, body))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d", rec.Code)
	}

	calls := su.Calls()
	if len(calls) != 1 {
		t.Fatalf("HandleStatus calls=%d, want 1", len(calls))
	}
	// The fake's monotonic check means the underlying state stays "read"
	// — verifying this call didn't regress it is the point of the test;
	// HandleStatus is still invoked (the reconciler doesn't pre-filter),
	// same replay-safe posture as WhatsApp's deliverStatus.
	su.mu.Lock()
	got := su.state["mid-1"]
	su.mu.Unlock()
	if got != inbox.MessageStatusRead {
		t.Fatalf("state regressed to %q, want read to stay read", got)
	}
}
