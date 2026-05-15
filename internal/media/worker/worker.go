// Package worker is the domain logic for the media-scan pipeline
// (SIN-62804 / F2-05c). It consumes `media.scan.requested` deliveries,
// calls a MediaScanner for the verdict, persists it via the
// MessageMediaStore port, and publishes `media.scan.completed`.
//
// Hexagonal boundary:
//
//   - Inputs (Delivery, Publisher) are narrow interfaces. The NATS
//     adapter (internal/adapter/messaging/nats) implements them; tests
//     use in-process fakes.
//   - Outputs (scanner.MediaScanner, scanner.MessageMediaStore) are
//     the domain ports defined in internal/media/scanner.
//
// Delivery semantics (at-least-once):
//
//   - Ack is called ONLY after persistence is confirmed. A persistence
//     error returns from Handle so the broker redelivers.
//   - A redelivery against an already-terminal row (errAlreadyFinalised)
//     is treated as success: the worker acks without rescanning or
//     republishing — matches the AC "se scan_status já é
//     clean/infected, no-op".
//   - A missing row (ErrNotFound) is treated as poison: the worker
//     acks and logs. Re-delivering it would never succeed.
//   - The completed-event publish happens AFTER persistence and BEFORE
//     ack so a crash between publish and ack republishes the verdict
//     (downstream consumers must dedup on (tenant_id, message_id)).
package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/media/alert"
	"github.com/pericles-luz/crm/internal/media/quarantine"
	"github.com/pericles-luz/crm/internal/media/scanner"
)

// SubjectRequested is the NATS subject the worker subscribes to.
// Producers (the upload path, SIN-62788 F2-05a) publish here when a
// new media blob is saved and needs a scan verdict.
const SubjectRequested = "media.scan.requested"

// SubjectCompleted is the NATS subject the worker emits to when a
// verdict has been persisted. Consumers (the quarantine flow,
// SIN-62805 F2-05d) listen here.
const SubjectCompleted = "media.scan.completed"

// Request is the JSON payload carried on SubjectRequested. The shape
// is deliberately closed — extra fields are tolerated by the JSON
// decoder but not propagated.
type Request struct {
	TenantID  uuid.UUID `json:"tenant_id"`
	MessageID uuid.UUID `json:"message_id"`
	Key       string    `json:"key"`
}

// Completed is the JSON payload carried on SubjectCompleted.
type Completed struct {
	TenantID  uuid.UUID      `json:"tenant_id"`
	MessageID uuid.UUID      `json:"message_id"`
	Key       string         `json:"key"`
	Status    scanner.Status `json:"status"`
	EngineID  string         `json:"engine_id"`
}

// Delivery is one redeliverable NATS message handed to Handle. Ack is
// idempotent in the adapter; calling it more than once is a no-op.
type Delivery interface {
	Data() []byte
	Ack(ctx context.Context) error
}

// Publisher is the narrow surface the worker uses to emit a completed
// event. The real adapter wraps JetStream Publish; tests collect into
// a slice.
type Publisher interface {
	Publish(ctx context.Context, subject string, body []byte) error
}

// Handler ties the worker's collaborators together. Construct via New;
// every field New populates is required.
//
// Defense-in-depth ([SIN-62805] F2-05d) is wired through the optional
// Quarantiner and Alerter fields. When set, the worker calls
// Quarantiner.Move on every infected verdict and Alerter.Notify so the
// `#security` channel is paged with `{tenant_id, message_id,
// engine_id, signature}`. Both fields default to nil — the in-process
// worker tests exercise the happy path without standing up a MinIO or
// Slack test double — but production wiring (cmd/mediascan-worker)
// MUST supply both.
//
// Both defense-in-depth calls are BEST-EFFORT: failures are logged and
// the delivery is still acked. The verdict is already persisted and
// republished, and a future reconcile sweep can clean stragglers.
// Returning an error here would trigger an unbounded redelivery loop
// during transient MinIO or Slack outages.
type Handler struct {
	Scanner     scanner.MediaScanner
	Store       scanner.MessageMediaStore
	Publisher   Publisher
	Logger      *slog.Logger
	Quarantiner quarantine.Quarantiner
	Alerter     alert.Alerter
}

// New validates that every collaborator is wired and returns a Handler
// ready for use. Logger may be nil; we default to slog.Default so
// callers do not have to plumb a logger through tests.
func New(s scanner.MediaScanner, store scanner.MessageMediaStore, pub Publisher, logger *slog.Logger) (*Handler, error) {
	if s == nil {
		return nil, errors.New("worker: MediaScanner is required")
	}
	if store == nil {
		return nil, errors.New("worker: MessageMediaStore is required")
	}
	if pub == nil {
		return nil, errors.New("worker: Publisher is required")
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Handler{Scanner: s, Store: store, Publisher: pub, Logger: logger}, nil
}

// Handle is the single per-delivery entrypoint. Returning a non-nil
// error tells the adapter NOT to ack (the broker will redeliver).
// A nil return means the delivery has been processed (either a real
// verdict was persisted+published, or it was determined to be a
// poison/already-finalised redelivery that the worker has acked).
func (h *Handler) Handle(ctx context.Context, msg Delivery) error {
	var req Request
	if err := json.Unmarshal(msg.Data(), &req); err != nil {
		// Malformed payloads are poison: log and ack — no amount of
		// redelivery will turn invalid JSON into valid JSON.
		h.Logger.WarnContext(ctx, "media.scan: drop unparseable payload", "err", err.Error())
		return ackPoison(ctx, msg)
	}
	if req.TenantID == uuid.Nil || req.MessageID == uuid.Nil || req.Key == "" {
		h.Logger.WarnContext(ctx, "media.scan: drop incomplete payload",
			"tenant_id", req.TenantID, "message_id", req.MessageID, "key_empty", req.Key == "")
		return ackPoison(ctx, msg)
	}

	result, err := h.Scanner.Scan(ctx, req.Key)
	if err != nil {
		// Transport/engine failure — do NOT ack, let the broker
		// redeliver.
		return fmt.Errorf("worker: scan %q: %w", req.Key, err)
	}

	persistErr := h.Store.UpdateScanResult(ctx, req.TenantID, req.MessageID, result)
	switch {
	case persistErr == nil:
		// fall through to publish + ack
	case errors.Is(persistErr, scanner.ErrAlreadyFinalised):
		// A previous delivery already wrote the verdict. Ack the
		// redelivery without republishing so downstream consumers
		// do not see duplicates beyond what at-least-once already
		// guarantees.
		h.Logger.InfoContext(ctx, "media.scan: redelivery against finalised row, acking",
			"tenant_id", req.TenantID, "message_id", req.MessageID)
		return ackPoison(ctx, msg)
	case errors.Is(persistErr, scanner.ErrNotFound):
		h.Logger.WarnContext(ctx, "media.scan: target row missing, acking poison",
			"tenant_id", req.TenantID, "message_id", req.MessageID)
		return ackPoison(ctx, msg)
	default:
		return fmt.Errorf("worker: persist %s/%s: %w", req.TenantID, req.MessageID, persistErr)
	}

	// Defense-in-depth on infected. Best-effort: failures here are
	// logged but do not prevent publish + ack. The verdict is already
	// persisted, and a reconcile sweep cleans any stragglers.
	if result.Status == scanner.StatusInfected {
		h.runDefenseInDepth(ctx, req, result)
	}

	completed := Completed{
		TenantID:  req.TenantID,
		MessageID: req.MessageID,
		Key:       req.Key,
		Status:    result.Status,
		EngineID:  result.EngineID,
	}
	body, err := json.Marshal(completed)
	if err != nil {
		// json.Marshal failing for closed value types is a
		// programmer bug, not a transient issue — surface it.
		return fmt.Errorf("worker: marshal completed: %w", err)
	}
	if err := h.Publisher.Publish(ctx, SubjectCompleted, body); err != nil {
		// Publish failed AFTER persistence. Return error so the
		// broker redelivers; the next attempt will hit
		// ErrAlreadyFinalised in the store, then publish again.
		return fmt.Errorf("worker: publish completed: %w", err)
	}
	if err := msg.Ack(ctx); err != nil {
		return fmt.Errorf("worker: ack: %w", err)
	}
	return nil
}

// ackPoison acks a delivery that the worker has chosen to drop
// (poison/already-finalised/missing). Errors from Ack itself surface
// to the caller — failing to ack means the broker will redeliver, and
// the worker will land here again on the next attempt.
func ackPoison(ctx context.Context, msg Delivery) error {
	if err := msg.Ack(ctx); err != nil {
		return fmt.Errorf("worker: ack poison: %w", err)
	}
	return nil
}

// runDefenseInDepth performs Move + Notify on the infected verdict.
// Both collaborators are optional; nil fields are silently skipped.
// Failures are logged at error level so a Loki query can detect a
// degraded defense layer without blocking the worker's main loop.
func (h *Handler) runDefenseInDepth(ctx context.Context, req Request, result scanner.ScanResult) {
	if h.Quarantiner != nil {
		if qErr := h.Quarantiner.Move(ctx, req.Key); qErr != nil {
			h.Logger.ErrorContext(ctx, "media.scan: quarantine move failed",
				"tenant_id", req.TenantID, "message_id", req.MessageID,
				"key", req.Key, "err", qErr.Error())
		} else {
			h.Logger.InfoContext(ctx, "media.scan: blob quarantined",
				"tenant_id", req.TenantID, "message_id", req.MessageID, "key", req.Key)
		}
	}
	if h.Alerter != nil {
		event := alert.Event{
			TenantID:  req.TenantID,
			MessageID: req.MessageID,
			Key:       req.Key,
			EngineID:  result.EngineID,
			Signature: result.Signature,
		}
		if aErr := h.Alerter.Notify(ctx, event); aErr != nil {
			h.Logger.ErrorContext(ctx, "media.scan: alert notify failed",
				"tenant_id", req.TenantID, "message_id", req.MessageID, "err", aErr.Error())
		}
	}
}
