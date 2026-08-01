package nats

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/media/worker"
)

// mediaScanPublishTarget is the JetStream surface
// MediaScanRequestPublisher writes to. *SDKAdapter satisfies it via
// Publish.
type mediaScanPublishTarget interface {
	Publish(ctx context.Context, subject string, body []byte) error
}

// MediaScanRequestPublisher publishes media.scan.requested envelopes
// (worker.SubjectRequested) for the inbound Messenger/Instagram
// media-scan pipeline (F2-05). Subject + Request shape are imported
// from the consumer package (internal/media/worker) so the producer
// cannot drift from the contract cmd/mediascan-worker pins.
type MediaScanRequestPublisher struct {
	target mediaScanPublishTarget
}

// NewMediaScanRequestPublisher wraps target (an *SDKAdapter in
// production). nil is rejected so a misconfigured boot fails fast.
func NewMediaScanRequestPublisher(target mediaScanPublishTarget) (*MediaScanRequestPublisher, error) {
	if target == nil {
		return nil, errors.New("nats: MediaScanRequestPublisher target is required")
	}
	return &MediaScanRequestPublisher{target: target}, nil
}

// PublishScanRequest implements messenger.MediaScanPublisher and
// instagram.MediaScanPublisher (identical method shape — both channel
// packages declare their own copy of the port per hexagonal
// convention).
func (p *MediaScanRequestPublisher) PublishScanRequest(ctx context.Context, tenantID, messageID uuid.UUID, key string) error {
	body, err := json.Marshal(worker.Request{TenantID: tenantID, MessageID: messageID, Key: key})
	if err != nil {
		return fmt.Errorf("nats: encode media scan request: %w", err)
	}
	if err := p.target.Publish(ctx, worker.SubjectRequested, body); err != nil {
		return fmt.Errorf("nats: publish media scan request: %w", err)
	}
	return nil
}
