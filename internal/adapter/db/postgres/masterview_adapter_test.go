package postgres_test

// SIN-62885 / Fase 2.5 C11 — integration tests for the masterview
// adapter (web/master.BillingViewer + LedgerViewer).
//
// freshDBWithBilling lives in subscription_billing_migration_test.go
// (same parent _test package per reference_testpg_shared_cluster_race).

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	postgresadapter "github.com/pericles-luz/crm/internal/adapter/db/postgres"
	"github.com/pericles-luz/crm/internal/adapter/db/postgres/masterview"
	"github.com/pericles-luz/crm/internal/adapter/db/postgres/testpg"
	"github.com/pericles-luz/crm/internal/web/master"
)

func newMasterviewCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func newMasterviewStore(t *testing.T, db *testpg.DB) *masterview.Store {
	t.Helper()
	store, err := masterview.New(db.RuntimePool())
	if err != nil {
		t.Fatalf("masterview store: %v", err)
	}
	return store
}

// seedMasterviewTenant creates a tenant row via the admin pool and
// returns its id. Mirrors seedTenantForBilling so this file stays
// self-contained on the helpers it actually needs.
func seedMasterviewTenant(t *testing.T, ctx context.Context, db *testpg.DB, label string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if _, err := db.AdminPool().Exec(ctx,
		`INSERT INTO tenants (id, name, host) VALUES ($1, $2, $3)`,
		id, label, fmt.Sprintf("masterview-%s-%s.crm.local", label, id),
	); err != nil {
		t.Fatalf("seed tenant %s: %v", label, err)
	}
	return id
}

// seedMasterviewSubscription writes an active subscription row for the
// (tenant, plan) pair.
func seedMasterviewSubscription(t *testing.T, ctx context.Context, db *testpg.DB, tenantID, planID, masterID uuid.UUID, periodStart, periodEnd time.Time) uuid.UUID {
	t.Helper()
	var subID uuid.UUID
	if err := postgresadapter.WithMasterOps(ctx, db.MasterOpsPool(), masterID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`INSERT INTO subscription
			   (tenant_id, plan_id, status, current_period_start, current_period_end)
			 VALUES ($1, $2, 'active', $3, $4)
			 RETURNING id`,
			tenantID, planID, periodStart, periodEnd).Scan(&subID)
	}); err != nil {
		t.Fatalf("seed subscription: %v", err)
	}
	return subID
}

// seedMasterviewCancelledInvoice writes one cancelled invoice with a
// CHECK-satisfying cancelled_reason. Splitting the helper keeps the
// happy-path seeder clean for the (more common) pending/paid cases.
func seedMasterviewCancelledInvoice(t *testing.T, ctx context.Context, db *testpg.DB,
	tenantID, subID, masterID uuid.UUID,
	periodStart, periodEnd time.Time, amount int,
) uuid.UUID {
	t.Helper()
	var invID uuid.UUID
	if err := postgresadapter.WithMasterOps(ctx, db.MasterOpsPool(), masterID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`INSERT INTO invoice
			   (tenant_id, subscription_id, period_start, period_end, amount_cents_brl, state, cancelled_reason)
			 VALUES ($1, $2, $3, $4, $5, 'cancelled_by_master', $6) RETURNING id`,
			tenantID, subID, periodStart, periodEnd, amount,
			"Cancelado para teste de integração C11").Scan(&invID)
	}); err != nil {
		t.Fatalf("seed cancelled invoice: %v", err)
	}
	return invID
}

// seedMasterviewInvoice writes one invoice row in the requested state.
func seedMasterviewInvoice(t *testing.T, ctx context.Context, db *testpg.DB, tenantID, subID, masterID uuid.UUID, periodStart, periodEnd time.Time, amount int, state string) uuid.UUID {
	t.Helper()
	var invID uuid.UUID
	if err := postgresadapter.WithMasterOps(ctx, db.MasterOpsPool(), masterID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`INSERT INTO invoice
			   (tenant_id, subscription_id, period_start, period_end, amount_cents_brl, state)
			 VALUES ($1, $2, $3, $4, $5, $6) RETURNING id`,
			tenantID, subID, periodStart, periodEnd, amount, state).Scan(&invID)
	}); err != nil {
		t.Fatalf("seed invoice: %v", err)
	}
	return invID
}

// seedMasterviewGrant writes a master_grant row. payload is a JSON
// map (e.g. {"amount": 100000} or {"period_days": 30}). When
// revokedAt is non-nil the row is revoked in a second statement so
// the revoke_consistency CHECK is satisfied.
func seedMasterviewGrant(t *testing.T, ctx context.Context, db *testpg.DB,
	tenantID, actorID uuid.UUID,
	kind string, payload map[string]any, reason string,
	createdAt time.Time,
	consumedAt, revokedAt *time.Time,
) (uuid.UUID, string) {
	t.Helper()
	id := uuid.New()
	external := fmt.Sprintf("01HTEST%010d", id.ID()&0xffff)
	payloadJSON, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	if err := postgresadapter.WithMasterOps(ctx, db.MasterOpsPool(), actorID, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx,
			`INSERT INTO master_grant
			   (id, external_id, tenant_id, kind, payload, reason, created_by_user_id, created_at)
			 VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`,
			id, external, tenantID, kind, payloadJSON, reason, actorID, createdAt,
		); err != nil {
			return err
		}
		if consumedAt != nil {
			if _, err := tx.Exec(ctx,
				`UPDATE master_grant SET consumed_at = $1, consumed_ref = $2 WHERE id = $3`,
				*consumedAt, "test", id,
			); err != nil {
				return err
			}
		}
		if revokedAt != nil {
			if _, err := tx.Exec(ctx,
				`UPDATE master_grant
				    SET revoked_at = $1, revoked_by_user_id = $2, revoke_reason = $3
				  WHERE id = $4`,
				*revokedAt, actorID, "Revogada para teste de integração", id,
			); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("seed grant: %v", err)
	}
	return id, external
}

// seedMasterviewLedger writes one wallet-aware token_ledger row. The
// caller controls source/master_grant_id pairing so AC #2 (cursor
// across mixed sources) is exercised.
func seedMasterviewLedger(t *testing.T, ctx context.Context, db *testpg.DB,
	tenantID, walletID uuid.UUID, kind string, amount int64,
	source string, masterGrantID *uuid.UUID,
	occurredAt time.Time, idempotencyKey, externalRef string,
) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if err := postgresadapter.WithTenant(ctx, db.RuntimePool(), tenantID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO token_ledger
			   (id, wallet_id, tenant_id, kind, amount, idempotency_key, external_ref, source, master_grant_id, occurred_at, created_at)
			 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$10)`,
			id, walletID, tenantID, kind, amount, idempotencyKey, externalRef, source, masterGrantID, occurredAt,
		)
		return err
	}); err != nil {
		t.Fatalf("seed ledger row: %v", err)
	}
	return id
}

// seedMasterviewWallet writes a token_wallet row for the tenant and
// returns the wallet id.
func seedMasterviewWallet(t *testing.T, ctx context.Context, db *testpg.DB, tenantID uuid.UUID) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if _, err := db.AdminPool().Exec(ctx,
		`INSERT INTO token_wallet (id, tenant_id, balance, reserved) VALUES ($1,$2,0,0)`,
		id, tenantID,
	); err != nil {
		t.Fatalf("seed wallet: %v", err)
	}
	return id
}

// ---------------------------------------------------------------------------
// ViewBilling
// ---------------------------------------------------------------------------

func TestMasterview_ViewBilling_EmptyTenantReturnsZeroValueView(t *testing.T) {
	db := freshDBWithBilling(t)
	ctx := newMasterviewCtx(t)
	store := newMasterviewStore(t, db)

	tenantID := seedMasterviewTenant(t, ctx, db, "empty")
	view, err := store.ViewBilling(ctx, tenantID)
	if err != nil {
		t.Fatalf("view billing: %v", err)
	}
	if !view.Subscription.IsEmpty() {
		t.Errorf("expected empty subscription, got %+v", view.Subscription)
	}
	if len(view.Invoices) != 0 {
		t.Errorf("expected 0 invoices, got %d", len(view.Invoices))
	}
	if len(view.Grants) != 0 {
		t.Errorf("expected 0 grants, got %d", len(view.Grants))
	}
	if view.TenantID != tenantID {
		t.Errorf("TenantID = %s, want %s", view.TenantID, tenantID)
	}
}

func TestMasterview_ViewBilling_FullAggregate(t *testing.T) {
	db := freshDBWithBilling(t)
	ctx := newMasterviewCtx(t)
	store := newMasterviewStore(t, db)

	tenantID, masterID := seedTenantUserMaster(t, db)
	planID := seedPlan(t, ctx, db, "pro", 1_000_000)
	pStart := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	pEnd := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	subID := seedMasterviewSubscription(t, ctx, db, tenantID, planID, masterID, pStart, pEnd)

	// Two invoices, one paid, one cancelled.
	seedMasterviewInvoice(t, ctx, db, tenantID, subID, masterID,
		time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC),
		19_900, "paid",
	)
	seedMasterviewCancelledInvoice(t, ctx, db, tenantID, subID, masterID,
		time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC),
		19_900,
	)

	// Three grants: active extra_tokens, revoked free_period, consumed extra_tokens.
	consumed := time.Date(2026, 3, 20, 8, 0, 0, 0, time.UTC)
	revoked := time.Date(2026, 4, 5, 10, 0, 0, 0, time.UTC)
	_, _ = seedMasterviewGrant(t, ctx, db, tenantID, masterID,
		"extra_tokens", map[string]any{"amount": 500_000},
		"Compensação por incidente",
		time.Date(2026, 4, 10, 9, 0, 0, 0, time.UTC),
		nil, nil,
	)
	_, _ = seedMasterviewGrant(t, ctx, db, tenantID, masterID,
		"free_subscription_period", map[string]any{"period_days": 30},
		"Período estendido para parceiro",
		time.Date(2026, 4, 8, 9, 0, 0, 0, time.UTC),
		nil, &revoked,
	)
	_, _ = seedMasterviewGrant(t, ctx, db, tenantID, masterID,
		"extra_tokens", map[string]any{"amount": 100_000},
		"Bonificação onboarding consumida",
		time.Date(2026, 3, 15, 9, 0, 0, 0, time.UTC),
		&consumed, nil,
	)

	view, err := store.ViewBilling(ctx, tenantID)
	if err != nil {
		t.Fatalf("view billing: %v", err)
	}
	// Subscription panel.
	if view.Subscription.IsEmpty() {
		t.Fatalf("expected subscription panel, got empty")
	}
	if view.Subscription.PlanSlug != "pro" {
		t.Errorf("PlanSlug = %q, want pro", view.Subscription.PlanSlug)
	}
	if view.Subscription.Status != "active" {
		t.Errorf("Status = %q, want active", view.Subscription.Status)
	}
	if !view.Subscription.NextInvoiceAt.Equal(pEnd) {
		t.Errorf("NextInvoiceAt = %v, want %v", view.Subscription.NextInvoiceAt, pEnd)
	}
	// Invoices panel — period DESC.
	if len(view.Invoices) != 2 {
		t.Fatalf("invoice count = %d, want 2", len(view.Invoices))
	}
	if view.Invoices[0].State != "paid" {
		t.Errorf("first invoice state = %q, want paid", view.Invoices[0].State)
	}
	if view.Invoices[1].State != "cancelled_by_master" {
		t.Errorf("second invoice state = %q, want cancelled_by_master", view.Invoices[1].State)
	}
	// Grants panel — AC #1 created_at DESC.
	if len(view.Grants) != 3 {
		t.Fatalf("grant count = %d, want 3", len(view.Grants))
	}
	// 2026-04-10 → 2026-04-08 → 2026-03-15.
	if !view.Grants[0].CreatedAt.After(view.Grants[1].CreatedAt) {
		t.Errorf("grants[0] should be newer than grants[1]")
	}
	if !view.Grants[1].CreatedAt.After(view.Grants[2].CreatedAt) {
		t.Errorf("grants[1] should be newer than grants[2]")
	}
	// State derivation.
	if view.Grants[0].Revoked || view.Grants[0].Consumed {
		t.Errorf("grants[0] should be active")
	}
	if !view.Grants[1].Revoked {
		t.Errorf("grants[1] should be revoked")
	}
	if !view.Grants[2].Consumed {
		t.Errorf("grants[2] should be consumed")
	}
	// Payload decoding.
	if view.Grants[0].Amount != 500_000 {
		t.Errorf("grants[0].Amount = %d, want 500000", view.Grants[0].Amount)
	}
	if view.Grants[1].PeriodDays != 30 {
		t.Errorf("grants[1].PeriodDays = %d, want 30", view.Grants[1].PeriodDays)
	}
}

// AC #3 — RLS isolation: tenant Y cannot see tenant X's billing via
// the same adapter call. The adapter must run inside WithTenant for
// the policy to fire.
func TestMasterview_ViewBilling_RLSIsolation(t *testing.T) {
	db := freshDBWithBilling(t)
	ctx := newMasterviewCtx(t)
	store := newMasterviewStore(t, db)

	tenantX, masterID := seedTenantUserMaster(t, db)
	planID := seedPlan(t, ctx, db, "pro", 1_000_000)
	seedMasterviewSubscription(t, ctx, db, tenantX, planID, masterID,
		time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC),
	)
	_, _ = seedMasterviewGrant(t, ctx, db, tenantX, masterID,
		"extra_tokens", map[string]any{"amount": 1000},
		"Grant para tenant X (não deve vazar)",
		time.Now().UTC(), nil, nil,
	)

	// Now ask the adapter for tenant Y (separate id) — RLS scopes the
	// view to Y, so we should see zero rows.
	tenantY := seedMasterviewTenant(t, ctx, db, "y")
	view, err := store.ViewBilling(ctx, tenantY)
	if err != nil {
		t.Fatalf("view billing for Y: %v", err)
	}
	if !view.Subscription.IsEmpty() {
		t.Errorf("RLS leak: tenant Y view carries subscription %+v", view.Subscription)
	}
	if len(view.Grants) != 0 {
		t.Errorf("RLS leak: tenant Y view carries %d grants", len(view.Grants))
	}
}

// ---------------------------------------------------------------------------
// ViewLedger
// ---------------------------------------------------------------------------

// TestMasterview_ViewBilling_GrantWithEmptyPayloadDegradesGracefully
// exercises the applyGrantPayload empty-payload + missing-key
// defensive branches. A real-world cause: a future grant kind that
// the UI does not yet recognise should render as "—" instead of
// crashing the page.
func TestMasterview_ViewBilling_GrantWithEmptyPayloadDegradesGracefully(t *testing.T) {
	db := freshDBWithBilling(t)
	ctx := newMasterviewCtx(t)
	store := newMasterviewStore(t, db)

	tenantID, masterID := seedTenantUserMaster(t, db)
	_, _ = seedMasterviewGrant(t, ctx, db, tenantID, masterID,
		"extra_tokens", map[string]any{}, // empty payload object
		"Grant com payload vazio (defesa em profundidade)",
		time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC),
		nil, nil,
	)
	_, _ = seedMasterviewGrant(t, ctx, db, tenantID, masterID,
		"free_subscription_period", map[string]any{"unrelated": "field"},
		"Payload sem a chave esperada (defesa em profundidade)",
		time.Date(2026, 5, 2, 0, 0, 0, 0, time.UTC),
		nil, nil,
	)

	view, err := store.ViewBilling(ctx, tenantID)
	if err != nil {
		t.Fatalf("view billing: %v", err)
	}
	if len(view.Grants) != 2 {
		t.Fatalf("grant count = %d, want 2", len(view.Grants))
	}
	for _, g := range view.Grants {
		if g.Amount != 0 {
			t.Errorf("Amount = %d, want 0 (empty payload)", g.Amount)
		}
		if g.PeriodDays != 0 {
			t.Errorf("PeriodDays = %d, want 0 (empty payload)", g.PeriodDays)
		}
	}
}

func TestMasterview_ViewLedger_Empty(t *testing.T) {
	db := freshDBWithBilling(t)
	ctx := newMasterviewCtx(t)
	store := newMasterviewStore(t, db)

	tenantID := seedMasterviewTenant(t, ctx, db, "empty-ledger")
	page, err := store.ViewLedger(ctx, master.LedgerOptions{TenantID: tenantID, PageSize: 10})
	if err != nil {
		t.Fatalf("view ledger: %v", err)
	}
	if len(page.Entries) != 0 {
		t.Errorf("entry count = %d, want 0", len(page.Entries))
	}
	if page.HasMore {
		t.Errorf("HasMore = true on empty ledger")
	}
}

func TestMasterview_ViewLedger_CursorPaginatesDescending(t *testing.T) {
	db := freshDBWithBilling(t)
	ctx := newMasterviewCtx(t)
	store := newMasterviewStore(t, db)

	tenantID, masterID := seedTenantUserMaster(t, db)
	walletID := seedMasterviewWallet(t, ctx, db, tenantID)

	// Seed a master_grant first so the master_grant ledger row passes
	// the token_ledger_master_grant_pairing CHECK.
	grantID, _ := seedMasterviewGrant(t, ctx, db, tenantID, masterID,
		"extra_tokens", map[string]any{"amount": 100_000},
		"Grant paginação", time.Now().UTC(), nil, nil,
	)

	base := time.Date(2026, 5, 16, 12, 0, 0, 0, time.UTC)
	const total = 25
	for i := 0; i < total; i++ {
		occurredAt := base.Add(-time.Duration(i) * time.Minute)
		var (
			kind   = "commit"
			amount = int64(-100)
			source = "consumption"
			mgID   *uuid.UUID
			extRef = fmt.Sprintf("wamid:%04d", i)
		)
		if i%5 == 0 {
			// Master-grant row.
			kind, amount, source = "grant", 100_000, "master_grant"
			gid := grantID
			mgID = &gid
			extRef = ""
		}
		seedMasterviewLedger(t, ctx, db, tenantID, walletID, kind, amount,
			source, mgID, occurredAt,
			fmt.Sprintf("rsv:%04d", i), extRef,
		)
	}

	// First page — 10 rows.
	page1, err := store.ViewLedger(ctx, master.LedgerOptions{TenantID: tenantID, PageSize: 10})
	if err != nil {
		t.Fatalf("page 1: %v", err)
	}
	if len(page1.Entries) != 10 {
		t.Fatalf("page1 size = %d, want 10", len(page1.Entries))
	}
	if !page1.HasMore {
		t.Errorf("HasMore = false on page 1; expected true")
	}
	// Descending.
	for i := 1; i < len(page1.Entries); i++ {
		if page1.Entries[i].OccurredAt.After(page1.Entries[i-1].OccurredAt) {
			t.Errorf("page1 row %d not in DESC order", i)
		}
	}

	// Second page using the cursor — should NOT include any page1 ids.
	page1IDs := map[uuid.UUID]struct{}{}
	for _, r := range page1.Entries {
		page1IDs[r.ID] = struct{}{}
	}
	page2, err := store.ViewLedger(ctx, master.LedgerOptions{
		TenantID:         tenantID,
		PageSize:         10,
		CursorOccurredAt: page1.NextCursorOccurredAt,
		CursorID:         page1.NextCursorID,
	})
	if err != nil {
		t.Fatalf("page 2: %v", err)
	}
	for _, r := range page2.Entries {
		if _, dup := page1IDs[r.ID]; dup {
			t.Errorf("page2 leaked page1 id %s", r.ID)
		}
	}
	if len(page2.Entries) != 10 {
		t.Fatalf("page2 size = %d, want 10", len(page2.Entries))
	}
	if !page2.HasMore {
		t.Errorf("HasMore = false on page 2; expected true (5 rows remaining)")
	}

	// Third page — should drain to 5 with HasMore=false.
	page3, err := store.ViewLedger(ctx, master.LedgerOptions{
		TenantID:         tenantID,
		PageSize:         10,
		CursorOccurredAt: page2.NextCursorOccurredAt,
		CursorID:         page2.NextCursorID,
	})
	if err != nil {
		t.Fatalf("page 3: %v", err)
	}
	if len(page3.Entries) != 5 {
		t.Fatalf("page3 size = %d, want 5", len(page3.Entries))
	}
	if page3.HasMore {
		t.Errorf("HasMore = true on page 3; expected false (drained)")
	}
}

func TestMasterview_ViewLedger_CrossReferenceFields(t *testing.T) {
	db := freshDBWithBilling(t)
	ctx := newMasterviewCtx(t)
	store := newMasterviewStore(t, db)

	tenantID, masterID := seedTenantUserMaster(t, db)
	walletID := seedMasterviewWallet(t, ctx, db, tenantID)
	planID := seedPlan(t, ctx, db, "pro", 1_000_000)
	pStart := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	pEnd := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	subID := seedMasterviewSubscription(t, ctx, db, tenantID, planID, masterID, pStart, pEnd)

	grantID, external := seedMasterviewGrant(t, ctx, db, tenantID, masterID,
		"extra_tokens", map[string]any{"amount": 100_000},
		"Crédito grant -> ledger ref", time.Now().UTC(), nil, nil,
	)

	// One master_grant row and one monthly_alloc row inside the active
	// subscription's period — both should link back to their ref.
	occGrant := time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC)
	occMonthly := time.Date(2026, 5, 5, 12, 0, 0, 0, time.UTC)
	gid := grantID
	seedMasterviewLedger(t, ctx, db, tenantID, walletID, "grant", 100_000,
		"master_grant", &gid, occGrant,
		"grant:"+external, "",
	)
	seedMasterviewLedger(t, ctx, db, tenantID, walletID, "grant", 1_000_000,
		"monthly_alloc", nil, occMonthly,
		"monthly:2026-05", "",
	)

	page, err := store.ViewLedger(ctx, master.LedgerOptions{TenantID: tenantID, PageSize: 10})
	if err != nil {
		t.Fatalf("view ledger: %v", err)
	}
	if len(page.Entries) != 2 {
		t.Fatalf("entry count = %d, want 2", len(page.Entries))
	}

	var grantRow, monthlyRow *master.LedgerRow
	for i := range page.Entries {
		row := &page.Entries[i]
		switch row.Source {
		case "master_grant":
			grantRow = row
		case "monthly_alloc":
			monthlyRow = row
		}
	}
	if grantRow == nil || monthlyRow == nil {
		t.Fatalf("missing entries: grant=%v monthly=%v", grantRow, monthlyRow)
	}
	// Cross-ref to master_grant.
	if grantRow.MasterGrantID != grantID {
		t.Errorf("master_grant link: got %s, want %s", grantRow.MasterGrantID, grantID)
	}
	if grantRow.MasterGrantExternalID != external {
		t.Errorf("master_grant external_id: got %q, want %q", grantRow.MasterGrantExternalID, external)
	}
	// Cross-ref to subscription (period contains the row).
	if monthlyRow.SubscriptionID != subID {
		t.Errorf("subscription link: got %s, want %s", monthlyRow.SubscriptionID, subID)
	}
	if monthlyRow.SubscriptionPlanSlug != "pro" {
		t.Errorf("plan slug: got %q, want pro", monthlyRow.SubscriptionPlanSlug)
	}
}

// AC #3 — RLS isolation on the ledger view: rows of tenant X must not
// surface in a tenant Y request.
func TestMasterview_ViewLedger_RLSIsolation(t *testing.T) {
	db := freshDBWithBilling(t)
	ctx := newMasterviewCtx(t)
	store := newMasterviewStore(t, db)

	tenantX, _ := seedTenantUserMaster(t, db)
	walletX := seedMasterviewWallet(t, ctx, db, tenantX)
	seedMasterviewLedger(t, ctx, db, tenantX, walletX, "commit", -100,
		"consumption", nil,
		time.Date(2026, 5, 16, 12, 0, 0, 0, time.UTC),
		"rsv:tenantX", "wamid:tenantX",
	)

	tenantY := seedMasterviewTenant(t, ctx, db, "y")
	page, err := store.ViewLedger(ctx, master.LedgerOptions{TenantID: tenantY, PageSize: 10})
	if err != nil {
		t.Fatalf("view ledger Y: %v", err)
	}
	if len(page.Entries) != 0 {
		t.Errorf("RLS leak: tenant Y page carries %d rows", len(page.Entries))
	}
}

// New rejects nil pool fast.
func TestMasterview_New_NilPoolRejected(t *testing.T) {
	if _, err := masterview.New(nil); err == nil {
		t.Fatalf("expected error for nil pool")
	}
}

func TestMasterview_ViewBilling_ZeroTenantRejected(t *testing.T) {
	db := freshDBWithBilling(t)
	ctx := newMasterviewCtx(t)
	store := newMasterviewStore(t, db)

	if _, err := store.ViewBilling(ctx, uuid.Nil); err == nil {
		t.Errorf("expected error for uuid.Nil tenant")
	}
}

func TestMasterview_ViewLedger_ZeroTenantRejected(t *testing.T) {
	db := freshDBWithBilling(t)
	ctx := newMasterviewCtx(t)
	store := newMasterviewStore(t, db)

	if _, err := store.ViewLedger(ctx, master.LedgerOptions{TenantID: uuid.Nil}); err == nil {
		t.Errorf("expected error for uuid.Nil tenant")
	}
}
