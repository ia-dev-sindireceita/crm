package rules

import (
	"context"
	"sort"

	"github.com/google/uuid"
)

// ResolveInput is the per-event call shape the cascade resolver
// expects. Mirrors the [RuleRepository.ListEffectiveForChannel]
// arguments so consumers can pipe the two without re-shaping data.
type ResolveInput struct {
	// TenantID scopes the entire resolution; uuid.Nil is rejected.
	TenantID uuid.UUID

	// Channel is the textual channel identifier the event arrived on
	// ('whatsapp', 'webchat', 'instagram', …). Empty means the event
	// is not channel-bound — channel-scope rules are skipped.
	Channel string

	// TeamID is the operator squad the conversation is currently
	// assigned to. uuid.Nil means the event is not team-bound —
	// team-scope rules are skipped.
	TeamID uuid.UUID
}

// ResolvedRule is one entry in the cascade output: the rule that won
// for its trigger plus the scope it came from (for audit records).
type ResolvedRule struct {
	Rule        Rule
	SourceScope Scope
}

// Resolver applies the cascade order (channel > team > tenant) on
// top of [RuleRepository] to return the effective rule set for a
// given event scope.
//
// The resolver is pure: same repository state + same ResolveInput →
// same []ResolvedRule. No clocks, no caches, no side effects beyond
// the repository call.
type Resolver struct {
	repo RuleRepository
}

// NewResolver returns a Resolver wired to repo. A nil repo is
// rejected with a typed nil-pointer error so cmd/server fails fast
// at boot instead of NPE'ing on first call.
func NewResolver(repo RuleRepository) (*Resolver, error) {
	if repo == nil {
		return nil, ErrInvalidRule
	}
	return &Resolver{repo: repo}, nil
}

// Resolve returns the cascade-effective rule set for the input event
// scope. Distinct triggers all survive. When two rules across
// different scopes carry the same TriggerSignature, the
// most-specific scope wins (channel > team > tenant) and the
// less-specific one is dropped. Rules with an empty signature
// (unknown trigger types or malformed configs) are always adopted —
// they cannot collide because the dedup map keys them by signature.
//
// Output order: stable, deterministic across runs against the same
// repository snapshot:
//
//  1. By scope rank ASC (channel rows first, then team, then tenant);
//  2. Within a scope, by Rule.CreatedAt ASC;
//  3. Tiebreak by Rule.ID lexicographic ASC.
//
// Callers iterate the slice in order to emit audit records, so the
// natural ordering follows the cascade narrative ("channel rule X
// fired before tenant rule Y").
func (r *Resolver) Resolve(ctx context.Context, in ResolveInput) ([]ResolvedRule, error) {
	if in.TenantID == uuid.Nil {
		return nil, ErrInvalidTenant
	}
	candidates, err := r.repo.ListEffectiveForChannel(ctx, in.TenantID, in.Channel, in.TeamID)
	if err != nil {
		return nil, err
	}
	return cascade(candidates), nil
}

// cascade is the pure algorithm extracted from [Resolver.Resolve] so
// it can be exercised in unit tests without spinning up a fake
// repository. Exported through [Resolver.Resolve]; the function
// stays unexported so the package surface is just the resolver.
//
// Algorithm:
//
//  1. Sort candidates by (scope rank, created_at, id) ascending so
//     iteration order matches the cascade order regardless of how
//     the repository ordered them.
//  2. Walk the sorted slice. For each rule with a non-empty
//     signature, adopt the first occurrence and drop subsequent
//     occurrences of the same signature (first-match-wins per
//     trigger). Rules with an empty signature are always adopted.
//  3. Disabled rules are skipped — they cannot win, they cannot
//     shadow.
//
// The output is already in cascade order (step 1's sort survives
// because we never reorder during the walk).
func cascade(candidates []Rule) []ResolvedRule {
	sorted := make([]Rule, 0, len(candidates))
	for _, c := range candidates {
		if !c.Enabled {
			continue
		}
		sorted = append(sorted, c)
	}
	sort.SliceStable(sorted, func(i, j int) bool {
		si, sj := scopeRank(sorted[i].Scope()), scopeRank(sorted[j].Scope())
		if si != sj {
			return si < sj
		}
		if !sorted[i].CreatedAt.Equal(sorted[j].CreatedAt) {
			return sorted[i].CreatedAt.Before(sorted[j].CreatedAt)
		}
		return sorted[i].ID.String() < sorted[j].ID.String()
	})

	seen := make(map[string]struct{}, len(sorted))
	out := make([]ResolvedRule, 0, len(sorted))
	for _, rule := range sorted {
		sig := rule.TriggerSignature()
		if sig != "" {
			if _, dup := seen[sig]; dup {
				continue
			}
			seen[sig] = struct{}{}
		}
		out = append(out, ResolvedRule{Rule: rule, SourceScope: rule.Scope()})
	}
	return out
}
