package usecase

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/inbox"
)

// statusDedupChannel is the value the status reconciler writes into
// the inbound_message_dedup ledger's channel column so the
// (channel, channel_external_id) UNIQUE constraint discriminates
// status replays from inbound-message replays. The status branch is
// idempotent at the domain level (Message.AdvanceStatus is monotonic),
// so the ledger row is a fast-path replay shield, not a correctness
// invariant.
const statusDedupChannel = "whatsapp_status"

// UpdateMessageStatus implements inbox.MessageStatusUpdater. The
// carrier adapter calls HandleStatus once per statuses[] entry in the
// webhook payload; the use case applies Message.AdvanceStatus and
// persists the new state inside the tenant scope.
type UpdateMessageStatus struct {
	repo  inbox.Repository
	dedup inbox.InboundDedupRepository
}

// NewUpdateMessageStatus wires the use case. nil dependencies are
// programming errors caught at construction so the composition root
// crashes at boot rather than on the first webhook delivery.
func NewUpdateMessageStatus(repo inbox.Repository, dedup inbox.InboundDedupRepository) (*UpdateMessageStatus, error) {
	if repo == nil {
		return nil, errors.New("inbox/usecase: repo must not be nil")
	}
	if dedup == nil {
		return nil, errors.New("inbox/usecase: dedup must not be nil")
	}
	return &UpdateMessageStatus{repo: repo, dedup: dedup}, nil
}

// MustNewUpdateMessageStatus is the panic-on-error variant for the
// composition root.
func MustNewUpdateMessageStatus(repo inbox.Repository, dedup inbox.InboundDedupRepository) *UpdateMessageStatus {
	u, err := NewUpdateMessageStatus(repo, dedup)
	if err != nil {
		panic(err)
	}
	return u
}

// HandleStatus implements inbox.MessageStatusUpdater. Steps:
//
//  1. Reject obviously malformed events (no tenant / wamid / channel).
//  2. Claim "status:{wamid}:{status}" on the dedup ledger — a duplicate
//     means we already processed this exact carrier ACK.
//  3. Load the message under tenant scope. Missing → unknown_message
//     outcome (silent ack).
//  4. AdvanceStatus on the in-memory aggregate. ErrStatusRegression
//     becomes a noop outcome (replay-safe), ErrInvalidStatus is
//     propagated to the caller. Equal-rank transitions are no-ops
//     inside AdvanceStatus itself.
//  5. Persist the new status via Repository.UpdateMessage.
//  6. MarkProcessed on the ledger to close the row.
//
// Compile-time guard at the end of the file confirms it satisfies the
// inbox.MessageStatusUpdater port.
func (u *UpdateMessageStatus) HandleStatus(ctx context.Context, ev inbox.StatusUpdate) (inbox.StatusUpdateResult, error) {
	if ev.TenantID == uuid.Nil {
		return inbox.StatusUpdateResult{}, inbox.ErrInvalidTenant
	}
	channel := strings.ToLower(strings.TrimSpace(ev.Channel))
	if channel == "" {
		return inbox.StatusUpdateResult{}, inbox.ErrInvalidChannel
	}
	externalID := strings.TrimSpace(ev.ChannelExternalID)
	if externalID == "" {
		return inbox.StatusUpdateResult{}, inbox.ErrInvalidStatus
	}
	if ev.NewStatus == "" {
		return inbox.StatusUpdateResult{}, inbox.ErrInvalidStatus
	}

	dedupKey := statusDedupKey(externalID, ev.NewStatus)
	if err := u.dedup.Claim(ctx, statusDedupChannel, dedupKey); err != nil {
		if errors.Is(err, inbox.ErrInboundAlreadyProcessed) {
			return inbox.StatusUpdateResult{
				Outcome:   inbox.StatusOutcomeNoop,
				NewStatus: ev.NewStatus,
			}, nil
		}
		return inbox.StatusUpdateResult{}, fmt.Errorf("dedup claim: %w", err)
	}

	m, err := u.repo.FindMessageByChannelExternalID(ctx, ev.TenantID, channel, externalID)
	if err != nil {
		if errors.Is(err, inbox.ErrNotFound) {
			// Close the dedup row so a retry does not loop here.
			_ = u.dedup.MarkProcessed(ctx, statusDedupChannel, dedupKey)
			return inbox.StatusUpdateResult{
				Outcome:   inbox.StatusOutcomeUnknownMessage,
				NewStatus: ev.NewStatus,
			}, nil
		}
		return inbox.StatusUpdateResult{}, fmt.Errorf("find message: %w", err)
	}

	prev := m.Status
	if err := m.AdvanceStatus(ev.NewStatus); err != nil {
		if errors.Is(err, inbox.ErrStatusRegression) {
			_ = u.dedup.MarkProcessed(ctx, statusDedupChannel, dedupKey)
			return inbox.StatusUpdateResult{
				Outcome:        inbox.StatusOutcomeNoop,
				PreviousStatus: prev,
				NewStatus:      ev.NewStatus,
			}, nil
		}
		return inbox.StatusUpdateResult{}, fmt.Errorf("advance status: %w", err)
	}
	if m.Status == prev {
		_ = u.dedup.MarkProcessed(ctx, statusDedupChannel, dedupKey)
		return inbox.StatusUpdateResult{
			Outcome:        inbox.StatusOutcomeNoop,
			PreviousStatus: prev,
			NewStatus:      m.Status,
		}, nil
	}

	if err := u.repo.UpdateMessage(ctx, m); err != nil {
		return inbox.StatusUpdateResult{}, fmt.Errorf("persist message: %w", err)
	}
	if err := u.dedup.MarkProcessed(ctx, statusDedupChannel, dedupKey); err != nil {
		return inbox.StatusUpdateResult{}, fmt.Errorf("dedup mark: %w", err)
	}

	return inbox.StatusUpdateResult{
		Outcome:        inbox.StatusOutcomeApplied,
		PreviousStatus: prev,
		NewStatus:      m.Status,
	}, nil
}

// statusDedupKey builds the dedup ledger row key for a (wamid, status)
// pair. The key is deterministic so concurrent replays of the same
// Meta callback collapse to a single row.
func statusDedupKey(channelExternalID string, status inbox.MessageStatus) string {
	return channelExternalID + ":" + string(status)
}

// Compile-time guard: UpdateMessageStatus must satisfy the port the
// adapter depends on.
var _ inbox.MessageStatusUpdater = (*UpdateMessageStatus)(nil)
