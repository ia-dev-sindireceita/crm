package master

// SIN-62885 / Fase 2.5 C11 — read-only master views for a tenant's
// billing history and token ledger.
//
//   GET /master/tenants/{id}/billing  — 3 panels: current subscription,
//                                       invoice list, master-grant history.
//   GET /master/tenants/{id}/ledger   — paginated token ledger (cursor),
//                                       cross-references grant + subscription.
//
// Both endpoints are read-only — no destructive actions live on this
// screen (grant issue/revoke is C10, plan assignment is C9).
//
// Authorization (C7 / SIN-62880):
//
//   - tenant.billing.view       guards the billing panel
//   - tenant.wallet.view_ledger guards the ledger page
//
// The handler trusts the iam.Principal already on the request context;
// the wire layer wraps each route in RequireAuth → RequireAction. The
// tenant scope under which the adapter runs (WithTenant) lets Postgres
// RLS satisfy AC #3: a gerente of tenant Y hitting tenant X's URL gets
// an empty result because the runtime role's RLS policy filters by the
// SET LOCAL app.tenant_id GUC.

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
)

// BillingView is the aggregate rendered by GET /master/tenants/{id}/
// billing. The adapter joins subscription + plan + invoice rows +
// master_grant rows so the handler can build a single page in one round
// trip. Missing pieces (e.g. no active subscription) collapse to zero
// values; the template degrades gracefully.
type BillingView struct {
	TenantID uuid.UUID

	// Subscription is the *current* active subscription for the tenant.
	// Empty when no active row exists (e.g. tenant freshly created
	// without a plan).
	Subscription SubscriptionRow

	// Invoices are the tenant's invoices ordered by period_start DESC.
	Invoices []InvoiceRow

	// Grants is the tenant's master_grant history ordered by created_at
	// DESC (AC #1). The same GrantRow projection used by C10 — Status
	// derives from Consumed / Revoked.
	Grants []GrantRow
}

// SubscriptionRow is the projection rendered on the billing page's
// "subscription atual" panel. PlanID is uuid.Nil when no active
// subscription exists, which the template renders as "Sem assinatura".
type SubscriptionRow struct {
	ID                 uuid.UUID
	PlanID             uuid.UUID
	PlanSlug           string
	PlanName           string
	PlanPriceCentsBRL  int
	Status             string
	CurrentPeriodStart time.Time
	CurrentPeriodEnd   time.Time
	// NextInvoiceAt is conventionally the active subscription's
	// current_period_end; renderers show it as "próximo invoice em ..."
	NextInvoiceAt time.Time
}

// IsEmpty reports whether the row carries no real subscription. The
// template uses this to render the empty-state panel instead of an
// incomplete table.
func (s SubscriptionRow) IsEmpty() bool {
	return s.ID == uuid.Nil
}

// InvoiceRow is the projection rendered on the invoice list. State is
// the persisted enum (`pending`/`paid`/`cancelled_by_master`) — the
// template maps to the Portuguese label via invoiceLabel.
type InvoiceRow struct {
	ID             uuid.UUID
	PeriodStart    time.Time
	PeriodEnd      time.Time
	AmountCentsBRL int
	State          string
}

// BillingViewer is the read-side port for GET /master/tenants/{id}/
// billing. Implementations MUST scope the underlying reads to tenantID
// so RLS guards the cross-tenant case (AC #3).
type BillingViewer interface {
	ViewBilling(ctx context.Context, tenantID uuid.UUID) (BillingView, error)
}

// LedgerOptions controls cursor pagination on GET /master/tenants/{id}/
// ledger. The cursor is (OccurredAt, ID) so we can break ties
// deterministically and avoid skipping rows when many entries share a
// timestamp. PageSize is clamped at the handler boundary.
type LedgerOptions struct {
	TenantID uuid.UUID

	// CursorOccurredAt + CursorID together define "the row I last
	// rendered". Pass zero values for the first page. The adapter
	// returns rows STRICTLY before (occurred_at < cursor) OR
	// (occurred_at == cursor AND id < cursor_id) — DESC order.
	CursorOccurredAt time.Time
	CursorID         uuid.UUID

	// PageSize is the upper bound on the number of rows returned.
	// Adapters MUST honour the limit so AC #2 (perf with 10k entries)
	// holds via index scan + LIMIT.
	PageSize int
}

// LedgerPage is the cursor-paginated payload returned by LedgerViewer.
// HasMore reports whether the adapter saw at least one more row beyond
// PageSize; the template uses it to decide whether to render the HTMX
// load-more trigger.
type LedgerPage struct {
	Entries []LedgerRow
	HasMore bool
	// NextCursorOccurredAt / NextCursorID are the cursor values for the
	// next "load more" request. Zero when HasMore == false.
	NextCursorOccurredAt time.Time
	NextCursorID         uuid.UUID
}

// LedgerRow is one row in the master ledger view. Source maps to the
// `token_ledger.source` enum (`monthly_alloc` / `master_grant` /
// `consumption`); the template renders a human-readable label.
//
// MasterGrantID and SubscriptionID are the cross-references the AC
// asks for: a master-grant ledger entry carries a non-nil
// MasterGrantID + its ExternalID; a monthly-alloc / consumption entry
// optionally carries the active SubscriptionID + plan slug. Adapters
// fill what they can; the template guards each pointer.
type LedgerRow struct {
	ID         uuid.UUID
	OccurredAt time.Time
	CreatedAt  time.Time

	Source string
	Kind   string
	Amount int64

	// MasterGrantID + MasterGrantExternalID — non-empty when Source ==
	// "master_grant". The template renders an inline link back to the
	// grants panel of the same tenant.
	MasterGrantID         uuid.UUID
	MasterGrantExternalID string

	// SubscriptionID + SubscriptionPlanSlug — opportunistically filled
	// when the row links to an active subscription period. Empty when
	// the row predates SIN-62880 or the link is ambiguous.
	SubscriptionID       uuid.UUID
	SubscriptionPlanSlug string

	// ExternalRef is the raw token_ledger.external_ref column. Useful
	// for reconciliation lookups; the template renders it under the
	// row's "Referência" cell.
	ExternalRef    string
	IdempotencyKey string
}

// LedgerViewer is the read-side port for GET /master/tenants/{id}/
// ledger. Implementations scope to tenantID; AC #3 (RLS isolation)
// derives from that scoping.
type LedgerViewer interface {
	ViewLedger(ctx context.Context, opts LedgerOptions) (LedgerPage, error)
}

// ErrTenantNotFound is the canonical "no such tenant id" surface for
// the C11 routes. Adapters return it from ViewBilling when the tenant
// row itself does not exist (distinct from "tenant exists but has no
// subscription/invoices/grants", which returns a populated zero-row
// BillingView). Handler maps to 404.
var ErrTenantNotFound = errors.New("web/master: tenant not found for billing/ledger view")
