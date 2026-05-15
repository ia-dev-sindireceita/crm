package inbox

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// StatusUpdate is the carrier-agnostic event the status reconciler
// hands to the inbox use case when a carrier reports a lifecycle
// transition for a previously-sent message (e.g. Meta's
// statuses[].status = "sent" / "delivered" / "read" / "failed").
//
// TenantID is resolved by the adapter before the use case is invoked
// (status updates ride the same envelope as inbound messages, so the
// metadata.phone_number_id → tenant lookup is identical).
//
// Channel + ChannelExternalID locate the message via
// Repository.FindMessageByChannelExternalID. NewStatus is the carrier
// state; the use case maps it onto Message.AdvanceStatus.
//
// OccurredAt is the carrier's timestamp for the event. It is recorded
// in the metric lag histogram and is not (currently) persisted on the
// message row — the storage layer only carries the message's CreatedAt.
//
// ErrorCode / ErrorTitle carry Meta's failure annotation when
// NewStatus is MessageStatusFailed. Both fields are empty otherwise.
type StatusUpdate struct {
	TenantID          uuid.UUID
	Channel           string
	ChannelExternalID string
	NewStatus         MessageStatus
	OccurredAt        time.Time
	ErrorCode         int
	ErrorTitle        string
}

// MessageStatusUpdater is the seam between a carrier-specific status
// reconciler (the WhatsApp adapter's statuses[] branch) and the inbox
// use case that owns the Message aggregate. The adapter MUST NOT
// reach into Repository.UpdateMessage directly because the lifecycle
// invariants live on Message.AdvanceStatus.
//
// HandleStatus returns nil for both "applied" and "no-op" outcomes —
// out-of-order ACKs are routine. Genuine failures (storage error,
// unknown wamid that should propagate) come back wrapped so the
// adapter can log them at the right severity.
type MessageStatusUpdater interface {
	HandleStatus(ctx context.Context, ev StatusUpdate) (StatusUpdateResult, error)
}

// StatusUpdateOutcome categorises the result of HandleStatus for
// metrics and logs. The string values are stable and used as
// Prometheus label values.
type StatusUpdateOutcome string

const (
	// StatusOutcomeApplied means AdvanceStatus moved the message
	// forward and the row was persisted.
	StatusOutcomeApplied StatusUpdateOutcome = "applied"
	// StatusOutcomeNoop means the update was monotonically older
	// than or equal to the current status (e.g. delivered after
	// read), so no row was written. Replay-safe.
	StatusOutcomeNoop StatusUpdateOutcome = "noop"
	// StatusOutcomeUnknownMessage means the carrier reported a status
	// for a wamid we do not have on file. Typically a tenant-mismatch
	// or a message older than the dedup retention window.
	StatusOutcomeUnknownMessage StatusUpdateOutcome = "unknown_message"
)

// StatusUpdateResult is the structured outcome of HandleStatus. The
// adapter consumes it to drive metrics and log levels; tests use it
// to assert on per-call behaviour without scraping the metrics
// registry.
type StatusUpdateResult struct {
	Outcome        StatusUpdateOutcome
	PreviousStatus MessageStatus
	NewStatus      MessageStatus
}
