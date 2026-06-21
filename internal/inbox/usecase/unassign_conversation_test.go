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

var unassignTestTime = time.Date(2026, 6, 21, 12, 0, 0, 0, time.UTC)

// --- fakes for the UnassignConversation ports -----------------------------

// fakeUnassignLedger records AppendUnassign calls. It does NOT mock the
// database — the postgres adapter binds the same contract; this exercises
// the use-case orchestration only.
type fakeUnassignLedger struct {
	mu       sync.Mutex
	appended []uuid.UUID // conversation ids, in order
	err      error
}

func (f *fakeUnassignLedger) AppendUnassign(_ context.Context, tenantID, conversationID uuid.UUID) (*inbox.Assignment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	f.appended = append(f.appended, conversationID)
	return inbox.HydrateAssignment(uuid.New(), tenantID, conversationID, uuid.Nil, unassignTestTime, inbox.LeadReasonUnassign), nil
}

func (f *fakeUnassignLedger) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.appended)
}

// fakeClearer records ClearAssignment calls and can fail on demand.
type fakeClearer struct {
	mu    sync.Mutex
	calls []uuid.UUID
	err   error
}

func (c *fakeClearer) ClearAssignment(_ context.Context, _, conversationID uuid.UUID) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err != nil {
		return c.err
	}
	c.calls = append(c.calls, conversationID)
	return nil
}

func (c *fakeClearer) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.calls)
}

// seedUnassignConv inserts a conversation with the given lead + lifecycle
// state into the in-memory repo and returns its id.
func seedUnassignConv(t *testing.T, repo *inMemoryRepo, tenantID uuid.UUID, assigned *uuid.UUID, state inbox.ConversationState) uuid.UUID {
	t.Helper()
	id := uuid.New()
	conv := inbox.HydrateConversation(id, tenantID, uuid.New(), "whatsapp", state, assigned, unassignTestTime, unassignTestTime)
	if err := repo.CreateConversation(context.Background(), conv); err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}
	return id
}

func newUnassign(t *testing.T, repo *inMemoryRepo, ledger *fakeUnassignLedger, clearer *fakeClearer) *usecase.UnassignConversation {
	t.Helper()
	uc, err := usecase.NewUnassignConversation(repo, ledger, clearer)
	if err != nil {
		t.Fatalf("NewUnassignConversation: %v", err)
	}
	return uc
}

// --- tests ----------------------------------------------------------------

func TestUnassignConversation_AssignedOpen_RecordsEventAndClears(t *testing.T) {
	repo := newInMemoryRepo()
	ledger := &fakeUnassignLedger{}
	clearer := &fakeClearer{}
	uc := newUnassign(t, repo, ledger, clearer)

	tenant := uuid.New()
	leader := uuid.New()
	conv := seedUnassignConv(t, repo, tenant, &leader, inbox.ConversationStateOpen)

	res, err := uc.Execute(context.Background(), usecase.UnassignConversationInput{TenantID: tenant, ConversationID: conv})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.AlreadyUnassigned {
		t.Errorf("AlreadyUnassigned = true, want false")
	}
	if ledger.count() != 1 {
		t.Errorf("ledger appends = %d, want 1", ledger.count())
	}
	if clearer.count() != 1 {
		t.Errorf("clearer calls = %d, want 1", clearer.count())
	}
}

func TestUnassignConversation_AlreadyUnassigned_NoOp(t *testing.T) {
	repo := newInMemoryRepo()
	ledger := &fakeUnassignLedger{}
	clearer := &fakeClearer{}
	uc := newUnassign(t, repo, ledger, clearer)

	tenant := uuid.New()
	conv := seedUnassignConv(t, repo, tenant, nil, inbox.ConversationStateOpen)

	res, err := uc.Execute(context.Background(), usecase.UnassignConversationInput{TenantID: tenant, ConversationID: conv})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !res.AlreadyUnassigned {
		t.Errorf("AlreadyUnassigned = false, want true")
	}
	// Idempotent no-op: an already-unassigned conversation must NOT append a
	// redundant ledger row or touch the cache.
	if ledger.count() != 0 {
		t.Errorf("ledger appends = %d, want 0 (no-op must not pollute the audit trail)", ledger.count())
	}
	if clearer.count() != 0 {
		t.Errorf("clearer calls = %d, want 0", clearer.count())
	}
}

func TestUnassignConversation_Closed_Rejected(t *testing.T) {
	tests := []struct {
		name     string
		assigned bool
	}{
		{"closed and assigned", true},
		{"closed and already unassigned", false},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			repo := newInMemoryRepo()
			ledger := &fakeUnassignLedger{}
			clearer := &fakeClearer{}
			uc := newUnassign(t, repo, ledger, clearer)

			tenant := uuid.New()
			var lead *uuid.UUID
			if tc.assigned {
				id := uuid.New()
				lead = &id
			}
			conv := seedUnassignConv(t, repo, tenant, lead, inbox.ConversationStateClosed)

			_, err := uc.Execute(context.Background(), usecase.UnassignConversationInput{TenantID: tenant, ConversationID: conv})
			if !errors.Is(err, inbox.ErrConversationClosed) {
				t.Fatalf("err = %v, want ErrConversationClosed", err)
			}
			// The close gate runs before any write, even on the no-op path.
			if ledger.count() != 0 || clearer.count() != 0 {
				t.Errorf("writes on closed conversation: ledger=%d clearer=%d, want 0/0", ledger.count(), clearer.count())
			}
		})
	}
}

func TestUnassignConversation_InputValidation(t *testing.T) {
	repo := newInMemoryRepo()
	ledger := &fakeUnassignLedger{}
	clearer := &fakeClearer{}
	uc := newUnassign(t, repo, ledger, clearer)

	tests := []struct {
		name string
		in   usecase.UnassignConversationInput
		want error
	}{
		{"nil tenant", usecase.UnassignConversationInput{TenantID: uuid.Nil, ConversationID: uuid.New()}, inbox.ErrInvalidTenant},
		{"nil conversation", usecase.UnassignConversationInput{TenantID: uuid.New(), ConversationID: uuid.Nil}, usecase.ErrNotFound},
		{"unknown conversation (IDOR)", usecase.UnassignConversationInput{TenantID: uuid.New(), ConversationID: uuid.New()}, usecase.ErrNotFound},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			_, err := uc.Execute(context.Background(), tc.in)
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestUnassignConversation_CrossTenant_IsNotFound(t *testing.T) {
	repo := newInMemoryRepo()
	uc := newUnassign(t, repo, &fakeUnassignLedger{}, &fakeClearer{})

	owner := uuid.New()
	leader := uuid.New()
	conv := seedUnassignConv(t, repo, owner, &leader, inbox.ConversationStateOpen)

	// A different tenant must not see (or unassign) another tenant's row.
	_, err := uc.Execute(context.Background(), usecase.UnassignConversationInput{TenantID: uuid.New(), ConversationID: conv})
	if !errors.Is(err, usecase.ErrNotFound) {
		t.Fatalf("cross-tenant err = %v, want ErrNotFound", err)
	}
}

func TestUnassignConversation_LedgerError_Propagates(t *testing.T) {
	repo := newInMemoryRepo()
	sentinel := errors.New("ledger boom")
	ledger := &fakeUnassignLedger{err: sentinel}
	clearer := &fakeClearer{}
	uc := newUnassign(t, repo, ledger, clearer)

	tenant := uuid.New()
	leader := uuid.New()
	conv := seedUnassignConv(t, repo, tenant, &leader, inbox.ConversationStateOpen)

	_, err := uc.Execute(context.Background(), usecase.UnassignConversationInput{TenantID: tenant, ConversationID: conv})
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want sentinel", err)
	}
	// Cache must not be cleared when the audit row failed to land.
	if clearer.count() != 0 {
		t.Errorf("clearer called despite ledger failure: %d", clearer.count())
	}
}

func TestUnassignConversation_ClearerError_Propagates(t *testing.T) {
	repo := newInMemoryRepo()
	sentinel := errors.New("clear boom")
	ledger := &fakeUnassignLedger{}
	clearer := &fakeClearer{err: sentinel}
	uc := newUnassign(t, repo, ledger, clearer)

	tenant := uuid.New()
	leader := uuid.New()
	conv := seedUnassignConv(t, repo, tenant, &leader, inbox.ConversationStateOpen)

	_, err := uc.Execute(context.Background(), usecase.UnassignConversationInput{TenantID: tenant, ConversationID: conv})
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want sentinel", err)
	}
	// The audit row WAS written before the cache clear failed — on retry the
	// clear converges (a second unassign event is harmless).
	if ledger.count() != 1 {
		t.Errorf("ledger appends = %d, want 1", ledger.count())
	}
}

func TestNewUnassignConversation_NilGuards(t *testing.T) {
	repo := newInMemoryRepo()
	ledger := &fakeUnassignLedger{}
	clearer := &fakeClearer{}
	tests := []struct {
		name    string
		conv    usecase.UnassignConversationInput
		build   func() (*usecase.UnassignConversation, error)
		wantErr bool
	}{
		{name: "nil reader", build: func() (*usecase.UnassignConversation, error) {
			return usecase.NewUnassignConversation(nil, ledger, clearer)
		}, wantErr: true},
		{name: "nil ledger", build: func() (*usecase.UnassignConversation, error) {
			return usecase.NewUnassignConversation(repo, nil, clearer)
		}, wantErr: true},
		{name: "nil clearer", build: func() (*usecase.UnassignConversation, error) {
			return usecase.NewUnassignConversation(repo, ledger, nil)
		}, wantErr: true},
		{name: "all wired", build: func() (*usecase.UnassignConversation, error) {
			return usecase.NewUnassignConversation(repo, ledger, clearer)
		}, wantErr: false},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			uc, err := tc.build()
			if tc.wantErr {
				if err == nil {
					t.Fatalf("err = nil, want non-nil")
				}
				return
			}
			if err != nil || uc == nil {
				t.Fatalf("err = %v, uc = %v, want a use case", err, uc)
			}
		})
	}
}

func TestMustNewUnassignConversation_PanicsOnNil(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatalf("MustNewUnassignConversation did not panic on nil reader")
		}
	}()
	_ = usecase.MustNewUnassignConversation(nil, &fakeUnassignLedger{}, &fakeClearer{})
}
