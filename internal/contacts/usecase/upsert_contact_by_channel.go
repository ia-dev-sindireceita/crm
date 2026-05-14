// Package usecase holds the application services for the contacts
// aggregate. PR3 only ships UpsertContactByChannel — the idempotent
// "resolve an inbound (channel, external_id) to a Contact, creating
// one if absent" entry point used by the webhook receiver (PR6) and
// the inbound-message materialiser (PR4).
//
// Use-cases depend on the contacts port; they never import database
// drivers. Wiring (which Repository implementation is plugged in) is
// done at the composition root in cmd/server.
package usecase

import (
	"context"
	"errors"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/contacts"
)

// UpsertContactByChannel resolves an inbound (channel, external_id)
// pair to a Contact in the named tenant, creating one if no contact
// currently claims the identity.
//
// Idempotency contract:
//
//   - Two callers racing with the same (tenant, channel, external_id):
//     exactly one INSERT succeeds; the loser re-reads and returns the
//     winner's Contact. Both callers see the same UUID.
//   - 100 concurrent calls → 1 row inserted, 99 cache-hit on the
//     existing row (PR3 AC #4).
//
// The use-case is deliberately not "create_or_get_contact" — it names
// the channel identity as the natural key, which matches how PR6's
// webhook receiver wakes up: it has a phone number, not a UUID.
type UpsertContactByChannel struct {
	repo contacts.Repository
}

// New returns a use-case bound to the given repository. nil repo is a
// programming error caught at construction.
func New(repo contacts.Repository) (*UpsertContactByChannel, error) {
	if repo == nil {
		return nil, errors.New("contacts/usecase: repository must not be nil")
	}
	return &UpsertContactByChannel{repo: repo}, nil
}

// MustNew is the panic-on-error variant for the composition root, where
// nil Repository is a wiring bug that should crash the process before
// the first request.
func MustNew(repo contacts.Repository) *UpsertContactByChannel {
	u, err := New(repo)
	if err != nil {
		panic(err)
	}
	return u
}

// Input is the use-case argument. DisplayName is the fallback name
// used only when a brand-new contact is created (typically the carrier
// profile name, e.g. WhatsApp's `pushName`); when the contact already
// exists, DisplayName is ignored — we never overwrite a contact's
// curated name from an inbound message.
type Input struct {
	TenantID    uuid.UUID
	Channel     string
	ExternalID  string
	DisplayName string
}

// Result reports the resolved contact and whether THIS call created
// the row. The boolean is useful to the webhook receiver for emitting
// a "new contact" event exactly once.
type Result struct {
	Contact *contacts.Contact
	Created bool
}

// Execute runs the upsert. See the type doc-comment for the full
// idempotency contract.
//
// Algorithm:
//
//  1. FindByChannelIdentity(tenant, channel, externalID).
//     - Hit: return the existing contact, Created=false.
//     - Miss: continue.
//  2. Build a fresh Contact + ChannelIdentity in memory.
//  3. Save it.
//     - Success: return the new contact, Created=true.
//     - ErrChannelIdentityConflict (someone won the race):
//     re-Find and return the winner, Created=false.
//
// We do NOT retry indefinitely. A second NotFound after a conflict is
// the cross-tenant case (the identity is claimed by ANOTHER tenant);
// we surface the original conflict so the caller can decide.
func (u *UpsertContactByChannel) Execute(ctx context.Context, in Input) (Result, error) {
	if in.TenantID == uuid.Nil {
		return Result{}, contacts.ErrInvalidTenant
	}

	existing, err := u.repo.FindByChannelIdentity(ctx, in.TenantID, in.Channel, in.ExternalID)
	if err == nil {
		return Result{Contact: existing, Created: false}, nil
	}
	if !errors.Is(err, contacts.ErrNotFound) {
		return Result{}, err
	}

	c, err := contacts.New(in.TenantID, in.DisplayName)
	if err != nil {
		return Result{}, err
	}
	if err := c.AddChannelIdentity(in.Channel, in.ExternalID); err != nil {
		return Result{}, err
	}

	saveErr := u.repo.Save(ctx, c)
	if saveErr == nil {
		return Result{Contact: c, Created: true}, nil
	}
	if !errors.Is(saveErr, contacts.ErrChannelIdentityConflict) {
		return Result{}, saveErr
	}

	// Race: another caller inserted the same identity between our
	// Find and Save. Re-Find under the same tenant scope.
	winner, findErr := u.repo.FindByChannelIdentity(ctx, in.TenantID, in.Channel, in.ExternalID)
	if findErr == nil {
		return Result{Contact: winner, Created: false}, nil
	}
	// NotFound after a conflict means the identity is claimed by a
	// different tenant. Surface the original conflict (not the
	// second-Find error) so the caller can distinguish "your inbox is
	// fine" from "this number is owned by someone else".
	if errors.Is(findErr, contacts.ErrNotFound) {
		return Result{}, contacts.ErrChannelIdentityConflict
	}
	return Result{}, findErr
}
