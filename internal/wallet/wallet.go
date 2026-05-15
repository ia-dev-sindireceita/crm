package wallet

import (
	"math"
	"time"

	"github.com/google/uuid"
)

// TokenWallet is the per-tenant aggregate holding both the available
// balance and the currently-reserved amount. It maps 1:1 to the
// token_wallet row (migration 0089).
//
// Invariants enforced by the methods on this type:
//
//  1. balance >= 0          (never overdraw)
//  2. reserved >= 0         (never under-reserve)
//  3. reserved <= balance   (never reserve what you don't have)
//
// The database carries the same CHECKs as a defense-in-depth layer:
// even if a buggy adapter or a hand-typed SQL statement skipped the
// domain, Postgres still refuses to commit a negative-balance row.
type TokenWallet struct {
	id        uuid.UUID
	tenantID  uuid.UUID
	balance   int64
	reserved  int64
	version   int64
	createdAt time.Time
	updatedAt time.Time
}

// Hydrator is the named constructor for rebuilding a TokenWallet from
// durable state. It exists as a typed builder rather than a bare
// package-level function so call sites are syntactically visible:
// callers must spell out wallet.NewHydrator().Hydrate(...) instead of
// the easier-to-misuse wallet.Hydrate(...). The compiler does not
// otherwise enforce the "adapters only" rule; this builder is a
// defense-in-depth layer on top of the existing invariant guards
// (New, RLS, version stamp, DB CHECK).
type Hydrator struct{}

// NewHydrator returns the canonical Hydrator. The type is stateless
// today; the constructor exists so adapters can hold it as a field
// and future wiring (audit logging, tracing) has a single seam.
func NewHydrator() Hydrator { return Hydrator{} }

// Hydrate reconstructs a TokenWallet from durable state. Only adapters
// should reach this path; passing trusted persisted values bypasses
// the invariants that New enforces because the database already vetted
// them (the CHECK constraints from migration 0089). Callers that want
// to construct a fresh wallet for a brand-new tenant should use New.
func (Hydrator) Hydrate(id, tenantID uuid.UUID, balance, reserved, version int64, createdAt, updatedAt time.Time) *TokenWallet {
	return hydrate(id, tenantID, balance, reserved, version, createdAt, updatedAt)
}

func hydrate(id, tenantID uuid.UUID, balance, reserved, version int64, createdAt, updatedAt time.Time) *TokenWallet {
	return &TokenWallet{
		id:        id,
		tenantID:  tenantID,
		balance:   balance,
		reserved:  reserved,
		version:   version,
		createdAt: createdAt,
		updatedAt: updatedAt,
	}
}

// New constructs an empty wallet for tenantID. version starts at 0;
// the next ApplyWithLock bumps it to 1.
func New(tenantID uuid.UUID, now time.Time) (*TokenWallet, error) {
	if tenantID == uuid.Nil {
		return nil, ErrZeroTenant
	}
	return &TokenWallet{
		id:        uuid.New(),
		tenantID:  tenantID,
		version:   0,
		createdAt: now,
		updatedAt: now,
	}, nil
}

// ID is the wallet's primary key.
func (w *TokenWallet) ID() uuid.UUID { return w.id }

// TenantID is the wallet's tenant scope.
func (w *TokenWallet) TenantID() uuid.UUID { return w.tenantID }

// Balance is the gross wallet balance, before reservations.
func (w *TokenWallet) Balance() int64 { return w.balance }

// Reserved is the amount currently held by in-flight reservations.
func (w *TokenWallet) Reserved() int64 { return w.reserved }

// Available is balance minus reserved — the amount that may be
// reserved by the next caller.
func (w *TokenWallet) Available() int64 { return w.balance - w.reserved }

// Version is the optimistic-lock stamp. Every mutating call bumps it.
func (w *TokenWallet) Version() int64 { return w.version }

// CreatedAt is the wallet row's creation timestamp.
func (w *TokenWallet) CreatedAt() time.Time { return w.createdAt }

// UpdatedAt is the wallet row's last-touch timestamp. The postgres
// adapter refreshes it via the BEFORE UPDATE trigger created in
// migration 0090 (this PR); the domain mirrors the new value so
// in-memory state stays consistent with the row after ApplyWithLock.
func (w *TokenWallet) UpdatedAt() time.Time { return w.updatedAt }

// Reserve increases reserved by amount. It returns ErrInsufficientFunds
// when balance - reserved < amount, leaving the wallet unchanged.
//
// The method does not write to durable storage: the use-case orchestrator
// pairs the in-memory mutation with a ledger entry and Repository.ApplyWithLock
// to make the change atomic at the database layer.
func (w *TokenWallet) Reserve(amount int64, now time.Time) error {
	if amount <= 0 {
		return ErrInvalidAmount
	}
	if w.balance-w.reserved < amount {
		return ErrInsufficientFunds
	}
	w.reserved += amount
	w.version++
	w.updatedAt = now
	return nil
}

// Commit decrements both reserved and balance by amount. The amount
// MAY be smaller than the original reservation (the LLM consumed less
// than the upper bound) — the difference is released by the same call,
// no second Release needed.
//
// commitAmount must be in (0, reservedAtCall]. The use-case is
// responsible for passing the per-reservation upper bound; the
// aggregate only enforces that we never debit more than is currently
// reserved.
func (w *TokenWallet) Commit(commitAmount, reservedAtCall int64, now time.Time) error {
	if commitAmount <= 0 {
		return ErrInvalidAmount
	}
	if reservedAtCall <= 0 {
		return ErrInvalidAmount
	}
	if commitAmount > reservedAtCall {
		// Commit is bounded by the reservation; over-commit means the
		// caller mis-tracked the original reserve and we MUST refuse.
		return ErrInvalidAmount
	}
	if w.reserved < reservedAtCall {
		// In-memory state is out of date — re-load and retry. The
		// version conflict is the canonical way the use-case detects
		// this and reloads.
		return ErrVersionConflict
	}
	// Atomically: balance -= commitAmount, reserved -= reservedAtCall.
	// The released portion (reservedAtCall - commitAmount) is implicitly
	// returned to the available pool.
	w.balance -= commitAmount
	w.reserved -= reservedAtCall
	w.version++
	w.updatedAt = now
	return nil
}

// Release rolls a reservation back without debiting the balance.
// Used for LLM timeouts and upstream errors (F37). The amount must be
// exactly the original reservation's amount; partial releases are
// expressed as a Commit with a smaller commitAmount.
func (w *TokenWallet) Release(amount int64, now time.Time) error {
	if amount <= 0 {
		return ErrInvalidAmount
	}
	if w.reserved < amount {
		return ErrVersionConflict
	}
	w.reserved -= amount
	w.version++
	w.updatedAt = now
	return nil
}

// Grant credits the wallet (courtesy onboarding, paid top-up). The
// caller decides where the funds came from; the ledger row carries
// the source as external_ref / kind. Negative grants are not allowed —
// debits must go through Reserve+Commit so they are guarded by the
// available-balance check.
//
// Grant refuses to wrap int64. Without the overflow check, a sufficiently
// large amount could push balance past math.MaxInt64 and into negative
// territory, where the database CHECK (balance >= 0) catches the wrap
// post-update. That second-line defense surfaces as an opaque pg CHECK
// violation; rejecting at the domain returns the actionable
// ErrInvalidAmount so callers can map it to "amount too large".
func (w *TokenWallet) Grant(amount int64, now time.Time) error {
	if amount <= 0 {
		return ErrInvalidAmount
	}
	if amount > math.MaxInt64-w.balance {
		return ErrInvalidAmount
	}
	w.balance += amount
	w.version++
	w.updatedAt = now
	return nil
}
