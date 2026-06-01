package usecase

// SIN-62936 — synchronous applier for master_grant rows. Runs after
// the master UI persists the grant + audit row (SIN-62884 / C10) and
// closes the loop by:
//
//   - free_subscription_period: extend the tenant's active
//     Subscription.current_period_end by N days and persist; NO
//     invoice is created (that's what distinguishes a courtesy
//     extension from a paid renewal).
//   - extra_tokens: write a token_ledger row with
//     source='master_grant' and master_grant_id set, crediting the
//     wallet by the payload's amount.
//
// On either success path the grant is marked consumed_at = now() with
// consumed_ref pointing at the downstream artefact (subscription id or
// ledger entry id). A failure leaves consumed_at IS NULL so the master
// operator can revoke the grant (ADR-0098 §D4 — terminal states
// overlap exclusively).
//
// Idempotency: the grant's `consumed_at` is the source of truth.
// Calling Apply on an already-consumed grant returns (false, nil) and
// makes no further writes. Calling Apply on a revoked grant returns
// (false, nil) as well — a revoked grant cannot transition into
// consumed.
//
// Cross-domain coupling: this file is the only place in
// internal/wallet/usecase that imports internal/billing. It is the
// orchestration seam between the wallet domain (extra_tokens credit)
// and the billing domain (free_subscription_period extension); the
// alternative (a separate `internal/usecase/applymastergrant` package)
// would only push the same coupling up one level. The CTO triage on
// SIN-62936 named this file path explicitly.

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/billing"
	"github.com/pericles-luz/crm/internal/wallet"
)

// ApplyMasterGrantService is the synchronous applier. Construct via
// NewApplyMasterGrantService. The zero value is not safe to use.
type ApplyMasterGrantService struct {
	grants        wallet.MasterGrantRepository
	walletRepo    wallet.Repository
	subscriptions billing.SubscriptionRepository
	clock         Clock
	actorID       uuid.UUID
}

// NewApplyMasterGrantService constructs the applier. All five fields
// are required:
//
//   - grants: master_grant repository (typically the audited variant).
//   - walletRepo: token_wallet + token_ledger repository (runtime pool).
//   - subscriptions: subscription repository (writes via master_ops).
//   - clock: time source; defaults to time.Now when nil.
//   - actorID: master user id stamped on the audited save when
//     extending a subscription. Must be non-nil so the master_ops
//     audit chain always has a non-null actor.
func NewApplyMasterGrantService(
	grants wallet.MasterGrantRepository,
	walletRepo wallet.Repository,
	subscriptions billing.SubscriptionRepository,
	clock Clock,
	actorID uuid.UUID,
) (*ApplyMasterGrantService, error) {
	if grants == nil {
		return nil, errors.New("wallet/usecase: ApplyMasterGrantService: grants repository is nil")
	}
	if walletRepo == nil {
		return nil, errors.New("wallet/usecase: ApplyMasterGrantService: wallet repository is nil")
	}
	if subscriptions == nil {
		return nil, errors.New("wallet/usecase: ApplyMasterGrantService: subscription repository is nil")
	}
	if actorID == uuid.Nil {
		return nil, errors.New("wallet/usecase: ApplyMasterGrantService: actor id must not be uuid.Nil")
	}
	if clock == nil {
		clock = time.Now
	}
	return &ApplyMasterGrantService{
		grants:        grants,
		walletRepo:    walletRepo,
		subscriptions: subscriptions,
		clock:         clock,
		actorID:       actorID,
	}, nil
}

// ErrInvalidGrantPayload is returned when the grant's payload is
// missing the value the applier needs for that kind
// (period_days/amount must be a positive integer). The grant row
// itself is unchanged so the master operator can revoke + reissue.
var ErrInvalidGrantPayload = errors.New("wallet/usecase: master grant payload invalid")

// Apply runs the side-effect for grantID exactly once. The boolean
// return reports whether a new side-effect landed:
//
//   - (true, nil)  — the grant transitioned from pending to consumed
//     and the downstream artefact was written.
//   - (false, nil) — the grant was already consumed or revoked; no
//     writes happened.
//   - (false, err) — a failure occurred BEFORE marking consumed_at;
//     the grant remains revocable so the operator can recover.
//
// Failure semantics: every error path returns BEFORE
// grants.Consume(...) lands. If the downstream write (subscription
// extend or ledger insert) succeeds but Consume itself fails, the
// caller MUST surface the error — a retry will re-execute the
// downstream write (subscription extension is non-idempotent without
// the consume guard; ledger insert IS idempotent by
// (wallet_id, idempotency_key)). The ledger idempotency key is
// derived from the grant's ExternalID so a retry collapses to a
// no-op via wallet.ErrIdempotencyConflict, which the applier surfaces.
func (a *ApplyMasterGrantService) Apply(ctx context.Context, grantID uuid.UUID) (bool, error) {
	if grantID == uuid.Nil {
		return false, wallet.ErrNotFound
	}
	g, err := a.grants.GetByID(ctx, grantID)
	if err != nil {
		return false, err
	}
	if g.IsConsumed() || g.IsRevoked() {
		return false, nil
	}

	now := a.clock().UTC()
	var consumedRef string
	switch g.Kind() {
	case wallet.KindFreeSubscriptionPeriod:
		consumedRef, err = a.applyFreePeriod(ctx, g, now)
	case wallet.KindExtraTokens:
		consumedRef, err = a.applyExtraTokens(ctx, g, now)
	default:
		return false, fmt.Errorf("wallet/usecase: unknown master grant kind %q", g.Kind())
	}
	if err != nil {
		return false, err
	}
	if err := a.grants.Consume(ctx, grantID, consumedRef, now); err != nil {
		return false, fmt.Errorf("wallet/usecase: mark grant consumed: %w", err)
	}
	return true, nil
}

// applyFreePeriod handles KindFreeSubscriptionPeriod. Returns the
// active subscription's id as consumed_ref so the audit trail points
// back at the row that was mutated.
func (a *ApplyMasterGrantService) applyFreePeriod(ctx context.Context, g *wallet.MasterGrant, now time.Time) (string, error) {
	days := readInt64Payload(g.Payload(), "period_days")
	if days <= 0 {
		return "", fmt.Errorf("%w: period_days must be a positive integer", ErrInvalidGrantPayload)
	}
	sub, err := a.subscriptions.GetByTenant(ctx, g.TenantID())
	if err != nil {
		return "", fmt.Errorf("wallet/usecase: load active subscription: %w", err)
	}
	if err := sub.ExtendPeriod(time.Duration(days)*24*time.Hour, now); err != nil {
		return "", fmt.Errorf("wallet/usecase: extend subscription period: %w", err)
	}
	if err := a.subscriptions.SaveSubscription(ctx, sub, a.actorID); err != nil {
		return "", fmt.Errorf("wallet/usecase: persist subscription extension: %w", err)
	}
	return sub.ID().String(), nil
}

// applyExtraTokens handles KindExtraTokens. Writes a KindGrant
// token_ledger row with source='master_grant' and master_grant_id
// pointing at the grant, then returns the ledger entry id as
// consumed_ref so the audit trail closes the loop.
func (a *ApplyMasterGrantService) applyExtraTokens(ctx context.Context, g *wallet.MasterGrant, now time.Time) (string, error) {
	amount := readInt64Payload(g.Payload(), "amount")
	if amount <= 0 {
		return "", fmt.Errorf("%w: amount must be a positive integer", ErrInvalidGrantPayload)
	}
	w, err := a.walletRepo.LoadByTenant(ctx, g.TenantID())
	if err != nil {
		return "", fmt.Errorf("wallet/usecase: load wallet for grant apply: %w", err)
	}
	if err := w.Grant(amount, now); err != nil {
		return "", fmt.Errorf("wallet/usecase: grant amount: %w", err)
	}
	grantID := g.ID()
	entry := wallet.LedgerEntry{
		ID:             uuid.New(),
		WalletID:       w.ID(),
		TenantID:       w.TenantID(),
		Kind:           wallet.KindGrant,
		Amount:         wallet.SignedAmount(wallet.KindGrant, amount),
		IdempotencyKey: idempotencyKeyForGrant(g),
		ExternalRef:    g.ExternalID(),
		Source:         wallet.SourceMasterGrant,
		MasterGrantID:  &grantID,
		OccurredAt:     now,
		CreatedAt:      now,
	}
	if err := a.walletRepo.ApplyWithLock(ctx, w, []wallet.LedgerEntry{entry}); err != nil {
		return "", fmt.Errorf("wallet/usecase: apply ledger entry: %w", err)
	}
	return entry.ID.String(), nil
}

// idempotencyKeyForGrant derives the ledger idempotency key from the
// grant's ExternalID. The 128-byte cap (validateIdempotencyKey) is
// satisfied because ULID + the "master_grant:" prefix is ≤ 40 bytes.
// Reusing the ExternalID makes a retried Apply collapse to a single
// ledger row even if grants.Consume fails midway and the operator
// retries (ApplyWithLock returns ErrIdempotencyConflict, which the
// applier surfaces as a wrapped error).
func idempotencyKeyForGrant(g *wallet.MasterGrant) string {
	return "master_grant:" + g.ExternalID()
}

// readInt64Payload mirrors the same-named helper in web/master. The
// JSON decoding for master_grant.payload returns numbers as float64;
// the helper accepts both native int kinds and float64 so a payload
// built by hand in tests works alongside one round-tripped through
// the database.
func readInt64Payload(p map[string]any, key string) int64 {
	if p == nil {
		return 0
	}
	v, ok := p[key]
	if !ok {
		return 0
	}
	switch n := v.(type) {
	case int:
		return int64(n)
	case int32:
		return int64(n)
	case int64:
		return n
	case float32:
		return int64(n)
	case float64:
		return int64(n)
	}
	return 0
}
