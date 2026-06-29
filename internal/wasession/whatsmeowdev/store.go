package whatsmeowdev

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/google/uuid"
	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/store/sqlstore"
	waLog "go.mau.fi/whatsmeow/util/log"
)

// ErrTenantIsolation is returned by the production StoreProvider when a device
// is requested for a tenant the container cannot isolate from the tenant it is
// already bound to. A single whatsmeow sqlstore container holds exactly one
// device (DT-WA-02 single-tenant-per-container shape); serving a second,
// distinct tenant from it would resolve both tenants to the *same* WhatsApp
// session — a cross-tenant credential/session leak (BOLA). Until the migration
// ADR lands a real tenant discriminator, the only safe behaviour is to refuse,
// so the binding fails closed instead of silently aliasing tenants.
var ErrTenantIsolation = errors.New("whatsmeowdev: container cannot isolate this tenant")

// sqlStore is the production StoreProvider, backed by a whatsmeow Postgres
// sqlstore container (ADR 0107 D3). It is exercised by integration wiring
// against a live database, not by unit tests (the unit tests drive the device
// and factory through fakes, per rule 5 — no DB mocking). The tenant-binding
// guard (bindTenant) is pure and is unit-tested directly without a database.
type sqlStore struct {
	container *sqlstore.Container

	mu       sync.Mutex
	bound    uuid.UUID // the single tenant this container has been bound to
	boundSet bool      // whether bound has been assigned yet
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

// bindTenant enforces the single-tenant-per-container invariant (fail-closed).
//
// The zero UUID is rejected outright: a tenant discriminator is mandatory, and
// an absent/zero tenant must never resolve to "the first device". The first
// non-zero tenant seen binds the container; any subsequent request for a
// *different* tenant is refused with ErrTenantIsolation. Re-requesting the same
// tenant is idempotent. This is the seam the DT-WA-02 migration replaces with a
// real per-tenant discriminator; the dangerous shared-container path must stay
// an explicit, commented opt-in and is never the default.
func (s *sqlStore) bindTenant(tenant uuid.UUID) error {
	if tenant == uuid.Nil {
		return fmt.Errorf("%w: tenant discriminator is required", ErrTenantIsolation)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.boundSet {
		s.bound = tenant
		s.boundSet = true
		return nil
	}
	if s.bound != tenant {
		return fmt.Errorf("%w: container bound to tenant %s", ErrTenantIsolation, s.bound)
	}
	return nil
}

// DeviceFor returns the per-tenant device store.
//
// ADR 0107 D3 / DT-WA-02: the final tenant->device isolation layout (tenant_id
// discriminator column vs schema-per-tenant) is settled in the migration ADR
// follow-up. Until that lands a single container can only safely serve a single
// tenant, so DeviceFor fails closed (bindTenant) for any second, distinct
// tenant rather than silently aliasing it onto the first tenant's session. For
// the bound tenant it resolves the container's single device, creating a fresh,
// unpaired one when none exists yet — the seam the migration child replaces
// without touching the device or the domain.
func (s *sqlStore) DeviceFor(ctx context.Context, tenant uuid.UUID) (*store.Device, error) {
	if err := s.bindTenant(tenant); err != nil {
		return nil, err
	}
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
