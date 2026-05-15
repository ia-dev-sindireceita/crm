package wallet

import (
	"context"
	"errors"

	"github.com/google/uuid"
)

// CourtesyGrantRepository is the persistence port for the
// on-tenant-creation courtesy-grant primitive (SIN-62730, ADR 0093).
//
// Issue MUST run all four statements in a single transaction:
//
//  1. INSERT INTO token_wallet (tenant_id, balance=0, reserved=0)
//  2. INSERT INTO courtesy_grant (tenant_id, amount, granted_by_user_id)
//  3. INSERT INTO token_ledger (wallet_id, tenant_id, kind='grant',
//     amount, idempotency_key='courtesy:'||tenantID, external_ref=grantID)
//  4. UPDATE token_wallet SET balance = $amount, version = 1
//
// The implementation MUST use the master_ops connection: token_wallet
// is INSERTable by app_runtime but courtesy_grant is master_ops-only
// (migration 0089). Running the whole flow under WithMasterOps means
// the same transactional context audits the wallet bootstrap too.
//
// Idempotency is enforced by the database:
//
//   - courtesy_grant has UNIQUE (tenant_id) — a second Issue with the
//     same tenantID lands a 23505 and the adapter returns
//     Issued{Granted: false} with the existing row's wallet/grant IDs.
//   - token_ledger has UNIQUE (wallet_id, idempotency_key) — the
//     ledger row keyed on courtesy:{tenantID} is "defense in depth"
//     against a courtesy_grant row that somehow lost its ledger pair.
//
// Concurrent calls for the same tenantID therefore collapse to a
// single grant; the loser sees Granted=false and returns no error.
type CourtesyGrantRepository interface {
	Issue(ctx context.Context, tenantID, actorID uuid.UUID, amount int64) (Issued, error)
}

// Issued is the outcome of CourtesyGrantRepository.Issue. Granted is
// true when this call wrote the four rows; false when the call was a
// no-op (a prior Issue had already landed for tenantID). The wallet
// and grant IDs are populated in both cases so the caller can attach
// follow-up work to them.
type Issued struct {
	Granted  bool
	WalletID uuid.UUID
	GrantID  uuid.UUID
}

// ErrCourtesyGrantDisabled is returned by IssueCourtesyGrantService
// when the feature flag is off. Callers MUST treat this as a soft
// "skip" rather than a hard failure — tenant creation continues, the
// wallet is just not bootstrapped.
var ErrCourtesyGrantDisabled = errors.New("wallet: courtesy grant disabled by configuration")
