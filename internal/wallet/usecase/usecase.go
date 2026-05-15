// Package usecase orchestrates the wallet domain. Each function in
// this package loads the aggregate via the Repository port, applies a
// domain mutation, and persists with ApplyWithLock. The package owns
// retry-on-version-conflict semantics so callers see a single attempt.
//
// The package depends on internal/wallet only — it does not import
// pgx, database/sql, net/http, or any adapter. The orchestration
// layer is the only place that "knows" about retries; the domain
// stays pure.
package usecase

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/wallet"
)

// Clock is the time source. Defaulted to time.Now; tests inject a
// frozen clock to make the version/updated_at fields deterministic.
type Clock func() time.Time

// MaxIdempotencyKeyLen caps the idempotency key length accepted at the
// use-case boundary. The cap is well above the longest key the
// well-behaved callers produce (a UUID is 36 chars; a tenant_id + nonce
// pair fits in <80) and well below the byte budget the UNIQUE index on
// token_ledger.idempotency_key can index without bloating B-tree pages.
// 128 bytes is the same bound the surrounding services use for caller-
// supplied opaque identifiers.
const MaxIdempotencyKeyLen = 128

// validateIdempotencyKey enforces the empty + length checks at the
// boundary. Inlined into every entry point so the failure surfaces
// before any repository call.
func validateIdempotencyKey(key string) error {
	if key == "" {
		return wallet.ErrEmptyIdempotencyKey
	}
	if len(key) > MaxIdempotencyKeyLen {
		return wallet.ErrIdempotencyKeyTooLong
	}
	return nil
}

// maxAttempts caps the optimistic-retry loop on a version conflict
// during ApplyWithLock. The bound is the worst-case adversarial-
// scheduling depth: under the F30 race-test (N=100 concurrent
// reserves against a 50-token wallet), the slowest winner can lose
// up to N-1 races before its UPDATE lands. We pick a multiple of
// that bound so a small additional concurrency burst is absorbed
// without surfacing a misleading "exhausted retries" error.
//
// On real Postgres the SELECT … FOR UPDATE serialises contention at
// the database, so the in-process worst-case is the upper bound; the
// adapter never hits it in practice. The bound exists so a
// pathological tight loop (or a buggy adapter) trips a hard ceiling
// rather than spinning forever.
const maxAttempts = 256

// Service is the wallet use-case orchestrator. It exposes Reserve,
// Commit, Release, and Grant; each uses Repository.LoadByTenant +
// ApplyWithLock and absorbs ErrVersionConflict with a bounded retry.
type Service struct {
	repo  wallet.Repository
	clock Clock
}

// NewService constructs a Service. A nil repo is rejected so the
// caller sees a fast panic at construction rather than a confusing
// nil-deref on the first call. clock defaults to time.Now when nil.
func NewService(repo wallet.Repository, clock Clock) (*Service, error) {
	if repo == nil {
		return nil, errors.New("wallet/usecase: repo is nil")
	}
	if clock == nil {
		clock = time.Now
	}
	return &Service{repo: repo, clock: clock}, nil
}

// Reserve attempts to reserve amount from tenantID's wallet using
// idempotencyKey. On retry with the same key it returns the prior
// reservation (no extra debit, no error) — that is the F37 contract
// that makes LLM call setup safe to repeat.
//
// Returns ErrInsufficientFunds when the available balance is too low,
// ErrIdempotencyConflict when the same key has been used with a
// different amount, and ErrNotFound when no wallet exists for the
// tenant.
func (s *Service) Reserve(ctx context.Context, tenantID uuid.UUID, amount int64, idempotencyKey string) (*wallet.Reservation, error) {
	if tenantID == uuid.Nil {
		return nil, wallet.ErrZeroTenant
	}
	if amount <= 0 {
		return nil, wallet.ErrInvalidAmount
	}
	if err := validateIdempotencyKey(idempotencyKey); err != nil {
		return nil, err
	}

	var lastErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		w, err := s.repo.LoadByTenant(ctx, tenantID)
		if err != nil {
			return nil, err
		}

		// Idempotency short-circuit: if a reserve row with this key
		// already exists on the wallet, return the matching reservation
		// rather than minting a new debit.
		prior, lookupErr := s.repo.LookupByIdempotencyKey(ctx, tenantID, w.ID(), idempotencyKey)
		if lookupErr == nil {
			if prior.Kind != wallet.KindReserve {
				return nil, wallet.ErrIdempotencyConflict
			}
			if -prior.Amount != amount {
				return nil, wallet.ErrIdempotencyConflict
			}
			rid, parseErr := uuid.Parse(prior.ExternalRef)
			if parseErr != nil {
				return nil, fmt.Errorf("wallet/usecase: malformed external_ref on retried reserve: %w", parseErr)
			}
			return &wallet.Reservation{
				ID:             rid,
				WalletID:       prior.WalletID,
				TenantID:       prior.TenantID,
				Amount:         amount,
				IdempotencyKey: idempotencyKey,
				CreatedAt:      prior.OccurredAt,
			}, nil
		}
		if !errors.Is(lookupErr, wallet.ErrNotFound) {
			return nil, lookupErr
		}

		now := s.clock()
		if err := w.Reserve(amount, now); err != nil {
			return nil, err
		}
		rid := uuid.New()
		entry := wallet.LedgerEntry{
			ID:             uuid.New(),
			WalletID:       w.ID(),
			TenantID:       w.TenantID(),
			Kind:           wallet.KindReserve,
			Amount:         wallet.SignedAmount(wallet.KindReserve, amount),
			IdempotencyKey: idempotencyKey,
			ExternalRef:    rid.String(),
			OccurredAt:     now,
			CreatedAt:      now,
		}
		if err := s.repo.ApplyWithLock(ctx, w, []wallet.LedgerEntry{entry}); err != nil {
			if errors.Is(err, wallet.ErrVersionConflict) || errors.Is(err, wallet.ErrIdempotencyConflict) {
				lastErr = err
				continue
			}
			return nil, err
		}
		return &wallet.Reservation{
			ID:             rid,
			WalletID:       w.ID(),
			TenantID:       w.TenantID(),
			Amount:         amount,
			IdempotencyKey: idempotencyKey,
			CreatedAt:      now,
		}, nil
	}
	return nil, fmt.Errorf("wallet/usecase: reserve exhausted retries: %w", lastErr)
}

// Commit consummates a reservation. actualAmount may be smaller than
// the original reservation (the LLM used fewer tokens than the upper
// bound); the difference is released back to available implicitly.
//
// Returns ErrReservationCompleted when the reservation has already
// been committed/released under a different idempotency key.
func (s *Service) Commit(ctx context.Context, r *wallet.Reservation, actualAmount int64, idempotencyKey string) error {
	if r == nil {
		return errors.New("wallet/usecase: reservation is nil")
	}
	if actualAmount <= 0 {
		return wallet.ErrInvalidAmount
	}
	if actualAmount > r.Amount {
		return wallet.ErrInvalidAmount
	}
	if err := validateIdempotencyKey(idempotencyKey); err != nil {
		return err
	}

	var lastErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		w, err := s.repo.LoadByTenant(ctx, r.TenantID)
		if err != nil {
			return err
		}
		if w.ID() != r.WalletID {
			// Defensive: a tenant should only ever have one wallet
			// (UNIQUE(tenant_id) on token_wallet). If the loaded
			// wallet has a different id, the reservation is stale.
			return wallet.ErrNotFound
		}

		// Idempotency short-circuit on the commit's own key.
		prior, lookupErr := s.repo.LookupByIdempotencyKey(ctx, r.TenantID, w.ID(), idempotencyKey)
		if lookupErr == nil {
			if prior.Kind != wallet.KindCommit {
				return wallet.ErrIdempotencyConflict
			}
			if prior.ExternalRef != r.ID.String() {
				return wallet.ErrIdempotencyConflict
			}
			if -prior.Amount != actualAmount {
				return wallet.ErrIdempotencyConflict
			}
			return nil
		}
		if !errors.Is(lookupErr, wallet.ErrNotFound) {
			return lookupErr
		}

		// Reservation-already-settled check (different idempotency key).
		if _, err := s.repo.LookupCompletedByExternalRef(ctx, r.TenantID, w.ID(), r.ID.String()); err == nil {
			return wallet.ErrReservationCompleted
		} else if !errors.Is(err, wallet.ErrNotFound) {
			return err
		}

		now := s.clock()
		if err := w.Commit(actualAmount, r.Amount, now); err != nil {
			if errors.Is(err, wallet.ErrVersionConflict) {
				lastErr = err
				continue
			}
			return err
		}
		entry := wallet.LedgerEntry{
			ID:             uuid.New(),
			WalletID:       w.ID(),
			TenantID:       w.TenantID(),
			Kind:           wallet.KindCommit,
			Amount:         wallet.SignedAmount(wallet.KindCommit, actualAmount),
			IdempotencyKey: idempotencyKey,
			ExternalRef:    r.ID.String(),
			OccurredAt:     now,
			CreatedAt:      now,
		}
		if err := s.repo.ApplyWithLock(ctx, w, []wallet.LedgerEntry{entry}); err != nil {
			if errors.Is(err, wallet.ErrVersionConflict) || errors.Is(err, wallet.ErrIdempotencyConflict) {
				lastErr = err
				continue
			}
			return err
		}
		return nil
	}
	return fmt.Errorf("wallet/usecase: commit exhausted retries: %w", lastErr)
}

// Release rolls a reservation back without debiting. Used when the
// upstream LLM call timed out or errored before producing a billable
// response.
//
// Returns ErrReservationCompleted when the reservation was already
// committed/released under a different idempotency key.
func (s *Service) Release(ctx context.Context, r *wallet.Reservation, idempotencyKey string) error {
	if r == nil {
		return errors.New("wallet/usecase: reservation is nil")
	}
	if err := validateIdempotencyKey(idempotencyKey); err != nil {
		return err
	}

	var lastErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		w, err := s.repo.LoadByTenant(ctx, r.TenantID)
		if err != nil {
			return err
		}
		if w.ID() != r.WalletID {
			return wallet.ErrNotFound
		}

		prior, lookupErr := s.repo.LookupByIdempotencyKey(ctx, r.TenantID, w.ID(), idempotencyKey)
		if lookupErr == nil {
			if prior.Kind != wallet.KindRelease {
				return wallet.ErrIdempotencyConflict
			}
			if prior.ExternalRef != r.ID.String() {
				return wallet.ErrIdempotencyConflict
			}
			if prior.Amount != r.Amount {
				return wallet.ErrIdempotencyConflict
			}
			return nil
		}
		if !errors.Is(lookupErr, wallet.ErrNotFound) {
			return lookupErr
		}

		if _, err := s.repo.LookupCompletedByExternalRef(ctx, r.TenantID, w.ID(), r.ID.String()); err == nil {
			return wallet.ErrReservationCompleted
		} else if !errors.Is(err, wallet.ErrNotFound) {
			return err
		}

		now := s.clock()
		if err := w.Release(r.Amount, now); err != nil {
			if errors.Is(err, wallet.ErrVersionConflict) {
				lastErr = err
				continue
			}
			return err
		}
		entry := wallet.LedgerEntry{
			ID:             uuid.New(),
			WalletID:       w.ID(),
			TenantID:       w.TenantID(),
			Kind:           wallet.KindRelease,
			Amount:         wallet.SignedAmount(wallet.KindRelease, r.Amount),
			IdempotencyKey: idempotencyKey,
			ExternalRef:    r.ID.String(),
			OccurredAt:     now,
			CreatedAt:      now,
		}
		if err := s.repo.ApplyWithLock(ctx, w, []wallet.LedgerEntry{entry}); err != nil {
			if errors.Is(err, wallet.ErrVersionConflict) || errors.Is(err, wallet.ErrIdempotencyConflict) {
				lastErr = err
				continue
			}
			return err
		}
		return nil
	}
	return fmt.Errorf("wallet/usecase: release exhausted retries: %w", lastErr)
}

// Grant credits the wallet (courtesy onboarding, paid top-up).
// externalRef carries the source identifier (e.g. the courtesy_grant
// row id). idempotencyKey makes the operation safe to retry.
func (s *Service) Grant(ctx context.Context, tenantID uuid.UUID, amount int64, idempotencyKey, externalRef string) error {
	if tenantID == uuid.Nil {
		return wallet.ErrZeroTenant
	}
	if amount <= 0 {
		return wallet.ErrInvalidAmount
	}
	if err := validateIdempotencyKey(idempotencyKey); err != nil {
		return err
	}

	var lastErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		w, err := s.repo.LoadByTenant(ctx, tenantID)
		if err != nil {
			return err
		}

		prior, lookupErr := s.repo.LookupByIdempotencyKey(ctx, tenantID, w.ID(), idempotencyKey)
		if lookupErr == nil {
			if prior.Kind != wallet.KindGrant {
				return wallet.ErrIdempotencyConflict
			}
			if prior.Amount != amount {
				return wallet.ErrIdempotencyConflict
			}
			return nil
		}
		if !errors.Is(lookupErr, wallet.ErrNotFound) {
			return lookupErr
		}

		now := s.clock()
		if err := w.Grant(amount, now); err != nil {
			return err
		}
		entry := wallet.LedgerEntry{
			ID:             uuid.New(),
			WalletID:       w.ID(),
			TenantID:       w.TenantID(),
			Kind:           wallet.KindGrant,
			Amount:         wallet.SignedAmount(wallet.KindGrant, amount),
			IdempotencyKey: idempotencyKey,
			ExternalRef:    externalRef,
			OccurredAt:     now,
			CreatedAt:      now,
		}
		if err := s.repo.ApplyWithLock(ctx, w, []wallet.LedgerEntry{entry}); err != nil {
			if errors.Is(err, wallet.ErrVersionConflict) || errors.Is(err, wallet.ErrIdempotencyConflict) {
				lastErr = err
				continue
			}
			return err
		}
		return nil
	}
	return fmt.Errorf("wallet/usecase: grant exhausted retries: %w", lastErr)
}
