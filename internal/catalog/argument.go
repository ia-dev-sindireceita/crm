package catalog

import (
	"strings"
	"time"

	"github.com/google/uuid"
)

// ScopeType selects how a product argument's scope is interpreted.
// The trio matches the scope_type CHECK in migration 0098 and the
// cascade order canal > equipe > tenant the resolver applies.
type ScopeType string

const (
	// ScopeTenant is the fallback scope every product argument
	// implicitly competes with — applies to every team and every
	// channel under the tenant.
	ScopeTenant ScopeType = "tenant"

	// ScopeTeam narrows the argument to one team (scopeID is the
	// team's UUID rendered as text — that is the migration's column
	// type).
	ScopeTeam ScopeType = "team"

	// ScopeChannel narrows the argument to one channel. scopeID is
	// the channel key (e.g. "whatsapp", "instagram") — short
	// identifiers, not UUIDs (see migration 0098's header notes).
	ScopeChannel ScopeType = "channel"
)

// specificity ranks scope types so the resolver can order matches
// most-specific first. Higher = more specific. Channel beats team
// beats tenant — same order as the W2A policy resolver and the
// cascade documented on migration 0098.
func (s ScopeType) specificity() int {
	switch s {
	case ScopeChannel:
		return 3
	case ScopeTeam:
		return 2
	case ScopeTenant:
		return 1
	default:
		return 0
	}
}

// Valid reports whether the scope type is one of the three documented
// constants. Constructors gate on this so a typo can't accidentally
// widen visibility.
func (s ScopeType) Valid() bool { return s.specificity() > 0 }

// ScopeAnchor is the (type, id) pair an individual ProductArgument
// row lives at. scopeID is `text` in the schema so the same struct
// serves channel keys and uuids alike — the resolver doesn't need a
// per-kind type.
type ScopeAnchor struct {
	Type ScopeType
	ID   string
}

// Validate returns ErrInvalidScope if the scope type is unknown or
// the id is blank after trimming.
func (s ScopeAnchor) Validate() error {
	if !s.Type.Valid() {
		return ErrInvalidScope
	}
	if strings.TrimSpace(s.ID) == "" {
		return ErrInvalidScope
	}
	return nil
}

// Scope is the runtime context the resolver matches arguments
// against. Tenant membership is implicit (passed alongside Scope as
// the tenantID parameter). TeamID and ChannelID are optional — an
// empty string means "no team context" or "no channel context" and
// only the tenant-scoped argument will match.
//
// The schema stores team_id as a UUID rendered to text and channel
// keys as short identifiers; both arrive at Scope as plain strings so
// the resolver does not depend on uuid.UUID for the team case.
type Scope struct {
	TeamID    string
	ChannelID string
}

// matches reports whether anchor applies under this Scope. The
// tenant anchor always matches (it is the catch-all). Team and
// channel anchors must agree on the corresponding id.
func (s Scope) matches(anchor ScopeAnchor) bool {
	switch anchor.Type {
	case ScopeTenant:
		return true
	case ScopeTeam:
		return s.TeamID != "" && anchor.ID == s.TeamID
	case ScopeChannel:
		return s.ChannelID != "" && anchor.ID == s.ChannelID
	default:
		return false
	}
}

// ProductArgument is the selling pitch for one product within one
// scope (tenant / team / channel). It maps 1:1 to the
// product_argument row (migration 0098).
//
// Invariants enforced by NewProductArgument:
//
//  1. tenantID, productID != uuid.Nil — every argument belongs to a
//     tenant-owned product.
//  2. anchor.Type is one of the three documented constants.
//  3. anchor.ID is non-empty after trimming.
//  4. text is non-empty after trimming — an empty pitch is
//     un-renderable in the IA prompt.
type ProductArgument struct {
	id        uuid.UUID
	tenantID  uuid.UUID
	productID uuid.UUID
	anchor    ScopeAnchor
	text      string
	createdAt time.Time
	updatedAt time.Time
}

// NewProductArgument constructs a ProductArgument with the supplied
// invariants. The caller supplies `now` so tests can pin time.
func NewProductArgument(
	tenantID, productID uuid.UUID,
	anchor ScopeAnchor,
	text string,
	now time.Time,
) (*ProductArgument, error) {
	if tenantID == uuid.Nil {
		return nil, ErrZeroTenant
	}
	if productID == uuid.Nil {
		return nil, ErrInvalidArgument
	}
	if err := anchor.Validate(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(text) == "" {
		return nil, ErrInvalidArgument
	}
	return &ProductArgument{
		id:        uuid.New(),
		tenantID:  tenantID,
		productID: productID,
		anchor:    anchor,
		text:      text,
		createdAt: now,
		updatedAt: now,
	}, nil
}

// HydrateProductArgument reconstructs a ProductArgument from durable
// state. Only adapters should call this.
func HydrateProductArgument(
	id, tenantID, productID uuid.UUID,
	anchor ScopeAnchor,
	text string,
	createdAt, updatedAt time.Time,
) *ProductArgument {
	return &ProductArgument{
		id:        id,
		tenantID:  tenantID,
		productID: productID,
		anchor:    anchor,
		text:      text,
		createdAt: createdAt,
		updatedAt: updatedAt,
	}
}

func (a *ProductArgument) ID() uuid.UUID        { return a.id }
func (a *ProductArgument) TenantID() uuid.UUID  { return a.tenantID }
func (a *ProductArgument) ProductID() uuid.UUID { return a.productID }
func (a *ProductArgument) Anchor() ScopeAnchor  { return a.anchor }
func (a *ProductArgument) Text() string         { return a.text }
func (a *ProductArgument) CreatedAt() time.Time { return a.createdAt }
func (a *ProductArgument) UpdatedAt() time.Time { return a.updatedAt }

// Rewrite updates the argument text. Returns ErrInvalidArgument when
// the new text is blank after trimming.
func (a *ProductArgument) Rewrite(text string, now time.Time) error {
	if strings.TrimSpace(text) == "" {
		return ErrInvalidArgument
	}
	a.text = text
	a.updatedAt = now
	return nil
}
