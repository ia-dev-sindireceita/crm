package postgres_test

// SIN-62965 / Fase 4 C14 — adapter tests for the dunning Store,
// TickStore, and CourtesyOverrideStore against the real Postgres
// testpg harness. Covers AC#1 (8d → suspended_outbound, 31d →
// suspended_full), AC#2 (CourtesyGrant.free_subscription_period
// override resets to current), AC#3 (payment confirmation drops the
// row back to current).
//
// Tests live in the parent postgres_test package (mastersession
// pattern) to share the bootstrap and avoid the ALTER ROLE race on the
// shared CI cluster — see reference_testpg_shared_cluster_race in
// agent memory.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/prometheus/client_golang/prometheus"

	postgresadapter "github.com/pericles-luz/crm/internal/adapter/db/postgres"
	dunningpg "github.com/pericles-luz/crm/internal/adapter/db/postgres/dunning"
	"github.com/pericles-luz/crm/internal/adapter/db/postgres/testpg"
	billingdunning "github.com/pericles-luz/crm/internal/billing/dunning"
	dunningworker "github.com/pericles-luz/crm/internal/worker/dunning"
)

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func newDunningCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func newDunningStore(t *testing.T, db *testpg.DB) *dunningpg.Store {
	t.Helper()
	store, err := dunningpg.New(db.RuntimePool(), db.MasterOpsPool())
	if err != nil {
		t.Fatalf("dunning store: %v", err)
	}
	return store
}

// seedDunningSubscription seeds a tenant, plan, and active subscription
// for the dunning-adapter tests. masterID is the audit actor used by
// the master_ops writes. Returns the tenant/subscription ids.
func seedDunningSubscription(t *testing.T, ctx context.Context, db *testpg.DB, label string) (tenantID, subID, planID, masterID uuid.UUID) {
	t.Helper()
	tenantID = seedTenantForBilling(t, ctx, db, label)
	masterID = uuid.New()
	if _, err := db.AdminPool().Exec(ctx,
		`INSERT INTO users (id, tenant_id, email, password_hash, role, is_master)
		 VALUES ($1, NULL, $2, 'x', 'master', true)`,
		masterID, fmt.Sprintf("dunning-%s-%s@x", label, masterID)); err != nil {
		t.Fatalf("seed master user: %v", err)
	}
	planID = seedPlan(t, ctx, db, fmt.Sprintf("dunning-%s", uuid.NewString()[:8]), 1_000_000)
	subID = seedActiveSubscription(t, ctx, db, tenantID, planID, masterID)
	return
}

// seedPendingInvoice writes one pending invoice for sub with the given
// period_start as its billing date.
func seedPendingInvoice(t *testing.T, ctx context.Context, db *testpg.DB, tenantID, subID, masterID uuid.UUID, periodStart time.Time) uuid.UUID {
	t.Helper()
	var invID uuid.UUID
	if err := postgresadapter.WithMasterOps(ctx, db.MasterOpsPool(), masterID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`INSERT INTO invoice
			   (tenant_id, subscription_id, period_start, period_end, amount_cents_brl, state)
			 VALUES ($1, $2, $3, $4, 4990, 'pending') RETURNING id`,
			tenantID, subID, periodStart, periodStart.AddDate(0, 1, 0)).Scan(&invID)
	}); err != nil {
		t.Fatalf("seed invoice: %v", err)
	}
	return invID
}

// markInvoicePaid flips an invoice to 'paid' so the candidate listing
// no longer sees it as past-due.
func markInvoicePaid(t *testing.T, ctx context.Context, db *testpg.DB, masterID, invID uuid.UUID) {
	t.Helper()
	if err := postgresadapter.WithMasterOps(ctx, db.MasterOpsPool(), masterID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE invoice SET state = 'paid' WHERE id = $1`, invID)
		return err
	}); err != nil {
		t.Fatalf("mark paid: %v", err)
	}
}

// seedDunningRow inserts a fresh current dunning row via the Store.
func seedDunningRow(t *testing.T, ctx context.Context, store *dunningpg.Store, tenantID, subID, masterID uuid.UUID, now time.Time) *billingdunning.DunningState {
	t.Helper()
	row, err := billingdunning.NewDunningState(tenantID, subID, now)
	if err != nil {
		t.Fatalf("new dunning: %v", err)
	}
	if err := store.Save(ctx, row, masterID); err != nil {
		t.Fatalf("save dunning: %v", err)
	}
	return row
}

// seedFreeSubscriptionGrant writes a master_grant of kind
// free_subscription_period whose payload encodes N months. createdAt
// drives the override window (until = createdAt + N months).
func seedFreeSubscriptionGrant(t *testing.T, ctx context.Context, db *testpg.DB, tenantID, masterID uuid.UUID, months int, createdAt time.Time) {
	t.Helper()
	payload, err := json.Marshal(map[string]any{"months": months})
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	if err := postgresadapter.WithMasterOps(ctx, db.MasterOpsPool(), masterID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO master_grant
			   (id, external_id, tenant_id, kind, payload, reason, created_by_user_id, created_at)
			 VALUES ($1, $2, $3, 'free_subscription_period', $4, $5, $6, $7)`,
			uuid.New(),
			fmt.Sprintf("01H%020d", time.Now().UnixNano()&0xfffff),
			tenantID, payload,
			"courtesia testes integ adapter", masterID, createdAt,
		)
		return err
	}); err != nil {
		t.Fatalf("seed grant: %v", err)
	}
}

// runOneTick builds a worker for the testpg cluster and runs Tick once.
// Helpful for AC#1 / AC#2 end-to-end assertions.
func runOneTick(t *testing.T, ctx context.Context, db *testpg.DB, store *dunningpg.Store, masterID uuid.UUID, now time.Time) {
	t.Helper()
	tick, err := dunningpg.NewTickStore(store, db.MasterOpsPool())
	if err != nil {
		t.Fatalf("new tick store: %v", err)
	}
	courtesy, err := dunningpg.NewCourtesyOverrideStore(db.MasterOpsPool())
	if err != nil {
		t.Fatalf("new courtesy store: %v", err)
	}
	reg := prometheus.NewRegistry()
	w, err := dunningworker.New(dunningworker.Config{
		Candidates: tick,
		Saver:      tick,
		Courtesy:   courtesy,
		Metrics:    dunningworker.NewMetrics(reg),
		ActorID:    masterID,
		Clock:      func() time.Time { return now },
		Logger:     slog.New(slog.NewTextHandler(discardWriter{}, nil)),
	})
	if err != nil {
		t.Fatalf("worker: %v", err)
	}
	if err := w.Tick(ctx); err != nil {
		t.Fatalf("tick: %v", err)
	}
}

type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }

// ---------------------------------------------------------------------------
// Store: round-trip / not-found.
// ---------------------------------------------------------------------------

func TestDunningStore_RoundTrip(t *testing.T) {
	db := freshDBWithPhase4(t)
	ctx := newDunningCtx(t)
	store := newDunningStore(t, db)

	tenantID, subID, _, masterID := seedDunningSubscription(t, ctx, db, "round-trip")
	now := time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC)
	want := seedDunningRow(t, ctx, store, tenantID, subID, masterID, now)

	got, err := store.GetBySubscription(ctx, subID)
	if err != nil {
		t.Fatalf("GetBySubscription: %v", err)
	}
	if got.State() != billingdunning.StateCurrent {
		t.Errorf("state = %v, want current", got.State())
	}
	if got.SubscriptionID() != want.SubscriptionID() {
		t.Errorf("subID mismatch: got %v, want %v", got.SubscriptionID(), want.SubscriptionID())
	}
	if !got.EnteredStateAt().Equal(now) {
		t.Errorf("entered_state_at = %v, want %v", got.EnteredStateAt(), now)
	}
}

func TestDunningStore_GetBySubscription_NotFound(t *testing.T) {
	db := freshDBWithPhase4(t)
	ctx := newDunningCtx(t)
	store := newDunningStore(t, db)

	if _, err := store.GetBySubscription(ctx, uuid.New()); !errors.Is(err, billingdunning.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
	if _, err := store.GetBySubscription(ctx, uuid.Nil); !errors.Is(err, billingdunning.ErrZeroSubscription) {
		t.Fatalf("err = %v, want ErrZeroSubscription", err)
	}
}

func TestDunningStore_Save_UpsertWithOverride(t *testing.T) {
	db := freshDBWithPhase4(t)
	ctx := newDunningCtx(t)
	store := newDunningStore(t, db)

	tenantID, subID, _, masterID := seedDunningSubscription(t, ctx, db, "save-override")
	now := time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC)
	row := seedDunningRow(t, ctx, store, tenantID, subID, masterID, now)
	until := now.Add(30 * 24 * time.Hour)
	if err := row.ApplyOverride(until, "Courtesy reason ten plus.", now); err != nil {
		t.Fatalf("apply override: %v", err)
	}
	if err := store.Save(ctx, row, masterID); err != nil {
		t.Fatalf("save: %v", err)
	}
	got, err := store.GetBySubscription(ctx, subID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.OverrideUntil() == nil || !got.OverrideUntil().Equal(until) {
		t.Errorf("override_until = %v, want %v", got.OverrideUntil(), until)
	}
	if got.OverrideReason() != "Courtesy reason ten plus." {
		t.Errorf("override_reason = %q", got.OverrideReason())
	}
}

func TestDunningStore_CurrentForTenant_Tenant(t *testing.T) {
	db := freshDBWithPhase4(t)
	ctx := newDunningCtx(t)
	store := newDunningStore(t, db)

	tenantID, subID, _, masterID := seedDunningSubscription(t, ctx, db, "current-tenant")
	now := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	seedDunningRow(t, ctx, store, tenantID, subID, masterID, now)

	got, err := store.CurrentForTenant(ctx, tenantID)
	if err != nil {
		t.Fatalf("CurrentForTenant: %v", err)
	}
	if got.SubscriptionID() != subID {
		t.Errorf("subID mismatch: got %v, want %v", got.SubscriptionID(), subID)
	}
	if _, err := store.CurrentForTenant(ctx, uuid.Nil); !errors.Is(err, billingdunning.ErrZeroTenant) {
		t.Fatalf("err = %v, want ErrZeroTenant", err)
	}
}

func TestDunningStore_CurrentForTenant_NotFound(t *testing.T) {
	db := freshDBWithPhase4(t)
	ctx := newDunningCtx(t)
	store := newDunningStore(t, db)

	tenantID := seedTenantForBilling(t, ctx, db, "no-row")
	if _, err := store.CurrentForTenant(ctx, tenantID); !errors.Is(err, billingdunning.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

// ---------------------------------------------------------------------------
// AC#1 — 8 days past due → suspended_outbound; 31 days → suspended_full.
// ---------------------------------------------------------------------------

func TestDunningTick_AC1_EightDaysPastDue_OutboundSuspended(t *testing.T) {
	db := freshDBWithPhase4(t)
	ctx := newDunningCtx(t)
	store := newDunningStore(t, db)

	tenantID, subID, _, masterID := seedDunningSubscription(t, ctx, db, "ac1-8d")
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	dueDate := now.Add(-8 * 24 * time.Hour)
	seedDunningRow(t, ctx, store, tenantID, subID, masterID, dueDate)
	seedPendingInvoice(t, ctx, db, tenantID, subID, masterID, dueDate)

	runOneTick(t, ctx, db, store, masterID, now)

	got, err := store.GetBySubscription(ctx, subID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.State() != billingdunning.StateSuspendedOutbound {
		t.Fatalf("state = %v, want suspended_outbound", got.State())
	}
}

func TestDunningTick_AC1_ThirtyOneDays_FullSuspended(t *testing.T) {
	db := freshDBWithPhase4(t)
	ctx := newDunningCtx(t)
	store := newDunningStore(t, db)

	tenantID, subID, _, masterID := seedDunningSubscription(t, ctx, db, "ac1-31d")
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	dueDate := now.Add(-31 * 24 * time.Hour)
	seedDunningRow(t, ctx, store, tenantID, subID, masterID, dueDate)
	seedPendingInvoice(t, ctx, db, tenantID, subID, masterID, dueDate)

	runOneTick(t, ctx, db, store, masterID, now)

	got, err := store.GetBySubscription(ctx, subID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.State() != billingdunning.StateSuspendedFull {
		t.Fatalf("state = %v, want suspended_full", got.State())
	}
}

func TestDunningTick_Idempotent(t *testing.T) {
	db := freshDBWithPhase4(t)
	ctx := newDunningCtx(t)
	store := newDunningStore(t, db)

	tenantID, subID, _, masterID := seedDunningSubscription(t, ctx, db, "ac1-idem")
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	dueDate := now.Add(-8 * 24 * time.Hour)
	seedDunningRow(t, ctx, store, tenantID, subID, masterID, dueDate)
	seedPendingInvoice(t, ctx, db, tenantID, subID, masterID, dueDate)

	// Re-running the tick in the same window must NOT produce a second
	// row transition (audit trail is one row per transition).
	runOneTick(t, ctx, db, store, masterID, now)
	runOneTick(t, ctx, db, store, masterID, now.Add(10*time.Minute))

	got, err := store.GetBySubscription(ctx, subID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.State() != billingdunning.StateSuspendedOutbound {
		t.Errorf("state = %v, want suspended_outbound", got.State())
	}

	// EnteredStateAt should equal the FIRST tick's now (not the second).
	if !got.EnteredStateAt().Equal(now) {
		t.Errorf("entered_state_at = %v, want %v (idempotent: second tick must not bump)", got.EnteredStateAt(), now)
	}
}

// ---------------------------------------------------------------------------
// AC#2 — CourtesyGrant.free_subscription_period resets to current.
// ---------------------------------------------------------------------------

func TestDunningTick_AC2_CourtesyGrantResetsToCurrent(t *testing.T) {
	db := freshDBWithPhase4(t)
	ctx := newDunningCtx(t)
	store := newDunningStore(t, db)

	tenantID, subID, _, masterID := seedDunningSubscription(t, ctx, db, "ac2-grant")
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	dueDate := now.Add(-31 * 24 * time.Hour)
	seedDunningRow(t, ctx, store, tenantID, subID, masterID, dueDate)
	seedPendingInvoice(t, ctx, db, tenantID, subID, masterID, dueDate)
	// One tick promotes to suspended_full.
	runOneTick(t, ctx, db, store, masterID, now)
	if got, err := store.GetBySubscription(ctx, subID); err != nil {
		t.Fatalf("get: %v", err)
	} else if got.State() != billingdunning.StateSuspendedFull {
		t.Fatalf("pre-grant state = %v, want suspended_full", got.State())
	}

	// Master grants a 1-month free subscription period.
	seedFreeSubscriptionGrant(t, ctx, db, tenantID, masterID, 1, now)

	// Next tick must reset to current and cache the override.
	runOneTick(t, ctx, db, store, masterID, now.Add(time.Hour))

	got, err := store.GetBySubscription(ctx, subID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.State() != billingdunning.StateCurrent {
		t.Errorf("state = %v, want current (grant resets)", got.State())
	}
	if got.OverrideUntil() == nil {
		t.Errorf("override_until is nil, want a future timestamp")
	}
}

// ---------------------------------------------------------------------------
// AC#3 — Payment confirmed → state back to current.
// ---------------------------------------------------------------------------

func TestDunningTick_AC3_PaymentConfirmedNextTickResetsToCurrent(t *testing.T) {
	db := freshDBWithPhase4(t)
	ctx := newDunningCtx(t)
	store := newDunningStore(t, db)

	tenantID, subID, _, masterID := seedDunningSubscription(t, ctx, db, "ac3-pay-tick")
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	dueDate := now.Add(-31 * 24 * time.Hour)
	seedDunningRow(t, ctx, store, tenantID, subID, masterID, dueDate)
	invID := seedPendingInvoice(t, ctx, db, tenantID, subID, masterID, dueDate)
	runOneTick(t, ctx, db, store, masterID, now)
	if got, _ := store.GetBySubscription(ctx, subID); got.State() != billingdunning.StateSuspendedFull {
		t.Fatalf("pre-payment state = %v, want suspended_full", got.State())
	}

	// Payment lands; webhook handler in production calls
	// OnPaymentConfirmed for the immediate path. We assert the
	// tick-driven downgrade path here: mark the invoice paid and
	// rerun the tick.
	markInvoicePaid(t, ctx, db, masterID, invID)
	runOneTick(t, ctx, db, store, masterID, now.Add(time.Hour))

	got, err := store.GetBySubscription(ctx, subID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.State() != billingdunning.StateCurrent {
		t.Errorf("state = %v, want current (no pending invoice → MarkPaid)", got.State())
	}
}

func TestDunningWorker_AC3_OnPaymentConfirmed_Immediate(t *testing.T) {
	db := freshDBWithPhase4(t)
	ctx := newDunningCtx(t)
	store := newDunningStore(t, db)

	tenantID, subID, _, masterID := seedDunningSubscription(t, ctx, db, "ac3-pay-imm")
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	dueDate := now.Add(-31 * 24 * time.Hour)
	seedDunningRow(t, ctx, store, tenantID, subID, masterID, dueDate)
	seedPendingInvoice(t, ctx, db, tenantID, subID, masterID, dueDate)
	runOneTick(t, ctx, db, store, masterID, now)

	tick, err := dunningpg.NewTickStore(store, db.MasterOpsPool())
	if err != nil {
		t.Fatalf("tick store: %v", err)
	}
	reg := prometheus.NewRegistry()
	w, err := dunningworker.New(dunningworker.Config{
		Candidates: tick,
		Saver:      tick,
		Courtesy:   dunningworker.NoCourtesyOverride{},
		Metrics:    dunningworker.NewMetrics(reg),
		ActorID:    masterID,
		Clock:      func() time.Time { return now.Add(time.Hour) },
		Logger:     slog.New(slog.NewTextHandler(discardWriter{}, nil)),
	})
	if err != nil {
		t.Fatalf("worker: %v", err)
	}
	loader := func(ctx context.Context, id uuid.UUID) (*billingdunning.DunningState, error) {
		return store.GetBySubscription(ctx, id)
	}
	if err := w.OnPaymentConfirmed(ctx, loader, subID); err != nil {
		t.Fatalf("OnPaymentConfirmed: %v", err)
	}
	got, err := store.GetBySubscription(ctx, subID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.State() != billingdunning.StateCurrent {
		t.Errorf("state = %v, want current (immediate downgrade)", got.State())
	}
}

// ---------------------------------------------------------------------------
// CourtesyOverrideStore — payload parsing, revocation, multi-tenant.
// ---------------------------------------------------------------------------

func TestCourtesyOverrideStore_ActiveFor(t *testing.T) {
	db := freshDBWithPhase4(t)
	ctx := newDunningCtx(t)
	cs, err := dunningpg.NewCourtesyOverrideStore(db.MasterOpsPool())
	if err != nil {
		t.Fatalf("store: %v", err)
	}

	tenantID, _, _, masterID := seedDunningSubscription(t, ctx, db, "courtesy")
	now := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)

	// Nothing yet → ErrNoActiveOverride.
	if _, err := cs.ActiveFor(ctx, tenantID, now); !errors.Is(err, billingdunning.ErrNoActiveOverride) {
		t.Fatalf("err = %v, want ErrNoActiveOverride", err)
	}
	if _, err := cs.ActiveFor(ctx, uuid.Nil, now); !errors.Is(err, billingdunning.ErrZeroTenant) {
		t.Fatalf("err = %v, want ErrZeroTenant", err)
	}

	// 1-month grant created an hour ago → active.
	seedFreeSubscriptionGrant(t, ctx, db, tenantID, masterID, 1, now.Add(-time.Hour))
	got, err := cs.ActiveFor(ctx, tenantID, now)
	if err != nil {
		t.Fatalf("ActiveFor: %v", err)
	}
	if !got.Until.After(now) {
		t.Errorf("until = %v, want > %v", got.Until, now)
	}
	if got.Reason == "" {
		t.Errorf("reason is empty")
	}

	// Expired grant (months back enough that until < now) → not active.
	otherTenant := seedTenantForBilling(t, ctx, db, "expired")
	otherMaster := uuid.New()
	if _, err := db.AdminPool().Exec(ctx,
		`INSERT INTO users (id, tenant_id, email, password_hash, role, is_master)
		 VALUES ($1, NULL, $2, 'x', 'master', true)`,
		otherMaster, fmt.Sprintf("expired-%s@x", otherMaster)); err != nil {
		t.Fatalf("seed master: %v", err)
	}
	seedFreeSubscriptionGrant(t, ctx, db, otherTenant, otherMaster, 1, now.AddDate(0, -2, 0))
	if _, err := cs.ActiveFor(ctx, otherTenant, now); !errors.Is(err, billingdunning.ErrNoActiveOverride) {
		t.Fatalf("expired err = %v, want ErrNoActiveOverride", err)
	}
}

func TestCourtesyOverrideStore_IgnoresRevokedAndBadPayload(t *testing.T) {
	db := freshDBWithPhase4(t)
	ctx := newDunningCtx(t)
	cs, err := dunningpg.NewCourtesyOverrideStore(db.MasterOpsPool())
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	tenantID, _, _, masterID := seedDunningSubscription(t, ctx, db, "revoked")
	now := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)

	// Bad payload (no months) — must be skipped.
	if err := postgresadapter.WithMasterOps(ctx, db.MasterOpsPool(), masterID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO master_grant
			   (id, external_id, tenant_id, kind, payload, reason, created_by_user_id, created_at)
			 VALUES ($1, $2, $3, 'free_subscription_period', $4, $5, $6, $7)`,
			uuid.New(), fmt.Sprintf("01H%020d", time.Now().UnixNano()&0xfffff),
			tenantID, []byte(`{"unrelated":1}`), "Payload sem months campo", masterID, now,
		)
		return err
	}); err != nil {
		t.Fatalf("seed bad payload: %v", err)
	}

	// Revoked grant — must be skipped even with valid payload.
	if err := postgresadapter.WithMasterOps(ctx, db.MasterOpsPool(), masterID, func(tx pgx.Tx) error {
		id := uuid.New()
		if _, err := tx.Exec(ctx,
			`INSERT INTO master_grant
			   (id, external_id, tenant_id, kind, payload, reason, created_by_user_id, created_at)
			 VALUES ($1, $2, $3, 'free_subscription_period', $4, $5, $6, $7)`,
			id, fmt.Sprintf("01H%020d", time.Now().UnixNano()&0xfffff),
			tenantID, []byte(`{"months":1}`), "Revogada antes do uso", masterID, now,
		); err != nil {
			return err
		}
		_, err := tx.Exec(ctx,
			`UPDATE master_grant
			    SET revoked_at = $1, revoked_by_user_id = $2, revoke_reason = $3
			  WHERE id = $4`, now, masterID, "Revogada para fins de teste", id)
		return err
	}); err != nil {
		t.Fatalf("seed revoked: %v", err)
	}

	if _, err := cs.ActiveFor(ctx, tenantID, now); !errors.Is(err, billingdunning.ErrNoActiveOverride) {
		t.Fatalf("err = %v, want ErrNoActiveOverride (only bad payload + revoked rows exist)", err)
	}
}

// ---------------------------------------------------------------------------
// TickStore listing — past-due invoice attaches Pending; no pending → nil.
// ---------------------------------------------------------------------------

func TestTickStore_ListCandidates_Empty(t *testing.T) {
	db := freshDBWithPhase4(t)
	ctx := newDunningCtx(t)
	store := newDunningStore(t, db)
	tick, err := dunningpg.NewTickStore(store, db.MasterOpsPool())
	if err != nil {
		t.Fatalf("tick store: %v", err)
	}
	got, err := tick.ListCandidates(ctx, time.Now(), 100)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("len = %d, want 0", len(got))
	}
}

func TestTickStore_ListCandidates_AttachesPending(t *testing.T) {
	db := freshDBWithPhase4(t)
	ctx := newDunningCtx(t)
	store := newDunningStore(t, db)

	tenantID, subID, _, masterID := seedDunningSubscription(t, ctx, db, "attach-pending")
	now := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	seedDunningRow(t, ctx, store, tenantID, subID, masterID, now.Add(-10*24*time.Hour))
	invID := seedPendingInvoice(t, ctx, db, tenantID, subID, masterID, now.Add(-10*24*time.Hour))

	tick, err := dunningpg.NewTickStore(store, db.MasterOpsPool())
	if err != nil {
		t.Fatalf("tick store: %v", err)
	}
	got, err := tick.ListCandidates(ctx, now, 100)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len = %d, want 1", len(got))
	}
	c := got[0]
	if c.SubscriptionID != subID {
		t.Errorf("subID = %v, want %v", c.SubscriptionID, subID)
	}
	if c.Pending == nil {
		t.Fatalf("Pending = nil, want invoice %v", invID)
	}
	if c.Pending.ID != invID {
		t.Errorf("pending ID = %v, want %v", c.Pending.ID, invID)
	}
}

func TestTickStore_ListCandidates_NoPendingWhenAllPaid(t *testing.T) {
	db := freshDBWithPhase4(t)
	ctx := newDunningCtx(t)
	store := newDunningStore(t, db)

	tenantID, subID, _, masterID := seedDunningSubscription(t, ctx, db, "no-pending")
	now := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	seedDunningRow(t, ctx, store, tenantID, subID, masterID, now.Add(-10*24*time.Hour))
	invID := seedPendingInvoice(t, ctx, db, tenantID, subID, masterID, now.Add(-10*24*time.Hour))
	markInvoicePaid(t, ctx, db, masterID, invID)

	tick, err := dunningpg.NewTickStore(store, db.MasterOpsPool())
	if err != nil {
		t.Fatalf("tick store: %v", err)
	}
	got, err := tick.ListCandidates(ctx, now, 100)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len = %d, want 1", len(got))
	}
	if got[0].Pending != nil {
		t.Errorf("Pending = %+v, want nil after paid", got[0].Pending)
	}
}

func TestTickStore_ListCandidates_ExcludesCancelled(t *testing.T) {
	db := freshDBWithPhase4(t)
	ctx := newDunningCtx(t)
	store := newDunningStore(t, db)

	tenantID, subID, _, masterID := seedDunningSubscription(t, ctx, db, "excl-cancelled")
	now := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	row := seedDunningRow(t, ctx, store, tenantID, subID, masterID, now.Add(-90*24*time.Hour))
	// Force the row to cancelled via direct write under master_ops.
	if err := postgresadapter.WithMasterOps(ctx, db.MasterOpsPool(), masterID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`UPDATE subscription_dunning_states SET state = 'cancelled' WHERE id = $1`,
			row.ID())
		return err
	}); err != nil {
		t.Fatalf("force cancel: %v", err)
	}
	tick, err := dunningpg.NewTickStore(store, db.MasterOpsPool())
	if err != nil {
		t.Fatalf("tick store: %v", err)
	}
	got, err := tick.ListCandidates(ctx, now, 100)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("cancelled leaked: %d candidates", len(got))
	}
}
