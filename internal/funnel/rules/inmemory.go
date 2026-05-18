package rules

import (
	"context"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

// Compile-time assertion: the in-memory repository satisfies both
// ports. The HTMX editor and the cascade resolver both wire to this
// fake in tests, so a drift in either port shape fails the build of
// this file before any test does.
var (
	_ RuleRepository      = (*InMemoryRepository)(nil)
	_ RuleAdminRepository = (*InMemoryRepository)(nil)
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

// ListAll implements the [RuleAdminRepository] port — returns every
// rule under tenantID (enabled and disabled alike). Used by the
// HTMX editor to render the complete table.
func (r *InMemoryRepository) ListAll(_ context.Context, tenantID uuid.UUID) ([]Rule, error) {
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

// Get implements the [RuleAdminRepository] port.
func (r *InMemoryRepository) Get(_ context.Context, tenantID, id uuid.UUID) (Rule, error) {
	if tenantID == uuid.Nil {
		return Rule{}, ErrInvalidTenant
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, rule := range r.rules {
		if rule.TenantID == tenantID && rule.ID == id {
			return rule, nil
		}
	}
	return Rule{}, ErrNotFound
}

// Create implements the [RuleAdminRepository] port. Appends the row
// in O(1); duplicate IDs are NOT rejected — the caller is expected to
// use NewRule which generates a fresh id when none is supplied.
func (r *InMemoryRepository) Create(_ context.Context, rule Rule) error {
	if rule.TenantID == uuid.Nil {
		return ErrInvalidTenant
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.rules = append(r.rules, rule)
	return nil
}

// Update implements the [RuleAdminRepository] port. Overwrites the
// matching row in-place; ErrNotFound when no row matches.
func (r *InMemoryRepository) Update(_ context.Context, rule Rule) error {
	if rule.TenantID == uuid.Nil {
		return ErrInvalidTenant
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for i, existing := range r.rules {
		if existing.TenantID == rule.TenantID && existing.ID == rule.ID {
			rule.CreatedAt = existing.CreatedAt
			rule.UpdatedAt = time.Now().UTC()
			r.rules[i] = rule
			return nil
		}
	}
	return ErrNotFound
}

// SetEnabled implements the [RuleAdminRepository] port.
func (r *InMemoryRepository) SetEnabled(_ context.Context, tenantID, id uuid.UUID, enabled bool) error {
	if tenantID == uuid.Nil {
		return ErrInvalidTenant
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for i, existing := range r.rules {
		if existing.TenantID == tenantID && existing.ID == id {
			r.rules[i].Enabled = enabled
			r.rules[i].UpdatedAt = time.Now().UTC()
			return nil
		}
	}
	return ErrNotFound
}

// Delete implements the [RuleAdminRepository] port.
func (r *InMemoryRepository) Delete(_ context.Context, tenantID, id uuid.UUID) error {
	if tenantID == uuid.Nil {
		return ErrInvalidTenant
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for i, existing := range r.rules {
		if existing.TenantID == tenantID && existing.ID == id {
			r.rules = append(r.rules[:i], r.rules[i+1:]...)
			return nil
		}
	}
	return ErrNotFound
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
