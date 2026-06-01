package main

// SIN-62936 — constructor for the master_grant downstream applier
// wired into the C10 grant issuance flow (web/master.WalletGrantPort).
// The applier runs synchronously after the master persists a
// master_grant row:
//
//   - free_subscription_period → extend the active Subscription's
//     current_period_end by N days, no invoice.
//   - extra_tokens             → write a token_ledger grant row with
//     source='master_grant', credit the tenant wallet.
//
// Splitting the wire from the consumer follows the same playbook as
// courtesygrant_wire.go and master_grant_requests_wire.go: the pieces
// here are exported so a future cmd/server integration can plumb the
// C10 grant routes through the applier, with no half-wired path
// reaching production traffic in the meantime.

import (
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	billingadapter "github.com/pericles-luz/crm/internal/adapter/db/postgres/billing"
	walletadapter "github.com/pericles-luz/crm/internal/adapter/db/postgres/wallet"
	"github.com/pericles-luz/crm/internal/billing"
	"github.com/pericles-luz/crm/internal/wallet"
	walletusecase "github.com/pericles-luz/crm/internal/wallet/usecase"
	masterweb "github.com/pericles-luz/crm/internal/web/master"
)

// MasterGrantApplierDeps bundles the collaborators the wire
// constructor needs. The MasterOpsPool is used by both the
// master_grant adapter (writes consumed_at) and the billing
// SubscriptionRepository (writes the extended Subscription). The
// RuntimePool backs the wallet repository whose ApplyWithLock writes
// the token_ledger row for the extra_tokens path.
//
// ActorID is the master_ops user uuid stamped on the audited writes;
// the same value already powers the cap-policy + grants-create row.
//
// GrantsRepo is the pre-built (typically audited) MasterGrantRepository
// the C10 issuance flow uses; the applier shares it so a single audit
// chain spans Create → Consume.
type MasterGrantApplierDeps struct {
	MasterOpsPool *pgxpool.Pool
	RuntimePool   *pgxpool.Pool
	ActorID       uuid.UUID
	GrantsRepo    wallet.MasterGrantRepository
}

// ErrMasterGrantApplierDepsMissing is returned by
// BuildMasterGrantApplier when a required dep is nil/zero so
// cmd/server can fail boot rather than mount a half-wired surface.
var ErrMasterGrantApplierDepsMissing = errors.New("cmd/server: master grant applier dependencies missing")

// BuildMasterGrantApplier constructs the wallet.usecase
// ApplyMasterGrantService over a wallet repository and a billing
// SubscriptionRepository derived from the supplied pools.
//
// The function does NOT mutate the input deps and never returns a
// partially-populated service.
func BuildMasterGrantApplier(deps MasterGrantApplierDeps) (*walletusecase.ApplyMasterGrantService, error) {
	if deps.MasterOpsPool == nil ||
		deps.RuntimePool == nil ||
		deps.ActorID == uuid.Nil ||
		deps.GrantsRepo == nil {
		return nil, ErrMasterGrantApplierDepsMissing
	}
	walletRepo, err := walletadapter.NewRepository(deps.RuntimePool)
	if err != nil {
		return nil, err
	}
	billingStore, err := billingadapter.New(deps.RuntimePool, deps.MasterOpsPool)
	if err != nil {
		return nil, err
	}
	return walletusecase.NewApplyMasterGrantService(
		deps.GrantsRepo,
		walletRepo,
		// billingStore satisfies billing.SubscriptionRepository via
		// the embedded GetByTenant + SaveSubscription methods; the
		// explicit assignment keeps the contract visible at the wire
		// boundary.
		billing.SubscriptionRepository(billingStore),
		nil,
		deps.ActorID,
	)
}

// InstallMasterGrantApplier wires the applier into a WalletGrantPort
// via SetApplier. cmd/server calls this after BuildMasterGrantApplier
// so the C10 grants surface runs the downstream applier inside the
// same HTTP request as the row creation.
//
// A nil port or nil applier is rejected so a misconfigured deploy
// fails at boot rather than silently skipping the apply path.
func InstallMasterGrantApplier(port *masterweb.WalletGrantPort, applier *walletusecase.ApplyMasterGrantService) error {
	if port == nil {
		return errors.New("cmd/server: InstallMasterGrantApplier: port is nil")
	}
	if applier == nil {
		return errors.New("cmd/server: InstallMasterGrantApplier: applier is nil")
	}
	port.SetApplier(applier.Apply)
	return nil
}
