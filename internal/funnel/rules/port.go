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
