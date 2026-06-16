package usecase

import (
	"context"
	"errors"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/contacts"
)

// UpdateContact is the write-side use case backing the contact edit form
// (SIN-64976). It loads the aggregate under the tenant scope, applies the
// editable field change through the aggregate's own mutator (Contact.Rename)
// so the non-empty-name invariant lives with the aggregate (DDD lens), and
// persists via Repository.Update. Channel identities are out of scope here —
// they follow the identity-split flow.
type UpdateContact struct {
	repo contacts.Repository
}

// UpdateContactInput is the use-case argument. DisplayName is the new name;
// boundary validation (trim + non-empty) is enforced by Contact.Rename.
type UpdateContactInput struct {
	TenantID    uuid.UUID
	ContactID   uuid.UUID
	DisplayName string
}

// UpdateContactResult wraps the post-update projection so the caller can
// re-render the edited row without a second read.
type UpdateContactResult struct {
	Contact ContactSummaryView
}

// NewUpdateContact wires the use case. Returns an error when repo is nil.
func NewUpdateContact(repo contacts.Repository) (*UpdateContact, error) {
	if repo == nil {
		return nil, errors.New("contacts/usecase: repository must not be nil")
	}
	return &UpdateContact{repo: repo}, nil
}

// MustNewUpdateContact is the panic-on-error variant for the composition root.
func MustNewUpdateContact(repo contacts.Repository) *UpdateContact {
	u, err := NewUpdateContact(repo)
	if err != nil {
		panic(err)
	}
	return u
}

// Execute runs the edit pipeline:
//
//  1. Validate the tenant / contact id at the boundary.
//  2. Load the aggregate under the tenant scope (ErrNotFound → 404).
//  3. Apply the rename through the aggregate (ErrEmptyDisplayName → 422).
//  4. Persist via Update (ErrNotFound if the row vanished between read and
//     write — a lost update, surfaced rather than masked).
//
// Returns the projected, post-update contact.
func (u *UpdateContact) Execute(ctx context.Context, in UpdateContactInput) (UpdateContactResult, error) {
	if in.TenantID == uuid.Nil {
		return UpdateContactResult{}, contacts.ErrInvalidTenant
	}
	if in.ContactID == uuid.Nil {
		return UpdateContactResult{}, contacts.ErrNotFound
	}

	c, err := u.repo.FindByID(ctx, in.TenantID, in.ContactID)
	if err != nil {
		return UpdateContactResult{}, err
	}
	if err := c.Rename(in.DisplayName); err != nil {
		return UpdateContactResult{}, err
	}
	if err := u.repo.Update(ctx, c); err != nil {
		return UpdateContactResult{}, err
	}
	return UpdateContactResult{Contact: contactToSummary(c)}, nil
}
