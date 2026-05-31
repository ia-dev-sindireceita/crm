// Package walletdebitor adapts internal/wallet/usecase.Service to the
// inbox.WalletDebitor port consumed by inbox/usecase.SendOutbound.
//
// The adapter executes the reserve → charge → commit-or-release cycle
// required by the inbox port contract
// (internal/inbox/port_outbound.go:36-65). The wallet aggregate
// enforces atomicity at the row level (SELECT … FOR UPDATE + optimistic
// version stamp); this layer is only responsible for stitching the
// three calls together with the correct ordering and idempotency keys.
//
// Cost contract (PR4 AC #5):
//
//	When cost == 0 Debit MUST still invoke charge so the outbound flow
//	exercises the bookkeeping path uniformly. wallet.Reserve rejects
//	amount <= 0 with ErrInvalidAmount, so a zero cost short-circuits the
//	reservation but the charge callback still runs.
//
// Idempotency:
//
//	A retried SendOutbound that re-uses the same (tenant_id,
//	conversation_id, message_id) triple yields the same idempotency keys
//	and the wallet collapses duplicate Reserve/Commit/Release rows into
//	"no-op + return the prior result". Callers attach the triple to the
//	context via WithIdempotencyHints before invoking Debit. When the
//	hints are absent the adapter falls back to per-call UUIDs — each
//	individual call still satisfies the wallet contract but retries lose
//	deduplication.
package walletdebitor

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/inbox"
	"github.com/pericles-luz/crm/internal/wallet"
)

// WalletService is the slice of wallet/usecase.Service the adapter
// depends on. Declared as an interface so the unit tests can substitute
// a deterministic fake without booting a real wallet aggregate. Returning
// a narrow interface here also keeps the accept-broad/return-narrow rule
// idiomatic: callers wire *walletusecase.Service in production and the
// type satisfies this interface implicitly.
type WalletService interface {
	Reserve(ctx context.Context, tenantID uuid.UUID, amount int64, idempotencyKey string) (*wallet.Reservation, error)
	Commit(ctx context.Context, r *wallet.Reservation, actualAmount int64, idempotencyKey string) error
	Release(ctx context.Context, r *wallet.Reservation, idempotencyKey string) error
}

// IdempotencyKeyFn produces the per-operation idempotency keys the
// wallet sees for a single Debit call. op is one of "reserve", "commit"
// or "release". Implementations MUST return a non-empty key shorter than
// wallet/usecase.MaxIdempotencyKeyLen (128 bytes).
type IdempotencyKeyFn func(ctx context.Context, tenantID uuid.UUID, op string) string

// Adapter implements inbox.WalletDebitor by delegating to
// wallet/usecase.Service.
type Adapter struct {
	svc            WalletService
	logger         *slog.Logger
	idempotencyKey IdempotencyKeyFn
	newUUID        func() uuid.UUID
}

// Option configures an Adapter at construction.
type Option func(*Adapter)

// WithLogger overrides the *slog.Logger used to record best-effort
// warnings (release-after-charge-failed, commit-after-charge-succeeded).
// A nil logger is ignored.
func WithLogger(l *slog.Logger) Option {
	return func(a *Adapter) {
		if l != nil {
			a.logger = l
		}
	}
}

// WithIdempotencyKeyFn replaces the default idempotency key generator.
// The default reads (conversation_id, message_id) from the context via
// WithIdempotencyHints and falls back to per-call UUIDs. A nil function
// is ignored.
func WithIdempotencyKeyFn(fn IdempotencyKeyFn) Option {
	return func(a *Adapter) {
		if fn != nil {
			a.idempotencyKey = fn
		}
	}
}

// New constructs an Adapter. svc is required; nil is rejected with a
// fast error so misconfiguration surfaces at construction rather than
// the first Debit call.
func New(svc WalletService, opts ...Option) (*Adapter, error) {
	if svc == nil {
		return nil, errors.New("walletdebitor: wallet service must not be nil")
	}
	a := &Adapter{
		svc:     svc,
		logger:  slog.Default(),
		newUUID: uuid.New,
	}
	a.idempotencyKey = a.defaultIdempotencyKey
	for _, opt := range opts {
		if opt != nil {
			opt(a)
		}
	}
	return a, nil
}

// MustNew is the panic-on-error variant for the composition root.
func MustNew(svc WalletService, opts ...Option) *Adapter {
	a, err := New(svc, opts...)
	if err != nil {
		panic(err)
	}
	return a
}

// Debit implements inbox.WalletDebitor. The flow is:
//
//  1. Resolve cost. cost == 0 short-circuits the reservation and runs
//     charge directly so the outbound flow keeps a uniform shape
//     (PR4 AC #5).
//  2. Reserve(cost). On failure return the wallet error verbatim so the
//     use case can map ErrInsufficientFunds to the wire response.
//  3. Invoke charge. On non-nil error Release the reservation and return
//     the original charge error. A Release failure is logged at warn —
//     the F37 reconciler will collect the orphan reservation.
//  4. Commit(cost). A Commit failure after a successful charge is
//     returned wrapped so the caller can distinguish "send failed" from
//     "send succeeded but bookkeeping failed". Commit failure is also
//     logged so operators see the divergence promptly.
func (a *Adapter) Debit(ctx context.Context, tenantID uuid.UUID, cost int64, charge func(ctx context.Context) error) error {
	if charge == nil {
		return errors.New("walletdebitor: charge callback must not be nil")
	}
	if cost < 0 {
		return wallet.ErrInvalidAmount
	}
	if cost == 0 {
		return charge(ctx)
	}

	reserveKey := a.idempotencyKey(ctx, tenantID, opReserve)
	reservation, err := a.svc.Reserve(ctx, tenantID, cost, reserveKey)
	if err != nil {
		return err
	}

	if chargeErr := charge(ctx); chargeErr != nil {
		releaseKey := a.idempotencyKey(ctx, tenantID, opRelease)
		if releaseErr := a.svc.Release(ctx, reservation, releaseKey); releaseErr != nil {
			a.logger.Warn("walletdebitor: release after failed charge failed",
				"err", releaseErr.Error(),
				"tenant_id", tenantID.String(),
				"reservation_id", reservation.ID.String(),
				"amount", cost,
			)
		}
		return chargeErr
	}

	commitKey := a.idempotencyKey(ctx, tenantID, opCommit)
	if commitErr := a.svc.Commit(ctx, reservation, cost, commitKey); commitErr != nil {
		a.logger.Error("walletdebitor: commit after successful charge failed",
			"err", commitErr.Error(),
			"tenant_id", tenantID.String(),
			"reservation_id", reservation.ID.String(),
			"amount", cost,
		)
		return fmt.Errorf("walletdebitor: commit after successful charge: %w", commitErr)
	}
	return nil
}

// Operation tags passed to the IdempotencyKeyFn. The wallet ledger keys
// off (wallet_id, idempotency_key); the operation tag is included in the
// default key to keep reserve/commit/release rows distinct even when the
// caller derives the rest of the key from the same (conversation,
// message) pair.
const (
	opReserve = "reserve"
	opCommit  = "commit"
	opRelease = "release"
)

// defaultIdempotencyKey derives the wallet idempotency key from the
// hints attached to ctx via WithIdempotencyHints. When both
// conversation_id and message_id are present the key is deterministic:
//
//	inbox-send:<op>:<tenant>:<conversation>:<message>
//
// A retried SendOutbound that re-uses the same triple therefore collapses
// to a wallet no-op. When either hint is missing the adapter falls back
// to a per-call UUID so the wallet contract (non-empty key) is still
// honoured; deduplication is lost but each individual call remains
// correct.
func (a *Adapter) defaultIdempotencyKey(ctx context.Context, tenantID uuid.UUID, op string) string {
	if hints, ok := idempotencyHintsFromContext(ctx); ok {
		return fmt.Sprintf("inbox-send:%s:%s:%s:%s",
			op,
			tenantID.String(),
			hints.conversationID.String(),
			hints.messageID.String(),
		)
	}
	return fmt.Sprintf("inbox-send:%s:%s:%s", op, tenantID.String(), a.newUUID().String())
}

// idempotencyHints is the parsed payload attached to the context by
// WithIdempotencyHints. The struct is unexported because external
// packages should never construct it directly — the only blessed entry
// points are WithIdempotencyHints (writer) and the adapter's default key
// generator (reader).
type idempotencyHints struct {
	conversationID uuid.UUID
	messageID      uuid.UUID
}

// hintsKey is a private type used as the context.Value key so it cannot
// collide with any other package's keys.
type hintsKey struct{}

// WithIdempotencyHints attaches (conversationID, messageID) to ctx so
// the default IdempotencyKeyFn can build a stable wallet key. Both IDs
// must be non-zero; otherwise the call returns ctx unchanged and the
// adapter falls back to per-call UUIDs (the caller intentionally opts
// out of idempotency).
//
// Wireup at the SendOutbound layer (PR C) is expected to attach these
// hints from the about-to-be-saved Message before invoking Debit. The
// adapter keeps the helper here so the context key stays private to
// this package.
func WithIdempotencyHints(ctx context.Context, conversationID, messageID uuid.UUID) context.Context {
	if conversationID == uuid.Nil || messageID == uuid.Nil {
		return ctx
	}
	return context.WithValue(ctx, hintsKey{}, idempotencyHints{
		conversationID: conversationID,
		messageID:      messageID,
	})
}

func idempotencyHintsFromContext(ctx context.Context) (idempotencyHints, bool) {
	v, ok := ctx.Value(hintsKey{}).(idempotencyHints)
	return v, ok
}

// Compile-time guarantee that *Adapter satisfies the inbox port.
var _ inbox.WalletDebitor = (*Adapter)(nil)
