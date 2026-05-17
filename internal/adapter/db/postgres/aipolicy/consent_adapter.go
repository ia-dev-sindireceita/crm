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

// ConsentStore satisfies aipolicy.ConsentRepository. The compile-time
// assertion fails the build before any caller notices a port change.
var _ domain.ConsentRepository = (*ConsentStore)(nil)

// ConsentStore is the pgx-backed adapter for ai_policy_consent
// (migration 0101). Construct via NewConsentStore; the pool MUST be
// the app_runtime pool so the RLS policies on ai_policy_consent
// (tenant_isolation_select/insert/update/delete) apply.
type ConsentStore struct {
	pool postgres.TxBeginner
}

// NewConsentStore wraps pool and returns a ready ConsentStore. A nil
// pool yields postgres.ErrNilPool so cmd/server fails fast on
// misconfiguration.
func NewConsentStore(pool *pgxpool.Pool) (*ConsentStore, error) {
	if pool == nil {
		return nil, postgres.ErrNilPool
	}
	return &ConsentStore{pool: pool}, nil
}

// Get returns the ai_policy_consent row for (tenantID, kind, scopeID).
// The (tenant_id, scope_kind, scope_id) UNIQUE index from migration
// 0101 satisfies the lookup in one index probe. A missing row collapses
// to (zero, false, nil); transport/driver failures bubble up wrapped.
//
// RLS hides rows belonging to other tenants so the same (kind, id)
// pair can coexist across tenants without leaking.
func (s *ConsentStore) Get(
	ctx context.Context,
	tenantID uuid.UUID,
	kind domain.ScopeType,
	scopeID string,
) (domain.Consent, bool, error) {
	var zero domain.Consent
	if tenantID == uuid.Nil {
		return zero, false, fmt.Errorf("aipolicy/postgres: Consent.Get: %w", domain.ErrInvalidTenant)
	}
	if !kind.IsValid() {
		return zero, false, fmt.Errorf("aipolicy/postgres: Consent.Get: %w", domain.ErrInvalidScopeType)
	}
	if strings.TrimSpace(scopeID) == "" {
		return zero, false, fmt.Errorf("aipolicy/postgres: Consent.Get: %w", domain.ErrInvalidScopeID)
	}

	var consent domain.Consent
	found := false
	err := postgres.WithTenant(ctx, s.pool, tenantID, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
			SELECT tenant_id, scope_kind, scope_id,
			       actor_user_id, payload_hash,
			       anonymizer_version, prompt_version, accepted_at
			  FROM ai_policy_consent
			 WHERE scope_kind = $1
			   AND scope_id   = $2
		`, string(kind), scopeID)
		var scopeStr string
		var hashBytes []byte
		var actor *uuid.UUID
		if err := row.Scan(
			&consent.TenantID,
			&scopeStr,
			&consent.ScopeID,
			&actor,
			&hashBytes,
			&consent.AnonymizerVersion,
			&consent.PromptVersion,
			&consent.AcceptedAt,
		); err != nil {
			return err
		}
		consent.ScopeKind = domain.ScopeType(scopeStr)
		consent.ActorUserID = actor
		if len(hashBytes) != len(consent.PayloadHash) {
			return fmt.Errorf("payload_hash length=%d; want %d", len(hashBytes), len(consent.PayloadHash))
		}
		copy(consent.PayloadHash[:], hashBytes)
		found = true
		return nil
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return zero, false, nil
	}
	if err != nil {
		return zero, false, fmt.Errorf("aipolicy/postgres: Consent.Get: %w", err)
	}
	return consent, found, nil
}

// Upsert inserts a brand-new consent row or updates the existing one
// keyed by (tenant_id, scope_kind, scope_id). The UPDATE branch
// refreshes accepted_at to now() so the service does not have to
// trust caller-supplied timestamps for the "when did the operator
// last accept" answer.
//
// The ON CONFLICT predicate matches the UNIQUE constraint from
// migration 0101; UPDATE replaces payload_hash, anonymizer_version,
// prompt_version, and actor_user_id so a re-consent against a newer
// preview / version drops the old row's state without leaving a
// dangling tuple.
func (s *ConsentStore) Upsert(ctx context.Context, c domain.Consent) error {
	if c.TenantID == uuid.Nil {
		return fmt.Errorf("aipolicy/postgres: Consent.Upsert: %w", domain.ErrInvalidTenant)
	}
	if !c.ScopeKind.IsValid() {
		return fmt.Errorf("aipolicy/postgres: Consent.Upsert: %w", domain.ErrInvalidScopeType)
	}
	if strings.TrimSpace(c.ScopeID) == "" {
		return fmt.Errorf("aipolicy/postgres: Consent.Upsert: %w", domain.ErrInvalidScopeID)
	}
	if strings.TrimSpace(c.AnonymizerVersion) == "" {
		return fmt.Errorf("aipolicy/postgres: Consent.Upsert: %w", domain.ErrInvalidAnonymizerVersion)
	}
	if strings.TrimSpace(c.PromptVersion) == "" {
		return fmt.Errorf("aipolicy/postgres: Consent.Upsert: %w", domain.ErrInvalidPromptVersion)
	}

	hash := c.PayloadHash[:]
	return postgres.WithTenant(ctx, s.pool, c.TenantID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO ai_policy_consent
			  (tenant_id, scope_kind, scope_id, actor_user_id,
			   payload_hash, anonymizer_version, prompt_version)
			VALUES ($1, $2, $3, $4, $5, $6, $7)
			ON CONFLICT (tenant_id, scope_kind, scope_id)
			DO UPDATE SET
			   actor_user_id      = EXCLUDED.actor_user_id,
			   payload_hash       = EXCLUDED.payload_hash,
			   anonymizer_version = EXCLUDED.anonymizer_version,
			   prompt_version     = EXCLUDED.prompt_version,
			   accepted_at        = now()
		`,
			c.TenantID,
			string(c.ScopeKind),
			c.ScopeID,
			c.ActorUserID,
			hash,
			c.AnonymizerVersion,
			c.PromptVersion,
		)
		if err != nil {
			return fmt.Errorf("aipolicy/postgres: Consent.Upsert: %w", err)
		}
		return nil
	})
}
