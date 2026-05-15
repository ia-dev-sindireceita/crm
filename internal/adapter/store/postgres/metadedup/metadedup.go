// Package metadedup bridges the existing Postgres inbox store
// (internal/adapter/db/postgres/inbox) to the metashared.Deduper port.
// The composition root wires this around a *pginbox.Store so Meta
// channels can take a metashared.Deduper without coupling to the
// inbox storage package or its domain sentinel.
//
// The bridge is intentionally thin: it forwards Claim / MarkProcessed
// to the underlying store and translates inbox.ErrInboundAlreadyProcessed
// into metashared.ErrAlreadyProcessed. No new SQL — the ledger remains
// the single `inbound_message_dedup` table from migration 0088
// (SIN-62791 / Fase 1 reuse).
package metadedup

import (
	"context"
	"errors"

	"github.com/pericles-luz/crm/internal/adapter/channels/metashared"
	pginbox "github.com/pericles-luz/crm/internal/adapter/db/postgres/inbox"
	"github.com/pericles-luz/crm/internal/inbox"
)

// inboxDeduper is the narrow shape the bridge needs from the inbox
// adapter — only Claim and MarkProcessed. Accept-broad / return-narrow:
// the production wiring passes a *pginbox.Store, but the bridge does
// not depend on the rest of that struct so we can unit-test the error
// remap without standing up a Postgres cluster.
type inboxDeduper interface {
	Claim(ctx context.Context, channel, channelExternalID string) error
	MarkProcessed(ctx context.Context, channel, channelExternalID string) error
}

// Compile-time check: the production inbox store satisfies the narrow
// inner port. If the inbox adapter signature drifts, this fails fast.
var _ inboxDeduper = (*pginbox.Store)(nil)

// Store implements metashared.Deduper by delegating to an inboxDeduper.
type Store struct {
	inner inboxDeduper
}

// New constructs a Store. A nil inner is a programming error in the
// composition root; we surface it loudly so cmd/server fails fast.
func New(inner inboxDeduper) (*Store, error) {
	if inner == nil {
		return nil, errors.New("metadedup: inner inbox store is nil")
	}
	return &Store{inner: inner}, nil
}

// Claim forwards to the inbox store and rewrites the duplicate sentinel
// so callers depending on metashared.Deduper can rely on
// metashared.ErrAlreadyProcessed via errors.Is.
func (s *Store) Claim(ctx context.Context, channel, channelExternalID string) error {
	err := s.inner.Claim(ctx, channel, channelExternalID)
	if errors.Is(err, inbox.ErrInboundAlreadyProcessed) {
		return metashared.ErrAlreadyProcessed
	}
	return err
}

// MarkProcessed forwards directly; there is no error-sentinel mismatch
// on this branch — the inbox store returns inbox.ErrNotFound when no
// row exists, and callers of metashared.Deduper documenting their
// reaction to that case keep the wrapping.
func (s *Store) MarkProcessed(ctx context.Context, channel, channelExternalID string) error {
	return s.inner.MarkProcessed(ctx, channel, channelExternalID)
}

// Compile-time check: Store satisfies the port.
var _ metashared.Deduper = (*Store)(nil)
