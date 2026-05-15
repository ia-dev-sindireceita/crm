package usecase_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/inbox"
	inboxusecase "github.com/pericles-luz/crm/internal/inbox/usecase"
)

// inMemoryLeadPolicy implements inboxusecase.TenantLeadPolicy for the
// F2-07.2 auto-attribution unit tests. It surfaces the same shape the
// production adapter returns (*uuid.UUID for the configured user, nil
// for "no default") and lets the test pin the lead user per tenant id
// without spinning up Postgres.
type inMemoryLeadPolicy struct {
	mu       sync.Mutex
	leadByID map[uuid.UUID]*uuid.UUID
	calls    int
	err      error
}

func newInMemoryLeadPolicy() *inMemoryLeadPolicy {
	return &inMemoryLeadPolicy{leadByID: map[uuid.UUID]*uuid.UUID{}}
}

func (r *inMemoryLeadPolicy) setLead(tenantID uuid.UUID, leadUserID *uuid.UUID) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.leadByID[tenantID] = leadUserID
}

func (r *inMemoryLeadPolicy) DefaultLeadUserID(_ context.Context, tenantID uuid.UUID) (*uuid.UUID, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	if r.err != nil {
		return nil, r.err
	}
	v, ok := r.leadByID[tenantID]
	if !ok {
		return nil, nil
	}
	if v == nil {
		return nil, nil
	}
	cp := *v
	return &cp, nil
}

func (r *inMemoryLeadPolicy) callCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

// inMemoryAssignmentRepo implements inbox.AssignmentRepository for the
// F2-07.2 tests. It records the rows the use-case appends so the test
// can assert (a) the ordering of the AppendHistory call against the
// CreateConversation call and (b) the (conversationID, userID, reason)
// tuple the use-case produced.
type inMemoryAssignmentRepo struct {
	mu      sync.Mutex
	history []*inbox.Assignment
	now     time.Time
	err     error
}

func newInMemoryAssignmentRepo() *inMemoryAssignmentRepo {
	return &inMemoryAssignmentRepo{now: time.Date(2026, 5, 15, 12, 0, 0, 0, time.UTC)}
}

func (r *inMemoryAssignmentRepo) AppendHistory(
	_ context.Context,
	tenantID, conversationID, userID uuid.UUID,
	reason inbox.LeadReason,
) (*inbox.Assignment, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.err != nil {
		return nil, r.err
	}
	a := inbox.HydrateAssignment(uuid.New(), tenantID, conversationID, userID, r.now, reason)
	r.history = append(r.history, a)
	return a, nil
}

func (r *inMemoryAssignmentRepo) LatestAssignment(_ context.Context, tenantID, conversationID uuid.UUID) (*inbox.Assignment, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := len(r.history) - 1; i >= 0; i-- {
		a := r.history[i]
		if a.TenantID == tenantID && a.ConversationID == conversationID {
			cp := *a
			return &cp, nil
		}
	}
	return nil, inbox.ErrNotFound
}

func (r *inMemoryAssignmentRepo) ListHistory(_ context.Context, tenantID, conversationID uuid.UUID) ([]*inbox.Assignment, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []*inbox.Assignment
	for _, a := range r.history {
		if a.TenantID == tenantID && a.ConversationID == conversationID {
			cp := *a
			out = append(out, &cp)
		}
	}
	return out, nil
}

func (r *inMemoryAssignmentRepo) appendCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.history)
}

// TestNewReceiveInboundWithLeadership_RejectsNilLeadershipDeps covers
// the explicit nil-guard branches on the production constructor.
func TestNewReceiveInboundWithLeadership_RejectsNilLeadershipDeps(t *testing.T) {
	repo := newInMemoryRepo()
	dedup := newInMemoryDedup()
	contactsU := newStubContactUpserter()
	leadPolicy := newInMemoryLeadPolicy()
	assignments := newInMemoryAssignmentRepo()

	// Nil lead policy.
	if _, err := inboxusecase.NewReceiveInboundWithLeadership(repo, dedup, contactsU, nil, assignments); err == nil {
		t.Error("nil leadPolicy: err = nil, want construction error")
	}
	// Nil assignments repo.
	if _, err := inboxusecase.NewReceiveInboundWithLeadership(repo, dedup, contactsU, leadPolicy, nil); err == nil {
		t.Error("nil assignments: err = nil, want construction error")
	}
	// Nil repo propagates through to the base constructor.
	if _, err := inboxusecase.NewReceiveInboundWithLeadership(nil, dedup, contactsU, leadPolicy, assignments); err == nil {
		t.Error("nil repo: err = nil, want construction error from base ctor")
	}
}

// TestMustNewReceiveInboundWithLeadership_PanicsOnNil mirrors the
// MustNewReceiveInbound panic check for the leadership constructor.
func TestMustNewReceiveInboundWithLeadership_PanicsOnNil(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Error("MustNewReceiveInboundWithLeadership did not panic on nil deps")
		}
	}()
	inboxusecase.MustNewReceiveInboundWithLeadership(nil, nil, nil, nil, nil)
}

// TestReceiveInbound_AssignsDefaultLead_OnFirstConversation is the
// F2-07.2 happy-path: tenant.default_lead_user_id is set, a new
// conversation is created, an assignment_history row is appended with
// reason='lead', and the conversation surfaces the configured user via
// AssignedUserID + Lead().
func TestReceiveInbound_AssignsDefaultLead_OnFirstConversation(t *testing.T) {
	repo := newInMemoryRepo()
	dedup := newInMemoryDedup()
	contactsU := newStubContactUpserter()
	leadPolicy := newInMemoryLeadPolicy()
	assignments := newInMemoryAssignmentRepo()

	tenantID := uuid.New()
	leadUserID := uuid.New()
	leadPolicy.setLead(tenantID, &leadUserID)

	u := inboxusecase.MustNewReceiveInboundWithLeadership(repo, dedup, contactsU, leadPolicy, assignments)
	res, err := u.Execute(context.Background(), inbox.InboundEvent{
		TenantID:          tenantID,
		Channel:           "whatsapp",
		ChannelExternalID: "wamid.lead.1",
		SenderExternalID:  "+5511999990001",
		SenderDisplayName: "Alice",
		Body:              "hello",
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Conversation == nil {
		t.Fatal("Conversation = nil")
	}
	if res.Conversation.AssignedUserID == nil || *res.Conversation.AssignedUserID != leadUserID {
		t.Fatalf("AssignedUserID = %v, want %v", res.Conversation.AssignedUserID, leadUserID)
	}
	lead := res.Conversation.Lead()
	if lead == nil {
		t.Fatal("Lead() = nil, want assigned")
	}
	if lead.UserID != leadUserID {
		t.Errorf("Lead.UserID = %v, want %v", lead.UserID, leadUserID)
	}
	if lead.Reason != inbox.LeadReasonLead {
		t.Errorf("Lead.Reason = %q, want %q", lead.Reason, inbox.LeadReasonLead)
	}
	if assignments.appendCount() != 1 {
		t.Errorf("assignment_history rows = %d, want 1", assignments.appendCount())
	}
	if leadPolicy.callCount() != 1 {
		t.Errorf("lead policy calls = %d, want 1", leadPolicy.callCount())
	}
}

// TestReceiveInbound_NoDefaultLead_KeepsConversationUnassigned covers
// the AC bullet "se ausente, fica sem líder": tenant exists but
// DefaultLeadUserID is nil, so no assignment_history row is created and
// AssignedUserID stays nil ("sem líder" in the UI).
func TestReceiveInbound_NoDefaultLead_KeepsConversationUnassigned(t *testing.T) {
	repo := newInMemoryRepo()
	dedup := newInMemoryDedup()
	contactsU := newStubContactUpserter()
	leadPolicy := newInMemoryLeadPolicy()
	assignments := newInMemoryAssignmentRepo()

	tenantID := uuid.New()
	leadPolicy.setLead(tenantID, nil)

	u := inboxusecase.MustNewReceiveInboundWithLeadership(repo, dedup, contactsU, leadPolicy, assignments)
	res, err := u.Execute(context.Background(), inbox.InboundEvent{
		TenantID:          tenantID,
		Channel:           "whatsapp",
		ChannelExternalID: "wamid.nolead.1",
		SenderExternalID:  "+5511999990002",
		SenderDisplayName: "Bob",
		Body:              "hello",
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Conversation.AssignedUserID != nil {
		t.Errorf("AssignedUserID = %v, want nil", res.Conversation.AssignedUserID)
	}
	if res.Conversation.Lead() != nil {
		t.Errorf("Lead() = %+v, want nil", res.Conversation.Lead())
	}
	if assignments.appendCount() != 0 {
		t.Errorf("assignment_history rows = %d, want 0", assignments.appendCount())
	}
}

// TestReceiveInbound_DefaultLead_OnlyAppliesToNewConversation covers
// the contract that the auto-attribution runs only when a conversation
// is freshly created. A subsequent inbound event that reuses the open
// conversation MUST NOT add a second 'lead' row — that would corrupt
// the assignment_history ledger.
func TestReceiveInbound_DefaultLead_OnlyAppliesToNewConversation(t *testing.T) {
	repo := newInMemoryRepo()
	dedup := newInMemoryDedup()
	contactsU := newStubContactUpserter()
	leadPolicy := newInMemoryLeadPolicy()
	assignments := newInMemoryAssignmentRepo()

	tenantID := uuid.New()
	leadUserID := uuid.New()
	leadPolicy.setLead(tenantID, &leadUserID)

	u := inboxusecase.MustNewReceiveInboundWithLeadership(repo, dedup, contactsU, leadPolicy, assignments)
	ctx := context.Background()
	if _, err := u.Execute(ctx, inbox.InboundEvent{
		TenantID: tenantID, Channel: "whatsapp",
		ChannelExternalID: "wamid.same.1", SenderExternalID: "+5511999990003",
		SenderDisplayName: "Carol", Body: "first",
	}); err != nil {
		t.Fatalf("first Execute: %v", err)
	}
	if _, err := u.Execute(ctx, inbox.InboundEvent{
		TenantID: tenantID, Channel: "whatsapp",
		ChannelExternalID: "wamid.same.2", SenderExternalID: "+5511999990003",
		SenderDisplayName: "Carol", Body: "second",
	}); err != nil {
		t.Fatalf("second Execute: %v", err)
	}

	if assignments.appendCount() != 1 {
		t.Errorf("assignment_history rows after 2nd event = %d, want 1", assignments.appendCount())
	}
	if leadPolicy.callCount() != 1 {
		t.Errorf("lead policy calls = %d, want 1 (reused conversation must skip lookup)", leadPolicy.callCount())
	}
}

// TestReceiveInbound_LeadPolicyError_Propagates makes sure a transient
// tenant-policy lookup failure does not silently land the conversation
// unassigned. The use-case returns the error so the carrier adapter
// can retry.
func TestReceiveInbound_LeadPolicyError_Propagates(t *testing.T) {
	repo := newInMemoryRepo()
	dedup := newInMemoryDedup()
	contactsU := newStubContactUpserter()
	leadPolicy := newInMemoryLeadPolicy()
	assignments := newInMemoryAssignmentRepo()
	leadPolicy.err = errors.New("db timeout")

	u := inboxusecase.MustNewReceiveInboundWithLeadership(repo, dedup, contactsU, leadPolicy, assignments)
	_, err := u.Execute(context.Background(), inbox.InboundEvent{
		TenantID: uuid.New(), Channel: "whatsapp",
		ChannelExternalID: "wamid.err.1", SenderExternalID: "+5511999990004",
		SenderDisplayName: "Dee", Body: "boom",
	})
	if err == nil {
		t.Fatal("err = nil, want propagated tenant lookup error")
	}
}

// TestReceiveInbound_AssignmentRepoError_Propagates: AppendHistory failed
// after CreateConversation succeeded. We surface the error so the carrier
// retries — at-least-once is the carrier contract and the dedup ledger
// makes the retry safe.
func TestReceiveInbound_AssignmentRepoError_Propagates(t *testing.T) {
	repo := newInMemoryRepo()
	dedup := newInMemoryDedup()
	contactsU := newStubContactUpserter()
	leadPolicy := newInMemoryLeadPolicy()
	assignments := newInMemoryAssignmentRepo()
	assignments.err = errors.New("history insert failed")

	tenantID := uuid.New()
	leadUserID := uuid.New()
	leadPolicy.setLead(tenantID, &leadUserID)

	u := inboxusecase.MustNewReceiveInboundWithLeadership(repo, dedup, contactsU, leadPolicy, assignments)
	_, err := u.Execute(context.Background(), inbox.InboundEvent{
		TenantID: tenantID, Channel: "whatsapp",
		ChannelExternalID: "wamid.histerr.1", SenderExternalID: "+5511999990005",
		SenderDisplayName: "Eve", Body: "x",
	})
	if err == nil {
		t.Fatal("err = nil, want propagated AppendHistory error")
	}
}

// TestReceiveInbound_LegacyConstructor_SkipsLeadership confirms that
// NewReceiveInbound (without leadership ports) still works and yields
// an unassigned conversation. This is the back-compat guarantee for
// the existing test suite and any composition root that has not been
// upgraded yet.
func TestReceiveInbound_LegacyConstructor_SkipsLeadership(t *testing.T) {
	repo := newInMemoryRepo()
	dedup := newInMemoryDedup()
	contactsU := newStubContactUpserter()
	u := inboxusecase.MustNewReceiveInbound(repo, dedup, contactsU)
	res, err := u.Execute(context.Background(), inbox.InboundEvent{
		TenantID: uuid.New(), Channel: "whatsapp",
		ChannelExternalID: "wamid.legacy.1", SenderExternalID: "+5511999990006",
		SenderDisplayName: "Frank", Body: "hi",
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Conversation.AssignedUserID != nil {
		t.Errorf("AssignedUserID = %v, want nil (legacy constructor)", res.Conversation.AssignedUserID)
	}
}
