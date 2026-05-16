package aipolicy

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
)

// Resolver runs the cascade ADR-0042 defines: channel > team >
// tenant > default. The struct is intentionally tiny: the
// Repository port is the only collaborator, and the Resolve method
// is pure given a fixed repository state. A fake Repository in
// tests reproduces every branch.
type Resolver struct {
	repo Repository
}

// NewResolver returns a ready Resolver. A nil repo is a programming
// bug; the constructor rejects it instead of letting Resolve panic
// at the call site.
func NewResolver(repo Repository) (*Resolver, error) {
	if repo == nil {
		return nil, errors.New("aipolicy: repository is required")
	}
	return &Resolver{repo: repo}, nil
}

// Resolve returns the effective Policy for the call described by in,
// plus a ResolveSource tag the caller can record on the audit row.
//
// The cascade short-circuits: the first hit wins in full. The
// tenant-level lookup uses in.TenantID rendered as text because
// migration 0098 stores scope_id as TEXT (tenant ids are uuids that
// the resolver stringifies at this boundary).
//
// A SourceDefault outcome is the deny-by-default fallback: no row
// matched, so the caller receives DefaultPolicy() with AIEnabled =
// false. The use-case is expected to reject the LLM call when
// AIEnabled is false; the resolver itself never decides "deny", it
// only reports which row applied.
func (r *Resolver) Resolve(ctx context.Context, in ResolveInput) (Policy, ResolveSource, error) {
	var zero Policy
	if in.TenantID == uuid.Nil {
		return zero, "", ErrInvalidTenant
	}

	if in.ChannelID != nil && *in.ChannelID != "" {
		policy, ok, err := r.repo.Get(ctx, in.TenantID, ScopeChannel, *in.ChannelID)
		if err != nil {
			return zero, "", fmt.Errorf("aipolicy: resolve channel: %w", err)
		}
		if ok {
			return policy, SourceChannel, nil
		}
	}

	if in.TeamID != nil && *in.TeamID != "" {
		policy, ok, err := r.repo.Get(ctx, in.TenantID, ScopeTeam, *in.TeamID)
		if err != nil {
			return zero, "", fmt.Errorf("aipolicy: resolve team: %w", err)
		}
		if ok {
			return policy, SourceTeam, nil
		}
	}

	policy, ok, err := r.repo.Get(ctx, in.TenantID, ScopeTenant, in.TenantID.String())
	if err != nil {
		return zero, "", fmt.Errorf("aipolicy: resolve tenant: %w", err)
	}
	if ok {
		return policy, SourceTenant, nil
	}

	return DefaultPolicy(), SourceDefault, nil
}
