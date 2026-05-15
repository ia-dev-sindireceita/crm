package usecase_test

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/wallet"
	"github.com/pericles-luz/crm/internal/wallet/usecase"
)

// countingRepo wraps a wallet.Repository and counts LoadByTenant calls.
// Defined locally to this test file so the existing fakeRepo stays
// untouched.
type countingRepo struct {
	wallet.Repository
	loads atomic.Int64
}

func (c *countingRepo) LoadByTenant(ctx context.Context, tenantID uuid.UUID) (*wallet.TokenWallet, error) {
	c.loads.Add(1)
	return c.Repository.LoadByTenant(ctx, tenantID)
}

// TestIdempotencyKey_LengthCap encodes finding #3 from SIN-62748: each
// entry point on Service MUST reject an over-long idempotency key with
// ErrIdempotencyKeyTooLong before reaching the repository. The cap is
// usecase.MaxIdempotencyKeyLen (128 bytes today); the test asserts the
// exact boundary at len = cap (accepted) and len = cap+1 (rejected).
func TestIdempotencyKey_LengthCap(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	tid := uuid.New()

	atCap := strings.Repeat("k", usecase.MaxIdempotencyKeyLen)
	tooLong := strings.Repeat("k", usecase.MaxIdempotencyKeyLen+1)

	repo := newFakeRepo()
	repo.seed(tid, 100, fixedTime)
	svc := newSvc(t, repo)

	// Reserve at the boundary --------------------------------------
	if _, err := svc.Reserve(ctx, tid, 1, atCap); err != nil {
		t.Errorf("Reserve(len=MaxIdempotencyKeyLen): got %v, want nil", err)
	}
	if _, err := svc.Reserve(ctx, tid, 1, tooLong); !errors.Is(err, wallet.ErrIdempotencyKeyTooLong) {
		t.Errorf("Reserve(len=MaxIdempotencyKeyLen+1): got %v, want ErrIdempotencyKeyTooLong", err)
	}

	// Commit -------------------------------------------------------
	res, err := svc.Reserve(ctx, tid, 1, "rsv-cap")
	if err != nil {
		t.Fatalf("seed Reserve: %v", err)
	}
	if err := svc.Commit(ctx, res, 1, tooLong); !errors.Is(err, wallet.ErrIdempotencyKeyTooLong) {
		t.Errorf("Commit(too-long key): got %v, want ErrIdempotencyKeyTooLong", err)
	}

	// Release ------------------------------------------------------
	res2, err := svc.Reserve(ctx, tid, 1, "rsv-cap2")
	if err != nil {
		t.Fatalf("seed Reserve 2: %v", err)
	}
	if err := svc.Release(ctx, res2, tooLong); !errors.Is(err, wallet.ErrIdempotencyKeyTooLong) {
		t.Errorf("Release(too-long key): got %v, want ErrIdempotencyKeyTooLong", err)
	}

	// Grant --------------------------------------------------------
	if err := svc.Grant(ctx, tid, 5, tooLong, "src-X"); !errors.Is(err, wallet.ErrIdempotencyKeyTooLong) {
		t.Errorf("Grant(too-long key): got %v, want ErrIdempotencyKeyTooLong", err)
	}
}

// TestIdempotencyKey_OverlongRejectedBeforeRepo asserts the rejection
// fires at the boundary, never reaching the Repository. This matters
// because the whole point of the cap is to keep an attacker from
// forcing the database UNIQUE index to absorb a multi-kilobyte key.
func TestIdempotencyKey_OverlongRejectedBeforeRepo(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	tid := uuid.New()

	inner := newFakeRepo()
	inner.seed(tid, 100, fixedTime)
	repo := &countingRepo{Repository: inner}
	svc := newSvc(t, repo)

	tooLong := strings.Repeat("k", usecase.MaxIdempotencyKeyLen+1)

	t.Run("Reserve", func(t *testing.T) {
		before := repo.loads.Load()
		if _, err := svc.Reserve(ctx, tid, 1, tooLong); !errors.Is(err, wallet.ErrIdempotencyKeyTooLong) {
			t.Errorf("got %v, want ErrIdempotencyKeyTooLong", err)
		}
		if got := repo.loads.Load() - before; got != 0 {
			t.Errorf("Reserve(too-long) reached repository: LoadByTenant delta = %d, want 0", got)
		}
	})

	// Seed a real reservation to drive Commit/Release.
	res, err := svc.Reserve(ctx, tid, 1, "rsv")
	if err != nil {
		t.Fatalf("seed Reserve: %v", err)
	}

	t.Run("Commit", func(t *testing.T) {
		before := repo.loads.Load()
		if err := svc.Commit(ctx, res, 1, tooLong); !errors.Is(err, wallet.ErrIdempotencyKeyTooLong) {
			t.Errorf("got %v, want ErrIdempotencyKeyTooLong", err)
		}
		if got := repo.loads.Load() - before; got != 0 {
			t.Errorf("Commit(too-long) reached repository: LoadByTenant delta = %d, want 0", got)
		}
	})

	res2, err := svc.Reserve(ctx, tid, 1, "rsv2")
	if err != nil {
		t.Fatalf("seed Reserve 2: %v", err)
	}

	t.Run("Release", func(t *testing.T) {
		before := repo.loads.Load()
		if err := svc.Release(ctx, res2, tooLong); !errors.Is(err, wallet.ErrIdempotencyKeyTooLong) {
			t.Errorf("got %v, want ErrIdempotencyKeyTooLong", err)
		}
		if got := repo.loads.Load() - before; got != 0 {
			t.Errorf("Release(too-long) reached repository: LoadByTenant delta = %d, want 0", got)
		}
	})

	t.Run("Grant", func(t *testing.T) {
		before := repo.loads.Load()
		if err := svc.Grant(ctx, tid, 5, tooLong, "src"); !errors.Is(err, wallet.ErrIdempotencyKeyTooLong) {
			t.Errorf("got %v, want ErrIdempotencyKeyTooLong", err)
		}
		if got := repo.loads.Load() - before; got != 0 {
			t.Errorf("Grant(too-long) reached repository: LoadByTenant delta = %d, want 0", got)
		}
	})
}
