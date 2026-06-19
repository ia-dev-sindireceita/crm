// Package aiassistadapter holds the composition-side adapters that
// bridge the internal/aiassist ports to their concrete collaborators
// when the port shapes do not match structurally.
//
// The first such bridge is PolicyResolver: internal/aipolicy.Resolver
// answers "which ai_policy row applies" with an aipolicy-shaped
// signature (a ResolveInput struct in, a (Policy, ResolveSource, error)
// triple out), whereas internal/aiassist.PolicyResolver wants the
// trimmed (tenantID, Scope) → (aiassist.Policy, error) shape the
// summarize use case calls. Rather than widen either domain port to
// know about the other, this adapter translates between them — the
// canonical hexagonal seam.
package aiassistadapter

import (
	"context"
	"errors"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/aiassist"
	"github.com/pericles-luz/crm/internal/aipolicy"
)

// aipolicyResolver is the slice of *aipolicy.Resolver this bridge
// consumes. Declaring it locally (accept-an-interface) keeps the
// bridge unit-testable with a hand-rolled fake — no Postgres, no
// concrete resolver — and documents the exact dependency.
type aipolicyResolver interface {
	Resolve(ctx context.Context, in aipolicy.ResolveInput) (aipolicy.Policy, aipolicy.ResolveSource, error)
}

// PolicyResolver adapts an aipolicy resolver to the
// aiassist.PolicyResolver port. The compile-time assertion below is
// the first failure point if the aiassist port drifts.
type PolicyResolver struct {
	inner aipolicyResolver
}

var _ aiassist.PolicyResolver = (*PolicyResolver)(nil)

// NewPolicyResolver wraps inner. A nil inner is a wiring bug; the
// constructor rejects it rather than letting Resolve panic at the
// first summarize call.
func NewPolicyResolver(inner aipolicyResolver) (*PolicyResolver, error) {
	if inner == nil {
		return nil, errors.New("aiassistadapter: aipolicy resolver is required")
	}
	return &PolicyResolver{inner: inner}, nil
}

// Resolve maps the aiassist call shape onto the aipolicy resolver and
// translates the resulting row into aiassist.Policy.
//
// Scope translation: aiassist.Scope carries ChannelID / TeamID as
// plain strings; aipolicy.ResolveInput wants *string (nil = "no scope
// at this level", so the cascade skips it). An empty string therefore
// maps to a nil pointer, not a pointer-to-empty, so the resolver does
// not look up a blank scope_id.
//
// MaxOutputTokens has no column on ai_policy, so it maps to 0 — the
// aiassist use case documents 0 as "adapter default output budget,
// reservation covers estimated prompt tokens only". The ResolveSource
// tag (channel/team/tenant/default) is dropped: the summarize use case
// does not branch on it, and the audit row the resolver feeds is a
// separate concern owned by the aipolicy surface.
func (r *PolicyResolver) Resolve(ctx context.Context, tenantID uuid.UUID, scope aiassist.Scope) (aiassist.Policy, error) {
	in := aipolicy.ResolveInput{TenantID: tenantID}
	if scope.ChannelID != "" {
		channel := scope.ChannelID
		in.ChannelID = &channel
	}
	if scope.TeamID != "" {
		team := scope.TeamID
		in.TeamID = &team
	}

	pol, _, err := r.inner.Resolve(ctx, in)
	if err != nil {
		return aiassist.Policy{}, err
	}
	return aiassist.Policy{
		AIEnabled:        pol.AIEnabled,
		OptIn:            pol.OptIn,
		Anonymize:        pol.Anonymize,
		Model:            pol.Model,
		MaxOutputTokens:  0,
		PromptVersion:    pol.PromptVersion,
		StructuredFields: pol.StructuredFields,
	}, nil
}
