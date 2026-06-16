package inbox

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pericles-luz/crm/internal/adapter/db/postgres"
	domain "github.com/pericles-luz/crm/internal/inbox"
)

// Compile-time assertion: UserDirectory satisfies the domain read port.
var _ domain.UserDirectory = (*UserDirectory)(nil)

// UserDirectory is the pgx-backed adapter resolving tenant user IDs to
// display labels for the inbox list's assigned-atendente column
// (SIN-64967). The users table (migration 0005) carries no display-name
// column, so the label is derived from the email local-part; the UX
// sister issue (SIN-64966) owns the final presentation (initials, or a
// proper name once a name column lands). Every lookup runs through
// WithTenant so a label can never leak across tenants — RLS on `users`
// hides rows from other tenants even though the id set is caller-supplied.
type UserDirectory struct {
	pool postgres.TxBeginner
}

// NewUserDirectory wraps pool. A nil pool yields postgres.ErrNilPool.
func NewUserDirectory(pool *pgxpool.Pool) (*UserDirectory, error) {
	if pool == nil {
		return nil, postgres.ErrNilPool
	}
	return &UserDirectory{pool: pool}, nil
}

// LabelsByID returns labels for the user ids present under tenantID.
// Missing ids (unknown, deleted, or RLS-hidden cross-tenant rows) are
// simply absent from the map. An empty id set short-circuits to an empty
// map without touching the database.
func (d *UserDirectory) LabelsByID(ctx context.Context, tenantID uuid.UUID, ids []uuid.UUID) (map[uuid.UUID]string, error) {
	if tenantID == uuid.Nil {
		return nil, fmt.Errorf("inbox/postgres: LabelsByID: tenant id is nil")
	}
	out := make(map[uuid.UUID]string, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	err := postgres.WithTenant(ctx, d.pool, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT id, email FROM users WHERE id = ANY($1)
		`, ids)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var (
				id    uuid.UUID
				email string
			)
			if err := rows.Scan(&id, &email); err != nil {
				return err
			}
			out[id] = userLabelFromEmail(email)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("inbox/postgres: LabelsByID: %w", err)
	}
	return out, nil
}

// userLabelFromEmail derives a human label from a user's email. The users
// table has no display-name column (migration 0005), so the local-part
// (before "@") is the best available label. Inputs without a usable
// local-part fall back to the trimmed email so the UI always has
// something to render.
func userLabelFromEmail(email string) string {
	email = strings.TrimSpace(email)
	if i := strings.IndexByte(email, '@'); i > 0 {
		return email[:i]
	}
	return email
}
