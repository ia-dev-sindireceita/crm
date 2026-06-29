package whatsmeowdev

import (
	"context"
	"time"

	"github.com/google/uuid"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/store"
	waLog "go.mau.fi/whatsmeow/util/log"

	"github.com/pericles-luz/crm/internal/wasession"
)

// StoreProvider resolves the whatsmeow credential store.Device for a tenant
// (ADR 0107 D3). The production implementation is backed by a Postgres
// sqlstore container (store.go); the final per-tenant isolation layout is
// settled in the migration ADR (DT-WA-02).
type StoreProvider interface {
	DeviceFor(ctx context.Context, tenant uuid.UUID) (*store.Device, error)
}

// sessionClient is the whatsmeow client capability the factory needs: every
// method the device uses at runtime plus event-handler registration.
type sessionClient interface {
	waClient
	AddEventHandler(handler whatsmeow.EventHandler) uint32
}

// clientFactory builds a sessionClient from a device store. It is a seam so
// the factory wiring is testable without a live whatsmeow client.
type clientFactory func(ds *store.Device, log waLog.Logger) sessionClient

func realClientFactory(ds *store.Device, log waLog.Logger) sessionClient {
	return whatsmeow.NewClient(ds, log)
}

// Factory implements wasession.DeviceFactory, building one whatsmeow-backed
// device per tenant and wiring its event handler into the supplied Sink.
type Factory struct {
	store     StoreProvider
	logger    waLog.Logger
	now       func() time.Time
	newClient clientFactory
}

// Option configures a Factory.
type Option func(*Factory)

// WithLogger sets the whatsmeow logger. It defaults to a no-op logger so the
// library never writes session details anywhere (ADR 0107 D6).
func WithLogger(l waLog.Logger) Option {
	return func(f *Factory) {
		if l != nil {
			f.logger = l
		}
	}
}

// WithClock overrides the clock used for QR expiry (tests inject a fixed one).
func WithClock(now func() time.Time) Option {
	return func(f *Factory) {
		if now != nil {
			f.now = now
		}
	}
}

// withClientFactory overrides the whatsmeow client constructor (test seam).
func withClientFactory(cf clientFactory) Option {
	return func(f *Factory) {
		if cf != nil {
			f.newClient = cf
		}
	}
}

// NewFactory builds a Factory over a StoreProvider.
func NewFactory(store StoreProvider, opts ...Option) *Factory {
	f := &Factory{
		store:     store,
		logger:    waLog.Noop,
		now:       time.Now,
		newClient: realClientFactory,
	}
	for _, o := range opts {
		o(f)
	}
	return f
}

// NewDevice implements wasession.DeviceFactory: it resolves the tenant's
// credential store, builds a whatsmeow client over it, and registers the
// device's event handler before returning the device to the Manager.
func (f *Factory) NewDevice(ctx context.Context, tenant uuid.UUID, sink wasession.Sink) (wasession.Device, error) {
	ds, err := f.store.DeviceFor(ctx, tenant)
	if err != nil {
		return nil, err
	}
	cli := f.newClient(ds, f.logger)
	d := newDevice(cli, tenant, sink, f.now)
	cli.AddEventHandler(d.handleEvent)
	return d, nil
}

var _ wasession.DeviceFactory = (*Factory)(nil)
