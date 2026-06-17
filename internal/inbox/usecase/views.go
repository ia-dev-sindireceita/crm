package usecase

import (
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/inbox"
)

// ErrNotFound is the re-exported "not found" sentinel for callers that
// must not import the inbox domain root directly (web/inbox is
// forbidden from importing internal/inbox per forbidwebboundary). It
// aliases inbox.ErrNotFound so errors.Is matches both spellings.
var ErrNotFound = inbox.ErrNotFound

// ErrConversationClosed re-exports inbox.ErrConversationClosed for the
// same reason — keeps the handler's import surface to the use-case
// package only.
var ErrConversationClosed = inbox.ErrConversationClosed

// ErrAlreadyAssigned re-exports inbox.ErrAlreadyAssigned so the
// web/inbox handler can check for the no-op idempotent assign path
// without importing the domain root.
var ErrAlreadyAssigned = inbox.ErrAlreadyAssigned

// ErrUserNotAssignable re-exports inbox.ErrUserNotAssignable for the
// same reason — the assign handler maps it to 403.
var ErrUserNotAssignable = inbox.ErrUserNotAssignable

// ConversationView is the read-only projection of an inbox.Conversation
// suitable for the HTMX inbox UI. It exists so the web/inbox handler
// package can consume conversation data without importing the domain
// root (and tripping the forbidwebboundary lint).
type ConversationView struct {
	ID             uuid.UUID
	ContactID      uuid.UUID
	Channel        string
	State          string
	AssignedUserID *uuid.UUID
	LastMessageAt  time.Time
	CreatedAt      time.Time

	// --- read-model enrichments (SIN-64967, GET /inbox list pane) ---

	// ContactDisplayName is the primary row label (the contact's name),
	// resolved adapter-side in the main listing query. Never empty: the
	// adapter falls back to the channel identifier, then the contact id,
	// so the template always has a human-renderable label instead of a
	// bare UUID. Populated only by the ListConversationSummaries use case.
	ContactDisplayName string
	// LastMessageSnippet is the truncated, whitespace-collapsed body of
	// the most recent message (server-side capped at inbox.SnippetMaxChars).
	// Empty when the conversation has no messages yet. Populated only by
	// the ListConversationSummaries use case; the legacy ListConversations
	// path leaves it empty.
	LastMessageSnippet string
	// LastMessageDirection is the direction of the most recent message
	// ("in"/"out"); empty when there are no messages.
	LastMessageDirection string
	// AwaitingReply is true when the last message was inbound ("in"),
	// i.e. the contact is waiting for the tenant to respond. Derived from
	// LastMessageDirection; no read-receipt schema is involved (CTO
	// decision: a real unread counter is a follow-up).
	AwaitingReply bool
	// AssignedUserLabel is the human label for AssignedUserID (resolved
	// via the UserDirectory port). nil when the conversation is
	// unassigned, when no directory is wired, or when the user row could
	// not be resolved under the tenant scope.
	AssignedUserLabel *string
}

// MessageView is the read-only projection of an inbox.Message used by
// the HTMX inbox UI. Direction and Status are exposed as strings so the
// templates can switch on the value without importing the domain
// enums.
//
// Media is the optional attachment projection. The bubble template
// renders nothing when Media is nil, the safe-to-render attachment
// when Media.ScanStatus is "clean", and a "blocked by security"
// placeholder when Media.ScanStatus is "infected" — the infected
// storage key is intentionally NOT exposed to the UI so a curious
// operator cannot deep-link to a quarantined payload via the network
// tab ([SIN-62805] F2-05d AC: "Sem expor a key infectada").
type MessageView struct {
	ID                uuid.UUID
	ConversationID    uuid.UUID
	Direction         string
	Body              string
	Status            string
	ChannelExternalID string
	SentByUserID      *uuid.UUID
	CreatedAt         time.Time
	Media             *MessageMediaView
}

// MessageMediaView is the closed projection of `message.media -> scan_*`
// fields the inbox UI reads. The template never receives the storage
// key when ScanStatus is anything but "clean"; the projector below
// drops it to honour the "Sem expor a key infectada" AC.
type MessageMediaView struct {
	// Hash is the content-addressed identifier used by the static
	// origin to serve the blob (`GET /t/{tenantID}/m/{hash}`). Empty
	// when ScanStatus != "clean" so the template cannot link to an
	// unsafe object.
	Hash string
	// Format is the closed Format enum value (e.g. "png", "pdf"). The
	// template uses it to pick the icon / Content-Disposition hint.
	Format string
	// ScanStatus is one of "pending", "clean", "infected". The
	// template branches on this directly.
	ScanStatus string
}

// conversationToView projects an inbox.Conversation onto the read-only
// view shape. Defined inside the usecase package so the domain root
// stays out of the import path of any handler that consumes the view.
func conversationToView(c *inbox.Conversation) ConversationView {
	return ConversationView{
		ID:             c.ID,
		ContactID:      c.ContactID,
		Channel:        c.Channel,
		State:          string(c.State),
		AssignedUserID: c.AssignedUserID,
		LastMessageAt:  c.LastMessageAt,
		CreatedAt:      c.CreatedAt,
	}
}

// listItemToView projects a read-model inbox.ConversationListItem
// (SIN-64967) onto ConversationView. It derives AwaitingReply (the last
// message is inbound and therefore unanswered) and attaches the
// assigned-atendente label the use case resolved through the
// UserDirectory port. labels may be nil (no directory wired / no
// assignees), in which case AssignedUserLabel is left nil.
func listItemToView(item inbox.ConversationListItem, labels map[uuid.UUID]string) ConversationView {
	v := ConversationView{
		ID:                   item.ID,
		ContactID:            item.ContactID,
		Channel:              item.Channel,
		State:                string(item.State),
		AssignedUserID:       item.AssignedUserID,
		LastMessageAt:        item.LastMessageAt,
		CreatedAt:            item.CreatedAt,
		ContactDisplayName:   item.ContactDisplayName,
		LastMessageSnippet:   item.LastMessageSnippet,
		LastMessageDirection: string(item.LastMessageDirection),
		AwaitingReply:        item.LastMessageDirection == inbox.MessageDirectionIn,
	}
	if item.AssignedUserID != nil && labels != nil {
		if label, ok := labels[*item.AssignedUserID]; ok && label != "" {
			l := label
			v.AssignedUserLabel = &l
		}
	}
	return v
}

// messageToView projects an inbox.Message onto the read-only view shape.
//
// MessageMedia is projected when the domain entity carries a non-nil
// Media block (i.e. the row had a non-null `message.media` jsonb
// payload). The projector drops the content-addressed Hash whenever
// ScanStatus is anything but "clean" — the inbox UI must never render
// a deep link to a pending-or-infected payload, and centralising the
// rule here keeps it out of every template branch.
func messageToView(m *inbox.Message) MessageView {
	v := MessageView{
		ID:                m.ID,
		ConversationID:    m.ConversationID,
		Direction:         string(m.Direction),
		Body:              m.Body,
		Status:            string(m.Status),
		ChannelExternalID: m.ChannelExternalID,
		SentByUserID:      m.SentByUserID,
		CreatedAt:         m.CreatedAt,
	}
	if m.Media != nil {
		hash := m.Media.Hash
		if m.Media.ScanStatus != "clean" {
			hash = ""
		}
		v.Media = &MessageMediaView{
			Hash:       hash,
			Format:     m.Media.Format,
			ScanStatus: m.Media.ScanStatus,
		}
	}
	return v
}
