package usecase_test

// fakeRepo is the in-process wallet.Repository implementation used by
// the use-case tests. It models the same SELECT…FOR UPDATE + version
// check + UNIQUE(idempotency_key) invariants the postgres adapter
// implements at the database, so the use-case retry logic can be
// exercised without booting Postgres. The integration suite in
// internal/adapter/db/postgres/wallet/ covers the SQL layer.

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/wallet"
)

type fakeRepo struct {
	mu       sync.Mutex // serialises all access — mirrors row-lock semantics
	wallets  map[uuid.UUID]*walletRow
	byTenant map[uuid.UUID]uuid.UUID // tenantID → walletID
	ledger   []wallet.LedgerEntry

	// Test hooks (deterministic race injection).
	applyEnter func()
	applyHits  atomic.Int64 // counts ApplyWithLock calls; tests assert retry behaviour
}

type walletRow struct {
	id        uuid.UUID
	tenantID  uuid.UUID
	balance   int64
	reserved  int64
	version   int64
	createdAt time.Time
	updatedAt time.Time
}

func newFakeRepo() *fakeRepo {
	return &fakeRepo{
		wallets:  map[uuid.UUID]*walletRow{},
		byTenant: map[uuid.UUID]uuid.UUID{},
	}
}

func (r *fakeRepo) seed(tenantID uuid.UUID, balance int64, now time.Time) uuid.UUID {
	r.mu.Lock()
	defer r.mu.Unlock()
	wid := uuid.New()
	r.wallets[wid] = &walletRow{
		id:        wid,
		tenantID:  tenantID,
		balance:   balance,
		reserved:  0,
		version:   0,
		createdAt: now,
		updatedAt: now,
	}
	r.byTenant[tenantID] = wid
	return wid
}

func (r *fakeRepo) LoadByTenant(ctx context.Context, tenantID uuid.UUID) (*wallet.TokenWallet, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	wid, ok := r.byTenant[tenantID]
	if !ok {
		return nil, wallet.ErrNotFound
	}
	row := r.wallets[wid]
	return wallet.Hydrate(row.id, row.tenantID, row.balance, row.reserved, row.version, row.createdAt, row.updatedAt), nil
}

func (r *fakeRepo) ApplyWithLock(ctx context.Context, w *wallet.TokenWallet, entries []wallet.LedgerEntry) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if r.applyEnter != nil {
		// Called WITHOUT the lock so the test can race a concurrent
		// caller in here before we serialise.
		r.applyEnter()
	}
	r.applyHits.Add(1)

	r.mu.Lock()
	defer r.mu.Unlock()

	row, ok := r.wallets[w.ID()]
	if !ok {
		return wallet.ErrNotFound
	}
	if row.version != w.Version()-1 {
		return wallet.ErrVersionConflict
	}
	// Idempotency check on all entries — the database UNIQUE index
	// would have rejected; we replicate it here.
	for _, e := range entries {
		for _, prior := range r.ledger {
			if prior.WalletID == e.WalletID && prior.IdempotencyKey == e.IdempotencyKey {
				return wallet.ErrIdempotencyConflict
			}
		}
	}
	// Apply.
	row.balance = w.Balance()
	row.reserved = w.Reserved()
	row.version = w.Version()
	row.updatedAt = w.UpdatedAt()
	r.ledger = append(r.ledger, entries...)
	return nil
}

func (r *fakeRepo) LookupByIdempotencyKey(ctx context.Context, tenantID, walletID uuid.UUID, key string) (wallet.LedgerEntry, error) {
	if err := ctx.Err(); err != nil {
		return wallet.LedgerEntry{}, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, e := range r.ledger {
		if e.WalletID == walletID && e.IdempotencyKey == key {
			return e, nil
		}
	}
	return wallet.LedgerEntry{}, wallet.ErrNotFound
}

func (r *fakeRepo) LookupCompletedByExternalRef(ctx context.Context, tenantID, walletID uuid.UUID, externalRef string) (wallet.LedgerEntry, error) {
	if err := ctx.Err(); err != nil {
		return wallet.LedgerEntry{}, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, e := range r.ledger {
		if e.WalletID == walletID && e.ExternalRef == externalRef && (e.Kind == wallet.KindCommit || e.Kind == wallet.KindRelease) {
			return e, nil
		}
	}
	return wallet.LedgerEntry{}, wallet.ErrNotFound
}

func (r *fakeRepo) ListOpenReservations(ctx context.Context, tenantID, walletID uuid.UUID) ([]wallet.LedgerEntry, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	out := []wallet.LedgerEntry{}
	for _, e := range r.ledger {
		if e.WalletID != walletID || e.Kind != wallet.KindReserve {
			continue
		}
		settled := false
		for _, s := range r.ledger {
			if s.WalletID == walletID && s.ExternalRef == e.ExternalRef && (s.Kind == wallet.KindCommit || s.Kind == wallet.KindRelease) {
				settled = true
				break
			}
		}
		if !settled {
			out = append(out, e)
		}
	}
	return out, nil
}

// snapshotBalance returns the (balance, reserved, version) tuple for
// the wallet whose id was returned by seed(). Tests use it for
// invariant assertions after a race.
func (r *fakeRepo) snapshotBalance(walletID uuid.UUID) (int64, int64, int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	row := r.wallets[walletID]
	return row.balance, row.reserved, row.version
}

// ledgerCount returns the number of ledger rows currently stored.
func (r *fakeRepo) ledgerCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.ledger)
}
