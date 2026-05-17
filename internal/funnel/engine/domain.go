package engine

import (
	"time"

	"github.com/google/uuid"
)

// InboundMessage is the decoded shape the engine operates on. It carries
// only what [Engine.Handle] needs: tenant + conversation + message ids
// for the action target, channel for cascade resolution, body for
// trigger evaluation, and an occurred_at for latency metrics.
//
// All uuid fields are non-Nil by the time [Engine.Handle] sees the
// value — [DecodeInboundMessage] rejects malformed wire payloads with
// [ErrInvalidEvent].
type InboundMessage struct {
	TenantID       uuid.UUID
	ConversationID uuid.UUID
	MessageID      uuid.UUID
	Channel        string
	Body           string
	OccurredAt     time.Time
}

// Application is the row appended to funnel_rule_applications after a
// rule successfully fires against an inbound message. The dedup
// contract is UNIQUE (rule_id, message_id); tenant_id is denormalized
// for RLS, and the other columns are for audit + metrics.
type Application struct {
	TenantID       uuid.UUID
	RuleID         uuid.UUID
	MessageID      uuid.UUID
	ConversationID uuid.UUID
	ActionType     string
	AppliedAt      time.Time
}

// SystemActorID is the pseudo-user the engine attributes automatic
// stage transitions to. funnel.Service.MoveConversation rejects a Nil
// actor; the engine has no human in the loop, so it stamps every
// system-driven transition with this sentinel uuid. Audit consumers
// recognise it and label the row "regra automática" in the UI.
//
// Pinned via uuid.MustParse so the value survives package init without
// a global mutable; the constant is unexported via lower-case helper —
// callers read it through [SystemActor].
var systemActorID = uuid.MustParse("00000000-0000-0000-0000-000000000ace")

// SystemActor returns the sentinel actor uuid the engine uses when
// calling [StageMover.MoveConversation]. Exposed as a function (not a
// var) so accidental package-level mutation is impossible.
func SystemActor() uuid.UUID { return systemActorID }

// MoveReason is the reason string the engine stamps on every automatic
// transition. The funnel transition history surfaces this verbatim;
// keeping the string short and stable makes the audit row diff-friendly.
const MoveReason = "funnel-rule-engine"
