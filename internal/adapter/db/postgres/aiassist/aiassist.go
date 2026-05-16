// Package aiassist (adapter) implements aiassist.SummaryRepository
// against Postgres. Reads and writes both route through the runtime
// pool inside WithTenant so RLS gates tenant visibility. The
// master_ops_audit_trigger on ai_summary is a no-op for the runtime
// role (see migration 0002), which is the intended runtime path —
// audit is only emitted when master_ops compensates.
package aiassist

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	postgresadapter "github.com/pericles-luz/crm/internal/adapter/db/postgres"
	"github.com/pericles-luz/crm/internal/aiassist"
)

// Store implements aiassist.SummaryRepository over a runtime-role
// pgx pool. Construction is via New; the zero value is not usable.
type Store struct {
	runtimePool postgresadapter.TxBeginner
}

// New constructs a Store. A nil pool is rejected so missing-wiring
// surfaces at boot rather than the first call.
func New(runtimePool *pgxpool.Pool) (*Store, error) {
	if runtimePool == nil {
		return nil, postgresadapter.ErrNilPool
	}
	return &Store{runtimePool: runtimePool}, nil
}

// Compile-time port satisfaction. The check stays alongside the
// constructor so a port-shape drift fails the package build, not just
// the call site.
var _ aiassist.SummaryRepository = (*Store)(nil)

// selectLatestValid is the hot-path query the cache lookup uses. The
// partial index ai_summary_conversation_id_idx WHERE invalidated_at IS
// NULL serves the predicate; ORDER BY generated_at DESC LIMIT 1 picks
// the most recent row. The expires_at check is "expires_at IS NULL OR
// expires_at > $3" so the "no TTL" row stays cache-fresh forever.
const selectLatestValid = `
SELECT id, tenant_id, conversation_id, summary_text, model, tokens_in, tokens_out,
       generated_at, expires_at, invalidated_at
  FROM ai_summary
 WHERE tenant_id = $1
   AND conversation_id = $2
   AND invalidated_at IS NULL
   AND (expires_at IS NULL OR expires_at > $3)
 ORDER BY generated_at DESC
 LIMIT 1`

// insertSummary inserts a freshly generated summary row. We insert a
// new row per generation rather than upsert so the audit history of
// past summaries survives — the partial-index predicate keeps the
// "valid summary for this conversation" query cheap regardless of
// how many historical rows accumulate.
const insertSummary = `
INSERT INTO ai_summary
  (id, tenant_id, conversation_id, summary_text, model, tokens_in, tokens_out,
   generated_at, expires_at, invalidated_at)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8, NULLIF($9, '0001-01-01 00:00:00'::timestamptz),
        NULLIF($10, '0001-01-01 00:00:00'::timestamptz))`

// invalidateAllForConversation marks every currently-valid summary on
// the conversation stale. The WHERE filter on invalidated_at IS NULL
// keeps the operation idempotent — a second call against the same
// conversation updates zero rows and returns nil. We set
// invalidated_at to the supplied now rather than now() so the audit
// trail matches the boundary-clock the inbox uses.
const invalidateAllForConversation = `
UPDATE ai_summary
   SET invalidated_at = $3
 WHERE tenant_id = $1
   AND conversation_id = $2
   AND invalidated_at IS NULL`

// GetLatestValid returns the most recent non-invalidated, non-expired
// summary for (tenantID, conversationID), or aiassist.ErrCacheMiss
// when no row qualifies at the supplied wall-clock.
func (s *Store) GetLatestValid(
	ctx context.Context,
	tenantID, conversationID uuid.UUID,
	now time.Time,
) (*aiassist.Summary, error) {
	if tenantID == uuid.Nil {
		return nil, aiassist.ErrZeroTenant
	}
	if conversationID == uuid.Nil {
		return nil, aiassist.ErrZeroConversation
	}
	var out *aiassist.Summary
	err := postgresadapter.WithTenant(ctx, s.runtimePool, tenantID, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, selectLatestValid, tenantID, conversationID, now)
		got, err := scanSummary(row)
		if err != nil {
			return err
		}
		out = got
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// Save persists a fresh summary row. The row's invariants are enforced
// by aiassist.NewSummary upstream; the adapter trusts the validated
// struct and lets the table's CHECK constraints (tokens_in >= 0,
// tokens_out >= 0) backstop a hypothetical bypass.
func (s *Store) Save(ctx context.Context, sm *aiassist.Summary) error {
	if sm == nil {
		return errors.New("aiassist/postgres: nil Summary")
	}
	if sm.TenantID == uuid.Nil {
		return aiassist.ErrZeroTenant
	}
	if sm.ConversationID == uuid.Nil {
		return aiassist.ErrZeroConversation
	}
	return postgresadapter.WithTenant(ctx, s.runtimePool, sm.TenantID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, insertSummary,
			sm.ID, sm.TenantID, sm.ConversationID, sm.Text, sm.Model,
			sm.TokensIn, sm.TokensOut, sm.GeneratedAt,
			sm.ExpiresAt, sm.InvalidatedAt,
		)
		if err != nil {
			return fmt.Errorf("aiassist/postgres: insert summary: %w", err)
		}
		return nil
	})
}

// InvalidateForConversation marks every currently-valid summary for
// (tenantID, conversationID) as invalidated_at = now. Idempotent.
func (s *Store) InvalidateForConversation(
	ctx context.Context,
	tenantID, conversationID uuid.UUID,
	now time.Time,
) error {
	if tenantID == uuid.Nil {
		return aiassist.ErrZeroTenant
	}
	if conversationID == uuid.Nil {
		return aiassist.ErrZeroConversation
	}
	return postgresadapter.WithTenant(ctx, s.runtimePool, tenantID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, invalidateAllForConversation, tenantID, conversationID, now)
		if err != nil {
			return fmt.Errorf("aiassist/postgres: invalidate summaries: %w", err)
		}
		return nil
	})
}

// scanSummary hydrates a Summary from a single-row pgx.Row. Nullable
// columns (expires_at, invalidated_at) are scanned into pgx-friendly
// pointers and copied into the zero-value time.Time slot when NULL.
func scanSummary(row pgx.Row) (*aiassist.Summary, error) {
	var sm aiassist.Summary
	var expires, invalidated *time.Time
	if err := row.Scan(
		&sm.ID,
		&sm.TenantID,
		&sm.ConversationID,
		&sm.Text,
		&sm.Model,
		&sm.TokensIn,
		&sm.TokensOut,
		&sm.GeneratedAt,
		&expires,
		&invalidated,
	); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, aiassist.ErrCacheMiss
		}
		return nil, fmt.Errorf("aiassist/postgres: scan summary: %w", err)
	}
	if expires != nil {
		sm.ExpiresAt = *expires
	}
	if invalidated != nil {
		sm.InvalidatedAt = *invalidated
	}
	return &sm, nil
}
