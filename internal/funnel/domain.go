package funnel

import (
	"time"

	"github.com/google/uuid"
)

// Stage is a per-tenant funnel column, e.g. "novo" or "ganho". Stage
// rows seed automatically on tenant insert (migration 0093) using the
// five default keys; tenants may add custom stages later via admin UI.
//
// Position drives the left-to-right order in the drag-and-drop board
// (F2-12); IsDefault marks the seed rows so admin tooling can keep
// renames idempotent.
type Stage struct {
	ID        uuid.UUID
	TenantID  uuid.UUID
	Key       string
	Label     string
	Position  int
	IsDefault bool
}

// Transition is one row in the append-only funnel ledger: the move of
// a Conversation from FromStageID (nil on first entry) to ToStageID at
// TransitionedAt by TransitionedByUserID.
//
// Reason is free-form text; the schema does not constrain it because
// future automatic-transition phases (Fase 4) will encode their own
// reason vocabulary outside this column.
type Transition struct {
	ID                   uuid.UUID
	TenantID             uuid.UUID
	ConversationID       uuid.UUID
	FromStageID          *uuid.UUID
	ToStageID            uuid.UUID
	TransitionedByUserID uuid.UUID
	TransitionedAt       time.Time
	Reason               string
}

// ConversationMovedEvent is the payload published on funnel.conversation_moved.
// Downstream consumers (audit log, real-time UI refresh) only need ids and the
// stable destination key; we do not embed the full Stage to keep the wire
// shape narrow and stable across stage renames.
type ConversationMovedEvent struct {
	TenantID             uuid.UUID
	ConversationID       uuid.UUID
	TransitionID         uuid.UUID
	FromStageID          *uuid.UUID
	ToStageID            uuid.UUID
	ToStageKey           string
	TransitionedByUserID uuid.UUID
	TransitionedAt       time.Time
	Reason               string
}

// EventNameConversationMoved is the canonical event name published on
// every funnel transition (F2-08 emits it; F2-12 board hot-swaps on it;
// Fase 4 automatic-rules subscribe to it).
const EventNameConversationMoved = "funnel.conversation_moved"

// ConversationCard is a read-side projection of a conversation in its
// current funnel stage. The card is denormalised from conversation +
// contact + funnel_transition so the board renders one row per
// conversation without a second round trip.
//
// The fields are deliberately UI-shaped: DisplayName is the contact's
// rendered name; Channel is the conversation's source carrier;
// LastMessageAt drives the column sort. The funnel domain owns the
// shape so the bounded-context boundary stays explicit — the board
// adapter materialises the JOIN result into this struct, and no
// downstream consumer ever sees a raw conversation or contact row.
type ConversationCard struct {
	ConversationID uuid.UUID
	ContactID      uuid.UUID
	DisplayName    string
	Channel        string
	LastMessageAt  time.Time
}

// BoardColumn is one stage on the F2-12 board: the stage definition
// plus the cards currently in it. Cards are ordered most-recent-first
// so the operator's eye lands on the freshest activity.
type BoardColumn struct {
	Stage Stage
	Cards []ConversationCard
}

// Board is the full read-side projection the GET /funnel handler
// renders. Columns are ordered by Stage.Position ascending so the
// board reads left-to-right "novo → qualificando → … → perdido".
type Board struct {
	Columns []BoardColumn
}
