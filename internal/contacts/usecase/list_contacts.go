package usecase

import (
	"context"
	"errors"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/contacts"
)

// ListContacts is the read-side use case backing the contacts list/search
// pane (SIN-64976). It owns the "tenant required" rule, hands the search
// term + pagination to the Repository as a domain ListFilter, and projects
// the returned aggregates onto the read-only ContactSummaryView the HTMX
// handler renders. Pagination policy (limit default/clamp) lives in the
// ListFilter value object, so neither the handler nor the adapter hard-codes
// it.
type ListContacts struct {
	repo contacts.Repository
}

// ListContactsInput is the use-case argument. Query is the free-text
// search term (matched against name + identity external ids); Limit/Offset
// drive offset pagination. A zero/negative Limit defaults via ListFilter.
type ListContactsInput struct {
	TenantID uuid.UUID
	Query    string
	Limit    int
	Offset   int
}

// ListContactsResult is the use-case return: the projected page plus the
// total matching count and the effective limit/offset (post-normalisation)
// so the handler can render pager controls without re-deriving them.
type ListContactsResult struct {
	Items  []ContactSummaryView
	Total  int
	Limit  int
	Offset int
}

// NewListContacts wires the use case. Returns an error when repo is nil.
func NewListContacts(repo contacts.Repository) (*ListContacts, error) {
	if repo == nil {
		return nil, errors.New("contacts/usecase: repository must not be nil")
	}
	return &ListContacts{repo: repo}, nil
}

// MustNewListContacts is the panic-on-error variant for the composition root.
func MustNewListContacts(repo contacts.Repository) *ListContacts {
	u, err := NewListContacts(repo)
	if err != nil {
		panic(err)
	}
	return u
}

// Execute runs the list pipeline. ErrInvalidTenant when TenantID is nil.
// Any storage error is propagated unwrapped-by-Is so the caller can map a
// genuine failure to 500.
func (u *ListContacts) Execute(ctx context.Context, in ListContactsInput) (ListContactsResult, error) {
	if in.TenantID == uuid.Nil {
		return ListContactsResult{}, contacts.ErrInvalidTenant
	}
	filter := contacts.ListFilter{
		Query:  in.Query,
		Limit:  in.Limit,
		Offset: in.Offset,
	}.Normalized()

	rows, total, err := u.repo.List(ctx, in.TenantID, filter)
	if err != nil {
		return ListContactsResult{}, err
	}
	views := make([]ContactSummaryView, 0, len(rows))
	for _, c := range rows {
		views = append(views, contactToSummary(c))
	}
	return ListContactsResult{
		Items:  views,
		Total:  total,
		Limit:  filter.Limit,
		Offset: filter.Offset,
	}, nil
}
