package usecase

import (
	"context"
	"errors"
	"strings"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/inbox"
)

// ListConversationSummaries is the read-side use case backing the inbox
// list pane (GET /inbox, SIN-64967). It validates the filter at the
// boundary, lists conversation projections through the read-model port,
// resolves the assigned-atendente label through the (optional) user
// directory, and derives the awaiting-reply indicator. No SQL lives
// here — the projection/filter logic is split between the use case
// (validation, derivation) and the adapter behind ConversationReadModel.
type ListConversationSummaries struct {
	read inbox.ConversationReadModel
	// dir is optional: when nil, AssignedUserLabel is left unresolved
	// (nil) on every item so the use case still works in deployments that
	// have not wired a directory adapter.
	dir inbox.UserDirectory
}

// ListConversationSummariesInput is the use-case argument.
type ListConversationSummariesInput struct {
	TenantID uuid.UUID
	// State filter: "" means both open and closed; "open"/"closed"
	// restrict. Any other value is rejected with ErrInvalidStatus.
	State string
	// Channel filter: "" means all carriers; a known carrier restricts.
	// The value is trimmed + lower-cased before validation; an unknown
	// carrier is rejected with ErrInvalidChannel.
	Channel string
	// AssignedUserID implements the "atribuídas a mim" filter: uuid.Nil
	// returns conversations regardless of assignee, a concrete id
	// restricts to that user. The handler sources the id from the
	// session, never from the request body.
	AssignedUserID uuid.UUID
	// Limit caps the page size; a zero or negative limit defaults to
	// defaultListLimit so the handler need not hardcode pagination policy.
	Limit int
}

// ListConversationSummariesResult is the use-case return. Items carry the
// snippet, last-message direction, AwaitingReply, and AssignedUserLabel
// the inbox list template renders.
type ListConversationSummariesResult struct {
	Items []ConversationView
}

// NewListConversationSummaries wires the use case. read is required; dir
// is optional (nil disables atendente-label resolution).
func NewListConversationSummaries(read inbox.ConversationReadModel, dir inbox.UserDirectory) (*ListConversationSummaries, error) {
	if read == nil {
		return nil, errors.New("inbox/usecase: read model must not be nil")
	}
	return &ListConversationSummaries{read: read, dir: dir}, nil
}

// MustNewListConversationSummaries is the panic-on-error variant for the
// composition root.
func MustNewListConversationSummaries(read inbox.ConversationReadModel, dir inbox.UserDirectory) *ListConversationSummaries {
	u, err := NewListConversationSummaries(read, dir)
	if err != nil {
		panic(err)
	}
	return u
}

// Execute runs the read pipeline: validate → list → resolve labels →
// project.
func (u *ListConversationSummaries) Execute(ctx context.Context, in ListConversationSummariesInput) (ListConversationSummariesResult, error) {
	if in.TenantID == uuid.Nil {
		return ListConversationSummariesResult{}, inbox.ErrInvalidTenant
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
		return ListConversationSummariesResult{}, inbox.ErrInvalidStatus
	}
	channel := strings.ToLower(strings.TrimSpace(in.Channel))
	if err := inbox.ValidateListChannel(channel); err != nil {
		return ListConversationSummariesResult{}, err
	}
	limit := in.Limit
	if limit <= 0 {
		limit = defaultListLimit
	}
	rows, err := u.read.ListConversationSummaries(ctx, in.TenantID, inbox.ConversationFilter{
		State:          state,
		Channel:        channel,
		AssignedUserID: in.AssignedUserID,
	}, limit)
	if err != nil {
		return ListConversationSummariesResult{}, err
	}
	labels, err := u.resolveLabels(ctx, in.TenantID, rows)
	if err != nil {
		return ListConversationSummariesResult{}, err
	}
	views := make([]ConversationView, 0, len(rows))
	for _, r := range rows {
		views = append(views, listItemToView(r, labels))
	}
	return ListConversationSummariesResult{Items: views}, nil
}

// resolveLabels collects the distinct assigned-user ids in rows and asks
// the directory for their labels in a single batched lookup (no N+1).
// Returns nil when no directory is wired or no row has an assignee.
func (u *ListConversationSummaries) resolveLabels(ctx context.Context, tenantID uuid.UUID, rows []inbox.ConversationListItem) (map[uuid.UUID]string, error) {
	if u.dir == nil {
		return nil, nil
	}
	seen := make(map[uuid.UUID]struct{})
	ids := make([]uuid.UUID, 0, len(rows))
	for _, r := range rows {
		if r.AssignedUserID == nil {
			continue
		}
		id := *r.AssignedUserID
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		return nil, nil
	}
	return u.dir.LabelsByID(ctx, tenantID, ids)
}
