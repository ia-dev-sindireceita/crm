package usecase

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/contacts"
	"github.com/pericles-luz/crm/internal/inbox"
)

// defaultDetailConversationLimit caps the conversation-history list on the
// contact detail view. The panel shows the most recent threads; deep
// history is out of scope for the detail read.
const defaultDetailConversationLimit = 50

// conversationHistoryReader is the narrow read port for a contact's
// conversation history. It is satisfied structurally by the postgres inbox
// Store's ListConversationsByContact (added alongside this use case; NOT
// part of the inbox.Repository port — see the adapter doc-comment). The
// reader is optional: a deployment that has not wired inbox storage passes
// nil and the detail view degrades to an empty history rather than failing
// the whole read.
type conversationHistoryReader interface {
	ListConversationsByContact(ctx context.Context, tenantID, contactID uuid.UUID, limit int) ([]*inbox.Conversation, error)
}

// ConversationSummaryView is the read-only projection of one conversation
// thread for the contact detail history list. The contact id is omitted —
// every row belongs to the contact being viewed.
type ConversationSummaryView struct {
	ID             uuid.UUID
	Channel        string
	State          string
	Assigned       bool
	AssignedUserID *uuid.UUID
	LastMessageAt  time.Time
	CreatedAt      time.Time
}

// ContactDetailView is the read-only projection backing the contact detail
// surface: the editable header fields, the linked channel identities, the
// derived channel set, and the recent conversation history.
type ContactDetailView struct {
	ID            uuid.UUID
	DisplayName   string
	Identities    []ContactIdentityView
	Channels      []string
	CreatedAt     time.Time
	UpdatedAt     time.Time
	Conversations []ConversationSummaryView
}

// GetContactDetail is the read-side use case backing the contact detail
// view (SIN-64976): it gathers the contact aggregate, its channel
// identities, and its recent conversation history in one call. The contact
// read is required; the conversation reader is optional and degrades to an
// empty history when nil.
type GetContactDetail struct {
	repo          contacts.Repository
	conversations conversationHistoryReader
}

// GetContactDetailInput is the use-case argument.
type GetContactDetailInput struct {
	TenantID  uuid.UUID
	ContactID uuid.UUID
	// ConversationLimit caps the history list; zero/negative defaults to
	// defaultDetailConversationLimit.
	ConversationLimit int
}

// GetContactDetailResult wraps the projected detail view.
type GetContactDetailResult struct {
	Contact ContactDetailView
}

// NewGetContactDetail wires the use case. repo is required; conversations
// is optional (nil → empty history). Returns an error when repo is nil.
func NewGetContactDetail(repo contacts.Repository, conversations conversationHistoryReader) (*GetContactDetail, error) {
	if repo == nil {
		return nil, errors.New("contacts/usecase: repository must not be nil")
	}
	return &GetContactDetail{repo: repo, conversations: conversations}, nil
}

// MustNewGetContactDetail is the panic-on-error variant for the composition
// root.
func MustNewGetContactDetail(repo contacts.Repository, conversations conversationHistoryReader) *GetContactDetail {
	u, err := NewGetContactDetail(repo, conversations)
	if err != nil {
		panic(err)
	}
	return u
}

// Execute runs the detail read. ErrInvalidTenant when TenantID is nil;
// contacts.ErrNotFound when the contact is missing under the tenant scope
// (the caller maps that to 404). A genuine conversation-history storage
// error is propagated so it is not silently masked as "no history".
func (u *GetContactDetail) Execute(ctx context.Context, in GetContactDetailInput) (GetContactDetailResult, error) {
	if in.TenantID == uuid.Nil {
		return GetContactDetailResult{}, contacts.ErrInvalidTenant
	}
	if in.ContactID == uuid.Nil {
		return GetContactDetailResult{}, contacts.ErrNotFound
	}

	c, err := u.repo.FindByID(ctx, in.TenantID, in.ContactID)
	if err != nil {
		return GetContactDetailResult{}, err
	}

	summary := contactToSummary(c)
	view := ContactDetailView{
		ID:          summary.ID,
		DisplayName: summary.DisplayName,
		Identities:  summary.Identities,
		Channels:    summary.Channels,
		CreatedAt:   summary.CreatedAt,
		UpdatedAt:   summary.UpdatedAt,
	}

	if u.conversations != nil {
		limit := in.ConversationLimit
		if limit <= 0 {
			limit = defaultDetailConversationLimit
		}
		convs, err := u.conversations.ListConversationsByContact(ctx, in.TenantID, in.ContactID, limit)
		if err != nil {
			return GetContactDetailResult{}, err
		}
		if len(convs) > 0 {
			view.Conversations = make([]ConversationSummaryView, 0, len(convs))
			for _, conv := range convs {
				view.Conversations = append(view.Conversations, conversationToSummary(conv))
			}
		}
	}

	return GetContactDetailResult{Contact: view}, nil
}

// conversationToSummary projects an inbox.Conversation onto the read-only
// history-row view. Kept here (not in the inbox package) so the contacts
// detail view owns its own projection shape.
func conversationToSummary(c *inbox.Conversation) ConversationSummaryView {
	return ConversationSummaryView{
		ID:             c.ID,
		Channel:        c.Channel,
		State:          string(c.State),
		Assigned:       c.AssignedUserID != nil,
		AssignedUserID: c.AssignedUserID,
		LastMessageAt:  c.LastMessageAt,
		CreatedAt:      c.CreatedAt,
	}
}
