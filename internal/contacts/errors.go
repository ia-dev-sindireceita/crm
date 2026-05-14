// Package contacts holds the Contact aggregate, the ChannelIdentity value
// object, and the Repository port used by Fase 1 inbox / webhook receiver
// (SIN-62193 → SIN-62726).
//
// The package is the domain core: it imports neither database/sql nor pgx,
// neither net/http nor any vendor SDK. Storage lives behind Repository
// (port.go); the concrete adapter is in
// internal/adapter/db/postgres/contacts.
//
// Sentinels are exported as package-level variables so callers (use-cases,
// adapters, HTTP handlers) can distinguish failure modes via errors.Is
// without depending on string-matching.
package contacts

import "errors"

// ErrInvalidTenant is returned by New when tenantID is uuid.Nil. A contact
// MUST belong to a tenant; the database enforces this via NOT NULL +
// foreign key, but the domain rejects it earlier so callers see a clean
// error instead of a constraint-violation surface.
var ErrInvalidTenant = errors.New("contacts: invalid tenant id")

// ErrEmptyDisplayName is returned by New when displayName is blank after
// trimming whitespace. The column itself accepts the empty string
// (DEFAULT empty in migration 0088) for adapter back-fills, but the
// domain constructor requires a non-empty name so the inbox UI never
// has to render a blank header.
var ErrEmptyDisplayName = errors.New("contacts: display name must not be empty")

// ErrInvalidChannel is returned by NewChannelIdentity when channel is
// blank after trimming. Channels are case-folded to lower so callers
// cannot accidentally split storage by casing.
var ErrInvalidChannel = errors.New("contacts: invalid channel")

// ErrInvalidExternalID is returned by NewChannelIdentity when externalID
// is blank after trimming.
var ErrInvalidExternalID = errors.New("contacts: invalid external id")

// ErrInvalidE164 is returned by NewChannelIdentity when channel ==
// "whatsapp" but externalID is not a valid E.164 number. WhatsApp's
// Cloud API delivers the sender's number as "+<country><subscriber>"
// (E.164); refusing anything else here keeps malformed data out of the
// dedup join key.
var ErrInvalidE164 = errors.New("contacts: external id is not a valid E.164 number")

// ErrChannelIdentityExists is returned by Contact.AddChannelIdentity when
// the contact already carries an identity on the same channel. The
// database guarantees this via UNIQUE(contact_id, channel), but the
// domain rejects it earlier so callers do not need to translate a
// constraint-violation back into a domain meaning.
var ErrChannelIdentityExists = errors.New("contacts: contact already has an identity on that channel")

// ErrNotFound is returned by Repository lookups when no row matches.
// Use errors.Is to test. Callers MUST NOT treat ErrNotFound as a
// transient failure — the row does not exist for the given scope.
var ErrNotFound = errors.New("contacts: not found")

// ErrChannelIdentityConflict is returned by Repository.Save when an
// INSERT would violate UNIQUE(channel, external_id). The use-case layer
// converts this into an idempotent "fetch the winner" flow; callers
// outside the use-case rarely see it.
var ErrChannelIdentityConflict = errors.New("contacts: channel identity already claimed")
