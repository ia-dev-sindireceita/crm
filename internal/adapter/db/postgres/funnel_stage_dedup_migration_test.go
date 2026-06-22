package postgres_test

// SIN-65577 / SIN-65584: 0125_funnel_stage_dedup_and_unique.
//
// Reproduces the staging defect — a funnel_stage table missing the
// UNIQUE (tenant_id, key) constraint that accumulated a full duplicate set
// of stages per tenant — and proves the forward migration:
//   * repoints funnel_transition rows off the loser stages onto the
//     deterministically-selected survivor (lowest position, then lowest id),
//   * deletes the loser rows (the ON DELETE RESTRICT FK no longer blocks
//     because step 1 cleared the references),
//   * (re)adds the UNIQUE (tenant_id, key) constraint idempotently,
//   * is tenant-agnostic (fixes every affected tenant), and
//   * is a clean no-op on a database that already has the constraint and no
//     duplicates.
//
// Lives in the parent postgres_test package per ADR 0087 / the
// reference_testpg_shared_cluster_race note so it shares the TestMain
// harness with the other postgres adapter tests.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// applyDedupMigration reads and executes 0125 up against the admin pool.
func applyDedupMigration(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(harness.MigrationsDir(),
		"0125_funnel_stage_dedup_and_unique.up.sql"))
	if err != nil {
		t.Fatalf("read 0125 up: %v", err)
	}
	if _, err := pool.Exec(ctx, string(body)); err != nil {
		t.Fatalf("apply 0125 up: %v", err)
	}
}

// dropFunnelStageUnique removes the UNIQUE (tenant_id, key) constraint to
// recreate the broken staging schema where duplicates were possible.
func dropFunnelStageUnique(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	if _, err := pool.Exec(ctx,
		`ALTER TABLE funnel_stage DROP CONSTRAINT funnel_stage_tenant_key_uniq`,
	); err != nil {
		t.Fatalf("drop unique constraint: %v", err)
	}
}

// duplicateFunnelStages force-inserts a second copy of every funnel_stage row
// for the tenant with fresh ids but identical (key, label, position). Only
// possible while the UNIQUE constraint is absent.
func duplicateFunnelStages(t *testing.T, ctx context.Context, pool *pgxpool.Pool, tenantID uuid.UUID) {
	t.Helper()
	if _, err := pool.Exec(ctx,
		`INSERT INTO funnel_stage (id, tenant_id, key, label, position, is_default)
		 SELECT gen_random_uuid(), tenant_id, key, label, position, is_default
		   FROM funnel_stage
		  WHERE tenant_id = $1`,
		tenantID,
	); err != nil {
		t.Fatalf("duplicate funnel_stage rows: %v", err)
	}
}

// stageIDsForKey returns the funnel_stage ids for a (tenant, key) pair,
// sorted ascending — survivor first under the migration's tie-break.
func stageIDsForKey(t *testing.T, ctx context.Context, pool *pgxpool.Pool, tenantID uuid.UUID, key string) []uuid.UUID {
	t.Helper()
	rows, err := pool.Query(ctx,
		`SELECT id FROM funnel_stage WHERE tenant_id = $1 AND key = $2`,
		tenantID, key)
	if err != nil {
		t.Fatalf("query stage ids: %v", err)
	}
	defer rows.Close()
	var ids []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan id: %v", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows err: %v", err)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i].String() < ids[j].String() })
	return ids
}

func funnelStageCount(t *testing.T, ctx context.Context, pool *pgxpool.Pool, tenantID uuid.UUID) (total, distinctKeys int) {
	t.Helper()
	if err := pool.QueryRow(ctx,
		`SELECT count(*), count(DISTINCT key) FROM funnel_stage WHERE tenant_id = $1`,
		tenantID).Scan(&total, &distinctKeys); err != nil {
		t.Fatalf("count stages: %v", err)
	}
	return total, distinctKeys
}

func uniqueConstraintPresent(t *testing.T, ctx context.Context, pool *pgxpool.Pool) bool {
	t.Helper()
	var present bool
	if err := pool.QueryRow(ctx,
		`SELECT EXISTS (
		   SELECT 1 FROM pg_constraint
		    WHERE conname = 'funnel_stage_tenant_key_uniq'
		      AND conrelid = 'funnel_stage'::regclass
		 )`).Scan(&present); err != nil {
		t.Fatalf("constraint probe: %v", err)
	}
	return present
}

// TestFunnelStageDedup_RepointsAndDeletesAndReadsConstraint is the core
// acceptance test: two tenants each carry a duplicate stage set plus
// transitions pointing at loser stages; after 0125 each tenant has exactly
// five distinct stages, the transitions point at the survivors, and the
// UNIQUE constraint is restored.
func TestFunnelStageDedup_RepointsAndDeletesAndRestoresConstraint(t *testing.T) {
	db, ctx := freshDBWithFunnelF2(t)
	pool := db.AdminPool()

	// Seed two tenants while the constraint still exists (the seed function's
	// ON CONFLICT (tenant_id, key) needs it), then break the schema exactly
	// like staging and duplicate each tenant's stage set.
	tenantA := seedFunnelTenant(t, pool)
	tenantB := seedFunnelTenant(t, pool)
	dropFunnelStageUnique(t, ctx, pool)
	duplicateFunnelStages(t, ctx, pool, tenantA)
	duplicateFunnelStages(t, ctx, pool, tenantB)

	// Sanity: 10 rows / 5 keys per tenant before the fix.
	if total, keys := funnelStageCount(t, ctx, pool, tenantA); total != 10 || keys != 5 {
		t.Fatalf("pre-fix tenantA: got %d rows / %d keys, want 10/5", total, keys)
	}
	if total, keys := funnelStageCount(t, ctx, pool, tenantB); total != 10 || keys != 5 {
		t.Fatalf("pre-fix tenantB: got %d rows / %d keys, want 10/5", total, keys)
	}

	// Build a transition for tenantA that points at LOSER stages so we can
	// prove the repoint. Survivor = lowest id for a fixed position; loser =
	// the other id.
	user := seedFunnelUser(t, pool, tenantA)
	conv := seedFunnelContactAndConversation(t, pool, tenantA)

	novoIDs := stageIDsForKey(t, ctx, pool, tenantA, "novo")         // [survivor, loser]
	propostaIDs := stageIDsForKey(t, ctx, pool, tenantA, "proposta") // [survivor, loser]
	if len(novoIDs) != 2 || len(propostaIDs) != 2 {
		t.Fatalf("expected 2 ids per key, got novo=%d proposta=%d", len(novoIDs), len(propostaIDs))
	}
	novoSurvivor, novoLoser := novoIDs[0], novoIDs[1]
	propostaSurvivor, propostaLoser := propostaIDs[0], propostaIDs[1]

	transitionID := uuid.New()
	if _, err := pool.Exec(ctx,
		`INSERT INTO funnel_transition
		   (id, tenant_id, conversation_id, from_stage_id, to_stage_id, transitioned_by_user_id)
		 VALUES ($1, $2, $3, $4, $5, $6)`,
		transitionID, tenantA, conv, novoLoser, propostaLoser, user,
	); err != nil {
		t.Fatalf("insert transition on losers: %v", err)
	}

	// Apply the fix.
	applyDedupMigration(t, ctx, pool)

	// Each tenant now has exactly five distinct stages.
	for name, id := range map[string]uuid.UUID{"tenantA": tenantA, "tenantB": tenantB} {
		total, keys := funnelStageCount(t, ctx, pool, id)
		if total != 5 || keys != 5 {
			t.Errorf("post-fix %s: got %d rows / %d keys, want 5/5", name, total, keys)
		}
	}

	// The constraint is back.
	if !uniqueConstraintPresent(t, ctx, pool) {
		t.Errorf("UNIQUE (tenant_id, key) constraint not present after 0125")
	}

	// The survivor rows are the ones that remain.
	if got := stageIDsForKey(t, ctx, pool, tenantA, "novo"); len(got) != 1 || got[0] != novoSurvivor {
		t.Errorf("novo survivor: got %v, want [%s]", got, novoSurvivor)
	}
	if got := stageIDsForKey(t, ctx, pool, tenantA, "proposta"); len(got) != 1 || got[0] != propostaSurvivor {
		t.Errorf("proposta survivor: got %v, want [%s]", got, propostaSurvivor)
	}

	// The transition formerly pointing at losers now points at survivors.
	var gotFrom, gotTo uuid.UUID
	if err := pool.QueryRow(ctx,
		`SELECT from_stage_id, to_stage_id FROM funnel_transition WHERE id = $1`,
		transitionID).Scan(&gotFrom, &gotTo); err != nil {
		t.Fatalf("read transition: %v", err)
	}
	if gotFrom != novoSurvivor {
		t.Errorf("transition from_stage_id = %s, want survivor %s", gotFrom, novoSurvivor)
	}
	if gotTo != propostaSurvivor {
		t.Errorf("transition to_stage_id = %s, want survivor %s", gotTo, propostaSurvivor)
	}
}

// TestFunnelStageDedup_IdempotentOnCleanDB proves 0125 is a no-op on a
// database that already has the constraint and no duplicates, and that
// re-running it after a fix is also a no-op (idempotent).
func TestFunnelStageDedup_IdempotentOnCleanDB(t *testing.T) {
	db, ctx := freshDBWithFunnelF2(t)
	pool := db.AdminPool()

	// freshDBWithFunnelF2 already has the constraint (0093). A clean tenant.
	tenant := seedFunnelTenant(t, pool)
	if total, keys := funnelStageCount(t, ctx, pool, tenant); total != 5 || keys != 5 {
		t.Fatalf("clean tenant pre-0125: got %d/%d, want 5/5", total, keys)
	}

	// Apply 0125 twice — both must succeed and leave the data untouched.
	applyDedupMigration(t, ctx, pool)
	applyDedupMigration(t, ctx, pool)

	if !uniqueConstraintPresent(t, ctx, pool) {
		t.Errorf("constraint missing after idempotent re-run")
	}
	if total, keys := funnelStageCount(t, ctx, pool, tenant); total != 5 || keys != 5 {
		t.Errorf("clean tenant post-0125: got %d/%d, want 5/5", total, keys)
	}

	// And the restored invariant is enforced: a duplicate key insert fails
	// with a unique_violation (SQLSTATE 23505).
	_, err := pool.Exec(ctx,
		`INSERT INTO funnel_stage (tenant_id, key, label, position)
		 VALUES ($1, 'novo', 'Dup', 99)`,
		tenant)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23505" {
		t.Errorf("expected unique_violation (23505) inserting duplicate key, got %v", err)
	}
}
