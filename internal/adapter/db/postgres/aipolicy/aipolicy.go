// Package aipolicy is the pgx-backed adapter for the
// aipolicy.Repository port (migration 0098: ai_policy).
//
// The package lives under internal/adapter/db/postgres/ so the
// forbidimport / notenant analyzers allow it to import pgx and call
// pgxpool methods through the sibling postgres.WithTenant helper.
// Every read and write routes through WithTenant so the RLS GUC
// app.tenant_id is set before the query runs.
//
// SIN-62351 (Fase 3 W2A, child of SIN-62196).
package aipolicy

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pericles-luz/crm/internal/adapter/db/postgres"
	domain "github.com/pericles-luz/crm/internal/aipolicy"
)

// Store satisfies aipolicy.Repository. The compile-time assertion
// fails the build before any caller notices a port change.
var _ domain.Repository = (*Store)(nil)

// Store is the pgx-backed adapter for ai_policy. Construct via New;
// the pool MUST be the app_runtime pool so RLS policies on ai_policy
// apply.
type Store struct {
	pool postgres.TxBeginner
}

// New wraps pool and returns a ready Store. A nil pool yields
// postgres.ErrNilPool so cmd/server fails fast on misconfiguration.
func New(pool *pgxpool.Pool) (*Store, error) {
	if pool == nil {
		return nil, postgres.ErrNilPool
	}
	return &Store{pool: pool}, nil
}

// Get returns the ai_policy row for (tenantID, scopeType, scopeID).
// The (tenant_id, scope_type, scope_id) UNIQUE index satisfies the
// lookup in one index probe. A missing row collapses to (zero, false,
// nil); transport/driver failures bubble up wrapped.
//
// RLS hides rows belonging to other tenants so the same scope_id /
// scope_type pair can coexist across tenants without leaking.
func (s *Store) Get(ctx context.Context, tenantID uuid.UUID, scopeType domain.ScopeType, scopeID string) (domain.Policy, bool, error) {
	var zero domain.Policy
	if tenantID == uuid.Nil {
		return zero, false, fmt.Errorf("aipolicy/postgres: Get: %w", domain.ErrInvalidTenant)
	}
	if !scopeType.IsValid() {
		return zero, false, fmt.Errorf("aipolicy/postgres: Get: %w", domain.ErrInvalidScopeType)
	}
	if strings.TrimSpace(scopeID) == "" {
		return zero, false, fmt.Errorf("aipolicy/postgres: Get: %w", domain.ErrInvalidScopeID)
	}

	var policy domain.Policy
	found := false
	err := postgres.WithTenant(ctx, s.pool, tenantID, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
			SELECT tenant_id, scope_type, scope_id,
			       model, prompt_version, tone, language,
			       ai_enabled, anonymize, opt_in,
			       created_at, updated_at
			  FROM ai_policy
			 WHERE scope_type = $1
			   AND scope_id   = $2
		`, string(scopeType), scopeID)
		var scopeStr string
		if err := row.Scan(
			&policy.TenantID,
			&scopeStr,
			&policy.ScopeID,
			&policy.Model,
			&policy.PromptVersion,
			&policy.Tone,
			&policy.Language,
			&policy.AIEnabled,
			&policy.Anonymize,
			&policy.OptIn,
			&policy.CreatedAt,
			&policy.UpdatedAt,
		); err != nil {
			return err
		}
		policy.ScopeType = domain.ScopeType(scopeStr)
		found = true
		return nil
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return zero, false, nil
	}
	if err != nil {
		return zero, false, fmt.Errorf("aipolicy/postgres: Get: %w", err)
	}
	return policy, found, nil
}

// Upsert inserts a brand-new policy row or updates the existing one
// keyed by (tenant_id, scope_type, scope_id). The adapter never
// invents created_at / updated_at; the column DEFAULTs handle the
// first write and the ON CONFLICT branch refreshes updated_at to
// now() so admin tooling can sort by recency without trusting
// caller-supplied timestamps.
func (s *Store) Upsert(ctx context.Context, p domain.Policy) error {
	if p.TenantID == uuid.Nil {
		return fmt.Errorf("aipolicy/postgres: Upsert: %w", domain.ErrInvalidTenant)
	}
	if !p.ScopeType.IsValid() {
		return fmt.Errorf("aipolicy/postgres: Upsert: %w", domain.ErrInvalidScopeType)
	}
	if strings.TrimSpace(p.ScopeID) == "" {
		return fmt.Errorf("aipolicy/postgres: Upsert: %w", domain.ErrInvalidScopeID)
	}

	return postgres.WithTenant(ctx, s.pool, p.TenantID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO ai_policy
			  (tenant_id, scope_type, scope_id,
			   model, prompt_version, tone, language,
			   ai_enabled, anonymize, opt_in)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
			ON CONFLICT (tenant_id, scope_type, scope_id)
			DO UPDATE SET
			   model          = EXCLUDED.model,
			   prompt_version = EXCLUDED.prompt_version,
			   tone           = EXCLUDED.tone,
			   language       = EXCLUDED.language,
			   ai_enabled     = EXCLUDED.ai_enabled,
			   anonymize      = EXCLUDED.anonymize,
			   opt_in         = EXCLUDED.opt_in,
			   updated_at     = now()
		`,
			p.TenantID,
			string(p.ScopeType),
			p.ScopeID,
			p.Model,
			p.PromptVersion,
			p.Tone,
			p.Language,
			p.AIEnabled,
			p.Anonymize,
			p.OptIn,
		)
		if err != nil {
			return fmt.Errorf("aipolicy/postgres: Upsert: %w", err)
		}
		return nil
	})
}

// List returns every ai_policy row for tenantID, ordered by
// (scope_type, scope_id) so the admin list page renders deterministically.
// RLS scopes the SELECT to the tenant; the explicit tenant_id predicate is
// belt-and-braces for the case where RLS is bypassed (master tooling).
func (s *Store) List(ctx context.Context, tenantID uuid.UUID) ([]domain.Policy, error) {
	if tenantID == uuid.Nil {
		return nil, fmt.Errorf("aipolicy/postgres: List: %w", domain.ErrInvalidTenant)
	}

	var out []domain.Policy
	err := postgres.WithTenant(ctx, s.pool, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT tenant_id, scope_type, scope_id,
			       model, prompt_version, tone, language,
			       ai_enabled, anonymize, opt_in,
			       created_at, updated_at
			  FROM ai_policy
			 ORDER BY scope_type, scope_id
		`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var p domain.Policy
			var scopeStr string
			if err := rows.Scan(
				&p.TenantID,
				&scopeStr,
				&p.ScopeID,
				&p.Model,
				&p.PromptVersion,
				&p.Tone,
				&p.Language,
				&p.AIEnabled,
				&p.Anonymize,
				&p.OptIn,
				&p.CreatedAt,
				&p.UpdatedAt,
			); err != nil {
				return err
			}
			p.ScopeType = domain.ScopeType(scopeStr)
			out = append(out, p)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("aipolicy/postgres: List: %w", err)
	}
	if out == nil {
		out = []domain.Policy{}
	}
	return out, nil
}

// Delete removes the ai_policy row keyed by (tenantID, scopeType,
// scopeID). The bool reports whether a row was actually removed:
// false + nil error means "no such row" so callers can treat the
// idempotent miss as success without an extra Get round-trip.
func (s *Store) Delete(ctx context.Context, tenantID uuid.UUID, scopeType domain.ScopeType, scopeID string) (bool, error) {
	if tenantID == uuid.Nil {
		return false, fmt.Errorf("aipolicy/postgres: Delete: %w", domain.ErrInvalidTenant)
	}
	if !scopeType.IsValid() {
		return false, fmt.Errorf("aipolicy/postgres: Delete: %w", domain.ErrInvalidScopeType)
	}
	if strings.TrimSpace(scopeID) == "" {
		return false, fmt.Errorf("aipolicy/postgres: Delete: %w", domain.ErrInvalidScopeID)
	}

	var removed bool
	err := postgres.WithTenant(ctx, s.pool, tenantID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			DELETE FROM ai_policy
			 WHERE scope_type = $1
			   AND scope_id   = $2
		`, string(scopeType), scopeID)
		if err != nil {
			return err
		}
		removed = tag.RowsAffected() > 0
		return nil
	})
	if err != nil {
		return false, fmt.Errorf("aipolicy/postgres: Delete: %w", err)
	}
	return removed, nil
}
