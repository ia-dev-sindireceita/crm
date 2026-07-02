// Package tenantusers is the pgx-backed adapter for the
// tenantusers.Repository port (the users table, migrations 0005 + 0130).
//
// The package lives under internal/adapter/db/postgres/ so the
// forbidimport / notenant analyzers allow it to import pgx and call
// pgxpool methods — every database call routes through the sibling
// postgres.WithTenant helper so the RLS GUC app.tenant_id is always set
// before reading or writing. RLS on `users` restricts every statement to
// the resolved tenant; master rows (is_master=true, tenant_id NULL) are
// invisible to these tenant-scoped reads/writes.
//
// SIN-66499 (parent SIN-66492 / SIN-66493).
package tenantusers

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pericles-luz/crm/internal/adapter/db/postgres"
	"github.com/pericles-luz/crm/internal/iam"
	"github.com/pericles-luz/crm/internal/tenantusers"
)

// Compile-time assertion that Store satisfies the Repository port.
var _ tenantusers.Repository = (*Store)(nil)

// pgUniqueViolation is the SQLSTATE for a unique-violation. users carries a
// UNIQUE index on (tenant_id, email) WHERE tenant_id IS NOT NULL
// (users_tenant_email_idx); a violation is translated into
// tenantusers.ErrEmailConflict.
const pgUniqueViolation = "23505"

// usersTenantEmailIndex is the partial unique index name from migration
// 0005. A 23505 naming it is the "email already used in this tenant" case.
const usersTenantEmailIndex = "users_tenant_email_idx"

// Store is the pgx-backed adapter for tenantusers.Repository. Construct via
// New(pool); the pool MUST be the app_runtime pool so the tenant-isolation
// RLS policies apply.
type Store struct {
	pool postgres.TxBeginner
}

// New wraps pool and returns a ready-to-use Store. A nil pool yields
// postgres.ErrNilPool so wiring mistakes fail loudly at construction.
func New(pool *pgxpool.Pool) (*Store, error) {
	if pool == nil {
		return nil, postgres.ErrNilPool
	}
	return &Store{pool: pool}, nil
}

// List returns every tenant user ordered by email for a stable table. RLS
// scopes the SELECT to the tenant; is_master rows have tenant_id NULL and
// are excluded automatically.
func (s *Store) List(ctx context.Context, tenantID uuid.UUID) ([]*tenantusers.User, error) {
	if tenantID == uuid.Nil {
		return nil, tenantusers.ErrInvalidTenant
	}
	var out []*tenantusers.User
	err := postgres.WithTenant(ctx, s.pool, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT id, tenant_id, email, role, active, must_change_password, created_at
			  FROM users
			 WHERE is_master = false
			 ORDER BY email
		`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			u, err := scanUser(rows)
			if err != nil {
				return err
			}
			out = append(out, u)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("tenantusers/postgres: List: %w", err)
	}
	return out, nil
}

// Get returns the tenant user with id, or tenantusers.ErrNotFound when no
// row matches the scope (including RLS-hidden / master rows).
func (s *Store) Get(ctx context.Context, tenantID, id uuid.UUID) (*tenantusers.User, error) {
	if tenantID == uuid.Nil {
		return nil, tenantusers.ErrInvalidTenant
	}
	var u *tenantusers.User
	err := postgres.WithTenant(ctx, s.pool, tenantID, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
			SELECT id, tenant_id, email, role, active, must_change_password, created_at
			  FROM users
			 WHERE id = $1 AND is_master = false
		`, id)
		var err error
		u, err = scanUser(row)
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, tenantusers.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("tenantusers/postgres: Get: %w", err)
	}
	return u, nil
}

// Create inserts a brand-new tenant user. is_master is always written
// false (anti-escalation: this adapter can never mint a master). A
// UNIQUE(tenant_id, email) violation maps to tenantusers.ErrEmailConflict.
func (s *Store) Create(ctx context.Context, u *tenantusers.User) error {
	if u == nil {
		return fmt.Errorf("tenantusers/postgres: Create: nil user")
	}
	if u.TenantID == uuid.Nil {
		return tenantusers.ErrInvalidTenant
	}
	if u.ID == uuid.Nil {
		return fmt.Errorf("tenantusers/postgres: Create: user id is nil")
	}
	created := u.CreatedAt
	if created.IsZero() {
		created = time.Now().UTC()
	}
	err := postgres.WithTenant(ctx, s.pool, u.TenantID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO users
				(id, tenant_id, email, password_hash, role, is_master, active, must_change_password, created_at)
			VALUES ($1, $2, $3, $4, $5, false, $6, $7, $8)
		`, u.ID, u.TenantID, u.Email, u.PasswordHash, string(u.Role), u.Active, u.MustChangePassword, created)
		return err
	})
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == pgUniqueViolation && pgErr.ConstraintName == usersTenantEmailIndex {
		return tenantusers.ErrEmailConflict
	}
	if err != nil {
		return fmt.Errorf("tenantusers/postgres: Create: %w", err)
	}
	return nil
}

// UpdateRole updates role for (tenantID, id). The is_master=false predicate
// keeps a master row untouchable even if one somehow shared the scope.
// Returns tenantusers.ErrNotFound when the UPDATE affects no row.
func (s *Store) UpdateRole(ctx context.Context, tenantID, id uuid.UUID, role iam.Role) error {
	if tenantID == uuid.Nil {
		return tenantusers.ErrInvalidTenant
	}
	var tag pgconn.CommandTag
	err := postgres.WithTenant(ctx, s.pool, tenantID, func(tx pgx.Tx) error {
		var err error
		tag, err = tx.Exec(ctx, `
			UPDATE users SET role = $2
			 WHERE id = $1 AND is_master = false
		`, id, string(role))
		return err
	})
	if err != nil {
		return fmt.Errorf("tenantusers/postgres: UpdateRole: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return tenantusers.ErrNotFound
	}
	return nil
}

// SetActive flips the active flag for (tenantID, id). Returns
// tenantusers.ErrNotFound when the UPDATE affects no row.
func (s *Store) SetActive(ctx context.Context, tenantID, id uuid.UUID, active bool) error {
	if tenantID == uuid.Nil {
		return tenantusers.ErrInvalidTenant
	}
	var tag pgconn.CommandTag
	err := postgres.WithTenant(ctx, s.pool, tenantID, func(tx pgx.Tx) error {
		var err error
		tag, err = tx.Exec(ctx, `
			UPDATE users SET active = $2
			 WHERE id = $1 AND is_master = false
		`, id, active)
		return err
	})
	if err != nil {
		return fmt.Errorf("tenantusers/postgres: SetActive: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return tenantusers.ErrNotFound
	}
	return nil
}

// CountActiveGerentes counts active tenant_gerente users in the tenant.
func (s *Store) CountActiveGerentes(ctx context.Context, tenantID uuid.UUID) (int, error) {
	if tenantID == uuid.Nil {
		return 0, tenantusers.ErrInvalidTenant
	}
	var n int
	err := postgres.WithTenant(ctx, s.pool, tenantID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT count(*) FROM users
			 WHERE is_master = false AND active AND role = $1
		`, string(iam.RoleTenantGerente)).Scan(&n)
	})
	if err != nil {
		return 0, fmt.Errorf("tenantusers/postgres: CountActiveGerentes: %w", err)
	}
	return n, nil
}

// rowScanner is the minimal surface shared by pgx.Row and pgx.Rows so
// scanUser serves both the single-row Get and the List loop.
type rowScanner interface {
	Scan(dest ...any) error
}

// scanUser materialises a users row via tenantusers.Hydrate. The
// password_hash column is never selected, so the hash never enters the
// domain on a read path.
func scanUser(row rowScanner) (*tenantusers.User, error) {
	var (
		id        uuid.UUID
		tenantID  uuid.UUID
		email     string
		role      string
		active    bool
		mustChg   bool
		createdAt time.Time
	)
	if err := row.Scan(&id, &tenantID, &email, &role, &active, &mustChg, &createdAt); err != nil {
		return nil, err
	}
	return tenantusers.Hydrate(id, tenantID, email, iam.Role(role), active, mustChg, createdAt), nil
}
