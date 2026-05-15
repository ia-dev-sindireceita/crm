package usecase

import (
	"context"
	"errors"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/inbox"
)

// ListConversations is the read-side use case that backs `GET /inbox` —
// it lists the tenant's conversations newest-message-first for the HTMX
// inbox list pane. The use case is intentionally tiny: it owns the
// "tenant required" rule and projects the domain aggregate onto the
// ConversationView the handler renders. Storage details (state filter,
// limit clamp) live in the Repository adapter.
type ListConversations struct {
	repo inbox.Repository
}

// ListConversationsInput is the use-case argument. Limit caps the page
// size; a zero or negative limit defaults to defaultListLimit so the
// handler does not need to hardcode pagination policy.
type ListConversationsInput struct {
	TenantID uuid.UUID
	// State filter: "" means both open and closed; "open" or "closed"
	// restricts. The handler exposes this as a query param later (PR10);
	// for PR9 the handler passes "open".
	State string
	Limit int
}

// ListConversationsResult is the use-case return.
type ListConversationsResult struct {
	Items []ConversationView
}

// defaultListLimit caps the inbox list page at a reasonable value for
// the read-side use case. PR10 will replace this with explicit
// pagination + cursor; for PR9 the operator sees the 50 newest threads.
const defaultListLimit = 50

// NewListConversations wires the use case. Returns an error when repo is nil.
func NewListConversations(repo inbox.Repository) (*ListConversations, error) {
	if repo == nil {
		return nil, errors.New("inbox/usecase: repo must not be nil")
	}
	return &ListConversations{repo: repo}, nil
}

// MustNewListConversations is the panic-on-error variant for the composition root.
func MustNewListConversations(repo inbox.Repository) *ListConversations {
	u, err := NewListConversations(repo)
	if err != nil {
		panic(err)
	}
	return u
}

// Execute runs the list pipeline.
func (u *ListConversations) Execute(ctx context.Context, in ListConversationsInput) (ListConversationsResult, error) {
	if in.TenantID == uuid.Nil {
		return ListConversationsResult{}, inbox.ErrInvalidTenant
	}
	limit := in.Limit
	if limit <= 0 {
		limit = defaultListLimit
	}
	state := inbox.ConversationState("")
	switch in.State {
	case "":
		// no filter
	case string(inbox.ConversationStateOpen):
		state = inbox.ConversationStateOpen
	case string(inbox.ConversationStateClosed):
		state = inbox.ConversationStateClosed
	default:
		return ListConversationsResult{}, inbox.ErrInvalidStatus
	}
	rows, err := u.repo.ListConversations(ctx, in.TenantID, state, limit)
	if err != nil {
		return ListConversationsResult{}, err
	}
	views := make([]ConversationView, 0, len(rows))
	for _, c := range rows {
		views = append(views, conversationToView(c))
	}
	return ListConversationsResult{Items: views}, nil
}
