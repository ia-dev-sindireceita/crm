package metashared

import (
	"context"
	"errors"
)

// ErrAlreadyProcessed indicates that a (channel, channelExternalID)
// pair has already been claimed by an earlier Claim call. Callers MUST
// treat this as the idempotency success path under retry (the carrier
// will keep retrying the same wamid / mid until we ACK; emitting a
// second downstream side-effect is the bug the dedup ledger prevents).
var ErrAlreadyProcessed = errors.New("metashared: inbound already processed")

// Deduper is the port the inbound webhook handlers consult to guard
// the `inbound_message_dedup` ledger (ADR 0087 / migration 0088). The
// ledger is a global (channel, channel_external_id) UNIQUE store that
// runs BEFORE tenant context is resolved — Meta does not include a
// tenant id in the inbound envelope, so the dedup decision must work
// without one.
//
// The two-phase shape mirrors the table's `processed_at` semantics:
//
//   - Claim returns nil on first-time success, ErrAlreadyProcessed on
//     duplicate, any other error on infrastructure failure (network /
//     constraint not named here / etc.). The caller wraps this in
//     errors.Is to distinguish the two non-nil paths.
//   - MarkProcessed flips processed_at = now() once the downstream
//     side-effects (message insert + wallet debit) have committed. A
//     handler that crashes between Claim and MarkProcessed leaves a
//     row with processed_at NULL; the 0075d_gc_jobs collector
//     reclaims those after the documented window.
//
// The Postgres-backed implementation is the existing inbox store
// (internal/adapter/db/postgres/inbox) reused via the bridge in
// internal/adapter/store/postgres/metadedup. The interface lives here
// so Meta-channel adapters (Instagram, Messenger) can take a
// metashared.Deduper without importing inbox storage internals.
type Deduper interface {
	Claim(ctx context.Context, channel, channelExternalID string) error
	MarkProcessed(ctx context.Context, channel, channelExternalID string) error
}
