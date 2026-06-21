package usecase

import (
	"context"
	"errors"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/inbox"
)

// ListMessagesSince is the incremental read-side use case backing the
// conversation thread's live-refresh poll (SIN-65419). The open
// conversation pane polls GET /inbox/conversations/{id}/messages/since
// every few seconds; this use case returns only the messages created
// after the caller's cursor so the handler can append the new inbound
// (auto-reply) bubbles without a full reload.
//
// It deliberately reuses the existing inbox.Repository.ListMessages port
// (no new SQL, no migration) and filters in-process. ListMessages is
// already O(N) and runs on every conversation view; the poll's footprint
// matches it. A pushed-down `created_at > $cursor` query is a possible
// efficiency follow-up but is not needed for v1's bounded threads.
type ListMessagesSince struct {
	repo inbox.Repository
}

// ListMessagesSinceInput is the use-case argument. AfterUnixNano is the
// exclusive cursor: only messages whose CreatedAt is strictly newer are
// returned. A zero (or negative) cursor means "the caller's thread is
// empty" — every message is returned, which is the correct first-fill
// for a conversation that rendered with no messages. The web handler
// guarantees this invariant: a non-empty initial thread always renders a
// non-zero cursor (the last message's CreatedAt), and a successful send
// advances the cursor past the just-sent message, so a zero cursor can
// only ever pair with an empty client thread (no duplicate-bubble risk).
type ListMessagesSinceInput struct {
	TenantID       uuid.UUID
	ConversationID uuid.UUID
	AfterUnixNano  int64
}

// ListMessagesSinceResult carries the new messages, oldest-first (same
// order as ListMessages) so the handler appends them to the thread in
// chronological order.
type ListMessagesSinceResult struct {
	Items []MessageView
}

// NewListMessagesSince wires the use case. Returns an error when repo is
// nil.
func NewListMessagesSince(repo inbox.Repository) (*ListMessagesSince, error) {
	if repo == nil {
		return nil, errors.New("inbox/usecase: repo must not be nil")
	}
	return &ListMessagesSince{repo: repo}, nil
}

// MustNewListMessagesSince is the panic-on-error variant for the
// composition root.
func MustNewListMessagesSince(repo inbox.Repository) *ListMessagesSince {
	u, err := NewListMessagesSince(repo)
	if err != nil {
		panic(err)
	}
	return u
}

// Execute runs the incremental list. The tenant/conversation validation
// mirrors ListMessages so the handler maps the same sentinels (invalid
// tenant → 500, missing conversation → 404). The cursor filter is
// strict-greater so re-polling with the same cursor returns nothing (the
// handler then answers 204 No Content — never 304, which htmx would swap
// and delete the thread; see SIN-65393).
func (u *ListMessagesSince) Execute(ctx context.Context, in ListMessagesSinceInput) (ListMessagesSinceResult, error) {
	if in.TenantID == uuid.Nil {
		return ListMessagesSinceResult{}, inbox.ErrInvalidTenant
	}
	if in.ConversationID == uuid.Nil {
		return ListMessagesSinceResult{}, inbox.ErrNotFound
	}
	rows, err := u.repo.ListMessages(ctx, in.TenantID, in.ConversationID)
	if err != nil {
		return ListMessagesSinceResult{}, err
	}
	views := make([]MessageView, 0, len(rows))
	for _, m := range rows {
		// AfterUnixNano <= 0 means the client thread is empty → return
		// everything (first fill). Otherwise keep only messages strictly
		// newer than the cursor so an unchanged poll yields nothing.
		if in.AfterUnixNano > 0 && m.CreatedAt.UnixNano() <= in.AfterUnixNano {
			continue
		}
		views = append(views, messageToView(m))
	}
	return ListMessagesSinceResult{Items: views}, nil
}
