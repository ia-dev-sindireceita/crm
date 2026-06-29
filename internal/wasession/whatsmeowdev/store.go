package whatsmeowdev

import (
	"context"

	"github.com/google/uuid"
	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/store/sqlstore"
	waLog "go.mau.fi/whatsmeow/util/log"
)

// sqlStore is the production StoreProvider, backed by a whatsmeow Postgres
// sqlstore container (ADR 0107 D3). It is exercised by integration wiring
// against a live database, not by unit tests (the unit tests drive the device
// and factory through fakes, per rule 5 — no DB mocking).
type sqlStore struct {
	container *sqlstore.Container
}

// Open connects the whatsmeow Postgres credential store at dsn and returns a
// Factory wired to it. Session credentials live entirely inside this store
// and are never logged (ADR 0107 D6); the no-op whatsmeow logger guarantees
// the library itself does not print them either.
func Open(ctx context.Context, dsn string, opts ...Option) (*Factory, error) {
	container, err := sqlstore.New(ctx, "postgres", dsn, waLog.Noop)
	if err != nil {
		return nil, err
	}
	return NewFactory(&sqlStore{container: container}, opts...), nil
}

// DeviceFor returns the per-tenant device store.
//
// ADR 0107 D3 / DT-WA-02: the final tenant->device isolation layout
// (tenant_id discriminator column vs schema-per-tenant) is settled in the
// migration ADR follow-up. Until that lands this resolves the container's
// single device, creating a fresh, unpaired one when none exists yet — which
// is correct for the single-tenant-per-container deployment shape and is the
// seam the migration child replaces without touching the device or domain.
func (s *sqlStore) DeviceFor(ctx context.Context, tenant uuid.UUID) (*store.Device, error) {
	dev, err := s.container.GetFirstDevice(ctx)
	if err != nil {
		return nil, err
	}
	if dev == nil {
		dev = s.container.NewDevice()
	}
	return dev, nil
}

var _ StoreProvider = (*sqlStore)(nil)
