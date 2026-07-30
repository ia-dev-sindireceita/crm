package dispatch

import (
	"context"
	"fmt"
	"sync"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/inbox"
)

// LedgerResult is the record a Ledger keeps for a claimed idempotency
// key. ChannelExternalID is the carrier-assigned id (e.g. Meta wamid)
// captured when the send completed; Done reports whether the send has
// finished (true) or is still in-flight (false).
type LedgerResult struct {
	ChannelExternalID string
	Done              bool
}

// Ledger is the at-most-once bookkeeping port behind Idempotent. It must
// make Claim atomic per (tenantID, key): exactly one concurrent caller
// wins the claim (claimed=true) and is responsible for the carrier call;
// every other caller observes claimed=false plus the winner's current
// LedgerResult.
//
// The composition root wires a concrete adapter. MemoryLedger (below) is
// the in-process default — correct for a single replica and for tests. A
// Redis/Postgres-backed adapter that survives restarts and spans replicas
// is the documented follow-up; the port makes it a drop-in.
type Ledger interface {
	// Claim atomically reserves key for a first-time send. On a fresh
	// key it records an in-flight entry and returns claimed=true. On an
	// already-claimed key it returns claimed=false and the current
	// result (Done=false while the winner's send is in-flight).
	Claim(ctx context.Context, tenantID uuid.UUID, key string) (prior LedgerResult, claimed bool, err error)
	// Complete records a successful send's channel-external-id and marks
	// the entry Done, so subsequent claims of the same key short-circuit
	// to the recorded id without a carrier call.
	Complete(ctx context.Context, tenantID uuid.UUID, key, channelExternalID string) error
	// Release removes an in-flight entry after a failed send so a later
	// retry with the same key is allowed to reach the carrier again.
	Release(ctx context.Context, tenantID uuid.UUID, key string) error
}

// Idempotent decorates an inbox.OutboundChannel so a send is delivered to
// the carrier at most once per (TenantID, IdempotencyKey). It is the
// app-side substitute for the server-side idempotency key Meta's Graph
// API does not provide.
//
//   - Empty OutboundMessage.IdempotencyKey (or a nil Ledger) → passthrough:
//     dedup is disabled and every send hits the carrier, preserving the
//     pre-dispatcher behaviour.
//   - First send for a key → claim, delegate to the carrier, record the
//     wamid on success (Release on failure so a retry can send again).
//   - Repeat send for a completed key → return the recorded wamid with no
//     carrier call (the operator double-submit / requeue case).
//   - Repeat send while the first is still in-flight → inbox.ErrChannelTransient
//     (fail the duplicate rather than risk a second delivery); the
//     original send still lands.
type Idempotent struct {
	inner  inbox.OutboundChannel
	ledger Ledger
}

// NewIdempotent wraps inner with ledger. A nil ledger disables dedup and
// returns inner unchanged.
func NewIdempotent(inner inbox.OutboundChannel, ledger Ledger) inbox.OutboundChannel {
	if ledger == nil {
		return inner
	}
	return &Idempotent{inner: inner, ledger: ledger}
}

// SendMessage implements inbox.OutboundChannel.
func (d *Idempotent) SendMessage(ctx context.Context, m inbox.OutboundMessage) (string, error) {
	if m.IdempotencyKey == "" {
		return d.inner.SendMessage(ctx, m)
	}
	prior, claimed, err := d.ledger.Claim(ctx, m.TenantID, m.IdempotencyKey)
	if err != nil {
		return "", fmt.Errorf("%w: idempotency claim: %v", inbox.ErrChannelTransient, err)
	}
	if !claimed {
		if prior.Done {
			// Duplicate of a completed send: no carrier call.
			return prior.ChannelExternalID, nil
		}
		// A send for this key is already in-flight (near-simultaneous
		// double-submit). Fail this attempt as transient; the original
		// send still delivers exactly once.
		return "", fmt.Errorf("%w: outbound send already in-flight for idempotency key", inbox.ErrChannelTransient)
	}
	wamid, sendErr := d.inner.SendMessage(ctx, m)
	if sendErr != nil {
		// Release so a later retry with the same key can reach the
		// carrier. Best-effort: a Release failure only risks blocking a
		// future retry of an already-failed send.
		_ = d.ledger.Release(ctx, m.TenantID, m.IdempotencyKey)
		return "", sendErr
	}
	if err := d.ledger.Complete(ctx, m.TenantID, m.IdempotencyKey, wamid); err != nil {
		// The carrier accepted the message — that outcome is authoritative.
		// A bookkeeping failure here only risks a future duplicate on
		// retry, never a lost send, so surface success.
		return wamid, nil
	}
	return wamid, nil
}

// Compile-time assertion that Idempotent implements inbox.OutboundChannel.
var _ inbox.OutboundChannel = (*Idempotent)(nil)

// MemoryLedger is the in-process Ledger. It is safe for concurrent use
// and correct for a single replica; it does not survive process restart
// and does not coordinate across replicas (see Ledger for the follow-up).
type MemoryLedger struct {
	mu      sync.Mutex
	entries map[string]LedgerResult
}

// NewMemoryLedger builds an empty in-process ledger.
func NewMemoryLedger() *MemoryLedger {
	return &MemoryLedger{entries: make(map[string]LedgerResult)}
}

func memKey(tenantID uuid.UUID, key string) string {
	return tenantID.String() + "|" + key
}

// Claim implements Ledger.
func (l *MemoryLedger) Claim(_ context.Context, tenantID uuid.UUID, key string) (LedgerResult, bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	mk := memKey(tenantID, key)
	if prior, ok := l.entries[mk]; ok {
		return prior, false, nil
	}
	l.entries[mk] = LedgerResult{Done: false}
	return LedgerResult{}, true, nil
}

// Complete implements Ledger.
func (l *MemoryLedger) Complete(_ context.Context, tenantID uuid.UUID, key, channelExternalID string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.entries[memKey(tenantID, key)] = LedgerResult{ChannelExternalID: channelExternalID, Done: true}
	return nil
}

// Release implements Ledger.
func (l *MemoryLedger) Release(_ context.Context, tenantID uuid.UUID, key string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.entries, memKey(tenantID, key))
	return nil
}

// Compile-time assertion that MemoryLedger implements Ledger.
var _ Ledger = (*MemoryLedger)(nil)
