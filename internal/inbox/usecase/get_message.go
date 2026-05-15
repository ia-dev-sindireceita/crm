package usecase

import (
	"context"
	"errors"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/inbox"
)

// GetMessage is the read-side use case that backs the realtime message
// status partial (SIN-62736). The HTMX bubble polls
// GET /inbox/conversations/:id/messages/:msgID/status every few seconds
// while the message is in a non-final state; the handler routes through
// this use case so the web boundary keeps its hexagonal hygiene
// (forbidwebboundary lint, no domain root imports from internal/web).
type GetMessage struct {
	repo inbox.Repository
}

// GetMessageInput is the use-case argument.
type GetMessageInput struct {
	TenantID       uuid.UUID
	ConversationID uuid.UUID
	MessageID      uuid.UUID
}

// GetMessageResult is the use-case return — a projected MessageView
// suitable for the message_bubble template.
type GetMessageResult struct {
	Message MessageView
}

// NewGetMessage wires the use case. Returns an error when repo is nil.
func NewGetMessage(repo inbox.Repository) (*GetMessage, error) {
	if repo == nil {
		return nil, errors.New("inbox/usecase: repo must not be nil")
	}
	return &GetMessage{repo: repo}, nil
}

// MustNewGetMessage is the panic-on-error variant for the composition root.
func MustNewGetMessage(repo inbox.Repository) *GetMessage {
	u, err := NewGetMessage(repo)
	if err != nil {
		panic(err)
	}
	return u
}

// Execute runs the lookup pipeline. ErrInvalidTenant when TenantID is
// nil; ErrNotFound when the conversation/message pair is missing under
// the tenant scope.
func (u *GetMessage) Execute(ctx context.Context, in GetMessageInput) (GetMessageResult, error) {
	if in.TenantID == uuid.Nil {
		return GetMessageResult{}, inbox.ErrInvalidTenant
	}
	if in.ConversationID == uuid.Nil || in.MessageID == uuid.Nil {
		return GetMessageResult{}, inbox.ErrNotFound
	}
	m, err := u.repo.GetMessage(ctx, in.TenantID, in.ConversationID, in.MessageID)
	if err != nil {
		return GetMessageResult{}, err
	}
	return GetMessageResult{Message: messageToView(m)}, nil
}
