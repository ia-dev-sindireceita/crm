package channels

import (
	"context"

	"github.com/google/uuid"
)

// RosterUser is the view of a tenant user eligible to appear in a
// channel's access roster (Screen 2 create/edit form, Screen 3
// maintenance). It is the channel bounded context's language for "a
// person who can be granted access to a channel"; the concrete adapter
// (internal/adapter/db/postgres/channels) reads the tenant's atendente /
// gerente users under RLS and derives DisplayName from the account
// e-mail (there is no display-name column on users).
//
// Role carries the raw tenant role string ('tenant_atendente' /
// 'tenant_gerente') so the web layer can render a human label without
// re-querying; the domain does not interpret it.
type RosterUser struct {
	ID          uuid.UUID
	DisplayName string
	Role        string
}

// AccessRepository is the write + roster-read port for per-channel
// access grants (the channel_access table, migration 0128). It sits
// alongside Repository (channel CRUD) and ChannelAccessPolicy (the
// per-user authorization *questions*). SIN-66389 shipped
// ChannelAccessPolicy as a read-only stub; SIN-66391 (P2) is its first
// consumer and needs to (a) list the tenant's assignable users to build
// the roster and (b) persist the gerente's chosen roster when a channel
// is created or edited.
//
// Every method is tenant-scoped and the concrete adapter runs each call
// inside postgres.WithTenant so the RLS policies on channel_access /
// users restrict the visible rows. Passing uuid.Nil for the tenant is a
// clean error, never a cross-tenant leak.
//
// NOTE: the write here is the gerente authoring the *initial / edited*
// roster from the management form (gerente-gated at the route). The
// per-resource access *enforcement* contract — atendente cannot
// self-grant, audit line on every change, user-deactivation cascade — is
// the P3 concern (SIN-66392) that loops in SecurityEngineer. This port
// deliberately stays a simple full-replace primitive so P3 can layer the
// enforcement + audit on top without reshaping it.
type AccessRepository interface {
	// ListRosterUsers returns every tenant user eligible to attend a
	// channel (roles tenant_atendente / tenant_gerente), ordered
	// deterministically by display label, so the roster renders stably
	// across reloads. An empty tenant yields no rows; uuid.Nil is a
	// clean error.
	ListRosterUsers(ctx context.Context, tenantID uuid.UUID) ([]RosterUser, error)

	// ChannelUserIDs returns the ids of the users currently granted
	// access to channelID within tenantID, ordered for determinism. It
	// backs the edit form's pre-check state and the registry's access
	// summary count. A channel with no explicit grants yields an empty
	// slice (nil error).
	ChannelUserIDs(ctx context.Context, tenantID, channelID uuid.UUID) ([]uuid.UUID, error)

	// ReplaceAccess sets the channel's access roster to exactly userIDs,
	// atomically (delete-all-then-insert inside one tenant-scoped
	// transaction). Duplicate ids in the input are de-duplicated; a nil
	// / empty slice clears every grant (the channel becomes
	// gerente-only until access is re-granted). Returns ErrNotFound when
	// channelID does not resolve under the tenant scope so the caller
	// maps it to a 404 rather than silently writing orphan grants. Only
	// ids that are assignable tenant users are accepted; the foreign key
	// on channel_access.user_id rejects anything else.
	ReplaceAccess(ctx context.Context, tenantID, channelID uuid.UUID, userIDs []uuid.UUID) error
}
