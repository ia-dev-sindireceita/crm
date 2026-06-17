package usecase_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/inbox"
	"github.com/pericles-luz/crm/internal/inbox/usecase"
)

// --- fakes for the AssignConversation ports -------------------------------

// fakeLedger is an in-memory assignment_history ledger. It records appended
// rows and serves LatestAssignment from the last appended row per
// conversation. It does NOT mock the database — the postgres adapter binds
// the same contract; this exercises the use-case orchestration only.
type fakeLedger struct {
	mu        sync.Mutex
	latest    map[uuid.UUID]*inbox.Assignment
	appended  []*inbox.Assignment
	appendErr error
	now       time.Time
}

func newFakeLedger() *fakeLedger {
	return &fakeLedger{
		latest: map[uuid.UUID]*inbox.Assignment{},
		now:    time.Date(2026, 6, 16, 12, 0, 0, 0, time.UTC),
	}
}

// seedLatest pre-loads a current lead for a conversation.
func (f *fakeLedger) seedLatest(tenantID, convID, userID uuid.UUID, reason inbox.LeadReason) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.latest[convID] = inbox.HydrateAssignment(uuid.New(), tenantID, convID, userID, f.now, reason)
}

func (f *fakeLedger) LatestAssignment(_ context.Context, _, conversationID uuid.UUID) (*inbox.Assignment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	a, ok := f.latest[conversationID]
	if !ok {
		return nil, inbox.ErrNotFound
	}
	cp := *a
	return &cp, nil
}

func (f *fakeLedger) AppendHistory(_ context.Context, tenantID, conversationID, userID uuid.UUID, reason inbox.LeadReason) (*inbox.Assignment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.appendErr != nil {
		return nil, f.appendErr
	}
	if !reason.Valid() {
		return nil, inbox.ErrInvalidLeadReason
	}
	a := inbox.HydrateAssignment(uuid.New(), tenantID, conversationID, userID, f.now, reason)
	f.appended = append(f.appended, a)
	f.latest[conversationID] = a
	cp := *a
	return &cp, nil
}

func (f *fakeLedger) appendCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.appended)
}

// fakeLeadCache records SetConversationLead calls and can fail on demand.
type fakeLeadCache struct {
	mu    sync.Mutex
	calls []uuid.UUID // target user ids, in order
	err   error
}

func (c *fakeLeadCache) SetConversationLead(_ context.Context, _, _, userID uuid.UUID) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err != nil {
		return c.err
	}
	c.calls = append(c.calls, userID)
	return nil
}

func (c *fakeLeadCache) callCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.calls)
}

// fakeGate models the attendant role/tenant gate: only (tenant,user) pairs
// added via allow report assignable. err short-circuits with a failure.
type fakeGate struct {
	mu      sync.Mutex
	allowed map[string]struct{}
	err     error
}

func newFakeGate() *fakeGate {
	return &fakeGate{allowed: map[string]struct{}{}}
}

func (g *fakeGate) allow(tenantID, userID uuid.UUID) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.allowed[tenantID.String()+"|"+userID.String()] = struct{}{}
}

func (g *fakeGate) IsAssignable(_ context.Context, tenantID, userID uuid.UUID) (bool, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.err != nil {
		return false, g.err
	}
	_, ok := g.allowed[tenantID.String()+"|"+userID.String()]
	return ok, nil
}

// seedOpenConversation inserts an open conversation into the in-memory repo.
func seedOpenConversation(t *testing.T, repo *inMemoryRepo, tenantID uuid.UUID) uuid.UUID {
	t.Helper()
	conv, err := inbox.NewConversation(tenantID, uuid.New(), "whatsapp")
	if err != nil {
		t.Fatalf("NewConversation: %v", err)
	}
	if err := repo.CreateConversation(context.Background(), conv); err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}
	return conv.ID
}

// --- tests ----------------------------------------------------------------

func TestAssignConversation_NewAssignment_Manual(t *testing.T) {
	repo := newInMemoryRepo()
	ledger := newFakeLedger()
	cache := &fakeLeadCache{}
	gate := newFakeGate()
	tenantID := uuid.New()
	target := uuid.New()
	convID := seedOpenConversation(t, repo, tenantID)
	gate.allow(tenantID, target)

	uc := usecase.MustNewAssignConversation(repo, ledger, cache, gate)
	res, err := uc.Execute(context.Background(), usecase.AssignConversationInput{
		TenantID:       tenantID,
		ConversationID: convID,
		TargetUserID:   target,
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Assignment == nil {
		t.Fatal("Assignment is nil")
	}
	if res.Assignment.Reason != inbox.LeadReasonManual {
		t.Errorf("reason = %q, want manual", res.Assignment.Reason)
	}
	if res.Assignment.UserID != target {
		t.Errorf("UserID = %v, want %v", res.Assignment.UserID, target)
	}
	if ledger.appendCount() != 1 {
		t.Errorf("append count = %d, want 1", ledger.appendCount())
	}
	if cache.callCount() != 1 {
		t.Errorf("lead-cache writes = %d, want 1", cache.callCount())
	}
}

func TestAssignConversation_Reassign_WhenLeadExists(t *testing.T) {
	repo := newInMemoryRepo()
	ledger := newFakeLedger()
	cache := &fakeLeadCache{}
	gate := newFakeGate()
	tenantID := uuid.New()
	oldLead := uuid.New()
	target := uuid.New()
	convID := seedOpenConversation(t, repo, tenantID)
	ledger.seedLatest(tenantID, convID, oldLead, inbox.LeadReasonLead)
	gate.allow(tenantID, target)

	uc := usecase.MustNewAssignConversation(repo, ledger, cache, gate)
	res, err := uc.Execute(context.Background(), usecase.AssignConversationInput{
		TenantID:       tenantID,
		ConversationID: convID,
		TargetUserID:   target,
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Assignment.Reason != inbox.LeadReasonReassign {
		t.Errorf("reason = %q, want reassign", res.Assignment.Reason)
	}
}

func TestAssignConversation_IdempotentSameLead(t *testing.T) {
	repo := newInMemoryRepo()
	ledger := newFakeLedger()
	cache := &fakeLeadCache{}
	gate := newFakeGate()
	tenantID := uuid.New()
	target := uuid.New()
	convID := seedOpenConversation(t, repo, tenantID)
	ledger.seedLatest(tenantID, convID, target, inbox.LeadReasonManual)
	gate.allow(tenantID, target)

	uc := usecase.MustNewAssignConversation(repo, ledger, cache, gate)
	_, err := uc.Execute(context.Background(), usecase.AssignConversationInput{
		TenantID:       tenantID,
		ConversationID: convID,
		TargetUserID:   target,
	})
	if !errors.Is(err, inbox.ErrAlreadyAssigned) {
		t.Fatalf("err = %v, want ErrAlreadyAssigned", err)
	}
	if ledger.appendCount() != 0 {
		t.Errorf("no-op should not append, got %d", ledger.appendCount())
	}
	if cache.callCount() != 0 {
		t.Errorf("no-op should not touch lead cache, got %d", cache.callCount())
	}
}

func TestAssignConversation_RejectsNonAssignableTarget(t *testing.T) {
	repo := newInMemoryRepo()
	ledger := newFakeLedger()
	cache := &fakeLeadCache{}
	gate := newFakeGate() // target NOT allowed → cross-tenant / wrong-role posture
	tenantID := uuid.New()
	convID := seedOpenConversation(t, repo, tenantID)

	uc := usecase.MustNewAssignConversation(repo, ledger, cache, gate)
	_, err := uc.Execute(context.Background(), usecase.AssignConversationInput{
		TenantID:       tenantID,
		ConversationID: convID,
		TargetUserID:   uuid.New(),
	})
	if !errors.Is(err, inbox.ErrUserNotAssignable) {
		t.Fatalf("err = %v, want ErrUserNotAssignable", err)
	}
	if ledger.appendCount() != 0 || cache.callCount() != 0 {
		t.Errorf("rejected assignment must not write: appends=%d cache=%d", ledger.appendCount(), cache.callCount())
	}
}

func TestAssignConversation_ConversationNotFound(t *testing.T) {
	repo := newInMemoryRepo()
	ledger := newFakeLedger()
	gate := newFakeGate()
	tenantID := uuid.New()
	target := uuid.New()
	gate.allow(tenantID, target)

	uc := usecase.MustNewAssignConversation(repo, ledger, &fakeLeadCache{}, gate)
	_, err := uc.Execute(context.Background(), usecase.AssignConversationInput{
		TenantID:       tenantID,
		ConversationID: uuid.New(), // never seeded
		TargetUserID:   target,
	})
	if !errors.Is(err, inbox.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestAssignConversation_ClosedConversation(t *testing.T) {
	repo := newInMemoryRepo()
	ledger := newFakeLedger()
	gate := newFakeGate()
	tenantID := uuid.New()
	target := uuid.New()
	gate.allow(tenantID, target)

	conv, err := inbox.NewConversation(tenantID, uuid.New(), "whatsapp")
	if err != nil {
		t.Fatalf("NewConversation: %v", err)
	}
	conv.Close()
	if err := repo.CreateConversation(context.Background(), conv); err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}

	uc := usecase.MustNewAssignConversation(repo, ledger, &fakeLeadCache{}, gate)
	_, err = uc.Execute(context.Background(), usecase.AssignConversationInput{
		TenantID:       tenantID,
		ConversationID: conv.ID,
		TargetUserID:   target,
	})
	if !errors.Is(err, inbox.ErrConversationClosed) {
		t.Fatalf("err = %v, want ErrConversationClosed", err)
	}
	if ledger.appendCount() != 0 {
		t.Errorf("closed conversation must not append, got %d", ledger.appendCount())
	}
}

func TestAssignConversation_InputValidation(t *testing.T) {
	repo := newInMemoryRepo()
	gate := newFakeGate()
	uc := usecase.MustNewAssignConversation(repo, newFakeLedger(), &fakeLeadCache{}, gate)
	convID := seedOpenConversation(t, repo, uuid.New())

	tests := []struct {
		name string
		in   usecase.AssignConversationInput
		want error
	}{
		{"nil tenant", usecase.AssignConversationInput{ConversationID: convID, TargetUserID: uuid.New()}, inbox.ErrInvalidTenant},
		{"nil conversation", usecase.AssignConversationInput{TenantID: uuid.New(), TargetUserID: uuid.New()}, inbox.ErrNotFound},
		{"nil target", usecase.AssignConversationInput{TenantID: uuid.New(), ConversationID: convID}, inbox.ErrInvalidAssignee},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := uc.Execute(context.Background(), tc.in)
			if !errors.Is(err, tc.want) {
				t.Errorf("err = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestAssignConversation_PortErrorsPropagate(t *testing.T) {
	tenantID := uuid.New()
	target := uuid.New()
	sentinel := errors.New("boom")

	t.Run("gate error", func(t *testing.T) {
		repo := newInMemoryRepo()
		convID := seedOpenConversation(t, repo, tenantID)
		gate := newFakeGate()
		gate.err = sentinel
		uc := usecase.MustNewAssignConversation(repo, newFakeLedger(), &fakeLeadCache{}, gate)
		_, err := uc.Execute(context.Background(), usecase.AssignConversationInput{TenantID: tenantID, ConversationID: convID, TargetUserID: target})
		if !errors.Is(err, sentinel) {
			t.Errorf("err = %v, want sentinel", err)
		}
	})

	t.Run("append error", func(t *testing.T) {
		repo := newInMemoryRepo()
		convID := seedOpenConversation(t, repo, tenantID)
		gate := newFakeGate()
		gate.allow(tenantID, target)
		ledger := newFakeLedger()
		ledger.appendErr = sentinel
		uc := usecase.MustNewAssignConversation(repo, ledger, &fakeLeadCache{}, gate)
		_, err := uc.Execute(context.Background(), usecase.AssignConversationInput{TenantID: tenantID, ConversationID: convID, TargetUserID: target})
		if !errors.Is(err, sentinel) {
			t.Errorf("err = %v, want sentinel", err)
		}
	})

	t.Run("lead-cache error", func(t *testing.T) {
		repo := newInMemoryRepo()
		convID := seedOpenConversation(t, repo, tenantID)
		gate := newFakeGate()
		gate.allow(tenantID, target)
		cache := &fakeLeadCache{err: sentinel}
		uc := usecase.MustNewAssignConversation(repo, newFakeLedger(), cache, gate)
		_, err := uc.Execute(context.Background(), usecase.AssignConversationInput{TenantID: tenantID, ConversationID: convID, TargetUserID: target})
		if !errors.Is(err, sentinel) {
			t.Errorf("err = %v, want sentinel", err)
		}
	})
}

func TestNewAssignConversation_RejectsNilDeps(t *testing.T) {
	repo := newInMemoryRepo()
	ledger := newFakeLedger()
	cache := &fakeLeadCache{}
	gate := newFakeGate()
	cases := []struct {
		name string
		r    usecaseConvReader
		l    usecaseLedger
		c    inbox.ConversationLeadStore
		g    usecaseGate
	}{
		{"nil reader", nil, ledger, cache, gate},
		{"nil ledger", repo, nil, cache, gate},
		{"nil cache", repo, ledger, nil, gate},
		{"nil gate", repo, ledger, cache, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := usecase.NewAssignConversation(tc.r, tc.l, tc.c, tc.g); err == nil {
				t.Error("want error for nil dependency")
			}
		})
	}
}

func TestMustNewAssignConversation_PanicsOnNil(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("expected panic")
		}
	}()
	usecase.MustNewAssignConversation(nil, nil, nil, nil)
}

// Local aliases mirroring the use-case's unexported port shapes so the
// nil-dependency table can pass typed nils without importing unexported
// names. They are structurally identical to the production interfaces.
type usecaseConvReader interface {
	GetConversation(ctx context.Context, tenantID, conversationID uuid.UUID) (*inbox.Conversation, error)
}
type usecaseLedger interface {
	LatestAssignment(ctx context.Context, tenantID, conversationID uuid.UUID) (*inbox.Assignment, error)
	AppendHistory(ctx context.Context, tenantID, conversationID, userID uuid.UUID, reason inbox.LeadReason) (*inbox.Assignment, error)
}
type usecaseGate interface {
	IsAssignable(ctx context.Context, tenantID, userID uuid.UUID) (bool, error)
}
