// Package walletui declares the read-side ports for F5 (gerente wallet
// UI, SIN-63942). Three ports back the four routes:
//
//   - GET /wallet              → DashboardReader.Snapshot
//   - GET /wallet/ledger       → LedgerReader.Page
//   - GET /wallet/ledger.csv   → LedgerReader.StreamCSV
//   - GET /wallet/topup        → TopupCatalogReader.ListPackages
//
// Adapters live in internal/adapter/db/postgres/walletui. Authorization
// is enforced at the handler boundary (RequireAuth + ActionWalletView +
// tenant-scope check against the principal). Adapters then run inside
// postgresadapter.WithTenant(tenantID) so RLS is the second line of
// defense for cross-tenant isolation.
package walletui

import (
	"context"
	"errors"
	"io"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/wallet"
)

// ErrTenantNotFound is returned by DashboardReader.Snapshot when the
// tenant id has no token_wallet row (distinct from "wallet exists but
// is empty"). Handlers map this to a 404 page; any other error is a 500.
var ErrTenantNotFound = errors.New("walletui: wallet not found for tenant")

// DashboardSnapshot aggregates everything GET /wallet needs in a single
// round trip: wallet balances, the 14-day consumption projection, the
// dunning banner state, and the last five ledger rows for the preview
// card.
type DashboardSnapshot struct {
	// Balance is token_wallet.balance.
	Balance int64

	// Reserved is token_wallet.reserved.
	Reserved int64

	// Available is Balance - Reserved, denormalized so the template can
	// render the headline number without re-doing arithmetic.
	Available int64

	// AvgDailyConsume is the |SUM(amount)| of kind='commit' rows over
	// the last 14 days divided by 14. Zero when no consumption is
	// recorded in the window — the template renders "—".
	AvgDailyConsume int64

	// DaysRemaining is Available / AvgDailyConsume, floored. nil when
	// AvgDailyConsume == 0 (cannot project). Pointer keeps "unknown"
	// distinct from "zero days left".
	DaysRemaining *int

	// DunningState mirrors subscription_dunning_states.state for the
	// tenant's active subscription. Values: "current", "warn",
	// "suspended_outbound", "suspended_full", "cancelled". Empty
	// string when the tenant has no active subscription / no dunning
	// row (treat as "current" in the template).
	DunningState string

	// DunningOverrideUntil is non-nil when master_ops granted a
	// reprieve still in effect (subscription_dunning_states.override_until
	// is non-NULL and > now()). The template uses this to render the
	// "reprieve until <date>" pill instead of the warn/suspend banner.
	DunningOverrideUntil *time.Time

	// LastFive is the five most-recent token_ledger rows for this
	// tenant, descending by (occurred_at, id). LGPD-redacted per
	// LedgerEntryView.
	LastFive []LedgerEntryView
}

// DashboardReader is the read-side port for GET /wallet. Implementations
// MUST scope reads to tenantID via WithTenant; cross-tenant access is
// denied at the handler boundary and additionally by RLS as a second
// line of defense.
type DashboardReader interface {
	// Snapshot returns the aggregate dashboard view. ErrTenantNotFound
	// when no token_wallet row exists for tenantID.
	Snapshot(ctx context.Context, tenantID uuid.UUID, now time.Time) (DashboardSnapshot, error)
}

// LedgerFilter is the shared filter for both the paginated ledger and
// the CSV stream. All fields except TenantID are optional.
type LedgerFilter struct {
	// TenantID scopes the query. Required (uuid.Nil rejected).
	TenantID uuid.UUID

	// FromOccurredAt is the inclusive lower bound on occurred_at. Zero
	// value means no lower bound.
	FromOccurredAt time.Time

	// ToOccurredAt is the exclusive upper bound on occurred_at. Zero
	// value means no upper bound.
	ToOccurredAt time.Time

	// Kinds restricts the result set to these wallet.LedgerKind values.
	// Empty (nil) means "all kinds".
	Kinds []wallet.LedgerKind
}

// LedgerPageOptions controls cursor pagination on GET /wallet/ledger.
// The cursor pair (CursorOccurredAt, CursorID) is "rows strictly before"
// in (occurred_at, id) DESC order — identical timestamps don't skip rows.
// PageSize is honoured verbatim by the adapter; the handler clamps it
// to a safe maximum before passing it in.
type LedgerPageOptions struct {
	Filter           LedgerFilter
	CursorOccurredAt time.Time
	CursorID         uuid.UUID
	PageSize         int
}

// LedgerPage is the cursor-paginated payload. NextCursorOccurredAt and
// NextCursorID are valid only when HasMore is true.
type LedgerPage struct {
	Entries              []LedgerEntryView
	HasMore              bool
	NextCursorOccurredAt time.Time
	NextCursorID         uuid.UUID
}

// LedgerEntryView is the gerente-facing ledger row. LGPD-safe by
// construction: NO raw conversation_id, NO message bodies. Anything
// the adapter cannot redact safely MUST be omitted.
//
// Field mapping from token_ledger columns:
//
//   - Kind / Amount / OccurredAt / ExternalRef: direct columns.
//   - Source: token_ledger.source (consumption/monthly_alloc/master_grant).
//   - ConversationIDHash: hex(sha256(metadata->>'conversation_id'))[:16];
//     empty when the row is not a consumption or has no conversation_id
//     in metadata.
//   - Model: metadata->>'model' (OpenRouter model id; safe to expose).
//   - PolicyID: uuid(metadata->>'ai_policy_id') if present and parseable,
//     else uuid.Nil.
//   - BalanceAfter: running balance projection computed by the adapter
//     over the page window. Set to 0 when unavailable; the page MUST
//     NOT fail when the projection is missing for a row.
type LedgerEntryView struct {
	ID                 uuid.UUID
	OccurredAt         time.Time
	Kind               wallet.LedgerKind
	Source             wallet.LedgerSource
	Amount             int64
	BalanceAfter       int64
	ConversationIDHash string
	Model              string
	PolicyID           uuid.UUID
	ExternalRef        string
}

// LedgerReader is the read-side port for GET /wallet/ledger and
// GET /wallet/ledger.csv. Implementations MUST scope reads to
// filter.TenantID via WithTenant.
type LedgerReader interface {
	// Page returns one cursor-paginated page of ledger rows ordered by
	// (occurred_at, id) DESC.
	Page(ctx context.Context, opts LedgerPageOptions) (LedgerPage, error)

	// StreamCSV writes the full filtered result set to w as CSV with a
	// header row, ordered by (occurred_at, id) DESC. Implementations
	// MUST stream (cursor over the result set, write each row to w as
	// it is scanned) — never buffer the whole result set in memory. The
	// handler owns Content-Type / Content-Disposition.
	StreamCSV(ctx context.Context, filter LedgerFilter, w io.Writer) error
}

// TopupPackage is one row from token_packages exposed to the gerente.
// PricePerKToken is computed (round-half-up to nearest cent) so the
// template can render the "melhor custo/token" badge from F5 without
// re-doing arithmetic.
type TopupPackage struct {
	ID     uuid.UUID
	Slug   string
	Name   string
	Tokens int64
	// PriceCentsBRL is the price in BRL cents (e.g. 1500 = R$ 15,00).
	PriceCentsBRL int
	// PricePerKToken is PriceCentsBRL * 1000 / Tokens rounded half-up.
	// Expressed in milli-cents per token (i.e. cents per 1k tokens
	// rounded). Computed by the adapter.
	PricePerKToken int
}

// TopupCatalogReader is the read-side port for GET /wallet/topup.
// token_packages has no RLS (it is a catalog), so adapters may use the
// runtime pool directly without WithTenant.
type TopupCatalogReader interface {
	// ListPackages returns the active token_packages ordered by
	// price ascending. Implementations MUST compute PricePerKToken.
	ListPackages(ctx context.Context) ([]TopupPackage, error)
}
