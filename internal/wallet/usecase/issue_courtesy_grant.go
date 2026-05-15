package usecase

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/wallet"
)

// IssueCourtesyGrantConfig parameterises the courtesy-grant flow.
//
// Amount is the credit issued in tokens (positive). The default at
// the cmd/server layer is 10_000 — adjust via COURTESY_GRANT_TOKENS.
//
// Disabled flips the whole flow off (Issue returns
// wallet.ErrCourtesyGrantDisabled without touching the database) so
// the dev/CI image can skip the bootstrap. Flip via
// COURTESY_GRANT_DISABLED=1.
//
// ActorID is the master_ops user id stamped on every
// master_ops_audit row produced by the underlying INSERTs. Use the
// system/onboarding user id; uuid.Nil is rejected at construction.
type IssueCourtesyGrantConfig struct {
	Amount   int64
	Disabled bool
	ActorID  uuid.UUID
}

// IssueCourtesyGrantService bootstraps a new tenant's wallet by
// emitting the F30 courtesy credit (SIN-62730).
//
// The service is intentionally thin: it validates the call, fans out
// to the CourtesyGrantRepository (which owns the atomic 4-statement
// transaction), and returns a soft "disabled" sentinel when the
// feature flag is off. All idempotency and race-tolerance lives in
// the repository, backed by the UNIQUE constraints on courtesy_grant
// and token_ledger (defense in depth, see migration 0089).
type IssueCourtesyGrantService struct {
	repo wallet.CourtesyGrantRepository
	cfg  IssueCourtesyGrantConfig
}

// NewIssueCourtesyGrantService constructs the service. The repo and a
// strictly positive Amount are required; a uuid.Nil ActorID is
// rejected so the master_ops audit chain always has a non-null actor.
func NewIssueCourtesyGrantService(repo wallet.CourtesyGrantRepository, cfg IssueCourtesyGrantConfig) (*IssueCourtesyGrantService, error) {
	if repo == nil {
		return nil, errors.New("wallet/usecase: courtesy repo is nil")
	}
	if cfg.Amount <= 0 {
		return nil, fmt.Errorf("wallet/usecase: courtesy amount must be positive (got %d)", cfg.Amount)
	}
	if cfg.ActorID == uuid.Nil {
		return nil, errors.New("wallet/usecase: courtesy actor id must not be uuid.Nil")
	}
	return &IssueCourtesyGrantService{repo: repo, cfg: cfg}, nil
}

// Issue bootstraps tenantID's wallet with the configured courtesy
// credit. On the first call for tenantID it returns Issued{Granted:
// true}; on every retry it returns Issued{Granted: false} with the
// existing wallet and grant IDs — that is the no-op contract that
// keeps the tenant-create flow safe to retry.
//
// When the feature is disabled, Issue returns
// wallet.ErrCourtesyGrantDisabled without touching the database; the
// caller MUST treat that as a soft skip and continue.
func (s *IssueCourtesyGrantService) Issue(ctx context.Context, tenantID uuid.UUID) (wallet.Issued, error) {
	if tenantID == uuid.Nil {
		return wallet.Issued{}, wallet.ErrZeroTenant
	}
	if s.cfg.Disabled {
		return wallet.Issued{}, wallet.ErrCourtesyGrantDisabled
	}
	out, err := s.repo.Issue(ctx, tenantID, s.cfg.ActorID, s.cfg.Amount)
	if err != nil {
		return wallet.Issued{}, err
	}
	return out, nil
}

// Amount reports the configured courtesy amount. Exposed so a
// future tenant-create flow can log/audit what it just issued.
func (s *IssueCourtesyGrantService) Amount() int64 { return s.cfg.Amount }
