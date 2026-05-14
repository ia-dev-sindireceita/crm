package usecase_test

// buggyRepo wraps fakeRepo and lets a test substitute one method with
// a custom behaviour (return a specific error, force a version
// conflict on the Nth call, etc). Each field is a hook that, when
// non-nil, replaces the wrapped fakeRepo method. The goal is to drive
// the use-case's error-handling branches deterministically.

import (
	"context"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/wallet"
)

type buggyRepo struct {
	inner *fakeRepo

	loadByTenant                 func(context.Context, uuid.UUID) (*wallet.TokenWallet, error)
	applyWithLock                func(context.Context, *wallet.TokenWallet, []wallet.LedgerEntry) error
	lookupByIdempotencyKey       func(context.Context, uuid.UUID, uuid.UUID, string) (wallet.LedgerEntry, error)
	lookupCompletedByExternalRef func(context.Context, uuid.UUID, uuid.UUID, string) (wallet.LedgerEntry, error)
	listOpenReservations         func(context.Context, uuid.UUID, uuid.UUID) ([]wallet.LedgerEntry, error)
}

func newBuggyRepo(inner *fakeRepo) *buggyRepo {
	return &buggyRepo{inner: inner}
}

func (b *buggyRepo) LoadByTenant(ctx context.Context, tenantID uuid.UUID) (*wallet.TokenWallet, error) {
	if b.loadByTenant != nil {
		return b.loadByTenant(ctx, tenantID)
	}
	return b.inner.LoadByTenant(ctx, tenantID)
}

func (b *buggyRepo) ApplyWithLock(ctx context.Context, w *wallet.TokenWallet, entries []wallet.LedgerEntry) error {
	if b.applyWithLock != nil {
		return b.applyWithLock(ctx, w, entries)
	}
	return b.inner.ApplyWithLock(ctx, w, entries)
}

func (b *buggyRepo) LookupByIdempotencyKey(ctx context.Context, tenantID, walletID uuid.UUID, key string) (wallet.LedgerEntry, error) {
	if b.lookupByIdempotencyKey != nil {
		return b.lookupByIdempotencyKey(ctx, tenantID, walletID, key)
	}
	return b.inner.LookupByIdempotencyKey(ctx, tenantID, walletID, key)
}

func (b *buggyRepo) LookupCompletedByExternalRef(ctx context.Context, tenantID, walletID uuid.UUID, externalRef string) (wallet.LedgerEntry, error) {
	if b.lookupCompletedByExternalRef != nil {
		return b.lookupCompletedByExternalRef(ctx, tenantID, walletID, externalRef)
	}
	return b.inner.LookupCompletedByExternalRef(ctx, tenantID, walletID, externalRef)
}

func (b *buggyRepo) ListOpenReservations(ctx context.Context, tenantID, walletID uuid.UUID) ([]wallet.LedgerEntry, error) {
	if b.listOpenReservations != nil {
		return b.listOpenReservations(ctx, tenantID, walletID)
	}
	return b.inner.ListOpenReservations(ctx, tenantID, walletID)
}
