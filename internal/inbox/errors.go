// Package inbox holds the Conversation aggregate, Message and Assignment
// entities, and the ports used by Fase 1 inbox handlers (SIN-62193 →
// SIN-62729).
//
// The package is the domain core: it imports neither database/sql nor pgx,
// neither net/http nor any vendor SDK. Storage lives behind Repository
// (port_repository.go); the inbound and outbound carrier seams live behind
// InboundChannel / OutboundChannel; the wallet debit seam lives behind
// WalletDebitor (so the inbox does not import internal/wallet directly).
//
// Sentinels are exported as package-level variables so callers (use-cases,
// adapters, HTTP handlers) can distinguish failure modes via errors.Is
// without depending on string-matching.
package inbox

import "errors"

// ErrInvalidTenant is returned when tenantID is uuid.Nil. A conversation
// MUST belong to a tenant; the database enforces this via NOT NULL +
// foreign key, but the domain rejects it earlier so callers see a clean
// error instead of a constraint-violation surface.
var ErrInvalidTenant = errors.New("inbox: invalid tenant id")

// ErrInvalidContact is returned when contactID is uuid.Nil. Every
// conversation is anchored on a contact; an "anonymous" conversation is
// a programming error.
var ErrInvalidContact = errors.New("inbox: invalid contact id")

// ErrInvalidChannel is returned by NewConversation when channel is blank
// after trimming. Channels are case-folded to lower so callers cannot
// accidentally split storage by casing.
var ErrInvalidChannel = errors.New("inbox: invalid channel")

// ErrConversationClosed is returned when callers attempt to record a
// message or assign a user on a conversation already in the closed
// state. Reopen first, then record.
var ErrConversationClosed = errors.New("inbox: conversation is closed")

// ErrConversationAlreadyOpen is returned by Reopen when the conversation
// is already in the open state. The transition is a no-op the caller
// must explicitly acknowledge so we never silently swallow a state bug.
var ErrConversationAlreadyOpen = errors.New("inbox: conversation already open")

// ErrInvalidAssignee is returned by AssignTo when userID is uuid.Nil.
// Use Close+Reopen or a future Unassign to clear an assignment; AssignTo
// always names a user.
var ErrInvalidAssignee = errors.New("inbox: invalid assignee user id")

// ErrInvalidDirection is returned by NewMessage when direction is not
// one of MessageDirectionIn / MessageDirectionOut.
var ErrInvalidDirection = errors.New("inbox: invalid message direction")

// ErrInvalidStatus is returned by NewMessage when status is not one of
// the allowed values, and by AdvanceStatus when the supplied next status
// is unknown.
var ErrInvalidStatus = errors.New("inbox: invalid message status")

// ErrStatusRegression is returned by AdvanceStatus when the supplied
// next status would move the message backwards in the lifecycle
// (e.g. read → delivered). The carriers we integrate with deliver
// these acks out-of-order; the rule is "monotonic", not "exclusive".
var ErrStatusRegression = errors.New("inbox: status regression rejected")

// ErrConversationMismatch is returned by RecordMessage when the message
// names a conversation_id that does not match the receiver. We do not
// silently overwrite the field; mismatches are programming errors.
var ErrConversationMismatch = errors.New("inbox: message belongs to a different conversation")

// ErrNotFound is returned by Repository lookups when no row matches the
// supplied scope.
var ErrNotFound = errors.New("inbox: not found")

// ErrInboundAlreadyProcessed is returned by the dedup port (and the
// receive-inbound use case) when an inbound (channel, channel_external_id)
// pair has already been claimed. Callers treat this as success: the
// vendor will retry the same wamid until we ACK, and we MUST NOT create
// a second message row.
var ErrInboundAlreadyProcessed = errors.New("inbox: inbound message already processed")

// ErrInvalidBody is returned by NewMessage when the body is empty after
// trimming. Carriers sometimes send a system-only delivery receipt that
// has no body; those are funneled through AdvanceStatus on an existing
// message, never through NewMessage.
var ErrInvalidBody = errors.New("inbox: message body must not be empty")

// ErrChannelDisabled is returned by an OutboundChannel adapter when the
// tenant's feature flag for the carrier is off. The use case treats it
// as a domain failure (the message will be marked failed); the operator
// learns about it through the rejected outcome on the send metric and
// can re-enable the flag without code changes.
var ErrChannelDisabled = errors.New("inbox: channel disabled for tenant")

// ErrChannelAuthFailed is returned by an OutboundChannel adapter when
// the carrier rejected our credentials (401/403). This is an
// infrastructure-level failure — SRE must rotate the token; the
// per-message retry pattern cannot fix it. The use case stops here and
// records the message as failed; alerting on this outcome is set up at
// the dashboard layer.
var ErrChannelAuthFailed = errors.New("inbox: channel auth failed")

// ErrChannelRejected is returned by an OutboundChannel adapter when the
// carrier rejected the message on domain grounds — invalid recipient,
// the 24h freeform window expired, the template is not approved, etc.
// Retries WILL NOT help. The adapter MUST preserve the carrier's
// human-readable message in the wrapped error so the operator sees why
// it failed without consulting the carrier dashboard.
var ErrChannelRejected = errors.New("inbox: channel rejected message")

// ErrChannelTransient is returned by an OutboundChannel adapter when
// the carrier or the network is temporarily unavailable — 5xx, timeout,
// connection reset. The adapter retries within its own bounded budget
// before surfacing this; the caller decides on higher-level retries
// (reconciler, requeue) and treats the per-message attempt as failed.
var ErrChannelTransient = errors.New("inbox: channel transient failure")
