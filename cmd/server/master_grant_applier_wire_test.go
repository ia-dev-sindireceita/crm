package main

// SIN-62936 — boot-time wire tests for the master grant applier.
// Mirrors the master_grant_requests_wire_test.go pattern: deps-
// missing fast-fail + Install*-side null guards. The happy-path
// constructor against a real pool is exercised by the integration
// tests in internal/adapter/db/postgres/wallet_master_grant_apply_test.go.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/wallet"
	masterweb "github.com/pericles-luz/crm/internal/web/master"
)

type stubGrantRepo struct{}

func (stubGrantRepo) Create(context.Context, *wallet.MasterGrant) error { return nil }
func (stubGrantRepo) GetByID(context.Context, uuid.UUID) (*wallet.MasterGrant, error) {
	return nil, wallet.ErrNotFound
}
func (stubGrantRepo) ListByTenant(context.Context, uuid.UUID) ([]*wallet.MasterGrant, error) {
	return nil, nil
}
func (stubGrantRepo) Revoke(context.Context, uuid.UUID, uuid.UUID, string, time.Time) error {
	return nil
}
func (stubGrantRepo) Consume(context.Context, uuid.UUID, string, time.Time) error { return nil }

func TestBuildMasterGrantApplier_RejectsMissingDeps(t *testing.T) {
	t.Parallel()
	base := MasterGrantApplierDeps{
		MasterOpsPool: nil, // every case keeps this nil; a real pool needs a DB
		RuntimePool:   nil,
		ActorID:       uuid.New(),
		GrantsRepo:    stubGrantRepo{},
	}
	cases := []struct {
		name   string
		mutate func(*MasterGrantApplierDeps)
	}{
		{"missing master pool", func(d *MasterGrantApplierDeps) { d.MasterOpsPool = nil }},
		{"missing runtime pool", func(d *MasterGrantApplierDeps) { d.RuntimePool = nil }},
		{"missing actor", func(d *MasterGrantApplierDeps) { d.ActorID = uuid.Nil }},
		{"missing repo", func(d *MasterGrantApplierDeps) { d.GrantsRepo = nil }},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d := base
			tc.mutate(&d)
			_, err := BuildMasterGrantApplier(d)
			if !errors.Is(err, ErrMasterGrantApplierDepsMissing) {
				t.Fatalf("err = %v, want ErrMasterGrantApplierDepsMissing", err)
			}
		})
	}
}

func TestInstallMasterGrantApplier_NullGuards(t *testing.T) {
	t.Parallel()
	port, err := masterweb.NewWalletGrantPort(stubGrantRepo{}, nil)
	if err != nil {
		t.Fatalf("NewWalletGrantPort: %v", err)
	}
	if err := InstallMasterGrantApplier(nil, nil); err == nil {
		t.Error("nil port: want error")
	}
	if err := InstallMasterGrantApplier(port, nil); err == nil {
		t.Error("nil applier: want error")
	}
}
