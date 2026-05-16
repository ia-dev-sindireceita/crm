package postgres_test

// SIN-62797 integration tests for the funnel BoardReader + history
// list (F2-12). These extend the F2-08 suite in
// funnel_adapter_test.go; they share the same TestMain + harness via
// the parent postgres_test package per the mastersession pattern.

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/funnel"
)

// stageIDByKey collects the {key -> stage id} map for the tenant —
// the board tests need to address stages by their stable key while
// asserting on uuid columns.
func stageIDByKey(t *testing.T, store interface {
	FindByKey(context.Context, uuid.UUID, string) (*funnel.Stage, error)
}, tenant uuid.UUID) map[string]uuid.UUID {
	t.Helper()
	out := map[string]uuid.UUID{}
	for _, key := range []string{"novo", "qualificando", "proposta", "ganho", "perdido"} {
		st, err := store.FindByKey(context.Background(), tenant, key)
		if err != nil {
			t.Fatalf("FindByKey(%q): %v", key, err)
		}
		out[key] = st.ID
	}
	return out
}

func TestFunnelAdapter_Board_FiveColumnsOrderedByPosition(t *testing.T) {
	db := freshDBWithFunnelAdapter(t)
	store := newFunnelStore(t, db)
	tenant := seedFunnelTenant(t, db.AdminPool())

	board, err := store.Board(context.Background(), tenant)
	if err != nil {
		t.Fatalf("Board: %v", err)
	}
	if got, want := len(board.Columns), 5; got != want {
		t.Fatalf("Board: got %d columns, want %d", got, want)
	}
	wantOrder := []string{"novo", "qualificando", "proposta", "ganho", "perdido"}
	for i, col := range board.Columns {
		if col.Stage.Key != wantOrder[i] {
			t.Errorf("Board[%d].Stage.Key = %q, want %q", i, col.Stage.Key, wantOrder[i])
		}
		if col.Stage.Position != i+1 {
			t.Errorf("Board[%d].Stage.Position = %d, want %d", i, col.Stage.Position, i+1)
		}
		if got := len(col.Cards); got != 0 {
			t.Errorf("Board[%d].Cards = %d, want 0 (no transitions seeded)", i, got)
		}
	}
}

func TestFunnelAdapter_Board_CardsLandInLatestStage(t *testing.T) {
	db := freshDBWithFunnelAdapter(t)
	store := newFunnelStore(t, db)
	tenant := seedFunnelTenant(t, db.AdminPool())
	user := seedFunnelUser(t, db.AdminPool(), tenant)
	conv := seedFunnelContactAndConversation(t, db.AdminPool(), tenant)

	stages := stageIDByKey(t, store, tenant)
	base := time.Date(2026, 5, 16, 12, 0, 0, 0, time.UTC)

	// First transition: conversation enters at "novo".
	if err := store.Create(context.Background(), &funnel.Transition{
		ID: uuid.New(), TenantID: tenant, ConversationID: conv,
		ToStageID: stages["novo"], TransitionedByUserID: user,
		TransitionedAt: base,
	}); err != nil {
		t.Fatalf("Create novo: %v", err)
	}
	// Second transition (latest): conversation moves to "ganho".
	fromID := stages["novo"]
	if err := store.Create(context.Background(), &funnel.Transition{
		ID: uuid.New(), TenantID: tenant, ConversationID: conv,
		FromStageID: &fromID, ToStageID: stages["ganho"],
		TransitionedByUserID: user, TransitionedAt: base.Add(time.Hour),
	}); err != nil {
		t.Fatalf("Create ganho: %v", err)
	}

	board, err := store.Board(context.Background(), tenant)
	if err != nil {
		t.Fatalf("Board: %v", err)
	}

	novoCount, ganhoCount := 0, 0
	for _, col := range board.Columns {
		switch col.Stage.Key {
		case "novo":
			novoCount = len(col.Cards)
		case "ganho":
			ganhoCount = len(col.Cards)
			for _, card := range col.Cards {
				if card.ConversationID == conv {
					if card.DisplayName != "Alice" {
						t.Errorf("ganho card DisplayName = %q, want Alice", card.DisplayName)
					}
					if card.Channel != "whatsapp" {
						t.Errorf("ganho card Channel = %q, want whatsapp", card.Channel)
					}
				}
			}
		}
	}
	if novoCount != 0 {
		t.Errorf("novo column has %d cards, want 0 (latest moved on)", novoCount)
	}
	if ganhoCount != 1 {
		t.Errorf("ganho column has %d cards, want 1", ganhoCount)
	}
}

func TestFunnelAdapter_Board_ClosedConversationsHidden(t *testing.T) {
	db := freshDBWithFunnelAdapter(t)
	store := newFunnelStore(t, db)
	tenant := seedFunnelTenant(t, db.AdminPool())
	user := seedFunnelUser(t, db.AdminPool(), tenant)
	conv := seedFunnelContactAndConversation(t, db.AdminPool(), tenant)
	stages := stageIDByKey(t, store, tenant)

	if err := store.Create(context.Background(), &funnel.Transition{
		ID: uuid.New(), TenantID: tenant, ConversationID: conv,
		ToStageID: stages["novo"], TransitionedByUserID: user,
		TransitionedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	// Close the conversation; the board should hide it.
	if _, err := db.AdminPool().Exec(context.Background(),
		`UPDATE conversation SET state='closed' WHERE id=$1`, conv); err != nil {
		t.Fatalf("close conv: %v", err)
	}
	board, err := store.Board(context.Background(), tenant)
	if err != nil {
		t.Fatalf("Board: %v", err)
	}
	for _, col := range board.Columns {
		if len(col.Cards) != 0 {
			t.Errorf("column %q has %d cards after close, want 0", col.Stage.Key, len(col.Cards))
		}
	}
}

func TestFunnelAdapter_Board_RejectsZeroTenant(t *testing.T) {
	db := freshDBWithFunnelAdapter(t)
	store := newFunnelStore(t, db)
	if _, err := store.Board(context.Background(), uuid.Nil); err == nil {
		t.Error("Board(uuid.Nil) err = nil, want validation error")
	}
}

func TestFunnelAdapter_Board_RLSScopesToTenant(t *testing.T) {
	db := freshDBWithFunnelAdapter(t)
	store := newFunnelStore(t, db)
	tenantA := seedFunnelTenant(t, db.AdminPool())
	tenantB := seedFunnelTenant(t, db.AdminPool())

	// Seed a card in tenantA's "novo" only.
	userA := seedFunnelUser(t, db.AdminPool(), tenantA)
	convA := seedFunnelContactAndConversation(t, db.AdminPool(), tenantA)
	stagesA := stageIDByKey(t, store, tenantA)
	if err := store.Create(context.Background(), &funnel.Transition{
		ID: uuid.New(), TenantID: tenantA, ConversationID: convA,
		ToStageID: stagesA["novo"], TransitionedByUserID: userA,
		TransitionedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	boardA, err := store.Board(context.Background(), tenantA)
	if err != nil {
		t.Fatalf("Board A: %v", err)
	}
	cardsA := 0
	for _, col := range boardA.Columns {
		cardsA += len(col.Cards)
	}
	if cardsA != 1 {
		t.Errorf("tenantA board cards = %d, want 1", cardsA)
	}

	boardB, err := store.Board(context.Background(), tenantB)
	if err != nil {
		t.Fatalf("Board B: %v", err)
	}
	cardsB := 0
	for _, col := range boardB.Columns {
		cardsB += len(col.Cards)
	}
	if cardsB != 0 {
		t.Errorf("tenantB board cards = %d, want 0 (RLS isolation)", cardsB)
	}
}

func TestFunnelAdapter_ListForConversation_OrderedAscending(t *testing.T) {
	db := freshDBWithFunnelAdapter(t)
	store := newFunnelStore(t, db)
	tenant := seedFunnelTenant(t, db.AdminPool())
	user := seedFunnelUser(t, db.AdminPool(), tenant)
	conv := seedFunnelContactAndConversation(t, db.AdminPool(), tenant)
	stages := stageIDByKey(t, store, tenant)
	base := time.Date(2026, 5, 16, 12, 0, 0, 0, time.UTC)

	want := []struct {
		stage string
		at    time.Time
	}{
		{"novo", base},
		{"qualificando", base.Add(time.Minute)},
		{"proposta", base.Add(2 * time.Minute)},
		{"ganho", base.Add(3 * time.Minute)},
	}
	var prev *uuid.UUID
	for _, step := range want {
		t := &funnel.Transition{
			ID: uuid.New(), TenantID: tenant, ConversationID: conv,
			ToStageID:            stages[step.stage],
			TransitionedByUserID: user, TransitionedAt: step.at,
		}
		if prev != nil {
			t.FromStageID = prev
		}
		if err := store.Create(context.Background(), t); err != nil {
			panic(err)
		}
		next := stages[step.stage]
		prev = &next
	}

	history, err := store.ListForConversation(context.Background(), tenant, conv)
	if err != nil {
		t.Fatalf("ListForConversation: %v", err)
	}
	if got := len(history); got != len(want) {
		t.Fatalf("history len = %d, want %d", got, len(want))
	}
	for i, h := range history {
		if !h.TransitionedAt.Equal(want[i].at) {
			t.Errorf("history[%d].TransitionedAt = %v, want %v", i, h.TransitionedAt, want[i].at)
		}
		if h.ToStageID != stages[want[i].stage] {
			t.Errorf("history[%d].ToStageID = %v, want %v", i, h.ToStageID, stages[want[i].stage])
		}
	}
}

func TestFunnelAdapter_ListForConversation_EmptyWhenNoHistory(t *testing.T) {
	db := freshDBWithFunnelAdapter(t)
	store := newFunnelStore(t, db)
	tenant := seedFunnelTenant(t, db.AdminPool())
	conv := seedFunnelContactAndConversation(t, db.AdminPool(), tenant)
	got, err := store.ListForConversation(context.Background(), tenant, conv)
	if err != nil {
		t.Fatalf("ListForConversation: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("len(got) = %d, want 0", len(got))
	}
}

func TestFunnelAdapter_ListForConversation_RejectsZeroTenant(t *testing.T) {
	db := freshDBWithFunnelAdapter(t)
	store := newFunnelStore(t, db)
	if _, err := store.ListForConversation(context.Background(), uuid.Nil, uuid.New()); err == nil {
		t.Error("ListForConversation(uuid.Nil) err = nil, want validation error")
	}
}

func TestFunnelAdapter_ListForConversation_ZeroConversationReturnsEmpty(t *testing.T) {
	db := freshDBWithFunnelAdapter(t)
	store := newFunnelStore(t, db)
	tenant := seedFunnelTenant(t, db.AdminPool())
	got, err := store.ListForConversation(context.Background(), tenant, uuid.Nil)
	if err != nil {
		t.Fatalf("ListForConversation: %v", err)
	}
	if got != nil {
		t.Errorf("got %v, want nil", got)
	}
}
