// Package funnelrules is the pgx-backed adapter for the funnel rules
// [RuleRepository] port (migration 0102 / funnel_rules table).
//
// The package lives under internal/adapter/db/postgres/ so the
// forbidimport / notenant analyzers allow it to import pgx and call
// pgxpool methods directly. Every tenant-scoped call routes through
// postgres.WithTenant so the RLS GUC app.tenant_id is set before
// reading or writing.
//
// SIN-62955 (Fase 4 internal/funnel rules + cascade resolver, child
// of [SIN-62197](/SIN/issues/SIN-62197)).
package funnelrules

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pericles-luz/crm/internal/adapter/db/postgres"
	domain "github.com/pericles-luz/crm/internal/funnel/rules"
)

// Compile-time assertion: Store satisfies the domain port. A drift
// in the port shape fails the build of this file before any caller
// notices.
var _ domain.RuleRepository = (*Store)(nil)

// Store is the pgx-backed adapter for the funnel rules port.
// Construct via New(pool); the pool MUST be the app_runtime pool so
// the RLS policies on funnel_rules apply.
type Store struct {
	pool postgres.TxBeginner
}

// New wraps pool and returns a ready Store. A nil pool yields
// postgres.ErrNilPool so cmd/server fails fast at boot.
func New(pool *pgxpool.Pool) (*Store, error) {
	if pool == nil {
		return nil, postgres.ErrNilPool
	}
	return &Store{pool: pool}, nil
}

// ListEffectiveForChannel implements the port. See the doc on the
// port for the contract; this method just translates it to one
// index-bounded SQL query inside a tenant-scoped transaction.
//
// The WHERE clause unions the three scope buckets:
//
//   - channel-scoped row: funnel_rules.channel = $1 (non-empty exact
//     match — ” is rejected because the table's channel column is
//     never the empty string by construction; the column is either
//     NULL or a real channel identifier);
//   - team-scoped row:    channel IS NULL AND team_id = $2 (only
//     when $2 is not uuid.Nil);
//   - tenant-default row: channel IS NULL AND team_id IS NULL.
//
// ORDER BY mirrors the in-memory mirror in
// [domain.InMemoryRepository.ListEffectiveForChannel]: scope rank
// ASC (channel < team < tenant, encoded inline via CASE), then
// created_at ASC, then id ASC. The resolver re-sorts the same keys
// — the adapter-side ORDER BY exists so test fixtures are stable
// even when the resolver is bypassed.
func (s *Store) ListEffectiveForChannel(ctx context.Context, tenantID uuid.UUID, channel string, teamID uuid.UUID) ([]domain.Rule, error) {
	if tenantID == uuid.Nil {
		return nil, domain.ErrInvalidTenant
	}

	var channelArg any
	if channel != "" {
		channelArg = channel
	}
	var teamArg any
	if teamID != uuid.Nil {
		teamArg = teamID
	}

	var out []domain.Rule
	err := postgres.WithTenant(ctx, s.pool, tenantID, func(tx pgx.Tx) error {
		rows, qErr := tx.Query(ctx, selectEffectiveRules, channelArg, teamArg)
		if qErr != nil {
			return qErr
		}
		defer rows.Close()
		for rows.Next() {
			r, scanErr := scanRule(rows)
			if scanErr != nil {
				return scanErr
			}
			out = append(out, r)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("funnelrules/postgres: ListEffectiveForChannel: %w", err)
	}
	return out, nil
}

// selectEffectiveRules is the single query the adapter issues. The
// WHERE predicate is fully covered by the partial indexes
// funnel_rules_tenant_enabled_idx and
// funnel_rules_tenant_trigger_idx from migration 0102 — every
// tenant + enabled filter is index-only.
//
// Parameter ordering:
//
//   - $1 = channel (text or NULL — NULL skips the channel-scope
//     branch);
//   - $2 = team_id (uuid or NULL — NULL skips the team-scope branch).
//
// Tenant filter is enforced by RLS; the SQL never mentions
// tenant_id, but the postgres.WithTenant wrapper sets
// app.tenant_id and the table's policies do the rest.
const selectEffectiveRules = `
	SELECT id, tenant_id, channel, team_id, name,
	       trigger_type, trigger_config,
	       action_type,  action_config,
	       enabled, created_at, updated_at
	  FROM funnel_rules
	 WHERE enabled = TRUE
	   AND (
	         (channel = $1)
	      OR (channel IS NULL AND team_id = $2)
	      OR (channel IS NULL AND team_id IS NULL)
	       )
	 ORDER BY
	   CASE
	     WHEN channel  IS NOT NULL THEN 0
	     WHEN team_id  IS NOT NULL THEN 1
	     ELSE                           2
	   END ASC,
	   created_at ASC,
	   id ASC
`

// scanRule materialises one funnel_rules row into the domain entity.
// Pointer-typed scan targets carry the NULLABLE columns (channel,
// team_id) into Go zero-values when the database column is NULL.
func scanRule(row pgx.Row) (domain.Rule, error) {
	var (
		r           domain.Rule
		channel     *string
		teamID      *uuid.UUID
		triggerJSON []byte
		actionJSON  []byte
		triggerType string
		actionType  string
	)
	if err := row.Scan(
		&r.ID, &r.TenantID, &channel, &teamID, &r.Name,
		&triggerType, &triggerJSON,
		&actionType, &actionJSON,
		&r.Enabled, &r.CreatedAt, &r.UpdatedAt,
	); err != nil {
		return domain.Rule{}, err
	}
	if channel != nil {
		r.Channel = *channel
	}
	if teamID != nil && *teamID != uuid.Nil {
		r.TeamID = teamID
	}
	r.TriggerType = domain.TriggerType(triggerType)
	r.ActionType = domain.ActionType(actionType)
	if len(triggerJSON) > 0 {
		if err := json.Unmarshal(triggerJSON, &r.TriggerConfig); err != nil {
			return domain.Rule{}, fmt.Errorf("decode trigger_config: %w", err)
		}
	}
	if len(actionJSON) > 0 {
		if err := json.Unmarshal(actionJSON, &r.ActionConfig); err != nil {
			return domain.Rule{}, fmt.Errorf("decode action_config: %w", err)
		}
	}
	return r, nil
}
