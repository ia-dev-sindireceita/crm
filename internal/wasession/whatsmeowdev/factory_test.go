package whatsmeowdev

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"

	"github.com/pericles-luz/crm/internal/wasession"
)

type fakeStore struct {
	dev *store.Device
	err error
}

func (s *fakeStore) DeviceFor(ctx context.Context, tenant uuid.UUID) (*store.Device, error) {
	return s.dev, s.err
}

func TestNewDeviceWiresHandlerIntoSink(t *testing.T) {
	t.Parallel()
	cli := &fakeClient{loggedIn: true}
	fs := &fakeStore{dev: &store.Device{}}
	sink := &fakeSink{}
	f := NewFactory(fs, withClientFactory(func(ds *store.Device, log waLog.Logger) sessionClient { return cli }))

	dev, err := f.NewDevice(context.Background(), uuid.New(), sink)
	if err != nil {
		t.Fatalf("NewDevice: %v", err)
	}
	if !dev.Paired() {
		t.Error("device should reflect the logged-in client")
	}
	if cli.handler == nil {
		t.Fatal("AddEventHandler was not wired")
	}
	cli.handler(&events.Connected{})
	if len(sink.byKind(wasession.EventStatus)) != 1 {
		t.Error("event handler is not routed into the sink")
	}
}

func TestNewDeviceStoreError(t *testing.T) {
	t.Parallel()
	f := NewFactory(&fakeStore{err: errors.New("store boom")})
	if _, err := f.NewDevice(context.Background(), uuid.New(), &fakeSink{}); err == nil || err.Error() != "store boom" {
		t.Fatalf("NewDevice err = %v, want store boom", err)
	}
}

func TestFactoryOptionsApplied(t *testing.T) {
	t.Parallel()
	fixed := time.Unix(5, 0)
	f := NewFactory(&fakeStore{dev: &store.Device{}},
		WithClock(func() time.Time { return fixed }),
		WithLogger(waLog.Noop),
	)
	if f.now().Unix() != 5 {
		t.Error("WithClock not applied")
	}
	if f.logger == nil {
		t.Error("logger must not be nil")
	}
}

func TestFactoryNilOptionsKeepDefaults(t *testing.T) {
	t.Parallel()
	f := NewFactory(&fakeStore{}, WithClock(nil), WithLogger(nil), withClientFactory(nil))
	if f.now == nil {
		t.Error("nil WithClock should keep default clock")
	}
	if f.logger == nil {
		t.Error("nil WithLogger should keep default logger")
	}
	if f.newClient == nil {
		t.Error("nil client factory should keep default")
	}
}
