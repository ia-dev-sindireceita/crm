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
}

// MessageView is the read-only projection of an inbox.Message used by
// the HTMX inbox UI. Direction and Status are exposed as strings so the
// templates can switch on the value without importing the domain
// enums.
type MessageView struct {
	ID                uuid.UUID
	ConversationID    uuid.UUID
	Direction         string
	Body              string
	Status            string
	ChannelExternalID string
	SentByUserID      *uuid.UUID
	CreatedAt         time.Time
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

// messageToView projects an inbox.Message onto the read-only view shape.
func messageToView(m *inbox.Message) MessageView {
	return MessageView{
		ID:                m.ID,
		ConversationID:    m.ConversationID,
		Direction:         string(m.Direction),
		Body:              m.Body,
		Status:            string(m.Status),
		ChannelExternalID: m.ChannelExternalID,
		SentByUserID:      m.SentByUserID,
		CreatedAt:         m.CreatedAt,
	}
}
