package master

// WalletGrantPort wraps a wallet.MasterGrantRepository (typically the
// AuditedMasterGrantRepository decorator from SIN-62883) so it
// satisfies the master.GrantPort handler-edge contract. cmd/server
// wires this adapter; tests in this package inject a hand-rolled
// stub instead.
//
// Cap enforcement happens HERE rather than inside the wallet domain
// because the cap is a presentation-layer policy that depends on the
// tokens-equivalent equivalence chosen for free_subscription_period
// (FreeSubscriptionDayEquivalence). The wallet entity does not know
// about a per-day token rate; the C10 UI does, so the cap check is
// owned by the adapter.
//
// Application of the grant downstream (ledger insert for extra_tokens,
// subscription extension for free_subscription_period) is OUT OF
// SCOPE for SIN-62884. The UI creates the master_grant row + emits
// the audit event; a separate worker / usecase (own follow-up issue)
// will mark consumed_at + apply the side effect. CA #2 and CA #3
// from the issue therefore cover ledger / subscription state once
// the applier ships — this PR satisfies the row-creation precondition.

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/wallet"
)

// WalletGrantPort is the wallet-backed adapter for GrantPort.
type WalletGrantPort struct {
	repo wallet.MasterGrantRepository
	now  func() time.Time
}

// NewWalletGrantPort wires the adapter. repo MUST be the audited
// repository (SIN-62883) so master.grant.issued audit rows fire on
// Create; now defaults to time.Now.
func NewWalletGrantPort(repo wallet.MasterGrantRepository, now func() time.Time) (*WalletGrantPort, error) {
	if repo == nil {
		return nil, fmt.Errorf("web/master: WalletGrantPort: repo is nil")
	}
	if now == nil {
		now = time.Now
	}
	return &WalletGrantPort{repo: repo, now: now}, nil
}

// IssueGrant enforces the C10 cap policy, builds the wallet entity,
// and persists it via the audited repository (the C8 hook writes the
// master.grant.issued event inside).
func (a *WalletGrantPort) IssueGrant(ctx context.Context, in IssueGrantInput) (IssueGrantResult, error) {
	now := a.now().UTC()

	// Cap pre-flight. cumulativeSince looks at the trailing 365d of
	// grants for this tenant, summing each into tokens-equivalent.
	existing, err := a.repo.ListByTenant(ctx, in.TenantID)
	if err != nil {
		return IssueGrantResult{}, fmt.Errorf("web/master: list grants for cap: %w", err)
	}
	cumulative := cumulativeEquivalentSince(existing, now.Add(-TenantWindow))
	equivalent := CapEquivalence(in.Kind, in.Amount, in.PeriodDays)
	if err := EnforceCap(equivalent, cumulative); err != nil {
		return IssueGrantResult{}, err
	}

	// Domain construction. wallet.NewMasterGrant runs the kind /
	// reason invariants (≥10 chars, valid kind, non-nil ids).
	payload := buildGrantPayload(in)
	g, err := wallet.NewMasterGrant(in.TenantID, in.ActorUserID, walletKind(in.Kind), payload, in.Reason, now)
	if err != nil {
		return IssueGrantResult{}, fmt.Errorf("web/master: build master grant: %w", err)
	}
	if err := a.repo.Create(ctx, g); err != nil {
		return IssueGrantResult{}, fmt.Errorf("web/master: persist master grant: %w", err)
	}
	return IssueGrantResult{Grant: hydrateGrantRow(g)}, nil
}

// RevokeGrant translates the wallet.MasterGrantRepository revoke
// sentinels onto the presentation-layer ones the handler matches on.
func (a *WalletGrantPort) RevokeGrant(ctx context.Context, in RevokeGrantInput) error {
	err := a.repo.Revoke(ctx, in.GrantID, in.ActorUserID, in.Reason, a.now().UTC())
	switch {
	case errors.Is(err, wallet.ErrNotFound):
		return ErrGrantNotFound
	case errors.Is(err, wallet.ErrGrantAlreadyConsumed):
		return ErrGrantAlreadyConsumed
	case errors.Is(err, wallet.ErrGrantAlreadyRevoked):
		return ErrGrantAlreadyRevoked
	}
	return err
}

// ListGrants converts wallet entities into the presentation projection.
func (a *WalletGrantPort) ListGrants(ctx context.Context, tenantID uuid.UUID) ([]GrantRow, error) {
	grants, err := a.repo.ListByTenant(ctx, tenantID)
	if err != nil {
		return nil, fmt.Errorf("web/master: list grants: %w", err)
	}
	out := make([]GrantRow, 0, len(grants))
	for _, g := range grants {
		out = append(out, hydrateGrantRow(g))
	}
	return out, nil
}

// cumulativeEquivalentSince sums the tokens-equivalent values of every
// non-revoked grant created at or after the cutoff. Revoked grants do
// not count toward the cap (ADR-0098 §D5 — the cap protects against
// excess wealth transfer, and a revoked grant transferred nothing).
func cumulativeEquivalentSince(grants []*wallet.MasterGrant, cutoff time.Time) int64 {
	var sum int64
	for _, g := range grants {
		if g == nil {
			continue
		}
		if g.IsRevoked() {
			continue
		}
		if g.CreatedAt().Before(cutoff) {
			continue
		}
		kind := GrantKind(string(g.Kind()))
		amount := readInt64Payload(g.Payload(), "amount")
		periodDays := int(readInt64Payload(g.Payload(), "period_days"))
		sum += CapEquivalence(kind, amount, periodDays)
	}
	return sum
}

func buildGrantPayload(in IssueGrantInput) map[string]any {
	switch in.Kind {
	case GrantKindFreeSubscriptionPeriod:
		return map[string]any{"period_days": in.PeriodDays}
	case GrantKindExtraTokens:
		return map[string]any{"amount": in.Amount}
	default:
		return map[string]any{}
	}
}

func walletKind(k GrantKind) wallet.MasterGrantKind {
	switch k {
	case GrantKindFreeSubscriptionPeriod:
		return wallet.KindFreeSubscriptionPeriod
	case GrantKindExtraTokens:
		return wallet.KindExtraTokens
	default:
		return wallet.MasterGrantKind(string(k))
	}
}

func hydrateGrantRow(g *wallet.MasterGrant) GrantRow {
	row := GrantRow{
		ID:          g.ID(),
		ExternalID:  g.ExternalID(),
		TenantID:    g.TenantID(),
		Kind:        GrantKind(string(g.Kind())),
		Amount:      readInt64Payload(g.Payload(), "amount"),
		PeriodDays:  int(readInt64Payload(g.Payload(), "period_days")),
		Reason:      g.Reason(),
		CreatedByID: g.CreatedByUserID(),
		CreatedAt:   g.CreatedAt(),
		Consumed:    g.IsConsumed(),
		Revoked:     g.IsRevoked(),
	}
	if t := g.ConsumedAt(); t != nil {
		row.ConsumedAt = *t
	}
	if t := g.RevokedAt(); t != nil {
		row.RevokedAt = *t
	}
	if id := g.RevokedByUserID(); id != nil {
		row.RevokeBy = *id
	}
	return row
}

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

// Compile-time check that the adapter satisfies the union port.
var _ GrantPort = (*WalletGrantPort)(nil)
