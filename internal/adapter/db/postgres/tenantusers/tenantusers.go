// Package tenantusers is the Postgres adapter for the tenantusers domain
// Repository port (SIN-66496). Every method runs inside the tenant's RLS
// envelope via postgres.WithTenant, so a cross-tenant id resolves to
// not-found — never to another tenant's row (SIN-66494 G2, defense in depth
// on top of RLS).
package tenantusers

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
	"github.com/pericles-luz/crm/internal/iam"
	domain "github.com/pericles-luz/crm/internal/tenantusers"
)

// pgUniqueViolation is SQLSTATE 23505.
const pgUniqueViolation = "23505"

// tenantEmailUniqueConstraint is the unique index on users(tenant_id, email)
// from migration 0005. A collision maps to domain.ErrEmailTaken.
const tenantEmailUniqueConstraint = "users_tenant_email_idx"

// Store implements tenantusers.Repository against Postgres.
type Store struct {
	pool postgres.TxBeginner
}

// New constructs a Store. A nil pool is rejected.
func New(pool *pgxpool.Pool) (*Store, error) {
	if pool == nil {
		return nil, postgres.ErrNilPool
	}
	return &Store{pool: pool}, nil
}

// compile-time assertion that Store satisfies the domain port.
var _ domain.Repository = (*Store)(nil)

// List returns the tenant's users, active first then by email.
func (s *Store) List(ctx context.Context, tenantID uuid.UUID) ([]domain.User, error) {
	var out []domain.User
	err := postgres.WithTenant(ctx, s.pool, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT id, tenant_id, email, role, deactivated_at, created_at
			FROM users
			WHERE tenant_id = $1 AND is_master = false
			ORDER BY (deactivated_at IS NOT NULL), email
		`, tenantID)
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

// Get returns one non-master user by id within the tenant.
func (s *Store) Get(ctx context.Context, tenantID, id uuid.UUID) (domain.User, error) {
	var u domain.User
	err := postgres.WithTenant(ctx, s.pool, tenantID, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
			SELECT id, tenant_id, email, role, deactivated_at, created_at
			FROM users
			WHERE id = $1 AND tenant_id = $2 AND is_master = false
		`, id, tenantID)
		var err error
		u, err = scanUser(row)
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.User{}, domain.ErrUserNotFound
	}
	if err != nil {
		return domain.User{}, fmt.Errorf("tenantusers/postgres: Get: %w", err)
	}
	return u, nil
}

// Create inserts the user and its invite token atomically inside one
// tenant-scoped transaction. is_master is hard-coded false (SIN-66494 G1) —
// never taken from input; RLS + the users_master_xor_tenant CHECK are the two
// further layers that make escalation to master impossible even on a code
// bypass.
func (s *Store) Create(ctx context.Context, u domain.User, passwordHash string, tok domain.Token) error {
	err := postgres.WithTenant(ctx, s.pool, u.TenantID, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `
			INSERT INTO users (id, tenant_id, email, password_hash, role, is_master, created_at)
			VALUES ($1, $2, $3, $4, $5, false, $6)
		`, u.ID, u.TenantID, u.Email, passwordHash, string(u.Role), u.CreatedAt); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `
			INSERT INTO user_credential_tokens
				(tenant_id, user_id, token_sha256, purpose, expires_at, created_at)
			VALUES ($1, $2, $3, $4, $5, $6)
		`, u.TenantID, u.ID, tok.SHA256, string(tok.Purpose), tok.ExpiresAt, u.CreatedAt)
		return err
	})
	if isTenantEmailConflict(err) {
		return domain.ErrEmailTaken
	}
	if err != nil {
		return fmt.Errorf("tenantusers/postgres: Create: %w", err)
	}
	return nil
}

// UpdateRole changes the role guarded against anti-lockout (SIN-66494 G4).
// The correlated subquery counts OTHER active gerentes in the same statement,
// so two concurrent demotions cannot both win (no TOCTOU): demoting away from
// gerente only succeeds if at least one other active gerente remains. Zero
// rows affected on an existing gerente ⇒ ErrLastGerente.
func (s *Store) UpdateRole(ctx context.Context, tenantID, id uuid.UUID, newRole iam.Role) (iam.Role, error) {
	var before iam.Role
	err := postgres.WithTenant(ctx, s.pool, tenantID, func(tx pgx.Tx) error {
		var current string
		if err := tx.QueryRow(ctx, `
			SELECT role FROM users
			WHERE id = $1 AND tenant_id = $2 AND is_master = false AND deactivated_at IS NULL
		`, id, tenantID).Scan(&current); err != nil {
			return err
		}
		before = iam.Role(current)
		if before == newRole {
			return nil // no-op; nothing to guard
		}
		tag, err := tx.Exec(ctx, `
			UPDATE users SET role = $3
			WHERE id = $1 AND tenant_id = $2 AND is_master = false AND deactivated_at IS NULL
			  AND (role <> 'tenant_gerente' OR EXISTS (
				SELECT 1 FROM users
				WHERE tenant_id = $2 AND role = 'tenant_gerente'
				  AND deactivated_at IS NULL AND id <> $1
			  ))
		`, id, tenantID, string(newRole))
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return domain.ErrLastGerente
		}
		return nil
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return "", domain.ErrUserNotFound
	}
	if errors.Is(err, domain.ErrLastGerente) {
		return "", domain.ErrLastGerente
	}
	if err != nil {
		return "", fmt.Errorf("tenantusers/postgres: UpdateRole: %w", err)
	}
	return before, nil
}

// Deactivate soft-deactivates guarded against anti-lockout (SIN-66494 G4/G5).
// Same correlated-subquery guard as UpdateRole. Zero rows on an existing
// active gerente ⇒ ErrLastGerente.
func (s *Store) Deactivate(ctx context.Context, tenantID, id uuid.UUID) (iam.Role, error) {
	var role iam.Role
	err := postgres.WithTenant(ctx, s.pool, tenantID, func(tx pgx.Tx) error {
		var current string
		if err := tx.QueryRow(ctx, `
			SELECT role FROM users
			WHERE id = $1 AND tenant_id = $2 AND is_master = false AND deactivated_at IS NULL
		`, id, tenantID).Scan(&current); err != nil {
			return err
		}
		role = iam.Role(current)
		tag, err := tx.Exec(ctx, `
			UPDATE users SET deactivated_at = now()
			WHERE id = $1 AND tenant_id = $2 AND is_master = false AND deactivated_at IS NULL
			  AND (role <> 'tenant_gerente' OR EXISTS (
				SELECT 1 FROM users
				WHERE tenant_id = $2 AND role = 'tenant_gerente'
				  AND deactivated_at IS NULL AND id <> $1
			  ))
		`, id, tenantID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return domain.ErrLastGerente
		}
		return nil
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return "", domain.ErrUserNotFound
	}
	if errors.Is(err, domain.ErrLastGerente) {
		return "", domain.ErrLastGerente
	}
	if err != nil {
		return "", fmt.Errorf("tenantusers/postgres: Deactivate: %w", err)
	}
	return role, nil
}

// Reactivate clears deactivated_at on a currently-deactivated user.
func (s *Store) Reactivate(ctx context.Context, tenantID, id uuid.UUID) (iam.Role, error) {
	var role iam.Role
	err := postgres.WithTenant(ctx, s.pool, tenantID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			UPDATE users SET deactivated_at = NULL
			WHERE id = $1 AND tenant_id = $2 AND is_master = false AND deactivated_at IS NOT NULL
			RETURNING role
		`, id, tenantID).Scan((*string)(&role))
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return "", domain.ErrUserNotFound
	}
	if err != nil {
		return "", fmt.Errorf("tenantusers/postgres: Reactivate: %w", err)
	}
	return role, nil
}

// scanUser maps a row (id, tenant_id, email, role, deactivated_at, created_at)
// to a domain.User. Accepts either pgx.Row or pgx.Rows.
func scanUser(row interface {
	Scan(dest ...any) error
}) (domain.User, error) {
	var (
		u           domain.User
		role        string
		deactivated *time.Time
	)
	if err := row.Scan(&u.ID, &u.TenantID, &u.Email, &role, &deactivated, &u.CreatedAt); err != nil {
		return domain.User{}, err
	}
	u.Role = iam.Role(role)
	u.DeactivatedAt = deactivated
	return u, nil
}

func isTenantEmailConflict(err error) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return false
	}
	if pgErr.Code != pgUniqueViolation {
		return false
	}
	return pgErr.ConstraintName == tenantEmailUniqueConstraint ||
		strings.Contains(pgErr.Message, tenantEmailUniqueConstraint)
}
