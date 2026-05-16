package aipolicy

import (
	"context"

	"github.com/google/uuid"
)

// Repository is the storage port for ai_policy rows. The concrete
// adapter lives in internal/adapter/db/postgres/aipolicy; the
// resolver depends only on this interface so unit tests can
// substitute an in-memory fake without spinning up Postgres.
//
// The (tenant_id, scope_type, scope_id) UNIQUE index from migration
// 0098 guarantees that Get returns at most one row per scope. The
// boolean second return mirrors comma-ok lookups in Go and lets the
// resolver short-circuit without classifying every miss as an error.
type Repository interface {
	// Get returns the policy row scoped to (tenantID, scopeType,
	// scopeID), or false when no row matches. A non-nil error is
	// reserved for transport / driver failures; a missing row is the
	// false-without-error outcome the resolver depends on to cascade.
	Get(ctx context.Context, tenantID uuid.UUID, scopeType ScopeType, scopeID string) (Policy, bool, error)

	// Upsert persists policy, inserting on miss and updating on hit
	// against the (tenant_id, scope_type, scope_id) UNIQUE index.
	// Callers MUST populate TenantID, ScopeType, ScopeID; the adapter
	// rejects zero values with a typed error so a misconfigured admin
	// form does not silently write a wildcard policy.
	Upsert(ctx context.Context, policy Policy) error
}
