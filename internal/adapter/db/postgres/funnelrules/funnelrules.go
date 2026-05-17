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
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pericles-luz/crm/internal/adapter/db/postgres"
	domain "github.com/pericles-luz/crm/internal/funnel/rules"
)

// Compile-time assertion: Store satisfies the domain ports. Both the
// resolver-facing read port and the editor-facing admin port are
// implemented here, so a drift in either port shape fails the build
// of this file before any caller notices.
var (
	_ domain.RuleRepository      = (*Store)(nil)
	_ domain.RuleAdminRepository = (*Store)(nil)
)

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

// ListAll implements the [domain.RuleAdminRepository] port — returns
// every rule under tenantID, enabled and disabled alike, ordered by
// scope rank → created_at → id (cascade order). The editor uses this
// to render the complete table.
func (s *Store) ListAll(ctx context.Context, tenantID uuid.UUID) ([]domain.Rule, error) {
	if tenantID == uuid.Nil {
		return nil, domain.ErrInvalidTenant
	}
	var out []domain.Rule
	err := postgres.WithTenant(ctx, s.pool, tenantID, func(tx pgx.Tx) error {
		rows, qErr := tx.Query(ctx, selectAllRulesByTenant)
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
		return nil, fmt.Errorf("funnelrules/postgres: ListAll: %w", err)
	}
	return out, nil
}

// Get implements the [domain.RuleAdminRepository] port.
func (s *Store) Get(ctx context.Context, tenantID, id uuid.UUID) (domain.Rule, error) {
	if tenantID == uuid.Nil {
		return domain.Rule{}, domain.ErrInvalidTenant
	}
	var out domain.Rule
	err := postgres.WithTenant(ctx, s.pool, tenantID, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, selectRuleByID, id)
		r, scanErr := scanRule(row)
		if scanErr != nil {
			if errors.Is(scanErr, pgx.ErrNoRows) {
				return domain.ErrNotFound
			}
			return scanErr
		}
		out = r
		return nil
	})
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return domain.Rule{}, domain.ErrNotFound
		}
		return domain.Rule{}, fmt.Errorf("funnelrules/postgres: Get: %w", err)
	}
	return out, nil
}

// Create implements the [domain.RuleAdminRepository] port. The caller
// is expected to have validated the row via [domain.NewRule]; this
// adapter only marshals and inserts.
func (s *Store) Create(ctx context.Context, r domain.Rule) error {
	if r.TenantID == uuid.Nil {
		return domain.ErrInvalidTenant
	}
	channelArg, teamArg, err := scopeArgs(r)
	if err != nil {
		return err
	}
	triggerJSON, err := encodeJSON(r.TriggerConfig)
	if err != nil {
		return fmt.Errorf("encode trigger_config: %w", err)
	}
	actionJSON, err := encodeJSON(r.ActionConfig)
	if err != nil {
		return fmt.Errorf("encode action_config: %w", err)
	}
	wErr := postgres.WithTenant(ctx, s.pool, r.TenantID, func(tx pgx.Tx) error {
		_, ex := tx.Exec(ctx, insertRule,
			r.ID, r.TenantID, channelArg, teamArg, r.Name,
			string(r.TriggerType), triggerJSON,
			string(r.ActionType), actionJSON,
			r.Enabled, r.CreatedAt, r.UpdatedAt,
		)
		return ex
	})
	if wErr != nil {
		return fmt.Errorf("funnelrules/postgres: Create: %w", wErr)
	}
	return nil
}

// Update implements the [domain.RuleAdminRepository] port.
func (s *Store) Update(ctx context.Context, r domain.Rule) error {
	if r.TenantID == uuid.Nil {
		return domain.ErrInvalidTenant
	}
	channelArg, teamArg, err := scopeArgs(r)
	if err != nil {
		return err
	}
	triggerJSON, err := encodeJSON(r.TriggerConfig)
	if err != nil {
		return fmt.Errorf("encode trigger_config: %w", err)
	}
	actionJSON, err := encodeJSON(r.ActionConfig)
	if err != nil {
		return fmt.Errorf("encode action_config: %w", err)
	}
	wErr := postgres.WithTenant(ctx, s.pool, r.TenantID, func(tx pgx.Tx) error {
		tag, ex := tx.Exec(ctx, updateRule,
			channelArg, teamArg, r.Name,
			string(r.TriggerType), triggerJSON,
			string(r.ActionType), actionJSON,
			r.Enabled,
			r.ID,
		)
		if ex != nil {
			return ex
		}
		if tag.RowsAffected() == 0 {
			return domain.ErrNotFound
		}
		return nil
	})
	if wErr != nil {
		if errors.Is(wErr, domain.ErrNotFound) {
			return domain.ErrNotFound
		}
		return fmt.Errorf("funnelrules/postgres: Update: %w", wErr)
	}
	return nil
}

// SetEnabled implements the [domain.RuleAdminRepository] port.
func (s *Store) SetEnabled(ctx context.Context, tenantID, id uuid.UUID, enabled bool) error {
	if tenantID == uuid.Nil {
		return domain.ErrInvalidTenant
	}
	wErr := postgres.WithTenant(ctx, s.pool, tenantID, func(tx pgx.Tx) error {
		tag, ex := tx.Exec(ctx, setEnabled, enabled, id)
		if ex != nil {
			return ex
		}
		if tag.RowsAffected() == 0 {
			return domain.ErrNotFound
		}
		return nil
	})
	if wErr != nil {
		if errors.Is(wErr, domain.ErrNotFound) {
			return domain.ErrNotFound
		}
		return fmt.Errorf("funnelrules/postgres: SetEnabled: %w", wErr)
	}
	return nil
}

// Delete implements the [domain.RuleAdminRepository] port.
func (s *Store) Delete(ctx context.Context, tenantID, id uuid.UUID) error {
	if tenantID == uuid.Nil {
		return domain.ErrInvalidTenant
	}
	wErr := postgres.WithTenant(ctx, s.pool, tenantID, func(tx pgx.Tx) error {
		tag, ex := tx.Exec(ctx, deleteRule, id)
		if ex != nil {
			return ex
		}
		if tag.RowsAffected() == 0 {
			return domain.ErrNotFound
		}
		return nil
	})
	if wErr != nil {
		if errors.Is(wErr, domain.ErrNotFound) {
			return domain.ErrNotFound
		}
		return fmt.Errorf("funnelrules/postgres: Delete: %w", wErr)
	}
	return nil
}

// scopeArgs translates the Channel + TeamID slot pair into the
// SQL-parameter shape: NULL for empty/zero, real values otherwise.
// Channel-scoped rules carry channel != "" and team_id NULL;
// team-scoped rules carry channel NULL and team_id != Nil;
// tenant-default rules carry both NULL.
func scopeArgs(r domain.Rule) (any, any, error) {
	var channelArg any
	var teamArg any
	if r.Channel != "" {
		channelArg = r.Channel
	}
	if r.Channel == "" && r.TeamID != nil && *r.TeamID != uuid.Nil {
		teamArg = *r.TeamID
	}
	return channelArg, teamArg, nil
}

// encodeJSON marshals the opaque per-type config bag. A nil map maps
// to the canonical "{}" so the NOT NULL DEFAULT '{}' column never sees
// a Go-level nil.
func encodeJSON(m map[string]any) ([]byte, error) {
	if m == nil {
		return []byte("{}"), nil
	}
	return json.Marshal(m)
}

// selectAllRulesByTenant returns every rule under the active tenant
// — enabled and disabled alike — ordered by cascade rank → created_at
// → id. The editor renders the rows in this order so the table reads
// top-down in the same order the resolver picks winners.
//
// Tenant filter is enforced by RLS via postgres.WithTenant.
const selectAllRulesByTenant = `
	SELECT id, tenant_id, channel, team_id, name,
	       trigger_type, trigger_config,
	       action_type,  action_config,
	       enabled, created_at, updated_at
	  FROM funnel_rules
	 ORDER BY
	   CASE
	     WHEN channel  IS NOT NULL THEN 0
	     WHEN team_id  IS NOT NULL THEN 1
	     ELSE                           2
	   END ASC,
	   created_at ASC,
	   id ASC
`

const selectRuleByID = `
	SELECT id, tenant_id, channel, team_id, name,
	       trigger_type, trigger_config,
	       action_type,  action_config,
	       enabled, created_at, updated_at
	  FROM funnel_rules
	 WHERE id = $1
`

const insertRule = `
	INSERT INTO funnel_rules
	  (id, tenant_id, channel, team_id, name,
	   trigger_type, trigger_config,
	   action_type,  action_config,
	   enabled, created_at, updated_at)
	VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
`

// updateRule overwrites the editable columns and stamps updated_at via
// now() so storage owns the timestamp and the audit trigger sees the
// fresh value. tenant_id is NOT updatable — the RLS policy would reject
// a cross-tenant move anyway.
const updateRule = `
	UPDATE funnel_rules
	   SET channel        = $1,
	       team_id        = $2,
	       name           = $3,
	       trigger_type   = $4,
	       trigger_config = $5,
	       action_type    = $6,
	       action_config  = $7,
	       enabled        = $8,
	       updated_at     = now()
	 WHERE id = $9
`

const setEnabled = `
	UPDATE funnel_rules
	   SET enabled    = $1,
	       updated_at = now()
	 WHERE id = $2
`

const deleteRule = `
	DELETE FROM funnel_rules
	 WHERE id = $1
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
