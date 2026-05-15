package usecase_test

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/contacts"
	contactsusecase "github.com/pericles-luz/crm/internal/contacts/usecase"
	"github.com/pericles-luz/crm/internal/inbox"
)

// inMemoryRepo is an in-memory implementation of inbox.Repository that
// satisfies the production contract closely enough for use-case tests.
// It does NOT mock the database — production wiring binds the same
// methods to the Postgres adapter; this exists only so the orchestration
// of the use case can be exercised without a live cluster.
type inMemoryRepo struct {
	mu            sync.Mutex
	conversations map[uuid.UUID]*inbox.Conversation
	messages      map[uuid.UUID]*inbox.Message
}

func newInMemoryRepo() *inMemoryRepo {
	return &inMemoryRepo{
		conversations: map[uuid.UUID]*inbox.Conversation{},
		messages:      map[uuid.UUID]*inbox.Message{},
	}
}

func (r *inMemoryRepo) CreateConversation(_ context.Context, c *inbox.Conversation) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.conversations[c.ID]; ok {
		return errors.New("inMemoryRepo: duplicate conversation id")
	}
	cp := *c
	r.conversations[c.ID] = &cp
	return nil
}

func (r *inMemoryRepo) GetConversation(_ context.Context, tenantID, conversationID uuid.UUID) (*inbox.Conversation, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	c, ok := r.conversations[conversationID]
	if !ok || c.TenantID != tenantID {
		return nil, inbox.ErrNotFound
	}
	cp := *c
	return &cp, nil
}

func (r *inMemoryRepo) FindOpenConversation(_ context.Context, tenantID, contactID uuid.UUID, channel string) (*inbox.Conversation, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, c := range r.conversations {
		if c.TenantID == tenantID && c.ContactID == contactID && c.Channel == channel && c.State == inbox.ConversationStateOpen {
			cp := *c
			return &cp, nil
		}
	}
	return nil, inbox.ErrNotFound
}

func (r *inMemoryRepo) SaveMessage(_ context.Context, m *inbox.Message) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.messages[m.ID]; ok {
		return errors.New("inMemoryRepo: duplicate message id")
	}
	cp := *m
	r.messages[m.ID] = &cp
	conv, ok := r.conversations[m.ConversationID]
	if !ok {
		return inbox.ErrNotFound
	}
	if m.CreatedAt.After(conv.LastMessageAt) {
		conv.LastMessageAt = m.CreatedAt
	}
	return nil
}

func (r *inMemoryRepo) UpdateMessage(_ context.Context, m *inbox.Message) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.messages[m.ID]; !ok {
		return inbox.ErrNotFound
	}
	cp := *m
	r.messages[m.ID] = &cp
	return nil
}

// FindMessageByChannelExternalID satisfies inbox.Repository. Tenant
// scope mirrors the Postgres adapter: rows owned by another tenant
// collapse to ErrNotFound.
func (r *inMemoryRepo) FindMessageByChannelExternalID(_ context.Context, tenantID uuid.UUID, channel, channelExternalID string) (*inbox.Message, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, m := range r.messages {
		if m.TenantID != tenantID || m.ChannelExternalID != channelExternalID {
			continue
		}
		conv, ok := r.conversations[m.ConversationID]
		if !ok || conv.Channel != channel {
			continue
		}
		cp := *m
		return &cp, nil
	}
	return nil, inbox.ErrNotFound
}

func (r *inMemoryRepo) messageCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.messages)
}

func (r *inMemoryRepo) conversationCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.conversations)
}

// inMemoryDedup is an in-memory implementation of
// inbox.InboundDedupRepository. Claim is atomic via the mutex.
type inMemoryDedup struct {
	mu      sync.Mutex
	claimed map[string]time.Time
	done    map[string]time.Time
}

func newInMemoryDedup() *inMemoryDedup {
	return &inMemoryDedup{claimed: map[string]time.Time{}, done: map[string]time.Time{}}
}

func (d *inMemoryDedup) Claim(_ context.Context, channel, externalID string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	key := channel + "|" + externalID
	if _, ok := d.claimed[key]; ok {
		return inbox.ErrInboundAlreadyProcessed
	}
	d.claimed[key] = time.Now()
	return nil
}

func (d *inMemoryDedup) MarkProcessed(_ context.Context, channel, externalID string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	key := channel + "|" + externalID
	if _, ok := d.claimed[key]; !ok {
		return inbox.ErrNotFound
	}
	d.done[key] = time.Now()
	return nil
}

// stubContactUpserter records the calls and returns a contact built
// for the first call's input. Subsequent calls return the same contact
// — matching the production idempotency contract.
type stubContactUpserter struct {
	mu      sync.Mutex
	created *contacts.Contact
	calls   int
}

func newStubContactUpserter() *stubContactUpserter {
	return &stubContactUpserter{}
}

func (s *stubContactUpserter) Execute(_ context.Context, in contactsusecase.Input) (contactsusecase.Result, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	if s.created == nil {
		c, err := contacts.New(in.TenantID, in.DisplayName)
		if err != nil {
			return contactsusecase.Result{}, err
		}
		if err := c.AddChannelIdentity(in.Channel, in.ExternalID); err != nil {
			return contactsusecase.Result{}, err
		}
		s.created = c
		return contactsusecase.Result{Contact: c, Created: true}, nil
	}
	return contactsusecase.Result{Contact: s.created, Created: false}, nil
}

// stubWalletDebitor accumulates a per-tenant balance. Reservations
// always succeed; charge is invoked with the supplied context.
// Refunds: if charge returns non-nil, the balance is left untouched.
type stubWalletDebitor struct {
	mu      sync.Mutex
	balance map[uuid.UUID]int64
	calls   []int64
}

func newStubWalletDebitor() *stubWalletDebitor {
	return &stubWalletDebitor{balance: map[uuid.UUID]int64{}}
}

func (w *stubWalletDebitor) Debit(ctx context.Context, tenantID uuid.UUID, cost int64, charge func(ctx context.Context) error) error {
	w.mu.Lock()
	w.calls = append(w.calls, cost)
	if cost > 0 && w.balance[tenantID] < cost {
		w.mu.Unlock()
		return errors.New("stubWalletDebitor: insufficient funds")
	}
	if cost > 0 {
		w.balance[tenantID] -= cost
	}
	w.mu.Unlock()
	if err := charge(ctx); err != nil {
		w.mu.Lock()
		if cost > 0 {
			w.balance[tenantID] += cost
		}
		w.mu.Unlock()
		return err
	}
	return nil
}

func (w *stubWalletDebitor) Calls() []int64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := make([]int64, len(w.calls))
	copy(out, w.calls)
	return out
}

func (w *stubWalletDebitor) Balance(tenantID uuid.UUID) int64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.balance[tenantID]
}

func (w *stubWalletDebitor) Credit(tenantID uuid.UUID, amount int64) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.balance[tenantID] += amount
}

// stubOutbound implements inbox.OutboundChannel by returning a fixed
// channelExternalID and recording calls. err, if set, is returned
// from SendMessage so failure paths can be tested.
type stubOutbound struct {
	mu                sync.Mutex
	channelExternalID string
	err               error
	calls             []inbox.OutboundMessage
}

func newStubOutbound(channelExternalID string) *stubOutbound {
	return &stubOutbound{channelExternalID: channelExternalID}
}

func (s *stubOutbound) SendMessage(_ context.Context, m inbox.OutboundMessage) (string, error) {
	s.mu.Lock()
	s.calls = append(s.calls, m)
	err := s.err
	s.mu.Unlock()
	if err != nil {
		return "", err
	}
	return s.channelExternalID, nil
}

func (s *stubOutbound) SetError(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.err = err
}

func (s *stubOutbound) Calls() []inbox.OutboundMessage {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]inbox.OutboundMessage, len(s.calls))
	copy(out, s.calls)
	return out
}
