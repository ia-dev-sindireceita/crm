package master_test

// SIN-62936 — additive tests for the WalletGrantPort applier hook.
// Lives in a dedicated file so the existing per-package adapter tests
// in grants_test.go stay untouched.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/wallet"
	"github.com/pericles-luz/crm/internal/web/master"
)

func TestWalletGrantPort_IssueGrant_RunsApplierAfterCreate(t *testing.T) {
	repo := &fakeWalletRepo{}
	port, err := master.NewWalletGrantPort(repo, func() time.Time { return time.Date(2026, 5, 16, 12, 0, 0, 0, time.UTC) })
	if err != nil {
		t.Fatalf("NewWalletGrantPort: %v", err)
	}
	var appliedID uuid.UUID
	port.SetApplier(func(_ context.Context, id uuid.UUID) (bool, error) {
		appliedID = id
		return true, nil
	})
	tenant := uuid.New()
	actor := uuid.New()
	res, err := port.IssueGrant(context.Background(), master.IssueGrantInput{
		ActorUserID: actor,
		TenantID:    tenant,
		Kind:        master.GrantKindExtraTokens,
		Amount:      1000,
		Reason:      "applier hook integration test",
	})
	if err != nil {
		t.Fatalf("IssueGrant: %v", err)
	}
	if appliedID == uuid.Nil {
		t.Fatal("applier hook was not invoked")
	}
	if appliedID != res.Grant.ID {
		t.Errorf("applier called with id %s, want %s (grant ID)", appliedID, res.Grant.ID)
	}
}

func TestWalletGrantPort_IssueGrant_ApplierErrorSurfacedAfterCreate(t *testing.T) {
	repo := &fakeWalletRepo{}
	port, _ := master.NewWalletGrantPort(repo, nil)
	sentinel := errors.New("apply failed")
	port.SetApplier(func(_ context.Context, _ uuid.UUID) (bool, error) {
		return false, sentinel
	})
	_, err := port.IssueGrant(context.Background(), master.IssueGrantInput{
		ActorUserID: uuid.New(),
		TenantID:    uuid.New(),
		Kind:        master.GrantKindExtraTokens,
		Amount:      1000,
		Reason:      "applier failure surfaced to caller",
	})
	if err == nil || !errors.Is(err, sentinel) {
		t.Errorf("IssueGrant: got %v, want apply sentinel surfaced", err)
	}
	if len(repo.rows) != 1 {
		t.Errorf("repo.Create rows = %d, want 1 (row must persist before apply runs)", len(repo.rows))
	}
	// Grant remains pending (consumed_at NULL via fake — Consume not invoked).
	if repo.rows[0].IsConsumed() {
		t.Error("grant must remain consumed=false when applier fails so master can revoke")
	}
}

func TestWalletGrantPort_IssueGrant_NoApplierWiredKeepsLegacyBehaviour(t *testing.T) {
	repo := &fakeWalletRepo{}
	port, _ := master.NewWalletGrantPort(repo, nil)
	// Do NOT SetApplier — exercise the pre-SIN-62936 row-creation-only contract.
	if _, err := port.IssueGrant(context.Background(), master.IssueGrantInput{
		ActorUserID: uuid.New(),
		TenantID:    uuid.New(),
		Kind:        master.GrantKindExtraTokens,
		Amount:      500,
		Reason:      "legacy behaviour without applier wired",
	}); err != nil {
		t.Fatalf("IssueGrant: %v", err)
	}
	if len(repo.rows) != 1 {
		t.Fatalf("row not created; rows=%d", len(repo.rows))
	}
	if repo.rows[0].IsConsumed() {
		t.Error("unexpected consumed=true on row when no applier wired")
	}
}

func TestWalletGrantPort_SetApplier_NilClears(t *testing.T) {
	repo := &fakeWalletRepo{}
	port, _ := master.NewWalletGrantPort(repo, nil)
	port.SetApplier(func(_ context.Context, _ uuid.UUID) (bool, error) {
		return false, errors.New("must not be called")
	})
	port.SetApplier(nil)
	if _, err := port.IssueGrant(context.Background(), master.IssueGrantInput{
		ActorUserID: uuid.New(),
		TenantID:    uuid.New(),
		Kind:        master.GrantKindExtraTokens,
		Amount:      500,
		Reason:      "applier cleared after set then nil",
	}); err != nil {
		t.Fatalf("IssueGrant after SetApplier(nil): %v", err)
	}
	// Use wallet import to keep import set obvious during refactors.
	_ = wallet.SourceMasterGrant
}

// ensure the wallet import is non-trivial in this file to avoid an
// unused-import flap during refactors.
var _ wallet.LedgerSource = wallet.SourceMasterGrant
