// identity_split.go — F2-13 (SIN-62799) read + write use cases backing
// the HTMX identity-split UI in internal/web/contacts.
//
// LoadIdentityForContact is the GET /contacts/{contactID} read side: it
// returns the current Identity for the contact plus all sibling
// IdentityLinks so the panel can render one row per merged contact.
//
// SplitIdentityLink is the POST /contacts/identity/split write side: it
// detaches a single contact_identity_link (creating a fresh Identity for
// the orphan contact) and returns the post-split Identity for the link's
// remaining contact so the HTMX swap re-renders the panel without a
// follow-up GET. Conversations migrate implicitly: conversation.contact_id
// keys conversations to the contact, not the identity, so detaching the
// link is enough to route future inbox lookups through the new identity.

package usecase

import (
	"context"
	"errors"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/contacts"
)

// IdentitySplitRepository is the storage surface the F2-13 use cases
// depend on. Declared here (not in /internal/contacts) so the
// composition root can plug in any adapter that satisfies it — in
// practice today, internal/adapter/db/postgres/contacts.IdentityStore.
type IdentitySplitRepository interface {
	FindByContactID(ctx context.Context, tenantID, contactID uuid.UUID) (*contacts.Identity, error)
	Split(ctx context.Context, tenantID, linkID uuid.UUID) error
}

// LoadIdentityForContact resolves the merged Identity tied to a contact
// id. The handler renders one row per IdentityLink (one per sibling
// contact on the same identity) with link_reason + timestamp.
type LoadIdentityForContact struct {
	repo IdentitySplitRepository
}

// NewLoadIdentityForContact wires the read use case.
func NewLoadIdentityForContact(repo IdentitySplitRepository) (*LoadIdentityForContact, error) {
	if repo == nil {
		return nil, errors.New("contacts/usecase: identity repository must not be nil")
	}
	return &LoadIdentityForContact{repo: repo}, nil
}

// LoadIdentityInput names the (tenant, contact) pair to look up.
type LoadIdentityInput struct {
	TenantID  uuid.UUID
	ContactID uuid.UUID
}

// LoadIdentityResult carries the hydrated identity.
type LoadIdentityResult struct {
	Identity *contacts.Identity
}

// Execute returns the identity for in.ContactID. ErrNotFound surfaces
// unchanged so the handler can render 404 cleanly.
func (u *LoadIdentityForContact) Execute(ctx context.Context, in LoadIdentityInput) (LoadIdentityResult, error) {
	if in.TenantID == uuid.Nil {
		return LoadIdentityResult{}, errors.New("contacts/usecase: tenant id must not be nil")
	}
	if in.ContactID == uuid.Nil {
		return LoadIdentityResult{}, errors.New("contacts/usecase: contact id must not be nil")
	}
	identity, err := u.repo.FindByContactID(ctx, in.TenantID, in.ContactID)
	if err != nil {
		return LoadIdentityResult{}, err
	}
	return LoadIdentityResult{Identity: identity}, nil
}

// SplitIdentityLink detaches the chosen IdentityLink (creating a new
// Identity for the orphan contact) and returns the survivor's updated
// Identity so the handler can render the post-split fragment.
//
// The surviving panel is keyed by SurvivorContactID (the contact whose
// page the operator was on). If the operator split themselves off (a
// contact uses the "Separate this contact" button on their own row),
// the caller passes the orphan's id and Execute returns the freshly
// created single-link Identity.
type SplitIdentityLink struct {
	repo IdentitySplitRepository
}

// NewSplitIdentityLink wires the write use case.
func NewSplitIdentityLink(repo IdentitySplitRepository) (*SplitIdentityLink, error) {
	if repo == nil {
		return nil, errors.New("contacts/usecase: identity repository must not be nil")
	}
	return &SplitIdentityLink{repo: repo}, nil
}

// SplitInput names the link to detach plus the contact whose post-split
// view the caller wants back.
type SplitInput struct {
	TenantID          uuid.UUID
	LinkID            uuid.UUID
	SurvivorContactID uuid.UUID
}

// SplitResult carries the survivor's updated Identity (with links).
type SplitResult struct {
	Identity *contacts.Identity
}

// Execute runs the split + re-hydration in two steps. The split itself
// is a single transaction inside the adapter; the re-load races the
// just-detached link, but contacts.ErrNotFound on the survivor (when
// the operator split themselves off into an empty identity that is
// then re-linked to a new one) returns to the caller unchanged.
func (u *SplitIdentityLink) Execute(ctx context.Context, in SplitInput) (SplitResult, error) {
	if in.TenantID == uuid.Nil {
		return SplitResult{}, errors.New("contacts/usecase: tenant id must not be nil")
	}
	if in.LinkID == uuid.Nil {
		return SplitResult{}, errors.New("contacts/usecase: link id must not be nil")
	}
	if in.SurvivorContactID == uuid.Nil {
		return SplitResult{}, errors.New("contacts/usecase: survivor contact id must not be nil")
	}
	if err := u.repo.Split(ctx, in.TenantID, in.LinkID); err != nil {
		return SplitResult{}, err
	}
	identity, err := u.repo.FindByContactID(ctx, in.TenantID, in.SurvivorContactID)
	if err != nil {
		return SplitResult{}, err
	}
	return SplitResult{Identity: identity}, nil
}
