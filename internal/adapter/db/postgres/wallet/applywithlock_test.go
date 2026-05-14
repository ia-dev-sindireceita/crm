package wallet_test

// Targeted ApplyWithLock branches: NotFound when no wallet row,
// IdempotencyConflict on duplicate ledger key, VersionConflict when
// the persisted version doesn't match the in-memory aggregate, the
// wallet=nil guard, and the wider nullIfEmpty path (an entry with
// empty external_ref).

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	walletadapter "github.com/pericles-luz/crm/internal/adapter/db/postgres/wallet"
	"github.com/pericles-luz/crm/internal/wallet"
)

func TestApplyWithLock_NilWallet(t *testing.T) {
	t.Parallel()
	db, _, _ := freshDB(t)
	repo, _ := walletadapter.NewRepository(db.RuntimePool())
	if err := repo.ApplyWithLock(newCtx(t), nil, nil); err == nil {
		t.Fatal("ApplyWithLock(nil): want error, got nil")
	}
}

func TestApplyWithLock_NotFound(t *testing.T) {
	t.Parallel()
	db, tenantID, _ := freshDB(t)
	repo, _ := walletadapter.NewRepository(db.RuntimePool())
	// Forge a wallet that doesn't exist in the DB.
	now := time.Now()
	ghost := wallet.Hydrate(uuid.New(), tenantID, 0, 0, 1, now, now)
	if err := repo.ApplyWithLock(newCtx(t), ghost, nil); !errors.Is(err, wallet.ErrNotFound) {
		t.Fatalf("ApplyWithLock(ghost): got %v, want ErrNotFound", err)
	}
}

func TestApplyWithLock_VersionConflict(t *testing.T) {
	t.Parallel()
	db, tenantID, masterID := freshDB(t)
	ctx := newCtx(t)
	seedWalletWithBalance(t, ctx, db, tenantID, masterID, 100)
	repo, _ := walletadapter.NewRepository(db.RuntimePool())

	// Load the real row, then forge a wallet at a wrong version.
	w, err := repo.LoadByTenant(ctx, tenantID)
	if err != nil {
		t.Fatalf("LoadByTenant: %v", err)
	}
	// Persisted version is 0; we send version=5 (delta != 1).
	bogus := wallet.Hydrate(w.ID(), w.TenantID(), w.Balance(), w.Reserved(), 5, w.CreatedAt(), w.UpdatedAt())
	if err := repo.ApplyWithLock(ctx, bogus, nil); !errors.Is(err, wallet.ErrVersionConflict) {
		t.Fatalf("ApplyWithLock(version=5 vs persisted=0): got %v, want ErrVersionConflict", err)
	}
}

func TestApplyWithLock_IdempotencyConflict(t *testing.T) {
	t.Parallel()
	db, tenantID, masterID := freshDB(t)
	ctx := newCtx(t)
	seedWalletWithBalance(t, ctx, db, tenantID, masterID, 100)
	repo, _ := walletadapter.NewRepository(db.RuntimePool())

	w, _ := repo.LoadByTenant(ctx, tenantID)
	now := time.Now()
	if err := w.Reserve(10, now); err != nil {
		t.Fatalf("Reserve in memory: %v", err)
	}
	rid := uuid.New()
	entry := wallet.LedgerEntry{
		ID:             uuid.New(),
		WalletID:       w.ID(),
		TenantID:       w.TenantID(),
		Kind:           wallet.KindReserve,
		Amount:         wallet.SignedAmount(wallet.KindReserve, 10),
		IdempotencyKey: "dup",
		ExternalRef:    rid.String(),
		OccurredAt:     now,
		CreatedAt:      now,
	}
	if err := repo.ApplyWithLock(ctx, w, []wallet.LedgerEntry{entry}); err != nil {
		t.Fatalf("first Apply: %v", err)
	}

	// Re-load and try to insert the same idempotency key. Reload so
	// the version stamp matches the DB.
	w2, _ := repo.LoadByTenant(ctx, tenantID)
	if err := w2.Reserve(5, now); err != nil {
		t.Fatalf("Reserve in memory (second): %v", err)
	}
	entry2 := wallet.LedgerEntry{
		ID:             uuid.New(),
		WalletID:       w2.ID(),
		TenantID:       w2.TenantID(),
		Kind:           wallet.KindReserve,
		Amount:         wallet.SignedAmount(wallet.KindReserve, 5),
		IdempotencyKey: "dup", // same key, different amount → still 23505 from the UNIQUE index
		ExternalRef:    uuid.New().String(),
		OccurredAt:     now,
		CreatedAt:      now,
	}
	if err := repo.ApplyWithLock(ctx, w2, []wallet.LedgerEntry{entry2}); !errors.Is(err, wallet.ErrIdempotencyConflict) {
		t.Fatalf("duplicate idempotency key: got %v, want ErrIdempotencyConflict", err)
	}
}

func TestApplyWithLock_EmptyExternalRef(t *testing.T) {
	t.Parallel()
	db, tenantID, masterID := freshDB(t)
	ctx := newCtx(t)
	seedWalletWithBalance(t, ctx, db, tenantID, masterID, 0)
	repo, _ := walletadapter.NewRepository(db.RuntimePool())

	w, _ := repo.LoadByTenant(ctx, tenantID)
	now := time.Now()
	if err := w.Grant(50, now); err != nil {
		t.Fatalf("Grant in memory: %v", err)
	}
	entry := wallet.LedgerEntry{
		ID:             uuid.New(),
		WalletID:       w.ID(),
		TenantID:       w.TenantID(),
		Kind:           wallet.KindGrant,
		Amount:         wallet.SignedAmount(wallet.KindGrant, 50),
		IdempotencyKey: "grant-1",
		ExternalRef:    "", // empty — nullIfEmpty should send NULL
		OccurredAt:     now,
		CreatedAt:      now,
	}
	if err := repo.ApplyWithLock(ctx, w, []wallet.LedgerEntry{entry}); err != nil {
		t.Fatalf("ApplyWithLock with empty external_ref: %v", err)
	}

	// Confirm the DB row has NULL external_ref.
	var hasRef bool
	if err := db.AdminPool().QueryRow(ctx,
		`SELECT external_ref IS NOT NULL FROM token_ledger WHERE idempotency_key = $1`, "grant-1",
	).Scan(&hasRef); err != nil {
		t.Fatalf("read external_ref: %v", err)
	}
	if hasRef {
		t.Error("external_ref was not NULL after empty-string send")
	}
}

func TestApplyWithLock_LedgerInsertNonUniqueError(t *testing.T) {
	t.Parallel()
	db, tenantID, masterID := freshDB(t)
	ctx := newCtx(t)
	seedWalletWithBalance(t, ctx, db, tenantID, masterID, 100)
	repo, _ := walletadapter.NewRepository(db.RuntimePool())

	w, _ := repo.LoadByTenant(ctx, tenantID)
	now := time.Now()
	if err := w.Reserve(10, now); err != nil {
		t.Fatalf("Reserve in memory: %v", err)
	}
	// Use an invalid kind that the table CHECK constraint (added in
	// migration 0089) rejects. This forces a non-23505 error path.
	entry := wallet.LedgerEntry{
		ID:             uuid.New(),
		WalletID:       w.ID(),
		TenantID:       w.TenantID(),
		Kind:           wallet.LedgerKind("bogus"),
		Amount:         -10,
		IdempotencyKey: "bk",
		ExternalRef:    uuid.New().String(),
		OccurredAt:     now,
		CreatedAt:      now,
	}
	err := repo.ApplyWithLock(ctx, w, []wallet.LedgerEntry{entry})
	if err == nil {
		t.Fatal("ApplyWithLock with check-violating kind: want error, got nil")
	}
	if errors.Is(err, wallet.ErrIdempotencyConflict) || errors.Is(err, wallet.ErrVersionConflict) {
		t.Errorf("check violation surfaced as a known sentinel: %v", err)
	}
}

func TestListOpenReservations_ZeroWalletReturnsNil(t *testing.T) {
	t.Parallel()
	db, tenantID, _ := freshDB(t)
	repo, _ := walletadapter.NewRepository(db.RuntimePool())
	got, err := repo.ListOpenReservations(newCtx(t), tenantID, uuid.Nil)
	if err != nil || got != nil {
		t.Errorf("ListOpenReservations(zero wallet): got %v / %v, want nil/nil", got, err)
	}
}
