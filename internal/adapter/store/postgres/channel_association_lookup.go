package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// ErrAssociationUnknown is returned by ChannelAssociationLookup when no
// tenant_channel_associations row matches the supplied (channel,
// association). Callers translate this to a silent drop via the
// errors.Is-comparable sentinel at their own boundary.
var ErrAssociationUnknown = errors.New("postgres: channel_association_lookup: no row")

// ChannelAssociationLookup is the reverse of TenantAssociationStore:
// given a (channel, association) pair it returns the owning tenant_id.
// The WhatsApp webhook adapter calls Resolve to translate
// phone_number_id into the inbox-side tenant uuid.
//
// The query targets the PRIMARY KEY (channel, association) on
// tenant_channel_associations from migration 0075a2 — a single
// index probe per call.
type ChannelAssociationLookup struct {
	db PgxConn
}

// NewChannelAssociationLookup returns a lookup bound to db.
func NewChannelAssociationLookup(db PgxConn) *ChannelAssociationLookup {
	return &ChannelAssociationLookup{db: db}
}

const associationLookupSQL = `
SELECT tenant_id
  FROM tenant_channel_associations
 WHERE channel = $1 AND association = $2
`

// Resolve returns the tenant_id that owns (channel, association). On
// no rows it returns ErrAssociationUnknown so the caller can short-
// circuit without inspecting pgx-specific errors.
func (l *ChannelAssociationLookup) Resolve(ctx context.Context, channel, association string) (uuid.UUID, error) {
	var raw [16]byte
	row := l.db.QueryRow(ctx, associationLookupSQL, channel, association)
	if err := row.Scan(&raw); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return uuid.Nil, ErrAssociationUnknown
		}
		return uuid.Nil, fmt.Errorf("association lookup: %w", err)
	}
	return uuid.UUID(raw), nil
}
