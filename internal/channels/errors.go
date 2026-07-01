// Package channels holds the Channel aggregate (a concrete channel
// instance a tenant operates, e.g. one WhatsApp number) and the storage
// + access-policy ports used by the multi-channel-per-tenant work
// (SIN-66378 → SIN-66389, Phase 1).
//
// The package is the domain core: it imports neither database/sql nor
// pgx, neither net/http nor any vendor SDK. Storage lives behind
// Repository (port.go); per-user channel authorization lives behind
// ChannelAccessPolicy. The concrete Postgres adapters are in
// internal/adapter/db/postgres/channels.
//
// Sentinels are exported as package-level variables so callers
// (use-cases, adapters, HTTP handlers) can distinguish failure modes via
// errors.Is without depending on string-matching.
package channels

import "errors"

// ErrInvalidTenant is returned by New when tenantID is uuid.Nil. A
// channel MUST belong to a tenant; the database enforces this via NOT
// NULL + foreign key, but the domain rejects it earlier so callers see a
// clean error instead of a constraint-violation surface.
var ErrInvalidTenant = errors.New("channels: invalid tenant id")

// ErrInvalidChannelKey is returned by New when channelKey is blank after
// trimming. Channel keys are case-folded to lower so callers cannot
// accidentally split storage by casing ('WhatsApp' vs 'whatsapp').
var ErrInvalidChannelKey = errors.New("channels: channel key must not be empty")

// ErrEmptyDisplayName is returned by Rename when the new display name is
// blank after trimming. The column itself defaults to the empty string
// in migration 0128 (used by the backfill placeholder) but an explicit
// rename to blank is rejected so the channel picker never renders a
// nameless option.
var ErrEmptyDisplayName = errors.New("channels: display name must not be empty")

// ErrNotFound is returned by Repository lookups when no row matches the
// given scope. Use errors.Is to test. RLS-hidden rows from other tenants
// collapse to this same sentinel so an adversary cannot distinguish
// "exists under another tenant" from "does not exist".
var ErrNotFound = errors.New("channels: not found")

// ErrChannelConflict is returned by Repository.Create when an INSERT
// would violate UNIQUE(tenant_id, channel_key, external_id) — the tenant
// already operates a channel instance with that (key, address) pair.
var ErrChannelConflict = errors.New("channels: channel already exists for tenant")
