package postgres

import (
	"context"
	"fmt"

	"github.com/google/uuid"
)

// ChannelAssociationWriter is the write side of ChannelAssociationLookup:
// it upserts a (channel, association) → tenant mapping into
// tenant_channel_associations (migration 0075a2). The /settings/channels
// onboarding flow calls SaveAssociation when a tenant registers a WhatsApp
// API channel so the inbound webhook entry-point can later resolve the
// tenant from the Meta phone_number_id (SIN-67143).
type ChannelAssociationWriter struct {
	db PgxConn
}

// NewChannelAssociationWriter returns a writer bound to db.
func NewChannelAssociationWriter(db PgxConn) *ChannelAssociationWriter {
	return &ChannelAssociationWriter{db: db}
}

// associationUpsertSQL upserts on the (channel, association) PRIMARY KEY so
// re-registering a number that moved to a different tenant repoints the
// existing row instead of failing on a duplicate key.
const associationUpsertSQL = `
INSERT INTO tenant_channel_associations (tenant_id, channel, association)
VALUES ($1, $2, $3)
ON CONFLICT (channel, association) DO UPDATE SET tenant_id = EXCLUDED.tenant_id
`

// SaveAssociation upserts the (channel, association) → tenantID mapping.
// tenantID is passed as a uuid.UUID value so pgx's UUID codec encodes it
// for the uuid column (matching the channels adapter's insert idiom).
func (w *ChannelAssociationWriter) SaveAssociation(ctx context.Context, tenantID uuid.UUID, channel, association string) error {
	if _, err := w.db.Exec(ctx, associationUpsertSQL, tenantID, channel, association); err != nil {
		return fmt.Errorf("save channel association: %w", err)
	}
	return nil
}
