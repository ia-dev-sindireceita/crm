package postgres_test

// SIN-63954 / Fase 4 — integration tests for the walletui adapter
// (web/walletui.DashboardReader + LedgerReader + TopupCatalogReader).
//
// freshDBWithPhase4 lives in phase4_migration_test.go (same parent
// _test package per reference_testpg_shared_cluster_race).

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	postgresadapter "github.com/pericles-luz/crm/internal/adapter/db/postgres"
	"github.com/pericles-luz/crm/internal/adapter/db/postgres/testpg"
	"github.com/pericles-luz/crm/internal/adapter/db/postgres/walletui"
	"github.com/pericles-luz/crm/internal/wallet"
	walletuiport "github.com/pericles-luz/crm/internal/web/walletui"
)

func newWalletUICtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func newWalletUIStore(t *testing.T, db *testpg.DB) *walletui.Store {
	t.Helper()
	store, err := walletui.New(db.RuntimePool())
	if err != nil {
		t.Fatalf("walletui store: %v", err)
	}
	return store
}

// seedWalletUITenant inserts a tenant row directly so the test does not
// have to mint a master user (this adapter is gerente-facing and does
// not touch the master_ops audit triggers for its reads).
func seedWalletUITenant(t *testing.T, ctx context.Context, db *testpg.DB, label string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if _, err := db.AdminPool().Exec(ctx,
		`INSERT INTO tenants (id, name, host) VALUES ($1, $2, $3)`,
		id, label, fmt.Sprintf("walletui-%s-%s.crm.local", label, id),
	); err != nil {
		t.Fatalf("seed tenant %s: %v", label, err)
	}
	return id
}

// seedWalletUIWallet writes a token_wallet row with the supplied
// balance and reserved. Returns the wallet id.
func seedWalletUIWallet(t *testing.T, ctx context.Context, db *testpg.DB, tenantID uuid.UUID, balance, reserved int64) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if _, err := db.AdminPool().Exec(ctx,
		`INSERT INTO token_wallet (id, tenant_id, balance, reserved, version)
		 VALUES ($1, $2, $3, $4, 1)`,
		id, tenantID, balance, reserved,
	); err != nil {
		t.Fatalf("seed wallet: %v", err)
	}
	return id
}

// seedWalletUILedger writes a single wallet-aware token_ledger row.
// metadata is a JSON literal so the test can drive the LGPD redaction
// path verbatim.
func seedWalletUILedger(t *testing.T, ctx context.Context, db *testpg.DB,
	tenantID, walletID uuid.UUID,
	kind wallet.LedgerKind, amount int64,
	source wallet.LedgerSource,
	occurredAt time.Time,
	metadataJSON string,
	externalRef string,
) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if err := postgresadapter.WithTenant(ctx, db.RuntimePool(), tenantID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO token_ledger
			   (id, wallet_id, tenant_id, kind, amount, idempotency_key, external_ref,
			    source, metadata, occurred_at, created_at)
			 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9::jsonb,$10,$10)`,
			id, walletID, tenantID, string(kind), amount, id.String(), externalRef,
			string(source), metadataJSON, occurredAt,
		)
		return err
	}); err != nil {
		t.Fatalf("seed ledger %s: %v", kind, err)
	}
	return id
}

// seedWalletUIDunning writes one subscription_dunning_states row tied
// to the supplied subscription id. override + reason are optional —
// pass empty strings to leave them NULL.
func seedWalletUIDunning(t *testing.T, ctx context.Context, db *testpg.DB,
	tenantID, subID uuid.UUID, state string, overrideUntil *time.Time, reason string,
) {
	t.Helper()
	var err error
	if overrideUntil == nil {
		_, err = db.AdminPool().Exec(ctx,
			`INSERT INTO subscription_dunning_states (tenant_id, subscription_id, state)
			 VALUES ($1, $2, $3)`, tenantID, subID, state)
	} else {
		_, err = db.AdminPool().Exec(ctx,
			`INSERT INTO subscription_dunning_states (tenant_id, subscription_id, state, override_until, override_reason)
			 VALUES ($1, $2, $3, $4, $5)`, tenantID, subID, state, *overrideUntil, reason)
	}
	if err != nil {
		t.Fatalf("seed dunning: %v", err)
	}
}

// seedWalletUIPackages inserts the canonical small/medium/large rows
// the F5 UI lists. Mirrors the production seed file (seed/token_packages.sql)
// without re-applying the file — keeps the test independent of the
// seed runner.
func seedWalletUIPackages(t *testing.T, ctx context.Context, db *testpg.DB) {
	t.Helper()
	rows := []struct {
		slug   string
		name   string
		tokens int64
		cents  int
	}{
		{"small", "Small", 1_000_000, 1500},
		{"medium", "Medium", 5_000_000, 4900},
		{"large", "Large", 20_000_000, 14900},
	}
	for _, r := range rows {
		if _, err := db.AdminPool().Exec(ctx,
			`INSERT INTO token_packages (slug, kind, name, tokens, price_cents_brl)
			 VALUES ($1, 'tokens', $2, $3, $4)
			 ON CONFLICT (slug) DO NOTHING`,
			r.slug, r.name, r.tokens, r.cents,
		); err != nil {
			t.Fatalf("seed token_packages %s: %v", r.slug, err)
		}
	}
}

// hashConvID mirrors the adapter's LGPD hash so tests can verify the
// projected value without poking at adapter internals.
func hashConvID(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])[:16]
}

// ---------------------------------------------------------------------------
// Snapshot
// ---------------------------------------------------------------------------

// TestWalletUI_Snapshot_HappyPath drives the dashboard aggregate end-
// to-end: a wallet with balance/reserved, 14 days of commit activity,
// dunning row + reprieve, and the last-five preview.
func TestWalletUI_Snapshot_HappyPath(t *testing.T) {
	db := freshDBWithPhase4(t)
	ctx := newWalletUICtx(t)
	store := newWalletUIStore(t, db)

	tenantID, masterID := seedTenantUserMaster(t, db)
	walletID := seedWalletUIWallet(t, ctx, db, tenantID, 100_000, 5_000)

	// Two commits inside the 14-day window so AvgDailyConsume > 0.
	now := time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC)
	seedWalletUILedger(t, ctx, db, tenantID, walletID,
		wallet.KindCommit, -7_000, wallet.SourceConsumption,
		now.Add(-2*24*time.Hour),
		`{"conversation_id":"conv-1","model":"gemini-flash"}`, "wamid-1",
	)
	seedWalletUILedger(t, ctx, db, tenantID, walletID,
		wallet.KindCommit, -7_000, wallet.SourceConsumption,
		now.Add(-10*24*time.Hour),
		`{"conversation_id":"conv-2","model":"haiku"}`, "wamid-2",
	)

	// Active subscription + dunning warn + reprieve.
	subID := seedActivePhase4Subscription(t, ctx, db, tenantID, masterID)
	override := now.Add(48 * time.Hour)
	seedWalletUIDunning(t, ctx, db, tenantID, subID, "warn", &override, "manual reprieve confirmed")

	snap, err := store.Snapshot(ctx, tenantID, now)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if snap.Balance != 100_000 {
		t.Errorf("Balance = %d, want 100000", snap.Balance)
	}
	if snap.Reserved != 5_000 {
		t.Errorf("Reserved = %d, want 5000", snap.Reserved)
	}
	if snap.Available != 95_000 {
		t.Errorf("Available = %d, want 95000", snap.Available)
	}
	// 14k tokens over 14 days = 1k tokens/day.
	if snap.AvgDailyConsume != 1_000 {
		t.Errorf("AvgDailyConsume = %d, want 1000", snap.AvgDailyConsume)
	}
	if snap.DaysRemaining == nil || *snap.DaysRemaining != 95 {
		t.Errorf("DaysRemaining = %v, want 95", snap.DaysRemaining)
	}
	if snap.DunningState != "warn" {
		t.Errorf("DunningState = %q, want warn", snap.DunningState)
	}
	if snap.DunningOverrideUntil == nil || !snap.DunningOverrideUntil.Equal(override) {
		t.Errorf("DunningOverrideUntil = %v, want %v", snap.DunningOverrideUntil, override)
	}
	if len(snap.LastFive) != 2 {
		t.Fatalf("LastFive count = %d, want 2", len(snap.LastFive))
	}
	// LastFive in DESC order, BalanceAfter rolled back from current balance.
	if snap.LastFive[0].BalanceAfter != 100_000 {
		t.Errorf("LastFive[0].BalanceAfter = %d, want 100000", snap.LastFive[0].BalanceAfter)
	}
	if snap.LastFive[1].BalanceAfter != 107_000 {
		// Rolling back the newer -7000 commit: 100000 - (-7000) = 107000.
		t.Errorf("LastFive[1].BalanceAfter = %d, want 107000", snap.LastFive[1].BalanceAfter)
	}
	// LGPD redaction on the most recent commit.
	if got, want := snap.LastFive[0].ConversationIDHash, hashConvID("conv-1"); got != want {
		t.Errorf("LastFive[0].ConversationIDHash = %q, want %q", got, want)
	}
	if snap.LastFive[0].Model != "gemini-flash" {
		t.Errorf("LastFive[0].Model = %q, want gemini-flash", snap.LastFive[0].Model)
	}
	if strings.Contains(snap.LastFive[0].ConversationIDHash, "conv-1") {
		t.Errorf("ConversationIDHash leaked raw value: %q", snap.LastFive[0].ConversationIDHash)
	}
}

// TestWalletUI_Snapshot_NoConsumptionLeavesDaysRemainingNil ensures the
// "cannot project" path collapses to a nil DaysRemaining, not a divide-
// by-zero panic.
func TestWalletUI_Snapshot_NoConsumptionLeavesDaysRemainingNil(t *testing.T) {
	db := freshDBWithPhase4(t)
	ctx := newWalletUICtx(t)
	store := newWalletUIStore(t, db)

	tenantID := seedWalletUITenant(t, ctx, db, "no-consume")
	seedWalletUIWallet(t, ctx, db, tenantID, 50_000, 0)

	snap, err := store.Snapshot(ctx, tenantID, time.Now().UTC())
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if snap.AvgDailyConsume != 0 {
		t.Errorf("AvgDailyConsume = %d, want 0", snap.AvgDailyConsume)
	}
	if snap.DaysRemaining != nil {
		t.Errorf("DaysRemaining = %v, want nil", snap.DaysRemaining)
	}
	if snap.DunningState != "" {
		t.Errorf("DunningState = %q, want empty", snap.DunningState)
	}
}

// TestWalletUI_Snapshot_NoWalletRowReturnsErrTenantNotFound is the
// distinguished "wallet missing" path the handler maps to 404.
func TestWalletUI_Snapshot_NoWalletRowReturnsErrTenantNotFound(t *testing.T) {
	db := freshDBWithPhase4(t)
	ctx := newWalletUICtx(t)
	store := newWalletUIStore(t, db)

	tenantID := seedWalletUITenant(t, ctx, db, "no-wallet")

	_, err := store.Snapshot(ctx, tenantID, time.Now().UTC())
	if err == nil {
		t.Fatal("snapshot: want ErrTenantNotFound, got nil")
	}
	if err != walletuiport.ErrTenantNotFound && !strings.Contains(err.Error(), "wallet not found") {
		t.Errorf("snapshot err = %v, want ErrTenantNotFound", err)
	}
}

// TestWalletUI_Snapshot_NilTenantRejected guards uuid.Nil at the
// adapter boundary.
func TestWalletUI_Snapshot_NilTenantRejected(t *testing.T) {
	db := freshDBWithPhase4(t)
	ctx := newWalletUICtx(t)
	store := newWalletUIStore(t, db)
	_, err := store.Snapshot(ctx, uuid.Nil, time.Now().UTC())
	if err != walletuiport.ErrTenantNotFound {
		t.Errorf("snapshot uuid.Nil = %v, want ErrTenantNotFound", err)
	}
}

// TestWalletUI_Snapshot_ExpiredOverrideClearsField confirms that an
// override timestamp in the past is treated as "no longer in effect".
func TestWalletUI_Snapshot_ExpiredOverrideClearsField(t *testing.T) {
	db := freshDBWithPhase4(t)
	ctx := newWalletUICtx(t)
	store := newWalletUIStore(t, db)

	tenantID, masterID := seedTenantUserMaster(t, db)
	seedWalletUIWallet(t, ctx, db, tenantID, 1_000, 0)
	subID := seedActivePhase4Subscription(t, ctx, db, tenantID, masterID)

	expired := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	seedWalletUIDunning(t, ctx, db, tenantID, subID, "warn", &expired, "reprieve from january")

	snap, err := store.Snapshot(ctx, tenantID, time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if snap.DunningState != "warn" {
		t.Errorf("DunningState = %q, want warn", snap.DunningState)
	}
	if snap.DunningOverrideUntil != nil {
		t.Errorf("DunningOverrideUntil = %v, want nil (expired)", snap.DunningOverrideUntil)
	}
}

// ---------------------------------------------------------------------------
// Page (ledger pagination)
// ---------------------------------------------------------------------------

// TestWalletUI_Page_CursorPagination seeds N+2 rows then walks the
// cursor and verifies (a) DESC order, (b) HasMore, (c) cursor
// continuity, (d) no row dropped at the cursor boundary.
func TestWalletUI_Page_CursorPagination(t *testing.T) {
	db := freshDBWithPhase4(t)
	ctx := newWalletUICtx(t)
	store := newWalletUIStore(t, db)

	tenantID := seedWalletUITenant(t, ctx, db, "cursor")
	walletID := seedWalletUIWallet(t, ctx, db, tenantID, 200_000, 0)

	base := time.Date(2026, 4, 1, 10, 0, 0, 0, time.UTC)
	const total = 12
	for i := 0; i < total; i++ {
		seedWalletUILedger(t, ctx, db, tenantID, walletID,
			wallet.KindCommit, -100, wallet.SourceConsumption,
			base.Add(time.Duration(i)*time.Hour),
			`{}`, "",
		)
	}

	pageSize := 5
	seen := make(map[uuid.UUID]bool)
	var page walletuiport.LedgerPage
	var prevOccurredAt time.Time
	for i := 0; i < 5; i++ {
		opts := walletuiport.LedgerPageOptions{
			Filter:           walletuiport.LedgerFilter{TenantID: tenantID},
			CursorOccurredAt: page.NextCursorOccurredAt,
			CursorID:         page.NextCursorID,
			PageSize:         pageSize,
		}
		var err error
		page, err = store.Page(ctx, opts)
		if err != nil {
			t.Fatalf("page %d: %v", i, err)
		}
		for _, e := range page.Entries {
			if seen[e.ID] {
				t.Errorf("page %d: duplicate id %s", i, e.ID)
			}
			seen[e.ID] = true
			if !prevOccurredAt.IsZero() && e.OccurredAt.After(prevOccurredAt) {
				t.Errorf("page %d: row %s out of DESC order", i, e.ID)
			}
			prevOccurredAt = e.OccurredAt
		}
		if !page.HasMore {
			break
		}
	}
	if len(seen) != total {
		t.Errorf("seen %d rows across pages, want %d", len(seen), total)
	}
}

// TestWalletUI_Page_FilterByKindAndDateRange exercises the kind +
// from/to filter knobs together.
func TestWalletUI_Page_FilterByKindAndDateRange(t *testing.T) {
	db := freshDBWithPhase4(t)
	ctx := newWalletUICtx(t)
	store := newWalletUIStore(t, db)

	tenantID := seedWalletUITenant(t, ctx, db, "filter")
	walletID := seedWalletUIWallet(t, ctx, db, tenantID, 1_000_000, 0)

	now := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	// Inside window: 3 commits + 1 grant.
	seedWalletUILedger(t, ctx, db, tenantID, walletID, wallet.KindCommit, -100, wallet.SourceConsumption,
		now.Add(-2*24*time.Hour), `{}`, "")
	seedWalletUILedger(t, ctx, db, tenantID, walletID, wallet.KindCommit, -100, wallet.SourceConsumption,
		now.Add(-1*24*time.Hour), `{}`, "")
	seedWalletUILedger(t, ctx, db, tenantID, walletID, wallet.KindCommit, -100, wallet.SourceConsumption,
		now.Add(-1*time.Hour), `{}`, "")
	seedWalletUILedger(t, ctx, db, tenantID, walletID, wallet.KindGrant, 500, wallet.SourceMonthlyAlloc,
		now.Add(-12*time.Hour), `{}`, "")
	// Outside window (too old).
	seedWalletUILedger(t, ctx, db, tenantID, walletID, wallet.KindCommit, -100, wallet.SourceConsumption,
		now.Add(-30*24*time.Hour), `{}`, "")

	page, err := store.Page(ctx, walletuiport.LedgerPageOptions{
		Filter: walletuiport.LedgerFilter{
			TenantID:       tenantID,
			FromOccurredAt: now.Add(-7 * 24 * time.Hour),
			ToOccurredAt:   now,
			Kinds:          []wallet.LedgerKind{wallet.KindCommit},
		},
		PageSize: 50,
	})
	if err != nil {
		t.Fatalf("page: %v", err)
	}
	if len(page.Entries) != 3 {
		t.Errorf("entries = %d, want 3 (commits only, in window)", len(page.Entries))
	}
	for _, e := range page.Entries {
		if e.Kind != wallet.KindCommit {
			t.Errorf("filter leak: kind = %s", e.Kind)
		}
	}
}

// TestWalletUI_Page_LGPDRedactsConversationID confirms the raw
// conversation id never appears in the projection — only the hash.
func TestWalletUI_Page_LGPDRedactsConversationID(t *testing.T) {
	db := freshDBWithPhase4(t)
	ctx := newWalletUICtx(t)
	store := newWalletUIStore(t, db)

	tenantID := seedWalletUITenant(t, ctx, db, "lgpd")
	walletID := seedWalletUIWallet(t, ctx, db, tenantID, 5_000, 0)

	rawConvID := "conv-LGPD-secret-abc-12345"
	seedWalletUILedger(t, ctx, db, tenantID, walletID,
		wallet.KindCommit, -100, wallet.SourceConsumption,
		time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC),
		fmt.Sprintf(`{"conversation_id":%q,"model":"gemini-flash","ai_policy_id":"00000000-0000-0000-0000-000000000123"}`, rawConvID),
		"wamid",
	)

	page, err := store.Page(ctx, walletuiport.LedgerPageOptions{
		Filter:   walletuiport.LedgerFilter{TenantID: tenantID},
		PageSize: 10,
	})
	if err != nil {
		t.Fatalf("page: %v", err)
	}
	if len(page.Entries) != 1 {
		t.Fatalf("entries = %d, want 1", len(page.Entries))
	}
	e := page.Entries[0]
	if e.ConversationIDHash == "" {
		t.Errorf("ConversationIDHash empty, want hash")
	}
	if e.ConversationIDHash != hashConvID(rawConvID) {
		t.Errorf("ConversationIDHash = %q, want %q", e.ConversationIDHash, hashConvID(rawConvID))
	}
	if strings.Contains(e.ConversationIDHash, "secret") || strings.Contains(e.ConversationIDHash, rawConvID) {
		t.Errorf("LGPD leak: %q exposes raw conversation_id %q", e.ConversationIDHash, rawConvID)
	}
	if e.Model != "gemini-flash" {
		t.Errorf("Model = %q, want gemini-flash", e.Model)
	}
	wantPolicy, _ := uuid.Parse("00000000-0000-0000-0000-000000000123")
	if e.PolicyID != wantPolicy {
		t.Errorf("PolicyID = %v, want %v", e.PolicyID, wantPolicy)
	}
}

// TestWalletUI_Page_RejectsNilTenant guards the boundary.
func TestWalletUI_Page_RejectsNilTenant(t *testing.T) {
	db := freshDBWithPhase4(t)
	ctx := newWalletUICtx(t)
	store := newWalletUIStore(t, db)
	_, err := store.Page(ctx, walletuiport.LedgerPageOptions{
		Filter:   walletuiport.LedgerFilter{TenantID: uuid.Nil},
		PageSize: 10,
	})
	if err != walletuiport.ErrTenantNotFound {
		t.Errorf("Page uuid.Nil = %v, want ErrTenantNotFound", err)
	}
}

// TestWalletUI_Page_BalanceAfterRollback verifies the running balance
// projection: starting from the wallet's current balance, each older
// row's BalanceAfter is the prior BalanceAfter minus the prior row's
// amount (newer rows DESC).
func TestWalletUI_Page_BalanceAfterRollback(t *testing.T) {
	db := freshDBWithPhase4(t)
	ctx := newWalletUICtx(t)
	store := newWalletUIStore(t, db)

	tenantID := seedWalletUITenant(t, ctx, db, "bal-roll")
	walletID := seedWalletUIWallet(t, ctx, db, tenantID, 1_000, 0)

	base := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	// Three commits, balance rolls back: 1000 → 1100 (rollback -100) → 1300 (rollback -200) → 1600 (rollback -300).
	seedWalletUILedger(t, ctx, db, tenantID, walletID, wallet.KindCommit, -300, wallet.SourceConsumption,
		base, `{}`, "")
	seedWalletUILedger(t, ctx, db, tenantID, walletID, wallet.KindCommit, -200, wallet.SourceConsumption,
		base.Add(1*time.Hour), `{}`, "")
	seedWalletUILedger(t, ctx, db, tenantID, walletID, wallet.KindCommit, -100, wallet.SourceConsumption,
		base.Add(2*time.Hour), `{}`, "")

	page, err := store.Page(ctx, walletuiport.LedgerPageOptions{
		Filter:   walletuiport.LedgerFilter{TenantID: tenantID},
		PageSize: 10,
	})
	if err != nil {
		t.Fatalf("page: %v", err)
	}
	if len(page.Entries) != 3 {
		t.Fatalf("entries = %d, want 3", len(page.Entries))
	}
	wantBalances := []int64{1_000, 1_100, 1_300}
	for i, e := range page.Entries {
		if e.BalanceAfter != wantBalances[i] {
			t.Errorf("entries[%d].BalanceAfter = %d, want %d", i, e.BalanceAfter, wantBalances[i])
		}
	}
}

// TestWalletUI_Page_CrossTenantIsolation verifies tenant A's ledger
// cannot be read via the WithTenant-scoped reader bound to tenant B
// (exercises RLS). Both tenants have rows in the same DB.
func TestWalletUI_Page_CrossTenantIsolation(t *testing.T) {
	db := freshDBWithPhase4(t)
	ctx := newWalletUICtx(t)
	store := newWalletUIStore(t, db)

	tenantA := seedWalletUITenant(t, ctx, db, "a")
	walletA := seedWalletUIWallet(t, ctx, db, tenantA, 5_000, 0)
	seedWalletUILedger(t, ctx, db, tenantA, walletA, wallet.KindCommit, -100, wallet.SourceConsumption,
		time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC), `{"conversation_id":"tenant-a-secret"}`, "")

	tenantB := seedWalletUITenant(t, ctx, db, "b")
	seedWalletUIWallet(t, ctx, db, tenantB, 5_000, 0)

	// Asking the adapter for tenant B should return zero ledger rows
	// even though tenant A has them — RLS scopes the SELECT.
	page, err := store.Page(ctx, walletuiport.LedgerPageOptions{
		Filter:   walletuiport.LedgerFilter{TenantID: tenantB},
		PageSize: 50,
	})
	if err != nil {
		t.Fatalf("page B: %v", err)
	}
	if len(page.Entries) != 0 {
		t.Errorf("RLS leak: tenant B saw %d rows from tenant A", len(page.Entries))
	}
}

// ---------------------------------------------------------------------------
// StreamCSV
// ---------------------------------------------------------------------------

// TestWalletUI_StreamCSV_HeaderAndOrdering verifies the canonical
// header row, DESC ordering, and LGPD redaction in the CSV output.
func TestWalletUI_StreamCSV_HeaderAndOrdering(t *testing.T) {
	db := freshDBWithPhase4(t)
	ctx := newWalletUICtx(t)
	store := newWalletUIStore(t, db)

	tenantID := seedWalletUITenant(t, ctx, db, "csv")
	walletID := seedWalletUIWallet(t, ctx, db, tenantID, 5_000, 0)

	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	seedWalletUILedger(t, ctx, db, tenantID, walletID, wallet.KindCommit, -100, wallet.SourceConsumption,
		now.Add(-2*time.Hour), `{"conversation_id":"older-conv","model":"haiku"}`, "wamid-older")
	seedWalletUILedger(t, ctx, db, tenantID, walletID, wallet.KindCommit, -200, wallet.SourceConsumption,
		now, `{"conversation_id":"newer-conv","model":"gemini-flash"}`, "wamid-newer")

	var buf bytes.Buffer
	if err := store.StreamCSV(ctx, walletuiport.LedgerFilter{TenantID: tenantID}, &buf); err != nil {
		t.Fatalf("StreamCSV: %v", err)
	}

	reader := csv.NewReader(&buf)
	rows, err := reader.ReadAll()
	if err != nil {
		t.Fatalf("parse csv: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("csv rows = %d, want 3 (header + 2 entries)", len(rows))
	}
	wantHeader := []string{"id", "occurred_at", "kind", "source", "amount", "conversation_id_hash", "model", "policy_id", "external_ref"}
	for i, col := range wantHeader {
		if rows[0][i] != col {
			t.Errorf("header[%d] = %q, want %q", i, rows[0][i], col)
		}
	}
	// First data row is the newer commit.
	if rows[1][2] != "commit" {
		t.Errorf("row 1 kind = %q, want commit", rows[1][2])
	}
	if rows[1][6] != "gemini-flash" {
		t.Errorf("row 1 model = %q, want gemini-flash", rows[1][6])
	}
	if rows[1][5] != hashConvID("newer-conv") {
		t.Errorf("row 1 conv hash = %q, want %q", rows[1][5], hashConvID("newer-conv"))
	}
	// LGPD: no raw conversation_id text anywhere.
	whole := buf.String()
	if strings.Contains(whole, "newer-conv") || strings.Contains(whole, "older-conv") {
		t.Errorf("CSV leaked raw conversation_id values")
	}
}

// TestWalletUI_StreamCSV_RejectsNilTenant guards the boundary.
func TestWalletUI_StreamCSV_RejectsNilTenant(t *testing.T) {
	db := freshDBWithPhase4(t)
	ctx := newWalletUICtx(t)
	store := newWalletUIStore(t, db)
	var buf bytes.Buffer
	err := store.StreamCSV(ctx, walletuiport.LedgerFilter{TenantID: uuid.Nil}, &buf)
	if err != walletuiport.ErrTenantNotFound {
		t.Errorf("StreamCSV uuid.Nil = %v, want ErrTenantNotFound", err)
	}
}

// TestWalletUI_StreamCSV_EmptyResultStillHasHeader exercises the
// header-on-empty-set path (the gerente exports a window with no
// rows).
func TestWalletUI_StreamCSV_EmptyResultStillHasHeader(t *testing.T) {
	db := freshDBWithPhase4(t)
	ctx := newWalletUICtx(t)
	store := newWalletUIStore(t, db)

	tenantID := seedWalletUITenant(t, ctx, db, "csv-empty")

	var buf bytes.Buffer
	if err := store.StreamCSV(ctx, walletuiport.LedgerFilter{TenantID: tenantID}, &buf); err != nil {
		t.Fatalf("StreamCSV: %v", err)
	}
	rows, err := csv.NewReader(&buf).ReadAll()
	if err != nil {
		t.Fatalf("parse csv: %v", err)
	}
	if len(rows) != 1 {
		t.Errorf("rows = %d, want 1 (header only)", len(rows))
	}
}

// ---------------------------------------------------------------------------
// TopupCatalogReader
// ---------------------------------------------------------------------------

// TestWalletUI_ListPackages_OrdersByPriceAsc verifies the canonical
// catalogue is returned ordered by price ASC with PricePerKToken
// computed correctly.
func TestWalletUI_ListPackages_OrdersByPriceAsc(t *testing.T) {
	db := freshDBWithPhase4(t)
	ctx := newWalletUICtx(t)
	store := newWalletUIStore(t, db)

	seedWalletUIPackages(t, ctx, db)

	pkgs, err := store.ListPackages(ctx)
	if err != nil {
		t.Fatalf("ListPackages: %v", err)
	}
	if len(pkgs) != 3 {
		t.Fatalf("packages = %d, want 3", len(pkgs))
	}
	// Slug order under price ASC: small (1500) < medium (4900) < large (14900).
	wantSlugs := []string{"small", "medium", "large"}
	for i, want := range wantSlugs {
		if pkgs[i].Slug != want {
			t.Errorf("pkgs[%d].Slug = %q, want %q", i, pkgs[i].Slug, want)
		}
	}
	// PricePerKToken: 1500*1000 / 1_000_000 = 1.5 → round-half-up → 2.
	if pkgs[0].PricePerKToken != 2 {
		t.Errorf("small PricePerKToken = %d, want 2", pkgs[0].PricePerKToken)
	}
	// 4900*1000 / 5_000_000 = 0.98 → round-half-up → 1.
	if pkgs[1].PricePerKToken != 1 {
		t.Errorf("medium PricePerKToken = %d, want 1", pkgs[1].PricePerKToken)
	}
	// 14900*1000 / 20_000_000 = 0.745 → round-half-up → 1.
	if pkgs[2].PricePerKToken != 1 {
		t.Errorf("large PricePerKToken = %d, want 1", pkgs[2].PricePerKToken)
	}
}

// TestWalletUI_ListPackages_EmptyCatalogueReturnsEmptySlice handles the
// "no rows seeded" state without nil-panic.
func TestWalletUI_ListPackages_EmptyCatalogueReturnsEmptySlice(t *testing.T) {
	db := freshDBWithPhase4(t)
	ctx := newWalletUICtx(t)
	store := newWalletUIStore(t, db)

	pkgs, err := store.ListPackages(ctx)
	if err != nil {
		t.Fatalf("ListPackages: %v", err)
	}
	if pkgs == nil {
		t.Errorf("packages = nil, want empty slice")
	}
	if len(pkgs) != 0 {
		t.Errorf("packages = %d, want 0", len(pkgs))
	}
}

// ---------------------------------------------------------------------------
// Constructor + nil-pool guard
// ---------------------------------------------------------------------------

func TestWalletUI_New_RejectsNilPool(t *testing.T) {
	_, err := walletui.New(nil)
	if err != postgresadapter.ErrNilPool {
		t.Errorf("New(nil) = %v, want ErrNilPool", err)
	}
}
