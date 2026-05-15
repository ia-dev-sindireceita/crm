package inbox

import (
	"context"

	"github.com/google/uuid"
)

// Repository is the storage port for the Conversation aggregate and
// its child entities (Message, Assignment). The concrete adapter lives
// in internal/adapter/db/postgres/inbox.
//
// Every method is tenant-scoped except claim/release on the global
// dedup ledger, which is consumed before tenant context is fully
// resolved (see InboundDedupRepository).
//
// The port is intentionally small — PR4 ships only the methods the
// receive-inbound and send-outbound use-cases need. PR6 (webhook
// receiver) and PR7+ (HTMX inbox UI) extend it with List / Update
// methods when their own use-cases need them.
type Repository interface {
	// CreateConversation persists a brand-new Conversation row. The
	// caller MUST construct it via NewConversation. Returns the same
	// pointer back on success so the caller can chain into a
	// SaveMessage on the conversation.
	CreateConversation(ctx context.Context, c *Conversation) error

	// GetConversation returns the conversation with the given id under
	// the tenant scope. Returns ErrNotFound when no row matches
	// (RLS-hidden rows from other tenants collapse to the same
	// sentinel).
	GetConversation(ctx context.Context, tenantID, conversationID uuid.UUID) (*Conversation, error)

	// FindOpenConversation returns the currently open conversation for
	// (tenantID, contactID, channel), or ErrNotFound if there is none.
	// "Open" means State == open. A closed conversation does not count
	// — the receiver opens a fresh one when the contact messages back
	// after we closed the previous thread.
	FindOpenConversation(ctx context.Context, tenantID, contactID uuid.UUID, channel string) (*Conversation, error)

	// SaveMessage persists m and updates the parent conversation's
	// LastMessageAt. The conversation MUST already exist. The
	// (channel_external_id) field is preserved verbatim — adapter does
	// no normalisation. Inbound messages with the same
	// (channel, channel_external_id) MUST be filtered earlier through
	// InboundDedupRepository.Claim; SaveMessage itself does not enforce
	// idempotency.
	SaveMessage(ctx context.Context, m *Message) error

	// UpdateMessage persists status / channel_external_id changes on an
	// existing message. The caller MUST have advanced the in-memory
	// state machine via AdvanceStatus first; this method is a
	// thin persistence escape hatch. Returns ErrNotFound when the row
	// does not exist under the tenant scope.
	UpdateMessage(ctx context.Context, m *Message) error

	// FindMessageByChannelExternalID returns the message with the given
	// (channel, channelExternalID) pair under the tenant scope. The
	// status reconciler (WhatsApp PR8) uses it to materialise the
	// message before advancing its lifecycle state. Returns ErrNotFound
	// when no row matches — either because the carrier sent a status
	// for an unknown wamid, or because RLS hid the row from the
	// reconciler's tenant.
	FindMessageByChannelExternalID(ctx context.Context, tenantID uuid.UUID, channel, channelExternalID string) (*Message, error)

	// ListConversations returns up to `limit` conversations under the
	// tenant scope, newest-last-message-first. The state filter is
	// optional: a zero-value state returns both open and closed; passing
	// ConversationStateOpen restricts to open conversations. The hot
	// inbox query (PR9) uses (tenant_id, state, last_message_at DESC),
	// covered by the composite index added in migration 0088. limit must
	// be > 0; the adapter clamps to a sensible upper bound to keep the
	// page lightweight.
	ListConversations(ctx context.Context, tenantID uuid.UUID, state ConversationState, limit int) ([]*Conversation, error)

	// ListMessages returns the messages for the conversation under the
	// tenant scope, oldest-first (so the inbox view renders top→bottom
	// like a chat). Returns ErrNotFound when no conversation matches the
	// (tenantID, conversationID) pair — distinguishes "conversation
	// exists but has zero messages" (empty slice, nil error) from
	// "conversation hidden by RLS" (ErrNotFound).
	ListMessages(ctx context.Context, tenantID, conversationID uuid.UUID) ([]*Message, error)

	// GetMessage returns the single message identified by (tenantID,
	// conversationID, messageID). Used by the realtime status partial
	// (SIN-62736) — the HTMX bubble polls the handler every few seconds
	// and the handler reads the latest row through this method without
	// the O(N) cost of ListMessages.
	//
	// Returns ErrNotFound when no row matches — either because the
	// message id is unknown, the conversation is hidden by RLS, or the
	// message belongs to a different conversation. The use case does
	// not distinguish those modes to avoid leaking cross-tenant existence
	// signals.
	GetMessage(ctx context.Context, tenantID, conversationID, messageID uuid.UUID) (*Message, error)
}

// InboundDedupRepository is the global idempotency ledger backing the
// inbound_message_dedup table (migration 0088). It is NOT tenant-scoped:
// the webhook receiver consults it before tenant context has been
// fully resolved (ADR 0087). The two-phase commit shape mirrors the
// table's processed_at semantics:
//
//	Claim → returns nil if the (channel, externalID) pair is fresh;
//	         ErrInboundAlreadyProcessed if any other call has claimed it.
//	MarkProcessed → flips processed_at = now() after the downstream
//	         message insert + wallet debit have succeeded.
//
// A crashed handler leaves a claim row with processed_at NULL; the
// 0075d_gc_jobs collector reclaims those after a window (separate PR).
type InboundDedupRepository interface {
	Claim(ctx context.Context, channel, channelExternalID string) error
	MarkProcessed(ctx context.Context, channel, channelExternalID string) error
}
