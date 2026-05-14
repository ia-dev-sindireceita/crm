package inbox

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// OutboundMessage is the carrier-agnostic payload the send-outbound
// use case hands to the outbound adapter. The carrier adapter (PR8)
// maps it into a vendor request. The domain stays free of vendor
// types.
//
// ConversationID and TenantID are carried through so adapters with
// per-conversation rate limiting (or per-tenant API tokens) have the
// scope they need without re-querying.
//
// ToExternalID is the carrier identity of the recipient (a phone
// number for WhatsApp). The use case resolves it from the
// conversation's contact via the contacts port.
type OutboundMessage struct {
	TenantID       uuid.UUID
	ConversationID uuid.UUID
	Channel        string
	ToExternalID   string
	Body           string
	OccurredAt     time.Time
}

// OutboundChannel is the seam through which the send-outbound use
// case talks to the carrier. The adapter returns the carrier-assigned
// channel-external-id (e.g. Meta wamid) so the use case can persist
// it on the message row. Errors map back to the message's failed
// state via AdvanceStatus(MessageStatusFailed).
type OutboundChannel interface {
	SendMessage(ctx context.Context, m OutboundMessage) (channelExternalID string, err error)
}

// WalletDebitor is the port the send-outbound use case uses to
// reserve and commit token spend before the carrier call. The
// implementation lives in internal/wallet (PR5); inbox does not
// import wallet directly so PR4 can land before PR5 and the dependency
// graph is one-way.
//
// The contract is "reserve, then commit on success / refund on
// failure". The use case calls Debit at the boundary: implementations
// MUST be atomic — partial spend with no carrier call is a wallet
// invariant violation.
type WalletDebitor interface {
	// Debit reserves cost tokens against the tenant's wallet,
	// invokes the supplied charge callback (which performs the
	// carrier send), and commits the reservation iff charge returns
	// nil. Any error from charge releases the reservation. cost may
	// be zero — the implementation MUST still call charge so the
	// outbound flow exercises wallet bookkeeping uniformly (PR4 AC #5).
	Debit(ctx context.Context, tenantID uuid.UUID, cost int64, charge func(ctx context.Context) error) error
}
