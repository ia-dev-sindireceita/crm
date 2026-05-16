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
