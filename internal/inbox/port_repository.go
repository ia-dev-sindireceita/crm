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
