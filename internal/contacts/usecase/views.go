package usecase

import (
	"sort"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/contacts"
)

// ContactIdentityView is the read-only projection of a single channel
// identity (e.g. ("whatsapp", "+5511999990001")) for the contacts
// management UI. It mirrors the inbox side panel's projection but lives
// here so the contacts web handler can consume contacts use-cases without
// importing the inbox use-case package or the contacts domain root.
type ContactIdentityView struct {
	Channel    string
	ExternalID string
}

// ContactSummaryView is the read-only projection of a Contact for the
// list pane and the edit result. It carries the editable fields plus the
// linked identities and the distinct set of channels (derived from the
// identities) so the list row can show a phone/email preview and a
// channel badge without a second read.
type ContactSummaryView struct {
	ID          uuid.UUID
	DisplayName string
	Identities  []ContactIdentityView
	// Channels is the sorted, de-duplicated set of channel names the
	// contact has an identity on (e.g. ["email", "whatsapp"]). Empty when
	// the contact has no identities.
	Channels  []string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// contactToSummary projects a domain Contact onto the read-only summary
// view. Defined in the use-case package so the web boundary never imports
// the contacts domain root.
func contactToSummary(c *contacts.Contact) ContactSummaryView {
	v := ContactSummaryView{
		ID:          c.ID,
		DisplayName: c.DisplayName,
		CreatedAt:   c.CreatedAt,
		UpdatedAt:   c.UpdatedAt,
	}
	ids := c.Identities()
	if len(ids) == 0 {
		return v
	}
	v.Identities = make([]ContactIdentityView, 0, len(ids))
	seen := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		v.Identities = append(v.Identities, ContactIdentityView{
			Channel:    id.Channel,
			ExternalID: id.ExternalID,
		})
		if _, ok := seen[id.Channel]; !ok {
			seen[id.Channel] = struct{}{}
			v.Channels = append(v.Channels, id.Channel)
		}
	}
	sort.Strings(v.Channels)
	return v
}
