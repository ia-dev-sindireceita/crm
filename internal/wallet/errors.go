// Package wallet is the token-economy domain (SIN-62727).
//
// The package owns the TokenWallet aggregate, its ledger, and the
// invariants that protect tenants from negative balances or double-debit
// races. Storage lives behind the Repository port; the postgres adapter
// in internal/adapter/db/postgres/wallet is the only blessed
// implementation. Domain code MUST stay free of database/sql, pgx,
// net/http, and other infrastructure imports — the forbidimport
// analyzer (tools/lint/forbidimport) enforces that on CI.
//
// Concurrency model (F30 — atomic reserve, ADR 0088):
//
//   - Reserve is a two-phase debit: it bumps `reserved` instead of
//     `balance`, leaving available capacity = balance - reserved.
//     A subsequent Commit decrements both reserved and balance by the
//     actual amount consumed; Release rolls reserved back without
//     touching balance.
//   - Concurrent Reserves race through a SELECT … FOR UPDATE in the
//     postgres adapter. The domain expresses the lock as a version
//     stamp: every mutating operation bumps `version`, and the adapter
//     refuses the UPDATE if the row's version has moved since the load.
//     Defense in depth: the table also carries CHECK (balance >= 0)
//     and CHECK (reserved >= 0) so even a buggy adapter cannot push
//     the row into an inconsistent state (migration 0089).
//
// Idempotency (F37 — commit-after-LLM resilience, ADR 0088):
//
//   - Every state-changing call takes an idempotency key. The ledger
//     enforces UNIQUE (wallet_id, idempotency_key) at the database
//     layer, so a retried Commit collapses to "no-op + return the
//     prior result" rather than double-debiting.
package wallet

import "errors"

// ErrInsufficientFunds is returned by Reserve when the wallet's
// available balance (balance - reserved) is less than the requested
// amount. Callers map this to "insufficient credits" on the wire.
var ErrInsufficientFunds = errors.New("wallet: insufficient funds")

// ErrNotFound is returned by the repository when a wallet does not
// exist for the requested tenant, or when a reservation lookup misses.
// Adapters MUST translate "no rows" to this sentinel so domain code can
// match with errors.Is instead of importing pgx.
var ErrNotFound = errors.New("wallet: not found")

// ErrIdempotencyConflict is returned by the repository when the same
// idempotency key has already been used with different parameters
// (different amount, different operation kind). Adapters MUST detect
// the conflict by reading the matching ledger row and comparing the
// canonical fields. A pure re-submission of the same parameters
// returns the prior result and no error — that is the contract that
// makes LLM retries safe.
var ErrIdempotencyConflict = errors.New("wallet: idempotency key reused with different parameters")

// ErrInvalidAmount is returned when a caller passes an amount <= 0 to
// Reserve / Commit / Release / Grant. Negative balances are forbidden
// by both the domain invariant and the CHECK constraint on
// token_wallet; we never accept the opposite sign at the boundary.
var ErrInvalidAmount = errors.New("wallet: amount must be positive")

// ErrEmptyIdempotencyKey is returned when a caller omits the
// idempotency key. The UNIQUE index on token_ledger requires a
// non-NULL key for every wallet-aware row, so an empty key would
// trip the database. We reject earlier so the message is actionable.
var ErrEmptyIdempotencyKey = errors.New("wallet: idempotency key must not be empty")

// ErrZeroTenant is returned when uuid.Nil is passed where a tenant id
// is required. Domain code never trusts uuid.Nil as a sentinel.
var ErrZeroTenant = errors.New("wallet: tenant id must not be uuid.Nil")

// ErrReservationCompleted is returned by Commit/Release when the
// reservation has already been committed or released. The repository
// detects this by looking for the matching reserve ledger row and
// then a follow-up commit/release row with the same external_ref.
// Combined with the idempotency check, this means: a true retry of
// the SAME (reservation_id, idempotency_key) pair returns the prior
// result; a DIFFERENT idempotency_key against a completed reservation
// surfaces this error.
var ErrReservationCompleted = errors.New("wallet: reservation already completed")

// ErrVersionConflict is returned by the repository when an optimistic
// version check fails. Callers retry by re-loading the wallet (the
// Reserve use-case does this transparently). Surface here so tests
// can assert the retry path.
var ErrVersionConflict = errors.New("wallet: optimistic version conflict")
