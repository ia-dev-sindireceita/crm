package rules

import (
	"context"

	"github.com/google/uuid"
)

// RuleRepository is the storage port for funnel_rules rows. The
// concrete adapter lives in internal/adapter/db/postgres/funnelrules;
// the resolver and consumer use-cases depend only on this interface
// so unit tests substitute an in-memory fake without touching pgx.
type RuleRepository interface {
	// ListEffectiveForChannel returns every enabled rule that applies
	// to an inbound event landing on (tenantID, channel, teamID). The
	// result is the UNION of three scopes:
	//
	//   - channel-scoped rows whose channel column equals `channel`
	//     (exact string match; "webchat" rules do not fire for
	//     "whatsapp" events — the AC#3 isolation contract);
	//   - team-scoped rows whose team_id equals teamID (when teamID
	//     is not uuid.Nil);
	//   - tenant-scoped rows (both channel and team_id NULL), which
	//     act as the per-tenant baseline.
	//
	// The adapter MUST NOT impose a cross-scope precedence — the
	// [Resolver] owns the cascade. The adapter SHOULD return rows in
	// a deterministic order (channel rank first, then team, then
	// tenant; within a scope by created_at ASC and id ASC) so that
	// test fixtures are stable even before the resolver runs.
	//
	// channel == "" with teamID == uuid.Nil collapses to a pure
	// tenant-scope query (only the rules with both columns NULL).
	// Callers that want "channel only" must pass channel != "".
	ListEffectiveForChannel(ctx context.Context, tenantID uuid.UUID, channel string, teamID uuid.UUID) ([]Rule, error)
}

// RuleAdminRepository is the write-side + editor-read-side port the
// HTMX rules editor (SIN-62961) depends on. It is intentionally split
// from [RuleRepository] so the cascade resolver, which only reads, does
// not pick up an indirect dependency on the write methods.
//
// All methods are tenant-scoped at the boundary: the editor passes the
// active tenant from request context, and the adapter wraps every call
// in postgres.WithTenant so the RLS GUC app.tenant_id is set before
// reading or writing.
type RuleAdminRepository interface {
	// ListAll returns every rule under tenantID — enabled and disabled
	// alike — so the editor can render a complete table. Ordering is
	// deterministic: scope rank ASC (channel < team < tenant), then
	// CreatedAt ASC, then ID ASC, mirroring the cascade order so the
	// list reads top-to-bottom in the same order the resolver picks
	// winners.
	ListAll(ctx context.Context, tenantID uuid.UUID) ([]Rule, error)

	// Get returns a single rule by id. ErrNotFound when no row matches
	// (id is foreign or belongs to another tenant — RLS hides it).
	Get(ctx context.Context, tenantID, id uuid.UUID) (Rule, error)

	// Create persists a new rule. The Rule MUST have been constructed
	// via [NewRule] so the structural invariants are already enforced.
	// The adapter rejects collisions at the SQL layer (no unique index
	// today, but the master_ops_audit trigger fires on INSERT for the
	// audit row).
	Create(ctx context.Context, r Rule) error

	// Update overwrites name, scope (channel + team_id), trigger,
	// action, and enabled on the existing row. updated_at is stamped
	// by the adapter via now(). ErrNotFound when no row matches.
	Update(ctx context.Context, r Rule) error

	// SetEnabled toggles the enabled flag without touching anything
	// else. The editor uses this for the inline toggle button so the
	// PATCH stays small and the audit trail records a focused event.
	SetEnabled(ctx context.Context, tenantID, id uuid.UUID, enabled bool) error

	// Delete removes the row. ErrNotFound when no row matches.
	Delete(ctx context.Context, tenantID, id uuid.UUID) error
}
