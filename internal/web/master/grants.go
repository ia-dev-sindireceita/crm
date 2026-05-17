package master

// SIN-62884 / Fase 2.5 C10: master operator UI for granting courtesy
// (free_subscription_period | extra_tokens) and revoking grants.
//
// The handler-edge ports (GrantIssuer / GrantRevoker / GrantLister)
// keep the master package independent of internal/wallet just like
// TenantCreator / TenantLister keep it independent of the tenant
// adapter. cmd/server wires the wallet-backed adapter (see
// grant_issuer_wallet.go) behind the same audited repository the C8
// hook decorates, so the master.grant.issued audit row fires on every
// successful Create irrespective of the transport layer.
//
// Cap policy (ADR-0098 §D5 / SIN-62241):
//
//   - per-grant            : 10,000,000 tokens-equivalent → 422 + UI
//                            explains "above limit, requires 4-eyes".
//   - per-tenant per-365d  : 100,000,000 tokens-equivalent → 422 + same
//                            UI explanation.
//   - alert threshold      : 1,000,000 tokens-equivalent → grant
//                            proceeds; the handler emits a slog warning
//                            so the existing alerter worker picks it up.
//
// Per-tenant scoping (vs per-master in the ADR) is a deliberate
// reduction for the UI layer — the per-master ceiling is harder to
// surface in the form and is enforced separately by the
// master_grant_request 4-eyes table (the schema exists in
// migration 0097 + tests; the approval flow itself is out of scope for
// this PR and will land as a follow-up).
//
// MFA step-up (ADR-0074 §D3 / ADR-0098 §D3):
//
// The POST routes (/grants and /grants/{id}/revoke) MUST sit behind
// mastermfa.RequireRecentMFA with MaxAge=15m. The middleware lives at
// the router layer; this handler trusts that gate and does not re-check.

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
)

// GrantKind mirrors wallet.MasterGrantKind without coupling the
// presentation layer to the domain package. The handler converts to
// the wallet kind at the issuer boundary.
type GrantKind string

const (
	// GrantKindFreeSubscriptionPeriod credits the tenant with a billing
	// period free of charge (period_days input).
	GrantKindFreeSubscriptionPeriod GrantKind = "free_subscription_period"
	// GrantKindExtraTokens credits the tenant's token wallet directly
	// (amount input, in tokens).
	GrantKindExtraTokens GrantKind = "extra_tokens"
)

// IssueGrantInput is the form-derived payload for POST
// /master/tenants/{id}/grants. Either PeriodDays (for
// free_subscription_period) or Amount (for extra_tokens) is set; the
// other is zero. Reason must be at least 10 characters (mirrors the DB
// CHECK and ADR-0098 §D2).
type IssueGrantInput struct {
	ActorUserID uuid.UUID
	TenantID    uuid.UUID
	Kind        GrantKind
	PeriodDays  int   // KindFreeSubscriptionPeriod only
	Amount      int64 // KindExtraTokens only
	Reason      string
}

// IssueGrantResult is the issued grant projection rendered on the row
// partial. ExternalID is the ULID exposed in URLs / audit logs; ID is
// the internal UUID used by Revoke / Consume.
type IssueGrantResult struct {
	Grant GrantRow
}

// GrantRow is the projection rendered in the grants table partial. It
// carries enough state for the row to decide whether the revoke
// button is enabled (Consumed/Revoked tombstones).
type GrantRow struct {
	ID          uuid.UUID
	ExternalID  string
	TenantID    uuid.UUID
	Kind        GrantKind
	PeriodDays  int
	Amount      int64
	Reason      string
	CreatedByID uuid.UUID
	CreatedAt   time.Time
	Consumed    bool
	ConsumedAt  time.Time
	Revoked     bool
	RevokedAt   time.Time
	RevokeBy    uuid.UUID
}

// IsRevocable reports whether the row may still be revoked. ADR-0098
// §D4: revoke is allowed only while Consumed == false (terminal
// states overlap exclusively).
func (g GrantRow) IsRevocable() bool {
	return !g.Consumed && !g.Revoked
}

// RevokeGrantInput is the body of POST /master/grants/{id}/revoke.
// Reason is the revoke justification (≥10 chars, mirrors the DB
// CHECK on revoke_reason).
type RevokeGrantInput struct {
	ActorUserID uuid.UUID
	GrantID     uuid.UUID
	Reason      string
}

// GrantIssuer is the write-side port for POST /master/tenants/{id}/
// grants. The adapter is responsible for cap enforcement, wallet/
// subscription wiring (out-of-scope for the UI), and audit emission
// (via the C8 AuditedMasterGrantRepository decorator).
type GrantIssuer interface {
	IssueGrant(ctx context.Context, in IssueGrantInput) (IssueGrantResult, error)
}

// GrantRevoker is the write-side port for POST /master/grants/{id}/
// revoke. Returns ErrGrantAlreadyConsumed when the grant has been
// applied downstream (the UI must point the operator to "criar grant
// compensatório" per ADR-0098 §D4).
type GrantRevoker interface {
	RevokeGrant(ctx context.Context, in RevokeGrantInput) error
}

// GrantLister is the read-side port for the grants table partial that
// follows the form. The adapter returns the tenant's grants newest-
// first (see wallet.MasterGrantRepository.ListByTenant).
type GrantLister interface {
	ListGrants(ctx context.Context, tenantID uuid.UUID) ([]GrantRow, error)
}

// Cap policy values (ADR-0098 §D5). Exported so the wallet-backed
// adapter and tests can share the same numeric bar.
const (
	// PerGrantCap is the hard ceiling for a single grant
	// (tokens-equivalent). Free-subscription-period kinds convert to
	// tokens via plan.MonthlyTokenQuota × months — the UI layer does
	// not have the plan resolved, so the cap check uses the explicit
	// Amount for extra_tokens and a fixed 12-month / 1M-tokens
	// equivalence for period_days (≈ free-tier monthly quota, a
	// conservative upper bound). Real per-plan accounting lives in
	// the wallet adapter when the applier ships.
	PerGrantCap int64 = 10_000_000

	// PerTenantWindowCap is the cumulative ceiling across all the
	// tenant's grants in the past TenantWindow. Same equivalence as
	// PerGrantCap.
	PerTenantWindowCap int64 = 100_000_000

	// AlertThresholdTokens emits a slog warning when a single grant
	// crosses this bar (the alerter worker picks the log up). Does
	// NOT block.
	AlertThresholdTokens int64 = 1_000_000

	// TenantWindow is the look-back used by PerTenantWindowCap.
	TenantWindow = 365 * 24 * time.Hour
)

// FreeSubscriptionDayEquivalence is the tokens-equivalent value per
// day of free subscription used by the cap calculator. 1M tokens / 30
// days ≈ 33_333 tokens/day → rounded to a flat 35_000 for predictable
// arithmetic. The real per-plan number lives downstream in the
// applier; the UI bar is intentionally conservative.
const FreeSubscriptionDayEquivalence int64 = 35_000

// Cap policy errors. The handler maps each to 422 + a user-facing
// explanation that names the over-the-cap action and links to the
// 4-eyes flow when one exists.
var (
	// ErrPerGrantCapExceeded is returned by the issuer when this
	// single grant exceeds PerGrantCap. UI: "valor acima do limite por
	// grant; requer aprovação 4-eyes (em construção)".
	ErrPerGrantCapExceeded = errors.New("web/master: per-grant cap exceeded")

	// ErrPerTenantWindowCapExceeded is returned when the tenant's
	// cumulative grants in the trailing 365 days plus this grant
	// exceed PerTenantWindowCap. UI: same 4-eyes pointer.
	ErrPerTenantWindowCapExceeded = errors.New("web/master: per-tenant 365d cap exceeded")

	// ErrGrantNotFound is returned by GrantRevoker when the grant id
	// does not exist. Handler → 404.
	ErrGrantNotFound = errors.New("web/master: grant not found")

	// ErrGrantAlreadyConsumed is returned by GrantRevoker when the
	// grant has already been applied downstream. Handler → 422 with
	// the compensating-grant pointer (ADR-0098 §D4).
	ErrGrantAlreadyConsumed = errors.New("web/master: grant already consumed")

	// ErrGrantAlreadyRevoked is returned by GrantRevoker when the
	// grant has already been revoked. Handler → 409 (idempotent re-
	// revoke is a UI race, not an error worth a 4-eyes message).
	ErrGrantAlreadyRevoked = errors.New("web/master: grant already revoked")
)

// CapEquivalence returns the tokens-equivalent value of a grant given
// its kind, amount, and period_days. Exported so adapters and tests
// share the same arithmetic.
func CapEquivalence(kind GrantKind, amount int64, periodDays int) int64 {
	switch kind {
	case GrantKindExtraTokens:
		return amount
	case GrantKindFreeSubscriptionPeriod:
		return int64(periodDays) * FreeSubscriptionDayEquivalence
	default:
		return 0
	}
}

// EnforceCap is the pure cap-policy helper used by both the adapter
// and the in-package handler tests. cumulative is the sum of the
// tenant's existing grants in the trailing TenantWindow; equivalent is
// the new grant's CapEquivalence.
//
// Returns ErrPerGrantCapExceeded when equivalent > PerGrantCap;
// ErrPerTenantWindowCapExceeded when cumulative+equivalent >
// PerTenantWindowCap. nil = grant allowed.
func EnforceCap(equivalent, cumulative int64) error {
	if equivalent > PerGrantCap {
		return ErrPerGrantCapExceeded
	}
	if cumulative+equivalent > PerTenantWindowCap {
		return ErrPerTenantWindowCapExceeded
	}
	return nil
}
