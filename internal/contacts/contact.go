package contacts

import (
	"strings"
	"time"

	"github.com/google/uuid"
)

// Contact is the aggregate root for a person/account record in the
// inbox. Identity is the UUID; equality is by ID. Tenant ownership is
// fixed at construction — a contact cannot move between tenants once
// created.
//
// The aggregate carries its ChannelIdentity set as a private slice
// (see Identities() for a defensive copy) because the invariant "at
// most one identity per channel" is enforced via AddChannelIdentity and
// must not be bypassed by direct slice manipulation.
type Contact struct {
	ID          uuid.UUID
	TenantID    uuid.UUID
	DisplayName string
	CreatedAt   time.Time
	UpdatedAt   time.Time

	identities []ChannelIdentity
}

// now is overridable so unit tests can pin timestamps.
var now = func() time.Time { return time.Now().UTC() }

// New builds a fresh Contact. tenantID MUST be non-Nil; displayName MUST
// be non-empty after trimming. The contact has a freshly generated UUID,
// no channel identities, and CreatedAt == UpdatedAt == now().
func New(tenantID uuid.UUID, displayName string) (*Contact, error) {
	if tenantID == uuid.Nil {
		return nil, ErrInvalidTenant
	}
	displayName = strings.TrimSpace(displayName)
	if displayName == "" {
		return nil, ErrEmptyDisplayName
	}
	t := now()
	return &Contact{
		ID:          uuid.New(),
		TenantID:    tenantID,
		DisplayName: displayName,
		CreatedAt:   t,
		UpdatedAt:   t,
	}, nil
}

// Hydrate rebuilds a Contact from stored fields without re-running the
// constructor's invariants. Adapter code uses it to materialise rows
// read from Postgres. Domain code MUST use New + AddChannelIdentity.
func Hydrate(id, tenantID uuid.UUID, displayName string, identities []ChannelIdentity, createdAt, updatedAt time.Time) *Contact {
	c := &Contact{
		ID:          id,
		TenantID:    tenantID,
		DisplayName: displayName,
		CreatedAt:   createdAt,
		UpdatedAt:   updatedAt,
	}
	if len(identities) > 0 {
		c.identities = append(c.identities, identities...)
	}
	return c
}

// AddChannelIdentity validates the (channel, externalID) pair via
// NewChannelIdentity and attaches it to the contact. The invariant "at
// most one identity per channel per contact" is enforced here:
// ErrChannelIdentityExists is returned if the contact already has any
// identity on the same channel.
func (c *Contact) AddChannelIdentity(channel, externalID string) error {
	id, err := NewChannelIdentity(channel, externalID)
	if err != nil {
		return err
	}
	for _, existing := range c.identities {
		if existing.Channel == id.Channel {
			return ErrChannelIdentityExists
		}
	}
	c.identities = append(c.identities, id)
	c.UpdatedAt = now()
	return nil
}

// Identities returns a defensive copy of the contact's identities.
// Callers that mutate the returned slice cannot corrupt the aggregate.
func (c *Contact) Identities() []ChannelIdentity {
	if len(c.identities) == 0 {
		return nil
	}
	out := make([]ChannelIdentity, len(c.identities))
	copy(out, c.identities)
	return out
}

// HasChannel reports whether the contact carries an identity on the
// named channel. Channel name is matched case-insensitively to mirror
// NewChannelIdentity's normalisation.
func (c *Contact) HasChannel(channel string) bool {
	channel = strings.ToLower(strings.TrimSpace(channel))
	for _, existing := range c.identities {
		if existing.Channel == channel {
			return true
		}
	}
	return false
}
