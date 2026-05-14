// Package contacts is the pgx-backed adapter for the
// contacts.Repository port (migrations 0088 contact +
// contact_channel_identity).
//
// The package lives under internal/adapter/db/postgres/ so the
// forbidimport / notenant analyzers allow it to import pgx and call
// pgxpool methods directly — every database call routes through the
// sibling postgres.WithTenant helper so the RLS GUC app.tenant_id is
// always set before reading or writing.
//
// SIN-62726 (PR3 of the Fase 1 inbox stack, child of SIN-62193).
package contacts

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pericles-luz/crm/internal/adapter/db/postgres"
	"github.com/pericles-luz/crm/internal/contacts"
)

// Compile-time assertion that Store satisfies contacts.Repository. If
// the port grows or shrinks, the build fails here before any caller
// notices.
var _ contacts.Repository = (*Store)(nil)

// pgUniqueViolation is the SQLSTATE for unique-violation. The
// contact_channel_identity table carries two UNIQUE constraints:
//   - contact_channel_identity_channel_external_uniq (channel,
//     external_id) — the global anti-duplicate-claim invariant.
//   - contact_channel_identity_contact_channel_uniq (contact_id,
//     channel) — the per-contact "one identity per channel" rule.
//
// We only translate the first into contacts.ErrChannelIdentityConflict;
// the second is a programming error (the domain rejects it before we
// reach the DB) and surfaces as a wrapped pgconn.PgError.
const pgUniqueViolation = "23505"
const channelExternalUniqueConstraint = "contact_channel_identity_channel_external_uniq"

// Store is the pgx-backed adapter for contacts.Repository. Construct
// via New(pool); the pool MUST be the app_runtime pool so the
// tenant-isolation RLS policies on contact + contact_channel_identity
// apply.
type Store struct {
	pool postgres.TxBeginner
	// now is overridable for tests so adapter integration tests can
	// pin timestamps. Production callers leave it nil; the adapter
	// reads time.Now().UTC() in that case.
	now func() time.Time
}

// New wraps pool and returns a ready-to-use Store. A nil pool yields
// postgres.ErrNilPool so wiring mistakes fail loudly at construction
// rather than panic at first request.
func New(pool *pgxpool.Pool) (*Store, error) {
	if pool == nil {
		return nil, postgres.ErrNilPool
	}
	return &Store{pool: pool}, nil
}

// WithClock returns a copy of s that uses fn for every "now" read.
// Tests use it to make Save deterministic. fn MUST NOT be nil.
func (s *Store) WithClock(fn func() time.Time) *Store {
	cp := *s
	cp.now = fn
	return &cp
}

func (s *Store) nowUTC() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now().UTC()
}

// Save inserts the contact row plus every ChannelIdentity attached to
// it inside a single tenant-scoped transaction. If the
// (channel, external_id) UNIQUE constraint fires on any identity, the
// whole transaction is rolled back and contacts.ErrChannelIdentityConflict
// is returned. Save is not an upsert: a duplicate contact ID is a
// programming error and bubbles up as a wrapped pg error.
func (s *Store) Save(ctx context.Context, c *contacts.Contact) error {
	if c == nil {
		return fmt.Errorf("contacts/postgres: Save: nil contact")
	}
	if c.TenantID == uuid.Nil {
		return fmt.Errorf("contacts/postgres: Save: tenant id is nil")
	}
	if c.ID == uuid.Nil {
		return fmt.Errorf("contacts/postgres: Save: contact id is nil")
	}

	created := c.CreatedAt
	if created.IsZero() {
		created = s.nowUTC()
	}
	updated := c.UpdatedAt
	if updated.IsZero() {
		updated = created
	}

	identities := c.Identities()
	err := postgres.WithTenant(ctx, s.pool, c.TenantID, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `
			INSERT INTO contact (id, tenant_id, display_name, created_at, updated_at)
			VALUES ($1, $2, $3, $4, $5)
		`, c.ID, c.TenantID, c.DisplayName, created, updated); err != nil {
			return err
		}
		for _, id := range identities {
			if _, err := tx.Exec(ctx, `
				INSERT INTO contact_channel_identity (tenant_id, contact_id, channel, external_id)
				VALUES ($1, $2, $3, $4)
			`, c.TenantID, c.ID, id.Channel, id.ExternalID); err != nil {
				return err
			}
		}
		return nil
	})
	if err == nil {
		// Persist the resolved timestamps back onto the aggregate so
		// callers see the same values the DB now holds.
		c.CreatedAt = created
		c.UpdatedAt = updated
		return nil
	}
	if isChannelExternalConflict(err) {
		return contacts.ErrChannelIdentityConflict
	}
	return fmt.Errorf("contacts/postgres: Save: %w", err)
}

// FindByID returns the contact under the given tenant scope. RLS-hidden
// rows (belonging to another tenant) collapse to ErrNotFound, matching
// the SessionStore.Get convention so adversaries cannot enumerate ids
// across tenants by timing.
func (s *Store) FindByID(ctx context.Context, tenantID, id uuid.UUID) (*contacts.Contact, error) {
	if tenantID == uuid.Nil {
		return nil, fmt.Errorf("contacts/postgres: FindByID: tenant id is nil")
	}
	if id == uuid.Nil {
		return nil, contacts.ErrNotFound
	}
	var (
		displayName string
		createdAt   time.Time
		updatedAt   time.Time
		identities  []contacts.ChannelIdentity
	)
	err := postgres.WithTenant(ctx, s.pool, tenantID, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
			SELECT display_name, created_at, updated_at
			  FROM contact
			 WHERE id = $1
		`, id)
		if err := row.Scan(&displayName, &createdAt, &updatedAt); err != nil {
			return err
		}
		loaded, err := scanIdentities(ctx, tx, id)
		if err != nil {
			return err
		}
		identities = loaded
		return nil
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, contacts.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("contacts/postgres: FindByID: %w", err)
	}
	return contacts.Hydrate(id, tenantID, displayName, identities, createdAt, updatedAt), nil
}

// FindByChannelIdentity resolves an inbound (channel, externalID) pair
// to a Contact under the given tenant scope. The query joins
// contact_channel_identity → contact so a cross-tenant identity is
// hidden by RLS on the contact side and we return ErrNotFound.
//
// Channel and externalID are normalised through contacts.NewChannelIdentity
// so the lookup is case-insensitive on channel and whitespace-trimmed.
// This mirrors the use-case layer's normalisation and prevents the
// "+5511…" vs " +5511… " mismatch from ever reaching the index.
func (s *Store) FindByChannelIdentity(ctx context.Context, tenantID uuid.UUID, channel, externalID string) (*contacts.Contact, error) {
	if tenantID == uuid.Nil {
		return nil, fmt.Errorf("contacts/postgres: FindByChannelIdentity: tenant id is nil")
	}
	normalised, err := contacts.NewChannelIdentity(channel, externalID)
	if err != nil {
		// Invalid input shape collapses to NotFound — the row cannot
		// exist by definition, and callers do not need to distinguish
		// "no such contact" from "you asked for an invalid identity".
		return nil, contacts.ErrNotFound
	}

	var (
		contactID   uuid.UUID
		displayName string
		createdAt   time.Time
		updatedAt   time.Time
	)
	err = postgres.WithTenant(ctx, s.pool, tenantID, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
			SELECT c.id, c.display_name, c.created_at, c.updated_at
			  FROM contact c
			  JOIN contact_channel_identity i ON i.contact_id = c.id
			 WHERE i.channel = $1
			   AND i.external_id = $2
		`, normalised.Channel, normalised.ExternalID)
		return row.Scan(&contactID, &displayName, &createdAt, &updatedAt)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, contacts.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("contacts/postgres: FindByChannelIdentity: %w", err)
	}

	var identities []contacts.ChannelIdentity
	if err := postgres.WithTenant(ctx, s.pool, tenantID, func(tx pgx.Tx) error {
		loaded, lerr := scanIdentities(ctx, tx, contactID)
		if lerr != nil {
			return lerr
		}
		identities = loaded
		return nil
	}); err != nil {
		return nil, fmt.Errorf("contacts/postgres: FindByChannelIdentity load identities: %w", err)
	}
	return contacts.Hydrate(contactID, tenantID, displayName, identities, createdAt, updatedAt), nil
}

// scanIdentities loads every channel identity for contactID using the
// supplied tx. Caller owns the transaction lifecycle.
func scanIdentities(ctx context.Context, tx pgx.Tx, contactID uuid.UUID) ([]contacts.ChannelIdentity, error) {
	rows, err := tx.Query(ctx, `
		SELECT channel, external_id
		  FROM contact_channel_identity
		 WHERE contact_id = $1
		 ORDER BY channel ASC
	`, contactID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []contacts.ChannelIdentity
	for rows.Next() {
		var channel, externalID string
		if err := rows.Scan(&channel, &externalID); err != nil {
			return nil, err
		}
		out = append(out, contacts.ChannelIdentity{Channel: channel, ExternalID: externalID})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// isChannelExternalConflict reports whether err describes a UNIQUE
// violation on contact_channel_identity_channel_external_uniq. We
// match both on the SQLSTATE (23505) and the constraint name so a
// future migration that renames the constraint trips a fast failure
// instead of silently flipping the error mapping.
func isChannelExternalConflict(err error) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return false
	}
	if pgErr.Code != pgUniqueViolation {
		return false
	}
	return pgErr.ConstraintName == channelExternalUniqueConstraint ||
		strings.Contains(pgErr.Message, channelExternalUniqueConstraint)
}
