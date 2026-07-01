package channels

import (
	"strings"
	"time"

	"github.com/google/uuid"
)

// Channel is the aggregate root for a concrete channel instance a tenant
// operates. Identity is the UUID; equality is by ID. Tenant ownership and
// the addressing tuple (ChannelKey, ExternalID) are fixed at
// construction — a channel cannot move between tenants or be re-addressed
// once created (re-addressing means a new channel instance). Only the
// display name, active flag, and restricted flag are mutable, each
// through its own method so the invariants live with the aggregate.
//
//   - ChannelKey  is the channel family ('whatsapp', 'instagram', …),
//     case-folded to lower.
//   - ExternalID  is the address within that family (the number/handle).
//     It may be the empty string for the legacy backfilled placeholder
//     instance (see migration 0128) before a real number is attached.
//   - Restricted, when true, signals that access is limited to the users
//     explicitly granted via channel_access rather than open to the
//     whole tenant. The enforcement lives in ChannelAccessPolicy; the
//     flag is the policy input.
type Channel struct {
	ID          uuid.UUID
	TenantID    uuid.UUID
	ChannelKey  string
	ExternalID  string
	DisplayName string
	IsActive    bool
	Restricted  bool
	CreatedAt   time.Time
}

// now is overridable so unit tests can pin timestamps.
var now = func() time.Time { return time.Now().UTC() }

// New builds a fresh, active, unrestricted Channel. tenantID MUST be
// non-Nil; channelKey MUST be non-empty after trimming and is
// lower-cased. externalID is trimmed (empty is allowed — it is the
// legacy/placeholder address). displayName is trimmed; when blank it
// defaults to the channel key so the picker always has something to
// render. The channel gets a freshly generated UUID and CreatedAt =
// now().
func New(tenantID uuid.UUID, channelKey, externalID, displayName string) (*Channel, error) {
	if tenantID == uuid.Nil {
		return nil, ErrInvalidTenant
	}
	channelKey = strings.ToLower(strings.TrimSpace(channelKey))
	if channelKey == "" {
		return nil, ErrInvalidChannelKey
	}
	externalID = strings.TrimSpace(externalID)
	displayName = strings.TrimSpace(displayName)
	if displayName == "" {
		displayName = channelKey
	}
	return &Channel{
		ID:          uuid.New(),
		TenantID:    tenantID,
		ChannelKey:  channelKey,
		ExternalID:  externalID,
		DisplayName: displayName,
		IsActive:    true,
		Restricted:  false,
		CreatedAt:   now(),
	}, nil
}

// Hydrate rebuilds a Channel from stored fields without re-running the
// constructor's invariants. Adapter code uses it to materialise rows
// read from Postgres. Domain code MUST use New.
func Hydrate(id, tenantID uuid.UUID, channelKey, externalID, displayName string, isActive, restricted bool, createdAt time.Time) *Channel {
	return &Channel{
		ID:          id,
		TenantID:    tenantID,
		ChannelKey:  channelKey,
		ExternalID:  externalID,
		DisplayName: displayName,
		IsActive:    isActive,
		Restricted:  restricted,
		CreatedAt:   createdAt,
	}
}

// Rename changes the channel's editable display name. It enforces the
// same non-empty invariant as the New constructor so an edit can never
// blank out the channel picker. A rename to the current name is a no-op.
// Returns ErrEmptyDisplayName on a blank name, leaving the aggregate
// untouched.
func (c *Channel) Rename(displayName string) error {
	displayName = strings.TrimSpace(displayName)
	if displayName == "" {
		return ErrEmptyDisplayName
	}
	c.DisplayName = displayName
	return nil
}

// SetActive toggles whether the channel instance is live. Deactivating a
// channel keeps its history and grants intact (deactivate-not-delete);
// the inbox simply stops surfacing it as a sendable channel.
func (c *Channel) SetActive(active bool) {
	c.IsActive = active
}

// SetRestricted toggles whether the channel is limited to explicitly
// granted users. The flag is advisory data for ChannelAccessPolicy; the
// aggregate does not enforce authorization itself.
func (c *Channel) SetRestricted(restricted bool) {
	c.Restricted = restricted
}
