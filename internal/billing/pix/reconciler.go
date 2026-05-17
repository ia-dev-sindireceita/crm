package pix

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
)

// reconciler is the default Reconciler implementation. The HTTP webhook
// receiver (C13) wraps an instance of this and feeds it normalised
// WebhookEvent values; the reconciler handles dedup + state transition
// + persistence. It depends only on the three ports declared in
// port.go, so it is fully unit-testable with in-memory fakes.
//
// Ordering of operations matters for the idempotency invariant
// (AC #1):
//
//  1. Record the event in EventLog FIRST. If Record returns
//     ErrDuplicateEvent, we are certain the transition has already been
//     applied by an earlier delivery — return Outcome{Duplicate: true}
//     without touching the charge.
//  2. Otherwise, fetch the charge by external_id.
//  3. Apply the transition. Idempotent no-ops (e.g. a `paid` event for
//     a charge that EventLog believes is fresh but is actually already
//     paid — possible if the dedup row was inserted by a previous
//     run that crashed between Record and Save) are tolerated by the
//     state machine (changed=false, nil).
//  4. Save the updated charge.
//
// actorID is the bot identity recorded on the master_ops audit trail
// (audit_decorator captures who performed the transition). Receivers
// inject the service-account uuid configured for the PSP webhook.
type reconciler struct {
	repo    Repository
	log     EventLog
	actorID uuid.UUID
}

// NewReconciler wires the default Reconciler. actorID is the
// service-account UUID recorded on the master_ops audit trail when the
// charge is persisted; the receiver typically passes the bot account
// configured for the PSP webhook integration. actorID may be uuid.Nil
// only in tests — the production receiver MUST pass a real id.
func NewReconciler(repo Repository, log EventLog, actorID uuid.UUID) Reconciler {
	return &reconciler{repo: repo, log: log, actorID: actorID}
}

// Apply implements Reconciler.
func (r *reconciler) Apply(ctx context.Context, evt WebhookEvent) (Outcome, error) {
	if !evt.EventType.IsKnown() {
		return Outcome{}, ErrUnknownEventType
	}
	if evt.ExternalID == "" {
		return Outcome{}, ErrEmptyExternalID
	}

	err := r.log.Record(ctx, evt.Source, evt.ExternalID, evt.EventType, evt.Payload, evt.OccurredAt)
	if err != nil {
		if errors.Is(err, ErrDuplicateEvent) {
			return Outcome{Duplicate: true}, nil
		}
		return Outcome{}, err
	}

	charge, err := r.repo.GetByExternalID(ctx, evt.ExternalID)
	if err != nil {
		return Outcome{}, err
	}

	changed, err := applyEvent(charge, evt.EventType, evt.OccurredAt)
	if err != nil {
		return Outcome{}, err
	}

	if changed {
		if err := r.repo.Save(ctx, charge, r.actorID); err != nil {
			return Outcome{}, err
		}
	}

	return Outcome{Charge: charge, Transitioned: changed}, nil
}

// applyEvent dispatches the webhook event_type to the matching state
// machine method. Kept as a package-level helper so the dispatch table
// has a single home and the reconciler's Apply stays narrow.
func applyEvent(c *PIXCharge, eventType WebhookEventType, now time.Time) (bool, error) {
	switch eventType {
	case WebhookEventPaid:
		return c.MarkPaid(now)
	case WebhookEventExpired:
		// Force-expire via webhook: bypasses the TTL guard because
		// the PSP authoritatively says the charge timed out. We
		// model this by walking through the state machine directly.
		if c.status == StatusPending {
			c.status = StatusExpired
			c.updatedAt = now
			return true, nil
		}
		if c.status == StatusExpired {
			return false, nil
		}
		return false, ErrInvalidTransition
	case WebhookEventCancelled:
		return c.Cancel(now)
	default:
		return false, ErrUnknownEventType
	}
}
