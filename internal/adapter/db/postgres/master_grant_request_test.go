package postgres_test

// SIN-63605 integration tests for the master/grant_request_repository
// adapter. Lives in the parent postgres_test package (mastersession
// pattern) to avoid the shared-cluster ALTER ROLE race that bites
// adapters whose tests run in their own test binary.
//
// Coverage (matches AC mapping in SIN-63605):
//
//  1. Create awaiting → master_ops_audit row present for the insert.
//  2. Approve cascades to a master_grant insert + audit; request is
//     transitioned to approved with second-approver / decided_at set.
//  3. Reject transitions the request to rejected without writing a
//     master_grant; audit row present for the request update.
//  4. Approve with actor==requester returns ErrGrantRequestApprover
//     IsCreator without mutating state.
//  5. Double-approve / approve-after-rejected returns ErrGrantRequest
//     AlreadyDecided.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"

	postgresadapter "github.com/pericles-luz/crm/internal/adapter/db/postgres"
	masterpg "github.com/pericles-luz/crm/internal/adapter/db/postgres/master"
	"github.com/pericles-luz/crm/internal/adapter/db/postgres/testpg"
	masterweb "github.com/pericles-luz/crm/internal/web/master"
)

// freshDBWithGrantRequests brings up a per-test DB with every
// migration the master_grant_request schema depends on. 0002
// (master_ops_audit) is applied by the harness; the remainder are
// applied in dependency order.
func freshDBWithGrantRequests(t *testing.T) *testpg.DB {
	t.Helper()
	db := harness.DB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	for _, name := range []string{
		"0004_create_tenant.up.sql",
		"0005_create_users.up.sql",
		"0089_wallet_basic.up.sql",
		"0090_wallet_updated_at_trigger.up.sql",
		"0097_subscription_plan_invoice_master_grant.up.sql",
	} {
		path := filepath.Join(harness.MigrationsDir(), name)
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if _, err := db.AdminPool().Exec(ctx, string(body)); err != nil {
			t.Fatalf("apply %s: %v", name, err)
		}
	}
	return db
}

// seedMasterUserForGrantRequests inserts a tenant + N master users
// (all distinct, all is_master=true, no tenant_id per the
// users_master_xor_tenant CHECK) and returns them.
func seedMasterUserForGrantRequests(t *testing.T, ctx context.Context, db *testpg.DB, n int) (uuid.UUID, []uuid.UUID) {
	t.Helper()
	tenantID := uuid.New()
	if _, err := db.AdminPool().Exec(ctx,
		`INSERT INTO tenants (id, name, host) VALUES ($1, $2, $3)`,
		tenantID, "grant-req-test", fmt.Sprintf("grant-req-%s.crm.local", tenantID),
	); err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	out := make([]uuid.UUID, n)
	for i := range out {
		uid := uuid.New()
		if _, err := db.AdminPool().Exec(ctx,
			`INSERT INTO users (id, tenant_id, email, password_hash, is_master)
			 VALUES ($1, NULL, $2, 'x', true)`,
			uid, fmt.Sprintf("master+%s@grant-req.test", uid),
		); err != nil {
			t.Fatalf("seed master user %d: %v", i, err)
		}
		out[i] = uid
	}
	return tenantID, out
}

// countAuditRowsByTable counts master_ops_audit rows for the given
// table/actor pair. Used to assert audit emission per AC #3.
func countAuditRowsByTable(t *testing.T, ctx context.Context, db *testpg.DB, table string, actor uuid.UUID) int {
	t.Helper()
	var n int
	if err := db.AdminPool().QueryRow(ctx,
		`SELECT count(*) FROM master_ops_audit
		   WHERE target_table = $1
		     AND actor_user_id = $2
		     AND query_kind   <> 'session_open'`,
		table, actor,
	).Scan(&n); err != nil {
		t.Fatalf("count audit rows for %s/%s: %v", table, actor, err)
	}
	return n
}

// ----- Constructor argument validation ----------------------------------

func TestGrantRequestStore_New_RejectsBadInputs(t *testing.T) {
	t.Parallel()
	if _, err := masterpg.NewGrantRequestStore(nil, uuid.New()); !errors.Is(err, postgresadapter.ErrNilPool) {
		t.Fatalf("nil pool err = %v, want ErrNilPool", err)
	}
}

// ----- CreateGrantRequest -----------------------------------------------

func TestGrantRequestStore_CreateGrantRequest_WritesAuditRow(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test — skipped in short mode")
	}
	db := freshDBWithGrantRequests(t)
	ctx := context.Background()
	tenantID, users := seedMasterUserForGrantRequests(t, ctx, db, 1)
	requesterID := users[0]

	store, err := masterpg.NewGrantRequestStore(db.MasterOpsPool(), requesterID)
	if err != nil {
		t.Fatalf("NewGrantRequestStore: %v", err)
	}

	req, err := store.CreateGrantRequest(ctx, masterweb.CreateGrantRequestInput{
		ActorUserID: requesterID,
		TenantID:    tenantID,
		Kind:        masterweb.GrantKindExtraTokens,
		Amount:      20_000_000,
		Reason:      "AC1 — over-cap create test",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if req.State != masterweb.GrantRequestStateAwaiting {
		t.Errorf("state = %s, want awaiting_approval", req.State)
	}
	if req.ExternalID == "" {
		t.Errorf("ExternalID empty; want ULID")
	}

	// AC #3 — audit row for the INSERT.
	if got := countAuditRowsByTable(t, ctx, db, "master_grant_request", requesterID); got < 1 {
		t.Errorf("audit rows for INSERT = %d, want ≥ 1", got)
	}

	// Cross-check the row landed.
	got, err := store.GetGrantRequest(ctx, req.ID)
	if err != nil {
		t.Fatalf("GetGrantRequest after create: %v", err)
	}
	if got.Amount != 20_000_000 {
		t.Errorf("Amount = %d, want 20_000_000", got.Amount)
	}
	if got.CreatedByID != requesterID {
		t.Errorf("CreatedByID = %s, want %s", got.CreatedByID, requesterID)
	}
}

// ----- ApproveGrantRequest happy path -----------------------------------

func TestGrantRequestStore_ApproveGrantRequest_PromotesToMasterGrant(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test — skipped in short mode")
	}
	db := freshDBWithGrantRequests(t)
	ctx := context.Background()
	tenantID, users := seedMasterUserForGrantRequests(t, ctx, db, 2)
	requesterID, approverID := users[0], users[1]

	store, err := masterpg.NewGrantRequestStore(db.MasterOpsPool(), approverID)
	if err != nil {
		t.Fatalf("NewGrantRequestStore: %v", err)
	}

	req, err := store.CreateGrantRequest(ctx, masterweb.CreateGrantRequestInput{
		ActorUserID: requesterID,
		TenantID:    tenantID,
		Kind:        masterweb.GrantKindFreeSubscriptionPeriod,
		PeriodDays:  365,
		Reason:      "AC2 — approve cascades to master_grant",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	grant, err := store.ApproveGrantRequest(ctx, masterweb.DecideGrantRequestInput{
		ActorUserID: approverID,
		RequestID:   req.ID,
	})
	if err != nil {
		t.Fatalf("Approve: %v", err)
	}
	if grant.TenantID != tenantID {
		t.Errorf("master_grant.TenantID = %s, want %s", grant.TenantID, tenantID)
	}
	if grant.PeriodDays != 365 {
		t.Errorf("master_grant.PeriodDays = %d, want 365", grant.PeriodDays)
	}
	if grant.CreatedByID != requesterID {
		t.Errorf("master_grant.CreatedByID = %s, want %s (original requester)", grant.CreatedByID, requesterID)
	}

	// Re-read request → approved + second-approver / decided_at set.
	updated, err := store.GetGrantRequest(ctx, req.ID)
	if err != nil {
		t.Fatalf("GetGrantRequest after approve: %v", err)
	}
	if updated.State != masterweb.GrantRequestStateApproved {
		t.Errorf("state = %s, want approved", updated.State)
	}
	if updated.SecondApproverID != approverID {
		t.Errorf("SecondApproverID = %s, want %s", updated.SecondApproverID, approverID)
	}
	if updated.DecidedAt.IsZero() {
		t.Errorf("DecidedAt should be populated")
	}

	// AC #3 — audit row count: at minimum 1 insert (request) + 1 update
	// (request → approved) + 1 insert (master_grant) → ≥ 3 rows total
	// across both tables. Split per-table for diagnostics.
	if got := countAuditRowsByTable(t, ctx, db, "master_grant_request", approverID); got < 1 {
		t.Errorf("audit rows on master_grant_request by approver = %d, want ≥ 1 (UPDATE)", got)
	}
	if got := countAuditRowsByTable(t, ctx, db, "master_grant", approverID); got < 1 {
		t.Errorf("audit rows on master_grant by approver = %d, want ≥ 1 (INSERT)", got)
	}
	if got := countAuditRowsByTable(t, ctx, db, "master_grant_request", requesterID); got < 1 {
		t.Errorf("audit rows on master_grant_request by requester = %d, want ≥ 1 (INSERT)", got)
	}

	// The master_grant row is reachable via the wallet adapter SELECT
	// path; here we just confirm the row count under master_ops to
	// avoid pulling the wallet package into this test.
	var masterGrantCount int
	if err := db.AdminPool().QueryRow(ctx,
		`SELECT count(*) FROM master_grant WHERE tenant_id = $1`, tenantID,
	).Scan(&masterGrantCount); err != nil {
		t.Fatalf("count master_grant: %v", err)
	}
	if masterGrantCount != 1 {
		t.Errorf("master_grant rows for tenant = %d, want 1", masterGrantCount)
	}
}

// ----- RejectGrantRequest -----------------------------------------------

func TestGrantRequestStore_RejectGrantRequest_DoesNotEmitMasterGrant(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test — skipped in short mode")
	}
	db := freshDBWithGrantRequests(t)
	ctx := context.Background()
	tenantID, users := seedMasterUserForGrantRequests(t, ctx, db, 2)
	requesterID, rejecterID := users[0], users[1]

	store, _ := masterpg.NewGrantRequestStore(db.MasterOpsPool(), rejecterID)

	req, err := store.CreateGrantRequest(ctx, masterweb.CreateGrantRequestInput{
		ActorUserID: requesterID,
		TenantID:    tenantID,
		Kind:        masterweb.GrantKindExtraTokens,
		Amount:      99_000_000,
		Reason:      "AC3 — reject path test reason",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if err := store.RejectGrantRequest(ctx, masterweb.DecideGrantRequestInput{
		ActorUserID: rejecterID,
		RequestID:   req.ID,
	}); err != nil {
		t.Fatalf("Reject: %v", err)
	}

	updated, err := store.GetGrantRequest(ctx, req.ID)
	if err != nil {
		t.Fatalf("GetGrantRequest after reject: %v", err)
	}
	if updated.State != masterweb.GrantRequestStateRejected {
		t.Errorf("state = %s, want rejected", updated.State)
	}
	if updated.SecondApproverID != rejecterID {
		t.Errorf("SecondApproverID = %s, want %s (rejecter)", updated.SecondApproverID, rejecterID)
	}

	// No master_grant row should have been written.
	var masterGrantCount int
	if err := db.AdminPool().QueryRow(ctx,
		`SELECT count(*) FROM master_grant WHERE tenant_id = $1`, tenantID,
	).Scan(&masterGrantCount); err != nil {
		t.Fatalf("count master_grant: %v", err)
	}
	if masterGrantCount != 0 {
		t.Errorf("reject should NOT create master_grant; got %d rows", masterGrantCount)
	}

	// AC #3 — audit row for the UPDATE on the request.
	if got := countAuditRowsByTable(t, ctx, db, "master_grant_request", rejecterID); got < 1 {
		t.Errorf("audit rows by rejecter on master_grant_request = %d, want ≥ 1", got)
	}
}

// ----- Approver == requester (AC #2) ------------------------------------

func TestGrantRequestStore_ApproveGrantRequest_ActorIsCreator(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test — skipped in short mode")
	}
	db := freshDBWithGrantRequests(t)
	ctx := context.Background()
	tenantID, users := seedMasterUserForGrantRequests(t, ctx, db, 1)
	requesterID := users[0]

	store, _ := masterpg.NewGrantRequestStore(db.MasterOpsPool(), requesterID)

	req, err := store.CreateGrantRequest(ctx, masterweb.CreateGrantRequestInput{
		ActorUserID: requesterID,
		TenantID:    tenantID,
		Kind:        masterweb.GrantKindExtraTokens,
		Amount:      20_000_000,
		Reason:      "AC2 — self-approval guard test",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	_, err = store.ApproveGrantRequest(ctx, masterweb.DecideGrantRequestInput{
		ActorUserID: requesterID,
		RequestID:   req.ID,
	})
	if !errors.Is(err, masterweb.ErrGrantRequestApproverIsCreator) {
		t.Fatalf("Approve(self) err = %v, want ErrGrantRequestApproverIsCreator", err)
	}

	// State must not have moved.
	got, err := store.GetGrantRequest(ctx, req.ID)
	if err != nil {
		t.Fatalf("GetGrantRequest: %v", err)
	}
	if got.State != masterweb.GrantRequestStateAwaiting {
		t.Errorf("state = %s, want awaiting_approval (must not transition on 422)", got.State)
	}
	// And no master_grant must have been written.
	var masterGrantCount int
	_ = db.AdminPool().QueryRow(ctx,
		`SELECT count(*) FROM master_grant WHERE tenant_id = $1`, tenantID,
	).Scan(&masterGrantCount)
	if masterGrantCount != 0 {
		t.Errorf("master_grant must not exist after blocked self-approve; got %d", masterGrantCount)
	}
}

func TestGrantRequestStore_RejectGrantRequest_ActorIsCreator(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test — skipped in short mode")
	}
	db := freshDBWithGrantRequests(t)
	ctx := context.Background()
	tenantID, users := seedMasterUserForGrantRequests(t, ctx, db, 1)
	requesterID := users[0]

	store, _ := masterpg.NewGrantRequestStore(db.MasterOpsPool(), requesterID)

	req, _ := store.CreateGrantRequest(ctx, masterweb.CreateGrantRequestInput{
		ActorUserID: requesterID,
		TenantID:    tenantID,
		Kind:        masterweb.GrantKindExtraTokens,
		Amount:      11_000_000,
		Reason:      "AC2 — self-reject guard test",
	})

	err := store.RejectGrantRequest(ctx, masterweb.DecideGrantRequestInput{
		ActorUserID: requesterID,
		RequestID:   req.ID,
	})
	if !errors.Is(err, masterweb.ErrGrantRequestApproverIsCreator) {
		t.Fatalf("Reject(self) err = %v, want ErrGrantRequestApproverIsCreator", err)
	}
}

// ----- Double-decide races (AC #5) --------------------------------------

func TestGrantRequestStore_ApproveGrantRequest_DoubleApprove(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test — skipped in short mode")
	}
	db := freshDBWithGrantRequests(t)
	ctx := context.Background()
	tenantID, users := seedMasterUserForGrantRequests(t, ctx, db, 3)
	requesterID, approverA, approverB := users[0], users[1], users[2]

	store, _ := masterpg.NewGrantRequestStore(db.MasterOpsPool(), approverA)

	req, _ := store.CreateGrantRequest(ctx, masterweb.CreateGrantRequestInput{
		ActorUserID: requesterID,
		TenantID:    tenantID,
		Kind:        masterweb.GrantKindExtraTokens,
		Amount:      42_000_000,
		Reason:      "AC5 — double-approve race test",
	})

	if _, err := store.ApproveGrantRequest(ctx, masterweb.DecideGrantRequestInput{
		ActorUserID: approverA,
		RequestID:   req.ID,
	}); err != nil {
		t.Fatalf("first Approve: %v", err)
	}

	_, err := store.ApproveGrantRequest(ctx, masterweb.DecideGrantRequestInput{
		ActorUserID: approverB,
		RequestID:   req.ID,
	})
	if !errors.Is(err, masterweb.ErrGrantRequestAlreadyDecided) {
		t.Fatalf("second Approve err = %v, want ErrGrantRequestAlreadyDecided", err)
	}

	// Only ONE master_grant row should exist.
	var masterGrantCount int
	_ = db.AdminPool().QueryRow(ctx,
		`SELECT count(*) FROM master_grant WHERE tenant_id = $1`, tenantID,
	).Scan(&masterGrantCount)
	if masterGrantCount != 1 {
		t.Errorf("master_grant rows = %d, want exactly 1 after double-approve", masterGrantCount)
	}
}

func TestGrantRequestStore_RejectThenApprove_AlreadyDecided(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test — skipped in short mode")
	}
	db := freshDBWithGrantRequests(t)
	ctx := context.Background()
	tenantID, users := seedMasterUserForGrantRequests(t, ctx, db, 3)
	requesterID, rejecter, approver := users[0], users[1], users[2]

	store, _ := masterpg.NewGrantRequestStore(db.MasterOpsPool(), approver)

	req, _ := store.CreateGrantRequest(ctx, masterweb.CreateGrantRequestInput{
		ActorUserID: requesterID,
		TenantID:    tenantID,
		Kind:        masterweb.GrantKindExtraTokens,
		Amount:      42_000_000,
		Reason:      "reject-then-approve race test reason",
	})

	if err := store.RejectGrantRequest(ctx, masterweb.DecideGrantRequestInput{
		ActorUserID: rejecter,
		RequestID:   req.ID,
	}); err != nil {
		t.Fatalf("Reject: %v", err)
	}

	_, err := store.ApproveGrantRequest(ctx, masterweb.DecideGrantRequestInput{
		ActorUserID: approver,
		RequestID:   req.ID,
	})
	if !errors.Is(err, masterweb.ErrGrantRequestAlreadyDecided) {
		t.Fatalf("Approve-after-Reject err = %v, want ErrGrantRequestAlreadyDecided", err)
	}
}

// ----- Listing ----------------------------------------------------------

func TestGrantRequestStore_ListAwaitingRequests_ReturnsOnlyAwaiting(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test — skipped in short mode")
	}
	db := freshDBWithGrantRequests(t)
	ctx := context.Background()
	tenantID, users := seedMasterUserForGrantRequests(t, ctx, db, 2)
	requesterID, approverID := users[0], users[1]

	store, _ := masterpg.NewGrantRequestStore(db.MasterOpsPool(), approverID)

	// Three requests: leave the first awaiting, approve the second,
	// reject the third.
	awaiting, _ := store.CreateGrantRequest(ctx, masterweb.CreateGrantRequestInput{
		ActorUserID: requesterID, TenantID: tenantID, Kind: masterweb.GrantKindExtraTokens,
		Amount: 11_000_000, Reason: "still awaiting reason text",
	})
	toApprove, _ := store.CreateGrantRequest(ctx, masterweb.CreateGrantRequestInput{
		ActorUserID: requesterID, TenantID: tenantID, Kind: masterweb.GrantKindExtraTokens,
		Amount: 12_000_000, Reason: "to be approved reason text",
	})
	toReject, _ := store.CreateGrantRequest(ctx, masterweb.CreateGrantRequestInput{
		ActorUserID: requesterID, TenantID: tenantID, Kind: masterweb.GrantKindExtraTokens,
		Amount: 13_000_000, Reason: "to be rejected reason text",
	})
	if _, err := store.ApproveGrantRequest(ctx, masterweb.DecideGrantRequestInput{
		ActorUserID: approverID, RequestID: toApprove.ID,
	}); err != nil {
		t.Fatalf("Approve toApprove: %v", err)
	}
	if err := store.RejectGrantRequest(ctx, masterweb.DecideGrantRequestInput{
		ActorUserID: approverID, RequestID: toReject.ID,
	}); err != nil {
		t.Fatalf("Reject toReject: %v", err)
	}

	list, err := store.ListAwaitingRequests(ctx)
	if err != nil {
		t.Fatalf("ListAwaitingRequests: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("ListAwaitingRequests returned %d, want exactly 1 (only the awaiting row)", len(list))
	}
	if list[0].ID != awaiting.ID {
		t.Errorf("listed id = %s, want awaiting.ID %s", list[0].ID, awaiting.ID)
	}
}

func TestGrantRequestStore_GetGrantRequest_NotFound(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test — skipped in short mode")
	}
	db := freshDBWithGrantRequests(t)
	ctx := context.Background()
	_, users := seedMasterUserForGrantRequests(t, ctx, db, 1)
	actor := users[0]

	store, _ := masterpg.NewGrantRequestStore(db.MasterOpsPool(), actor)
	_, err := store.GetGrantRequest(ctx, uuid.New())
	if !errors.Is(err, masterweb.ErrGrantRequestNotFound) {
		t.Errorf("err = %v, want ErrGrantRequestNotFound", err)
	}
}

// ----- Clock injection (small unit-shaped test against the live DB) ----

func TestGrantRequestStore_WithClock_PinsCreatedAt(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test — skipped in short mode")
	}
	db := freshDBWithGrantRequests(t)
	ctx := context.Background()
	tenantID, users := seedMasterUserForGrantRequests(t, ctx, db, 1)
	requesterID := users[0]

	frozen := time.Date(2026, 5, 27, 9, 0, 0, 0, time.UTC)
	store, err := masterpg.NewGrantRequestStore(db.MasterOpsPool(), requesterID, masterpg.WithClock(func() time.Time { return frozen }))
	if err != nil {
		t.Fatalf("NewGrantRequestStore: %v", err)
	}

	req, err := store.CreateGrantRequest(ctx, masterweb.CreateGrantRequestInput{
		ActorUserID: requesterID,
		TenantID:    tenantID,
		Kind:        masterweb.GrantKindExtraTokens,
		Amount:      11_000_000,
		Reason:      "clock-pin test reason text",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if !req.CreatedAt.Equal(frozen) {
		t.Errorf("CreatedAt = %v, want %v", req.CreatedAt, frozen)
	}
}

func TestGrantRequestStore_ApproveGrantRequest_ZeroActor_ReturnsErrZeroActor(t *testing.T) {
	db := freshDBWithGrantRequests(t)
	actor := uuid.New()
	store, err := masterpg.NewGrantRequestStore(db.MasterOpsPool(), actor)
	if err != nil {
		t.Fatalf("NewGrantRequestStore: %v", err)
	}

	_, err = store.ApproveGrantRequest(context.Background(), masterweb.DecideGrantRequestInput{
		ActorUserID: uuid.Nil, RequestID: uuid.New(), Reason: "zero actor",
	})
	if err == nil {
		t.Error("nil ActorUserID: want error, got nil")
	}
}

func TestGrantRequestStore_RejectGrantRequest_ZeroActor_ReturnsErrZeroActor(t *testing.T) {
	db := freshDBWithGrantRequests(t)
	actor := uuid.New()
	store, err := masterpg.NewGrantRequestStore(db.MasterOpsPool(), actor)
	if err != nil {
		t.Fatalf("NewGrantRequestStore: %v", err)
	}

	err = store.RejectGrantRequest(context.Background(), masterweb.DecideGrantRequestInput{
		ActorUserID: uuid.Nil, RequestID: uuid.New(), Reason: "zero actor",
	})
	if err == nil {
		t.Error("nil ActorUserID: want error, got nil")
	}
}

func TestGrantRequestStore_CreateGrantRequest_ZeroActor_ReturnsError(t *testing.T) {
	db := freshDBWithGrantRequests(t)
	store, err := masterpg.NewGrantRequestStore(db.MasterOpsPool(), uuid.New())
	if err != nil {
		t.Fatalf("NewGrantRequestStore: %v", err)
	}
	_, err = store.CreateGrantRequest(context.Background(), masterweb.CreateGrantRequestInput{
		ActorUserID: uuid.Nil,
		TenantID:    uuid.New(),
		Kind:        masterweb.GrantKindExtraTokens,
		Amount:      1000,
		Reason:      "zero actor test reason at least 10 chars",
	})
	if err == nil {
		t.Error("nil ActorUserID: want error, got nil")
	}
}
