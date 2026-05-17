package rules

import (
	"context"
	"sort"
	"strings"
	"sync"

	"github.com/google/uuid"
)

// InMemoryRepository is a tenant-scoped, race-safe [RuleRepository]
// for tests that exercise the [Resolver] without spinning up
// Postgres. It mirrors the documented adapter semantics:
//
//   - ListEffectiveForChannel returns the UNION of channel-, team-,
//     and tenant-scope rules under tenantID, filtered to enabled=true
//     and ordered by (scope rank, created_at ASC, id ASC).
//   - Cross-tenant rows are invisible — a rule under tenant B never
//     surfaces for tenant A even if their channel + team match.
//   - Channel match is exact-string ('webchat' rules do not fire for
//     'whatsapp' events).
//
// Production wiring uses the pgx-backed Store under
// internal/adapter/db/postgres/funnelrules; this type exists strictly
// to keep resolver and consumer-use-case tests off the database.
type InMemoryRepository struct {
	mu    sync.Mutex
	rules []Rule
}

// NewInMemoryRepository returns an empty repository ready to use.
// Safe for concurrent callers.
func NewInMemoryRepository() *InMemoryRepository {
	return &InMemoryRepository{}
}

// Seed bulk-inserts rule rows. The slice is copied; the repository
// owns its internal storage. Disabled rules survive the seed —
// ListEffectiveForChannel filters them on read so tests can assert
// the "enabled=false silently ignored" contract by seeding and
// then re-reading.
func (r *InMemoryRepository) Seed(rules ...Rule) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.rules = append(r.rules, rules...)
}

// ListEffectiveForChannel implements the [RuleRepository] port. See
// the doc comment on the port for the exact contract.
func (r *InMemoryRepository) ListEffectiveForChannel(_ context.Context, tenantID uuid.UUID, channel string, teamID uuid.UUID) ([]Rule, error) {
	if tenantID == uuid.Nil {
		return nil, ErrInvalidTenant
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	out := make([]Rule, 0)
	for _, rule := range r.rules {
		if rule.TenantID != tenantID {
			continue
		}
		if !rule.Enabled {
			continue
		}
		if !matchesScope(rule, channel, teamID) {
			continue
		}
		cp := rule
		out = append(out, cp)
	}
	sort.SliceStable(out, func(i, j int) bool {
		si, sj := scopeRank(out[i].Scope()), scopeRank(out[j].Scope())
		if si != sj {
			return si < sj
		}
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.Before(out[j].CreatedAt)
		}
		return out[i].ID.String() < out[j].ID.String()
	})
	return out, nil
}

// matchesScope is the in-memory mirror of the adapter's SQL WHERE
// clause. Returns true when the rule's scope columns line up with
// the (channel, teamID) event context.
func matchesScope(rule Rule, channel string, teamID uuid.UUID) bool {
	switch rule.Scope() {
	case ScopeChannel:
		return strings.EqualFold(rule.Channel, channel) && channel != ""
	case ScopeTeam:
		return teamID != uuid.Nil && rule.TeamID != nil && *rule.TeamID == teamID
	case ScopeTenant:
		// Tenant-default rules always match — every event under the
		// tenant is in their scope.
		return true
	default:
		return false
	}
}
