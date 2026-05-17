package pix

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	postgresadapter "github.com/pericles-luz/crm/internal/adapter/db/postgres"
	domainpix "github.com/pericles-luz/crm/internal/billing/pix"
)

// EventLogStore implements pix.EventLog against the webhook_events
// table (migration 0102). All writes route through WithMasterOps —
// migration 0102 grants INSERT/UPDATE/DELETE to app_master_ops and
// installs master_ops_audit on the table.
type EventLogStore struct {
	masterPool postgresadapter.TxBeginner
	actorID    uuid.UUID
}

// Compile-time port assertion.
var _ domainpix.EventLog = (*EventLogStore)(nil)

// NewEventLogStore wraps masterPool. actorID is the bot user-id
// recorded in master_ops_audit for every INSERT; uuid.Nil is rejected
// because the audit trigger demands a non-nil actor.
//
// masterPool is typed as postgresadapter.TxBeginner so production
// (*pgxpool.Pool) and tests (in-process fakes) can both satisfy the
// constructor without an adapter shim.
func NewEventLogStore(masterPool postgresadapter.TxBeginner, actorID uuid.UUID) (*EventLogStore, error) {
	if masterPool == nil {
		return nil, postgresadapter.ErrNilPool
	}
	if actorID == uuid.Nil {
		return nil, postgresadapter.ErrZeroActor
	}
	return &EventLogStore{masterPool: masterPool, actorID: actorID}, nil
}

const insertWebhookEvent = `
	INSERT INTO webhook_events (source, external_id, event_type, payload, received_at)
	VALUES ($1, $2, $3, $4::jsonb, $5)
`

// Record inserts the dedup row. A unique-violation on
// webhook_events_dedup_uniq translates to pix.ErrDuplicateEvent.
// payload is stored verbatim as jsonb; the receiver is expected to
// pass valid JSON bytes (the adapter only validates length, not shape).
func (s *EventLogStore) Record(
	ctx context.Context,
	source, externalID string,
	eventType domainpix.WebhookEventType,
	payload []byte,
	receivedAt time.Time,
) error {
	if source == "" {
		return fmt.Errorf("pix/postgres: Record: source is empty")
	}
	if externalID == "" {
		return domainpix.ErrEmptyExternalID
	}
	if !eventType.IsKnown() {
		return domainpix.ErrUnknownEventType
	}
	if len(payload) == 0 {
		payload = []byte(`{}`)
	}
	err := postgresadapter.WithMasterOps(ctx, s.masterPool, s.actorID, func(tx pgx.Tx) error {
		_, execErr := tx.Exec(ctx, insertWebhookEvent, source, externalID, string(eventType), payload, receivedAt)
		return execErr
	})
	if err == nil {
		return nil
	}
	if isUniqueViolation(err, "webhook_events_dedup_uniq") {
		return domainpix.ErrDuplicateEvent
	}
	return fmt.Errorf("pix/postgres: Record: %w", err)
}

// isUniqueViolation reports whether err is a pgconn.PgError carrying
// SQLSTATE 23505 on the named constraint. Mirrors the helper in the
// campaigns adapter; duplicated here so the package owns its own
// translator and the campaigns import does not leak in.
func isUniqueViolation(err error, constraint string) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return false
	}
	if pgErr.Code != "23505" {
		return false
	}
	return pgErr.ConstraintName == constraint
}
