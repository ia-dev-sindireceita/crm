package aipolicy

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// ConsentScope names the (tenant, scope_kind, scope_id) triple the
// ai_policy_consent ledger keys against (migration 0101). The
// ConsentService and adapter both take and return ConsentScope so the
// scope shape stays explicit at every boundary; callers cannot, for
// example, swap scope_kind and scope_id by accident the way two raw
// strings could collide.
//
// The TenantID is uuid.UUID (the table's tenant_id column is uuid).
// ScopeKind reuses ScopeType so the resolver and consent service share
// one canonical enum, and ScopeID stays a string because migration
// 0101 stores scope_id as TEXT — uuid-shaped scopes stringify at the
// boundary.
type ConsentScope struct {
	TenantID uuid.UUID
	Kind     ScopeType
	ID       string
}

// Consent mirrors one row of ai_policy_consent (migration 0101). The
// shape matches the migration column-for-column so the adapter is a
// simple Scan/Exec layer.
//
// PayloadHash is the SHA-256 digest of the anonymized payload preview
// the operator accepted. The cleartext is never persisted. Storing
// the hash on disk lets the gate verify "did the operator already
// see this exact preview" without exposing PII at rest.
//
// ActorUserID is *uuid.UUID because actor_user_id is NULLABLE in
// migration 0101 (ON DELETE SET NULL): when the operator who recorded
// the consent is later deleted the row survives, with actor_user_id
// blanked. The pointer-vs-uuid.Nil split lets the adapter round-trip
// the database NULL faithfully.
//
// AcceptedAt mirrors accepted_at; the adapter accepts the column
// DEFAULT now() on insert and refreshes it on UPDATE so callers don't
// have to seed timestamps.
type Consent struct {
	TenantID          uuid.UUID
	ScopeKind         ScopeType
	ScopeID           string
	ActorUserID       *uuid.UUID
	PayloadHash       [32]byte
	AnonymizerVersion string
	PromptVersion     string
	AcceptedAt        time.Time
}

// ConsentRepository is the storage port for ai_policy_consent rows.
// The pgx adapter lives in internal/adapter/db/postgres/aipolicy;
// ConsentService depends only on this interface so the service unit
// tests can substitute an in-memory fake.
//
// The (tenant_id, scope_kind, scope_id) UNIQUE constraint from
// migration 0101 guarantees Get returns at most one row per scope.
// Adapters MUST translate "no rows" to (zero, false, nil); a non-nil
// error is reserved for transport / driver failures.
type ConsentRepository interface {
	// Get returns the consent row scoped to (tenantID, kind, scopeID),
	// or false when no row matches.
	Get(ctx context.Context, tenantID uuid.UUID, kind ScopeType, scopeID string) (Consent, bool, error)

	// Upsert inserts a brand-new consent row or updates the existing
	// one keyed by (tenant_id, scope_kind, scope_id). The adapter MUST
	// refresh accepted_at on the UPDATE branch so the service can
	// report "when did the operator last accept" without trusting
	// caller-supplied timestamps.
	Upsert(ctx context.Context, c Consent) error
}
