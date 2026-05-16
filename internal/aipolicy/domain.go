package aipolicy

import (
	"time"

	"github.com/google/uuid"
)

// ScopeType names one of the three configuration layers ADR-0042
// describes. The string values match the CHECK constraint on
// ai_policy.scope_type so the resolver can compose database lookups
// without an intermediate translation table.
type ScopeType string

const (
	// ScopeTenant is the tenant-wide default policy. scope_id equals
	// the tenant id (rendered as text).
	ScopeTenant ScopeType = "tenant"
	// ScopeTeam carries one operator squad's override. scope_id is the
	// team uuid in text form.
	ScopeTeam ScopeType = "team"
	// ScopeChannel carries an external-customer / channel override.
	// scope_id is a short channel key such as "whatsapp" or a channel
	// uuid in text form, per migration 0098's commentary.
	ScopeChannel ScopeType = "channel"
)

// IsValid reports whether s names one of the three allowed scopes.
// Callers reading scope_type out of Postgres can assert validity
// without colliding with the CHECK constraint, and tests can fail
// loudly when the database returns an unexpected value.
func (s ScopeType) IsValid() bool {
	switch s {
	case ScopeTenant, ScopeTeam, ScopeChannel:
		return true
	}
	return false
}

// ResolveSource tags which cascade level produced the Policy that the
// resolver returns. The audit pipeline (ADR-0042 §D6) records it so
// operators can answer "which row authorised this call" by reading
// one column.
type ResolveSource string

const (
	// SourceChannel marks a hit on a channel-scoped row.
	SourceChannel ResolveSource = "channel"
	// SourceTeam marks a hit on a team-scoped row.
	SourceTeam ResolveSource = "team"
	// SourceTenant marks a hit on the tenant-scoped row.
	SourceTenant ResolveSource = "tenant"
	// SourceDefault marks the hard-coded DefaultPolicy() fallback,
	// returned only when no row matches any cascade level.
	SourceDefault ResolveSource = "default"
)

// Policy mirrors one row of the ai_policy table (migration 0098).
// The struct is the unit of configuration the resolver hands back to
// the use-case; the LLM port, anonymizer, and wallet all read from it
// once the cascade has produced a single source-of-truth row.
//
// The struct is intentionally flat: no pointers for nullable fields,
// because every column on ai_policy is NOT NULL. The TenantID stays
// uuid.UUID (the database column is uuid); ScopeID is text in
// Postgres so we carry it as a string and let the caller render
// uuid-shaped scopes via .String() at the boundary.
type Policy struct {
	TenantID      uuid.UUID
	ScopeType     ScopeType
	ScopeID       string
	Model         string
	PromptVersion string
	Tone          string
	Language      string
	AIEnabled     bool
	Anonymize     bool
	OptIn         bool
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// ResolveInput names a single call site: which tenant is asking for a
// policy, optionally narrowed by channel and team. Nil pointers mean
// "this call has no scope at this level" — the resolver skips that
// step instead of looking up an empty scope_id.
//
// ChannelID and TeamID are *string because migration 0098 stores
// scope_id as text: channel keys are short identifiers like "whatsapp"
// and the resolver must not invent a uuid encoding. Callers that have
// uuid-shaped channel/team ids stringify at the boundary.
type ResolveInput struct {
	TenantID  uuid.UUID
	ChannelID *string
	TeamID    *string
}

// DefaultPolicy returns the hard-coded fallback the resolver hands
// back when no row matches any cascade level. Defaults mirror the
// column DEFAULTs in migration 0098 so a tenant that does have a
// tenant-level row inserted via the column defaults gets the same
// policy as a tenant with no row at all.
//
// AIEnabled = false is the LGPD opt-in posture (ADR-0041): a tenant
// that has never been configured cannot accidentally call the LLM.
// Anonymize = true defends in depth: even when an operator flips
// AIEnabled true via the admin UI, the payload is still anonymised
// by default.
func DefaultPolicy() Policy {
	return Policy{
		Model:         "openrouter/auto",
		PromptVersion: "v1",
		Tone:          "neutro",
		Language:      "pt-BR",
		AIEnabled:     false,
		Anonymize:     true,
		OptIn:         false,
	}
}
