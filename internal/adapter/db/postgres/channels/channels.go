// Package channels is the pgx-backed adapter for the channels.Repository
// and channels.ChannelAccessPolicy ports (migration 0128 tenant_channels
// + channel_access).
//
// The package lives under internal/adapter/db/postgres/ so the
// forbidimport / notenant analyzers allow it to import pgx and call
// pgxpool methods — every database call routes through the sibling
// postgres.WithTenant helper so the RLS GUC app.tenant_id is always set
// before reading or writing.
//
// SIN-66389 (Phase 1 of the multi-channel-per-tenant work, child of
// SIN-66378).
package channels

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
	"github.com/pericles-luz/crm/internal/channels"
	"github.com/pericles-luz/crm/internal/inbox"
)

// Compile-time assertions that Store satisfies every channels port. If a
// port grows or shrinks, the build fails here before any caller notices.
var (
	_ channels.Repository          = (*Store)(nil)
	_ channels.ChannelAccessPolicy = (*Store)(nil)
	_ channels.AccessRepository    = (*Store)(nil)
)

// rosterRoles are the tenant roles eligible to attend a channel — the
// same {atendente, gerente} set the inbox assignee dropdown uses
// (inbox.Store.ListAssignable). Kept as SQL-side literals so the role
// gate is a single indexed predicate; the canonical Go names live in
// internal/iam.
const (
	roleTenantAtendente = "tenant_atendente"
	roleTenantGerente   = "tenant_gerente"
)

// pgUniqueViolation is the SQLSTATE for unique-violation. tenant_channels
// carries UNIQUE(tenant_id, channel_key, external_id); a violation is
// translated into channels.ErrChannelConflict.
const pgUniqueViolation = "23505"
const tenantChannelUniqueConstraint = "tenant_channels_tenant_key_external_uniq"

// Store is the pgx-backed adapter for channels.Repository and
// channels.ChannelAccessPolicy. Construct via New(pool); the pool MUST be
// the app_runtime pool so the tenant-isolation RLS policies apply.
type Store struct {
	pool postgres.TxBeginner
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

// Create inserts a brand-new channel instance under the channel's tenant
// scope. A UNIQUE(tenant_id, channel_key, external_id) violation is
// mapped to channels.ErrChannelConflict.
func (s *Store) Create(ctx context.Context, c *channels.Channel) error {
	if c == nil {
		return fmt.Errorf("channels/postgres: Create: nil channel")
	}
	if c.TenantID == uuid.Nil {
		return fmt.Errorf("channels/postgres: Create: tenant id is nil")
	}
	if c.ID == uuid.Nil {
		return fmt.Errorf("channels/postgres: Create: channel id is nil")
	}
	created := c.CreatedAt
	if created.IsZero() {
		created = time.Now().UTC()
	}
	err := postgres.WithTenant(ctx, s.pool, c.TenantID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO tenant_channels
				(id, tenant_id, channel_key, external_id, display_name, is_active, restricted, created_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		`, c.ID, c.TenantID, c.ChannelKey, c.ExternalID, c.DisplayName, c.IsActive, c.Restricted, created)
		return err
	})
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == pgUniqueViolation && pgErr.ConstraintName == tenantChannelUniqueConstraint {
		return channels.ErrChannelConflict
	}
	if err != nil {
		return fmt.Errorf("channels/postgres: Create: %w", err)
	}
	return nil
}

// List returns every channel instance for the tenant ordered
// (channel_key, external_id, id) for stable paging.
func (s *Store) List(ctx context.Context, tenantID uuid.UUID) ([]*channels.Channel, error) {
	if tenantID == uuid.Nil {
		return nil, fmt.Errorf("channels/postgres: List: tenant id is nil")
	}
	var out []*channels.Channel
	err := postgres.WithTenant(ctx, s.pool, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT id, channel_key, external_id, display_name, is_active, restricted, created_at
			  FROM tenant_channels
			 ORDER BY channel_key, external_id, id
		`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var (
				id          uuid.UUID
				channelKey  string
				externalID  string
				displayName string
				isActive    bool
				restricted  bool
				createdAt   time.Time
			)
			if err := rows.Scan(&id, &channelKey, &externalID, &displayName, &isActive, &restricted, &createdAt); err != nil {
				return err
			}
			out = append(out, channels.Hydrate(id, tenantID, channelKey, externalID, displayName, isActive, restricted, createdAt))
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("channels/postgres: List: %w", err)
	}
	return out, nil
}

// Get returns the channel with id under the tenant scope, or
// channels.ErrNotFound when no row matches (RLS-hidden rows collapse to
// the same sentinel).
func (s *Store) Get(ctx context.Context, tenantID, id uuid.UUID) (*channels.Channel, error) {
	if tenantID == uuid.Nil {
		return nil, fmt.Errorf("channels/postgres: Get: tenant id is nil")
	}
	if id == uuid.Nil {
		return nil, channels.ErrNotFound
	}
	var (
		channelKey  string
		externalID  string
		displayName string
		isActive    bool
		restricted  bool
		createdAt   time.Time
	)
	err := postgres.WithTenant(ctx, s.pool, tenantID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT channel_key, external_id, display_name, is_active, restricted, created_at
			  FROM tenant_channels
			 WHERE id = $1
		`, id).Scan(&channelKey, &externalID, &displayName, &isActive, &restricted, &createdAt)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, channels.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("channels/postgres: Get: %w", err)
	}
	return channels.Hydrate(id, tenantID, channelKey, externalID, displayName, isActive, restricted, createdAt), nil
}

// Rename updates the display name of the channel identified by
// (tenantID, id). A blank name is rejected before touching the database.
// Returns channels.ErrNotFound when the UPDATE affects no row.
func (s *Store) Rename(ctx context.Context, tenantID, id uuid.UUID, displayName string) error {
	if tenantID == uuid.Nil {
		return fmt.Errorf("channels/postgres: Rename: tenant id is nil")
	}
	if id == uuid.Nil {
		return channels.ErrNotFound
	}
	// Defensive re-validation: the aggregate's Rename enforces this too,
	// but the adapter must not let a blank name reach storage even if a
	// caller bypasses the aggregate.
	displayName = strings.TrimSpace(displayName)
	if displayName == "" {
		return channels.ErrEmptyDisplayName
	}
	return s.execAffectingOne(ctx, tenantID, "Rename", `
		UPDATE tenant_channels SET display_name = $2 WHERE id = $1
	`, id, displayName)
}

// SetActive flips the is_active flag of the channel identified by
// (tenantID, id). Returns channels.ErrNotFound when the UPDATE affects no
// row.
func (s *Store) SetActive(ctx context.Context, tenantID, id uuid.UUID, active bool) error {
	if tenantID == uuid.Nil {
		return fmt.Errorf("channels/postgres: SetActive: tenant id is nil")
	}
	if id == uuid.Nil {
		return channels.ErrNotFound
	}
	return s.execAffectingOne(ctx, tenantID, "SetActive", `
		UPDATE tenant_channels SET is_active = $2 WHERE id = $1
	`, id, active)
}

// execAffectingOne runs an UPDATE expected to touch exactly one row under
// the tenant scope; a zero rowcount becomes channels.ErrNotFound.
func (s *Store) execAffectingOne(ctx context.Context, tenantID uuid.UUID, op, sql string, args ...any) error {
	var affected int64
	err := postgres.WithTenant(ctx, s.pool, tenantID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, sql, args...)
		if err != nil {
			return err
		}
		affected = tag.RowsAffected()
		return nil
	})
	if err != nil {
		return fmt.Errorf("channels/postgres: %s: %w", op, err)
	}
	if affected == 0 {
		return channels.ErrNotFound
	}
	return nil
}

// CanAccessChannel reports whether userID has an explicit channel_access
// grant for channelID within tenantID. Absence of a grant is
// (false, nil).
func (s *Store) CanAccessChannel(ctx context.Context, tenantID, userID, channelID uuid.UUID) (bool, error) {
	if tenantID == uuid.Nil {
		return false, fmt.Errorf("channels/postgres: CanAccessChannel: tenant id is nil")
	}
	if userID == uuid.Nil || channelID == uuid.Nil {
		return false, nil
	}
	var ok bool
	err := postgres.WithTenant(ctx, s.pool, tenantID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM channel_access
				 WHERE channel_id = $1 AND user_id = $2
			)
		`, channelID, userID).Scan(&ok)
	})
	if err != nil {
		return false, fmt.Errorf("channels/postgres: CanAccessChannel: %w", err)
	}
	return ok, nil
}

// ListAccessibleChannelIDs returns the ids of every channel instance
// userID has an explicit grant for within tenantID, ordered for
// determinism.
func (s *Store) ListAccessibleChannelIDs(ctx context.Context, tenantID, userID uuid.UUID) ([]uuid.UUID, error) {
	if tenantID == uuid.Nil {
		return nil, fmt.Errorf("channels/postgres: ListAccessibleChannelIDs: tenant id is nil")
	}
	if userID == uuid.Nil {
		return nil, nil
	}
	var out []uuid.UUID
	err := postgres.WithTenant(ctx, s.pool, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT channel_id FROM channel_access
			 WHERE user_id = $1
			 ORDER BY channel_id
		`, userID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id uuid.UUID
			if err := rows.Scan(&id); err != nil {
				return err
			}
			out = append(out, id)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("channels/postgres: ListAccessibleChannelIDs: %w", err)
	}
	return out, nil
}

// ListRosterUsers returns the tenant's assignable users (roles
// tenant_atendente / tenant_gerente) ordered by e-mail so the access
// roster renders stably. DisplayName is derived from the e-mail via
// inbox.UserLabelFromEmail (no display-name column on users), matching
// the inbox assignee dropdown so the roster and the badge read
// identically. The query runs under WithTenant, so RLS restricts the
// users table to the tenant scope; the role predicate narrows further.
func (s *Store) ListRosterUsers(ctx context.Context, tenantID uuid.UUID) ([]channels.RosterUser, error) {
	if tenantID == uuid.Nil {
		return nil, fmt.Errorf("channels/postgres: ListRosterUsers: tenant id is nil")
	}
	var out []channels.RosterUser
	err := postgres.WithTenant(ctx, s.pool, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT id, email::text, role
			  FROM users
			 WHERE role IN ($1, $2)
			 ORDER BY email ASC
		`, roleTenantAtendente, roleTenantGerente)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var (
				id    uuid.UUID
				email string
				role  string
			)
			if err := rows.Scan(&id, &email, &role); err != nil {
				return err
			}
			out = append(out, channels.RosterUser{
				ID:          id,
				DisplayName: inbox.UserLabelFromEmail(email),
				Role:        role,
			})
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("channels/postgres: ListRosterUsers: %w", err)
	}
	return out, nil
}

// ChannelUserIDs returns the ids of the users currently granted access
// to channelID within tenantID, ordered for determinism. It backs the
// edit form's pre-check state and the registry access summary. It does
// NOT verify the channel exists — a non-existent / RLS-hidden channel
// simply has no grants and yields an empty slice; callers that need the
// existence signal use Get.
func (s *Store) ChannelUserIDs(ctx context.Context, tenantID, channelID uuid.UUID) ([]uuid.UUID, error) {
	if tenantID == uuid.Nil {
		return nil, fmt.Errorf("channels/postgres: ChannelUserIDs: tenant id is nil")
	}
	if channelID == uuid.Nil {
		return nil, nil
	}
	var out []uuid.UUID
	err := postgres.WithTenant(ctx, s.pool, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT user_id FROM channel_access
			 WHERE channel_id = $1
			 ORDER BY user_id
		`, channelID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id uuid.UUID
			if err := rows.Scan(&id); err != nil {
				return err
			}
			out = append(out, id)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("channels/postgres: ChannelUserIDs: %w", err)
	}
	return out, nil
}

// ReplaceAccess sets channelID's access roster to exactly userIDs in a
// single tenant-scoped transaction (verify-exists → delete-all →
// re-insert the de-duplicated set). A nil/empty slice clears every
// grant. Returns channels.ErrNotFound when the channel does not resolve
// under the tenant scope (unknown id or RLS-hidden) so the caller maps
// it to a 404 instead of writing orphan grants against a channel it
// cannot see. The channel_access.user_id foreign key rejects any id that
// is not a real tenant user, so a forged roster entry fails the whole
// transaction rather than granting phantom access.
func (s *Store) ReplaceAccess(ctx context.Context, tenantID, channelID uuid.UUID, userIDs []uuid.UUID) error {
	if tenantID == uuid.Nil {
		return fmt.Errorf("channels/postgres: ReplaceAccess: tenant id is nil")
	}
	if channelID == uuid.Nil {
		return channels.ErrNotFound
	}
	// De-duplicate while preserving determinism: the UNIQUE(channel_id,
	// user_id) constraint would reject dupes anyway, but filtering here
	// keeps the INSERT count honest and the error path clean.
	unique := make([]uuid.UUID, 0, len(userIDs))
	seen := make(map[uuid.UUID]struct{}, len(userIDs))
	for _, id := range userIDs {
		if id == uuid.Nil {
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		unique = append(unique, id)
	}
	err := postgres.WithTenant(ctx, s.pool, tenantID, func(tx pgx.Tx) error {
		var exists bool
		if err := tx.QueryRow(ctx, `
			SELECT EXISTS (SELECT 1 FROM tenant_channels WHERE id = $1)
		`, channelID).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			return channels.ErrNotFound
		}
		if _, err := tx.Exec(ctx, `
			DELETE FROM channel_access WHERE channel_id = $1
		`, channelID); err != nil {
			return err
		}
		for _, userID := range unique {
			if _, err := tx.Exec(ctx, `
				INSERT INTO channel_access (tenant_id, channel_id, user_id)
				VALUES ($1, $2, $3)
			`, tenantID, channelID, userID); err != nil {
				return err
			}
		}
		return nil
	})
	if errors.Is(err, channels.ErrNotFound) {
		return err
	}
	if err != nil {
		return fmt.Errorf("channels/postgres: ReplaceAccess: %w", err)
	}
	return nil
}
