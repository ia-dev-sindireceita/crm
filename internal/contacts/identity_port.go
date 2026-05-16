package contacts

import (
	"context"

	"github.com/google/uuid"
)

// IdentityRepository is the storage port for the Identity aggregate.
// The concrete adapter lives in
// internal/adapter/db/postgres/contacts (identity.go).
//
// All methods are tenant-scoped. The Postgres adapter runs each
// operation inside postgres.WithTenant so RLS on identity +
// contact_identity_link restricts visible rows to the caller's tenant.
//
// Feature flag: when feature.identity_merge.enabled is false for a
// tenant, Resolve performs only the direct (channel, externalID) lookup
// and skips phone/email cross-channel merge — callers configure this
// via IdentityStore.WithMergeEnabled(false).
type IdentityRepository interface {
	// Resolve finds or creates the Identity for an inbound contact
	// signal. It applies the merge rules from DecideMerge:
	//   - same phone (E.164)  → same Identity
	//   - same email (lower)  → same Identity
	//   - same (channel, externalID) → direct link, no cross-channel merge
	//
	// When two contacts with different identities both have leaders,
	// Resolve creates a MergeProposal (returned non-nil) instead of
	// auto-merging, and returns the winning identity (the leader's).
	//
	// phone and email may be empty strings when the caller has no
	// value for those signals; the lookup is skipped for empty inputs.
	Resolve(ctx context.Context, tenantID uuid.UUID,
		channel, externalID, phone, email string,
	) (*Identity, *MergeProposal, error)

	// Merge absorbs sourceID into targetID under tenantID. All
	// contact_identity_link rows pointing at sourceID are repointed to
	// targetID inside a single SELECT … FOR UPDATE transaction to prevent
	// concurrent races. sourceID.merged_into_id is set to targetID.
	Merge(ctx context.Context, tenantID, sourceID, targetID uuid.UUID, reason string) error

	// Split removes the contact_identity_link identified by linkID, then
	// creates a fresh Identity for that contact (1:1). Use when a merge
	// was incorrect.
	Split(ctx context.Context, tenantID, linkID uuid.UUID) error

	// FindByContactID returns the Identity currently linked to contactID
	// under tenantID, with Links populated for every sibling contact on
	// the same Identity. Used by the split UI (F2-13) to render the
	// identity panel: one row per IdentityLink, each with link_reason and
	// timestamp. Returns ErrNotFound when the contact has no link row
	// (e.g. the upsert use case has not yet linked it).
	FindByContactID(ctx context.Context, tenantID, contactID uuid.UUID) (*Identity, error)
}
