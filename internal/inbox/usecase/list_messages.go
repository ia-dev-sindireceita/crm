package usecase

import (
	"context"
	"errors"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/inbox"
)

// ListMessages is the read-side use case that backs the conversation
// view pane (`GET /inbox/conversations/:id`) — it returns the messages
// of a given conversation under the tenant scope, oldest-first.
type ListMessages struct {
	repo inbox.Repository
}

// ListMessagesInput is the use-case argument.
type ListMessagesInput struct {
	TenantID       uuid.UUID
	ConversationID uuid.UUID
}

// ListMessagesResult is the use-case return.
type ListMessagesResult struct {
	Items []MessageView
}

// NewListMessages wires the use case. Returns an error when repo is nil.
func NewListMessages(repo inbox.Repository) (*ListMessages, error) {
	if repo == nil {
		return nil, errors.New("inbox/usecase: repo must not be nil")
	}
	return &ListMessages{repo: repo}, nil
}

// MustNewListMessages is the panic-on-error variant for the composition root.
func MustNewListMessages(repo inbox.Repository) *ListMessages {
	u, err := NewListMessages(repo)
	if err != nil {
		panic(err)
	}
	return u
}

// Execute runs the list pipeline.
func (u *ListMessages) Execute(ctx context.Context, in ListMessagesInput) (ListMessagesResult, error) {
	if in.TenantID == uuid.Nil {
		return ListMessagesResult{}, inbox.ErrInvalidTenant
	}
	if in.ConversationID == uuid.Nil {
		return ListMessagesResult{}, inbox.ErrNotFound
	}
	rows, err := u.repo.ListMessages(ctx, in.TenantID, in.ConversationID)
	if err != nil {
		return ListMessagesResult{}, err
	}
	views := make([]MessageView, 0, len(rows))
	for _, m := range rows {
		views = append(views, messageToView(m))
	}
	return ListMessagesResult{Items: views}, nil
}
