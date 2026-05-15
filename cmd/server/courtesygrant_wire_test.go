package main

// SIN-62730 — env-parsing checks for the courtesy-grant wire-up. The
// service itself is exercised end-to-end by the
// internal/adapter/db/postgres integration suite; these tests pin
// the boundary contract that cmd/server applies before constructing
// it (default amount, disabled-overrides-actor, malformed inputs
// fail boot).

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/wallet"
	walletusecase "github.com/pericles-luz/crm/internal/wallet/usecase"
)

func mkEnv(pairs map[string]string) func(string) string {
	return func(k string) string { return pairs[k] }
}

func TestBuildCourtesyGrantConfig_DefaultAmount(t *testing.T) {
	t.Parallel()
	actor := uuid.New()
	cfg, err := buildCourtesyGrantConfig(mkEnv(map[string]string{
		envCourtesyActor: actor.String(),
	}))
	if err != nil {
		t.Fatalf("buildCourtesyGrantConfig: %v", err)
	}
	if cfg.Amount != defaultCourtesyAmount {
		t.Errorf("Amount = %d, want default %d", cfg.Amount, defaultCourtesyAmount)
	}
	if cfg.Disabled {
		t.Error("Disabled = true, want false by default")
	}
	if cfg.ActorID != actor {
		t.Errorf("ActorID = %s, want %s", cfg.ActorID, actor)
	}
}

func TestBuildCourtesyGrantConfig_CustomAmount(t *testing.T) {
	t.Parallel()
	actor := uuid.New()
	cfg, err := buildCourtesyGrantConfig(mkEnv(map[string]string{
		envCourtesyActor:  actor.String(),
		envCourtesyAmount: "25000",
	}))
	if err != nil {
		t.Fatalf("buildCourtesyGrantConfig: %v", err)
	}
	if cfg.Amount != 25_000 {
		t.Errorf("Amount = %d, want 25000", cfg.Amount)
	}
}

func TestBuildCourtesyGrantConfig_DisabledAllowsZeroActor(t *testing.T) {
	t.Parallel()
	cfg, err := buildCourtesyGrantConfig(mkEnv(map[string]string{
		envCourtesyDisabled: "1",
	}))
	if err != nil {
		t.Fatalf("buildCourtesyGrantConfig: %v", err)
	}
	if !cfg.Disabled {
		t.Error("Disabled = false, want true")
	}
	if cfg.ActorID != uuid.Nil {
		t.Errorf("ActorID = %s, want uuid.Nil when disabled", cfg.ActorID)
	}
}

func TestBuildCourtesyGrantConfig_EnabledRequiresActor(t *testing.T) {
	t.Parallel()
	_, err := buildCourtesyGrantConfig(mkEnv(map[string]string{}))
	if !errors.Is(err, ErrCourtesyActorRequired) {
		t.Fatalf("missing actor when enabled: got %v, want ErrCourtesyActorRequired", err)
	}
}

func TestBuildCourtesyGrantConfig_RejectsBadAmount(t *testing.T) {
	t.Parallel()
	actor := uuid.New()
	for _, raw := range []string{"abc", "-1", "0"} {
		_, err := buildCourtesyGrantConfig(mkEnv(map[string]string{
			envCourtesyActor:  actor.String(),
			envCourtesyAmount: raw,
		}))
		if err == nil {
			t.Errorf("COURTESY_GRANT_TOKENS=%q: want error, got nil", raw)
		}
	}
}

func TestBuildCourtesyGrantConfig_RejectsBadActor(t *testing.T) {
	t.Parallel()
	_, err := buildCourtesyGrantConfig(mkEnv(map[string]string{
		envCourtesyActor: "not-a-uuid",
	}))
	if err == nil {
		t.Fatal("malformed actor: want error, got nil")
	}
}

func TestBuildCourtesyGrantConfig_RejectsBadDisabled(t *testing.T) {
	t.Parallel()
	_, err := buildCourtesyGrantConfig(mkEnv(map[string]string{
		envCourtesyDisabled: "maybe",
	}))
	if err == nil {
		t.Fatal("malformed disabled flag: want error, got nil")
	}
}

func TestBuildCourtesyGrantService_DisabledReturnsNil(t *testing.T) {
	t.Parallel()
	cfg := walletusecase.IssueCourtesyGrantConfig{
		Amount:   defaultCourtesyAmount,
		Disabled: true,
	}
	svc, err := buildCourtesyGrantService(nil, cfg)
	if err != nil {
		t.Fatalf("buildCourtesyGrantService(disabled): %v", err)
	}
	if svc != nil {
		t.Errorf("disabled cfg should yield nil service, got %v", svc)
	}
}

func TestCourtesyIssuerAdapter_NilSvcIsNoOp(t *testing.T) {
	t.Parallel()
	a := courtesyIssuerAdapter{svc: nil}
	if err := a.IssueCourtesyGrant(t.Context(), uuid.New()); err != nil {
		t.Fatalf("nil svc: got %v, want nil", err)
	}
}

// fakeCourtesyRepo is a minimal in-process implementation used to
// drive the courtesyIssuerAdapter (non-nil svc path) without
// requiring a Postgres connection.
type fakeCourtesyRepo struct {
	seenTenant uuid.UUID
}

func (f *fakeCourtesyRepo) Issue(_ context.Context, tenantID, _ uuid.UUID, _ int64) (wallet.Issued, error) {
	f.seenTenant = tenantID
	return wallet.Issued{Granted: true, WalletID: uuid.New(), GrantID: uuid.New()}, nil
}

func TestCourtesyIssuerAdapter_DelegatesToService(t *testing.T) {
	t.Parallel()
	repo := &fakeCourtesyRepo{}
	svc, err := walletusecase.NewIssueCourtesyGrantService(repo, walletusecase.IssueCourtesyGrantConfig{
		Amount:  defaultCourtesyAmount,
		ActorID: uuid.New(),
	})
	if err != nil {
		t.Fatalf("NewIssueCourtesyGrantService: %v", err)
	}
	a := courtesyIssuerAdapter{svc: svc}
	tid := uuid.New()
	if err := a.IssueCourtesyGrant(t.Context(), tid); err != nil {
		t.Fatalf("IssueCourtesyGrant: %v", err)
	}
	if repo.seenTenant != tid {
		t.Errorf("repo did not see tenant id: got %s, want %s", repo.seenTenant, tid)
	}
}

func TestBuildCourtesyGrantService_NilPoolErrors(t *testing.T) {
	t.Parallel()
	cfg := walletusecase.IssueCourtesyGrantConfig{
		Amount:  defaultCourtesyAmount,
		ActorID: uuid.New(),
	}
	if _, err := buildCourtesyGrantService(nil, cfg); err == nil {
		t.Fatal("buildCourtesyGrantService(nil pool, enabled cfg): want error, got nil")
	}
}
