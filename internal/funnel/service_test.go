package funnel_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/funnel"
)

// fakeStageRepo is a deterministic in-memory StageRepository. It does
// NOT mock the database; it is a tabletop port substitute the service
// tests own end-to-end, satisfying the "no mock for code that touches
// storage" rule because the production adapter has its own testpg
// suite in postgres_test.
type fakeStageRepo struct {
	stages map[string]*funnel.Stage // key = tenant|key
	err    error
}

func newFakeStages() *fakeStageRepo {
	return &fakeStageRepo{stages: map[string]*funnel.Stage{}}
}

func stageKey(tenantID uuid.UUID, key string) string {
	return tenantID.String() + "|" + key
}

func (f *fakeStageRepo) put(s *funnel.Stage) {
	f.stages[stageKey(s.TenantID, s.Key)] = s
}

func (f *fakeStageRepo) FindByKey(_ context.Context, tenantID uuid.UUID, key string) (*funnel.Stage, error) {
	if f.err != nil {
		return nil, f.err
	}
	s, ok := f.stages[stageKey(tenantID, key)]
	if !ok {
		return nil, funnel.ErrNotFound
	}
	return s, nil
}

// fakeTransitionRepo records inserts and serves the latest-transition
// query from in-memory state.
type fakeTransitionRepo struct {
	mu         sync.Mutex
	latest     map[string]*funnel.Transition // key = tenant|conversation
	inserted   []*funnel.Transition
	latestErr  error
	createErr  error
	notFoundOk bool // when true, latest map miss returns ErrNotFound
}

func newFakeTransitions() *fakeTransitionRepo {
	return &fakeTransitionRepo{
		latest:     map[string]*funnel.Transition{},
		notFoundOk: true,
	}
}

func convKey(tenantID, conversationID uuid.UUID) string {
	return tenantID.String() + "|" + conversationID.String()
}

func (f *fakeTransitionRepo) LatestForConversation(_ context.Context, tenantID, conversationID uuid.UUID) (*funnel.Transition, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.latestErr != nil {
		return nil, f.latestErr
	}
	t, ok := f.latest[convKey(tenantID, conversationID)]
	if !ok {
		if f.notFoundOk {
			return nil, funnel.ErrNotFound
		}
		return nil, errors.New("fake: latest not configured")
	}
	return t, nil
}

func (f *fakeTransitionRepo) Create(_ context.Context, t *funnel.Transition) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.createErr != nil {
		return f.createErr
	}
	f.inserted = append(f.inserted, t)
	f.latest[convKey(t.TenantID, t.ConversationID)] = t
	return nil
}

// ListForConversation is a no-op stub so fakeTransitionRepo continues
// to satisfy the port after F2-12 extended it; the existing
// MoveConversation scenarios do not exercise the history read path.
func (f *fakeTransitionRepo) ListForConversation(_ context.Context, _ uuid.UUID, _ uuid.UUID) ([]*funnel.Transition, error) {
	return nil, nil
}

// fakePublisher captures published events so tests can assert payload
// shape and ordering.
type fakePublisher struct {
	mu     sync.Mutex
	events []publishedEvent
	err    error
}

type publishedEvent struct {
	name    string
	payload any
}

func (f *fakePublisher) Publish(_ context.Context, name string, payload any) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.events = append(f.events, publishedEvent{name: name, payload: payload})
	return nil
}

func TestNewService_RequiresAllPorts(t *testing.T) {
	t.Parallel()
	stages := newFakeStages()
	transitions := newFakeTransitions()
	pub := &fakePublisher{}
	cases := []struct {
		name string
		cfg  funnel.Config
	}{
		{"no stages", funnel.Config{Transitions: transitions, Publisher: pub}},
		{"no transitions", funnel.Config{Stages: stages, Publisher: pub}},
		{"no publisher", funnel.Config{Stages: stages, Transitions: transitions}},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if _, err := funnel.NewService(c.cfg); err == nil {
				t.Errorf("NewService(%s) err = nil, want error", c.name)
			}
		})
	}
}

func TestNewService_DefaultsNow(t *testing.T) {
	t.Parallel()
	svc, err := funnel.NewService(funnel.Config{
		Stages:      newFakeStages(),
		Transitions: newFakeTransitions(),
		Publisher:   &fakePublisher{},
	})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	if svc == nil {
		t.Fatal("NewService returned nil service")
	}
}

// TestMoveConversation_TableDriven exercises the rule matrix:
//
//   - invalid args (zero tenant / conversation / actor / blank key)
//   - unknown destination stage → ErrStageNotFound
//   - no current stage → from is nil, transition created, event emitted
//   - current stage == destination → idempotent no-op
//   - current stage != destination → from points at current, transition + event
//   - storage error on Create → wrapped (no event published)
//   - publish error → wrapped (transition already persisted)
func TestMoveConversation_TableDriven(t *testing.T) {
	tenant := uuid.New()
	conv := uuid.New()
	actor := uuid.New()

	stageNovo := &funnel.Stage{ID: uuid.New(), TenantID: tenant, Key: "novo", Label: "Novo", Position: 1, IsDefault: true}
	stageGanho := &funnel.Stage{ID: uuid.New(), TenantID: tenant, Key: "ganho", Label: "Ganho", Position: 4, IsDefault: true}

	pinned := time.Date(2026, 5, 16, 12, 0, 0, 0, time.UTC)

	type setup func() (*fakeStageRepo, *fakeTransitionRepo, *fakePublisher)

	baseSetup := func() (*fakeStageRepo, *fakeTransitionRepo, *fakePublisher) {
		s := newFakeStages()
		s.put(stageNovo)
		s.put(stageGanho)
		return s, newFakeTransitions(), &fakePublisher{}
	}

	cases := []struct {
		name          string
		setup         setup
		tenantID      uuid.UUID
		conversation  uuid.UUID
		toKey         string
		actor         uuid.UUID
		reason        string
		wantErr       error // matched with errors.Is when non-nil
		wantNoOp      bool  // true = no transition row inserted, no event published
		wantFromIsNil bool
		wantFromID    *uuid.UUID
	}{
		{
			name:         "zero tenant rejected",
			setup:        baseSetup,
			tenantID:     uuid.Nil,
			conversation: conv,
			toKey:        "novo",
			actor:        actor,
			wantErr:      funnel.ErrInvalidTenant,
			wantNoOp:     true,
		},
		{
			name:         "zero conversation rejected",
			setup:        baseSetup,
			tenantID:     tenant,
			conversation: uuid.Nil,
			toKey:        "novo",
			actor:        actor,
			wantErr:      funnel.ErrInvalidConversation,
			wantNoOp:     true,
		},
		{
			name:         "zero actor rejected",
			setup:        baseSetup,
			tenantID:     tenant,
			conversation: conv,
			toKey:        "novo",
			actor:        uuid.Nil,
			wantErr:      funnel.ErrInvalidActor,
			wantNoOp:     true,
		},
		{
			name:         "blank stage key rejected",
			setup:        baseSetup,
			tenantID:     tenant,
			conversation: conv,
			toKey:        "   ",
			actor:        actor,
			wantErr:      funnel.ErrInvalidStageKey,
			wantNoOp:     true,
		},
		{
			name:         "unknown stage key returns ErrStageNotFound",
			setup:        baseSetup,
			tenantID:     tenant,
			conversation: conv,
			toKey:        "imaginary",
			actor:        actor,
			wantErr:      funnel.ErrStageNotFound,
			wantNoOp:     true,
		},
		{
			name:          "first entry creates transition with nil from",
			setup:         baseSetup,
			tenantID:      tenant,
			conversation:  conv,
			toKey:         "novo",
			actor:         actor,
			reason:        "initial intake",
			wantFromIsNil: true,
		},
		{
			name: "same destination is idempotent no-op",
			setup: func() (*fakeStageRepo, *fakeTransitionRepo, *fakePublisher) {
				s, tr, p := baseSetup()
				tr.latest[convKey(tenant, conv)] = &funnel.Transition{
					ID:             uuid.New(),
					TenantID:       tenant,
					ConversationID: conv,
					ToStageID:      stageNovo.ID,
					TransitionedAt: pinned.Add(-1 * time.Hour),
				}
				return s, tr, p
			},
			tenantID:     tenant,
			conversation: conv,
			toKey:        "novo",
			actor:        actor,
			wantNoOp:     true,
		},
		{
			name: "different destination records from = current",
			setup: func() (*fakeStageRepo, *fakeTransitionRepo, *fakePublisher) {
				s, tr, p := baseSetup()
				tr.latest[convKey(tenant, conv)] = &funnel.Transition{
					ID:             uuid.New(),
					TenantID:       tenant,
					ConversationID: conv,
					ToStageID:      stageNovo.ID,
					TransitionedAt: pinned.Add(-1 * time.Hour),
				}
				return s, tr, p
			},
			tenantID:     tenant,
			conversation: conv,
			toKey:        "ganho",
			actor:        actor,
			reason:       "deal closed",
			wantFromID:   &stageNovo.ID,
		},
	}

	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			stages, transitions, pub := c.setup()
			svc, err := funnel.NewService(funnel.Config{
				Stages:      stages,
				Transitions: transitions,
				Publisher:   pub,
				Now:         func() time.Time { return pinned },
			})
			if err != nil {
				t.Fatalf("NewService: %v", err)
			}
			err = svc.MoveConversation(context.Background(), c.tenantID, c.conversation, c.toKey, c.actor, c.reason)
			if c.wantErr != nil {
				if !errors.Is(err, c.wantErr) {
					t.Fatalf("MoveConversation err = %v, want errors.Is(%v)", err, c.wantErr)
				}
			} else if err != nil {
				t.Fatalf("MoveConversation err = %v, want nil", err)
			}

			if c.wantNoOp {
				if got := len(transitions.inserted); got != 0 {
					t.Errorf("inserted = %d transitions, want 0", got)
				}
				if got := len(pub.events); got != 0 {
					t.Errorf("published %d events, want 0", got)
				}
				return
			}

			if got := len(transitions.inserted); got != 1 {
				t.Fatalf("inserted = %d transitions, want 1", got)
			}
			tr := transitions.inserted[0]
			if tr.TenantID != c.tenantID {
				t.Errorf("TenantID = %v, want %v", tr.TenantID, c.tenantID)
			}
			if tr.ConversationID != c.conversation {
				t.Errorf("ConversationID = %v, want %v", tr.ConversationID, c.conversation)
			}
			if tr.TransitionedByUserID != c.actor {
				t.Errorf("TransitionedByUserID = %v, want %v", tr.TransitionedByUserID, c.actor)
			}
			if !tr.TransitionedAt.Equal(pinned) {
				t.Errorf("TransitionedAt = %v, want %v", tr.TransitionedAt, pinned)
			}
			if tr.Reason != c.reason {
				t.Errorf("Reason = %q, want %q", tr.Reason, c.reason)
			}
			switch {
			case c.wantFromIsNil:
				if tr.FromStageID != nil {
					t.Errorf("FromStageID = %v, want nil", *tr.FromStageID)
				}
			case c.wantFromID != nil:
				if tr.FromStageID == nil {
					t.Fatalf("FromStageID = nil, want %v", *c.wantFromID)
				}
				if *tr.FromStageID != *c.wantFromID {
					t.Errorf("FromStageID = %v, want %v", *tr.FromStageID, *c.wantFromID)
				}
			}

			if got := len(pub.events); got != 1 {
				t.Fatalf("published %d events, want 1", got)
			}
			evt := pub.events[0]
			if evt.name != funnel.EventNameConversationMoved {
				t.Errorf("event name = %q, want %q", evt.name, funnel.EventNameConversationMoved)
			}
			payload, ok := evt.payload.(funnel.ConversationMovedEvent)
			if !ok {
				t.Fatalf("event payload type = %T, want funnel.ConversationMovedEvent", evt.payload)
			}
			if payload.TransitionID != tr.ID {
				t.Errorf("payload.TransitionID = %v, want %v", payload.TransitionID, tr.ID)
			}
			if payload.ToStageID != tr.ToStageID {
				t.Errorf("payload.ToStageID = %v, want %v", payload.ToStageID, tr.ToStageID)
			}
		})
	}
}

func TestMoveConversation_StageRepoErrorIsWrapped(t *testing.T) {
	t.Parallel()
	stages := newFakeStages()
	stages.err = errors.New("boom")
	svc, err := funnel.NewService(funnel.Config{
		Stages:      stages,
		Transitions: newFakeTransitions(),
		Publisher:   &fakePublisher{},
	})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	err = svc.MoveConversation(context.Background(), uuid.New(), uuid.New(), "novo", uuid.New(), "")
	if err == nil {
		t.Fatal("MoveConversation err = nil, want non-nil")
	}
	if !errorContains(err, "boom") {
		t.Errorf("err = %v, want wrap of %q", err, "boom")
	}
}

func TestMoveConversation_LatestTransitionErrorIsWrapped(t *testing.T) {
	t.Parallel()
	tenant := uuid.New()
	stages := newFakeStages()
	stages.put(&funnel.Stage{ID: uuid.New(), TenantID: tenant, Key: "novo", Label: "Novo", Position: 1})
	transitions := newFakeTransitions()
	transitions.latestErr = errors.New("query failed")
	svc, err := funnel.NewService(funnel.Config{
		Stages:      stages,
		Transitions: transitions,
		Publisher:   &fakePublisher{},
	})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	err = svc.MoveConversation(context.Background(), tenant, uuid.New(), "novo", uuid.New(), "")
	if err == nil {
		t.Fatal("MoveConversation err = nil, want non-nil")
	}
	if !errorContains(err, "query failed") {
		t.Errorf("err = %v, want wrap of %q", err, "query failed")
	}
}

func TestMoveConversation_CreateErrorIsWrapped(t *testing.T) {
	t.Parallel()
	tenant := uuid.New()
	stages := newFakeStages()
	stages.put(&funnel.Stage{ID: uuid.New(), TenantID: tenant, Key: "novo", Label: "Novo", Position: 1})
	transitions := newFakeTransitions()
	transitions.createErr = errors.New("insert failed")
	pub := &fakePublisher{}
	svc, err := funnel.NewService(funnel.Config{
		Stages:      stages,
		Transitions: transitions,
		Publisher:   pub,
	})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	err = svc.MoveConversation(context.Background(), tenant, uuid.New(), "novo", uuid.New(), "")
	if err == nil {
		t.Fatal("MoveConversation err = nil, want non-nil")
	}
	if !errorContains(err, "insert failed") {
		t.Errorf("err = %v, want wrap of %q", err, "insert failed")
	}
	if got := len(pub.events); got != 0 {
		t.Errorf("publish called %d times after Create failure, want 0", got)
	}
}

func TestMoveConversation_PublishErrorIsWrapped(t *testing.T) {
	t.Parallel()
	tenant := uuid.New()
	stages := newFakeStages()
	stages.put(&funnel.Stage{ID: uuid.New(), TenantID: tenant, Key: "novo", Label: "Novo", Position: 1})
	transitions := newFakeTransitions()
	pub := &fakePublisher{err: errors.New("bus down")}
	svc, err := funnel.NewService(funnel.Config{
		Stages:      stages,
		Transitions: transitions,
		Publisher:   pub,
	})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	err = svc.MoveConversation(context.Background(), tenant, uuid.New(), "novo", uuid.New(), "")
	if err == nil {
		t.Fatal("MoveConversation err = nil, want non-nil")
	}
	if !errorContains(err, "bus down") {
		t.Errorf("err = %v, want wrap of %q", err, "bus down")
	}
	// Transition row should still be persisted — the ledger is the source
	// of truth and the publish is best-effort fan-out.
	if got := len(transitions.inserted); got != 1 {
		t.Errorf("inserted = %d transitions, want 1 (persisted before publish)", got)
	}
}

func errorContains(err error, substr string) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), substr)
}
