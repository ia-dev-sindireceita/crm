package wasession

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"go.uber.org/goleak"
)

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

// fakeDevice is a programmable Device used to drive the supervisor without a
// real WhatsApp connection. It is a transport fake, not a database mock.
type fakeDevice struct {
	tenant uuid.UUID
	sink   Sink
	paired bool

	mu           sync.Mutex
	connectCalls int
	disconnects  int

	// connectFn is invoked for each Connect with the 1-based call number.
	// When nil, Connect blocks until ctx is cancelled (a healthy live
	// connection).
	connectFn func(ctx context.Context, sink Sink, tenant uuid.UUID, call int) error
	// sendFn overrides SendText when set.
	sendFn func(ctx context.Context, to, body string) (string, error)
}

func (f *fakeDevice) Connect(ctx context.Context) error {
	f.mu.Lock()
	f.connectCalls++
	call := f.connectCalls
	fn := f.connectFn
	f.mu.Unlock()
	if fn != nil {
		return fn(ctx, f.sink, f.tenant, call)
	}
	<-ctx.Done()
	return ctx.Err()
}

func (f *fakeDevice) Disconnect() {
	f.mu.Lock()
	f.disconnects++
	f.mu.Unlock()
}

func (f *fakeDevice) SendText(ctx context.Context, to, body string) (string, error) {
	if f.sendFn != nil {
		return f.sendFn(ctx, to, body)
	}
	return "ext-id", nil
}

func (f *fakeDevice) Paired() bool { return f.paired }

func (f *fakeDevice) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.connectCalls
}

// fakeFactory hands out preconfigured devices keyed by tenant.
type fakeFactory struct {
	mu      sync.Mutex
	devices map[uuid.UUID]*fakeDevice
	err     error
}

func newFakeFactory() *fakeFactory {
	return &fakeFactory{devices: make(map[uuid.UUID]*fakeDevice)}
}

func (f *fakeFactory) set(tenant uuid.UUID, d *fakeDevice) {
	f.mu.Lock()
	defer f.mu.Unlock()
	d.tenant = tenant
	f.devices[tenant] = d
}

func (f *fakeFactory) NewDevice(ctx context.Context, tenant uuid.UUID, sink Sink) (Device, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	d, ok := f.devices[tenant]
	if !ok {
		d = &fakeDevice{tenant: tenant}
		f.devices[tenant] = d
	}
	d.sink = sink
	return d, nil
}

// drain consumes events into a slice until stop is closed, so the bounded
// events channel never blocks Emit during a test.
func drain(m *Manager, stop <-chan struct{}) *collector {
	c := &collector{}
	go func() {
		for {
			select {
			case ev := <-m.Events():
				c.add(ev)
			case <-stop:
				return
			}
		}
	}()
	return c
}

type collector struct {
	mu     sync.Mutex
	events []Event
}

func (c *collector) add(ev Event) {
	c.mu.Lock()
	c.events = append(c.events, ev)
	c.mu.Unlock()
}

func (c *collector) byKind(k EventKind) []Event {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []Event
	for _, ev := range c.events {
		if ev.Kind == k {
			out = append(out, ev)
		}
	}
	return out
}

func fastBackoff() Option { return WithBackoff(Backoff{Base: time.Millisecond, Max: time.Millisecond}) }

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("condition not met within deadline")
}

func TestStartSessionUnpairedInitialStatus(t *testing.T) {
	ff := newFakeFactory()
	tenant := uuid.New()
	ff.set(tenant, &fakeDevice{paired: false}) // Connect blocks on ctx
	m := NewManager(ff, fastBackoff())
	stop := make(chan struct{})
	c := drain(m, stop)

	if err := m.StartSession(context.Background(), tenant); err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	waitFor(t, func() bool { return len(c.byKind(EventStatus)) >= 1 })

	st, ok := m.Status(tenant)
	if !ok || st != StatusUnpaired {
		t.Fatalf("status = %q ok=%v, want unpaired", st, ok)
	}
	sc := c.byKind(EventStatus)[0]
	if sc.Status.To != StatusUnpaired || sc.Status.From != "" {
		t.Errorf("initial status event = %+v", sc.Status)
	}

	close(stop)
	if err := m.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
}

func TestStartSessionPairedInitialStatus(t *testing.T) {
	ff := newFakeFactory()
	tenant := uuid.New()
	ff.set(tenant, &fakeDevice{paired: true})
	m := NewManager(ff, fastBackoff())
	stop := make(chan struct{})
	drain(m, stop)

	if err := m.StartSession(context.Background(), tenant); err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	waitFor(t, func() bool {
		st, ok := m.Status(tenant)
		return ok && st == StatusDisconnected
	})

	close(stop)
	_ = m.Shutdown(context.Background())
}

func TestStartSessionDuplicate(t *testing.T) {
	ff := newFakeFactory()
	tenant := uuid.New()
	ff.set(tenant, &fakeDevice{paired: true})
	m := NewManager(ff)
	stop := make(chan struct{})
	drain(m, stop)

	if err := m.StartSession(context.Background(), tenant); err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	if err := m.StartSession(context.Background(), tenant); !errors.Is(err, ErrSessionExists) {
		t.Fatalf("duplicate StartSession err = %v, want ErrSessionExists", err)
	}

	close(stop)
	_ = m.Shutdown(context.Background())
}

func TestStartSessionFactoryError(t *testing.T) {
	ff := newFakeFactory()
	ff.err = errors.New("store open failed")
	m := NewManager(ff)
	defer func() { _ = m.Shutdown(context.Background()) }()

	err := m.StartSession(context.Background(), uuid.New())
	if err == nil || err.Error() != "store open failed" {
		t.Fatalf("StartSession err = %v, want factory error", err)
	}
}

func TestStartSessionAfterShutdown(t *testing.T) {
	ff := newFakeFactory()
	m := NewManager(ff)
	if err := m.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	// Shutdown is idempotent.
	if err := m.Shutdown(context.Background()); err != nil {
		t.Fatalf("second Shutdown: %v", err)
	}
	if err := m.StartSession(context.Background(), uuid.New()); !errors.Is(err, ErrManagerClosed) {
		t.Fatalf("StartSession after shutdown = %v, want ErrManagerClosed", err)
	}
}

func TestSupervisorReconnects(t *testing.T) {
	ff := newFakeFactory()
	tenant := uuid.New()
	dev := &fakeDevice{
		paired: true,
		connectFn: func(ctx context.Context, sink Sink, tn uuid.UUID, call int) error {
			if call >= 3 {
				<-ctx.Done() // settle into a healthy connection
				return ctx.Err()
			}
			return nil // clean disconnect -> triggers a reconnect
		},
	}
	ff.set(tenant, dev)
	m := NewManager(ff, fastBackoff())
	stop := make(chan struct{})
	drain(m, stop)

	if err := m.StartSession(context.Background(), tenant); err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	waitFor(t, func() bool { return dev.calls() >= 3 })

	close(stop)
	if err := m.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
}

func TestSupervisorReconnectsAfterConnectError(t *testing.T) {
	ff := newFakeFactory()
	tenant := uuid.New()
	dev := &fakeDevice{
		paired: true,
		connectFn: func(ctx context.Context, sink Sink, tn uuid.UUID, call int) error {
			if call >= 3 {
				<-ctx.Done()
				return ctx.Err()
			}
			return errors.New("connect refused") // failures grow backoff
		},
	}
	ff.set(tenant, dev)
	m := NewManager(ff, fastBackoff())
	stop := make(chan struct{})
	drain(m, stop)

	if err := m.StartSession(context.Background(), tenant); err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	waitFor(t, func() bool { return dev.calls() >= 3 })

	close(stop)
	_ = m.Shutdown(context.Background())
}

func TestSupervisorStopsWhenBanned(t *testing.T) {
	ff := newFakeFactory()
	tenant := uuid.New()
	dev := &fakeDevice{
		paired: true,
		connectFn: func(ctx context.Context, sink Sink, tn uuid.UUID, call int) error {
			sink.Emit(newStatusEvent(tn, StatusChange{From: StatusConnected, To: StatusBanned, Reason: "logged out"}))
			return nil
		},
	}
	ff.set(tenant, dev)
	m := NewManager(ff, fastBackoff())
	stop := make(chan struct{})
	drain(m, stop)

	if err := m.StartSession(context.Background(), tenant); err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	waitFor(t, func() bool {
		st, ok := m.Status(tenant)
		return ok && st == StatusBanned
	})
	// Give the supervisor a beat; it must NOT reconnect a banned session.
	time.Sleep(20 * time.Millisecond)
	if got := dev.calls(); got != 1 {
		t.Fatalf("banned session reconnected: connect calls = %d, want 1", got)
	}

	close(stop)
	_ = m.Shutdown(context.Background())
}

func TestEmitFansOutInbound(t *testing.T) {
	ff := newFakeFactory()
	tenant := uuid.New()
	dev := &fakeDevice{
		paired: true,
		connectFn: func(ctx context.Context, sink Sink, tn uuid.UUID, call int) error {
			sink.Emit(newStatusEvent(tn, StatusChange{To: StatusConnected}))
			sink.Emit(newInboundEvent(tn, InboundMessage{ExternalID: "wamid.7", SenderE164: "5511999", Body: "olá"}))
			<-ctx.Done()
			return ctx.Err()
		},
	}
	ff.set(tenant, dev)
	m := NewManager(ff, fastBackoff())
	stop := make(chan struct{})
	c := drain(m, stop)

	if err := m.StartSession(context.Background(), tenant); err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	waitFor(t, func() bool { return len(c.byKind(EventInbound)) >= 1 })

	inb := c.byKind(EventInbound)[0]
	if inb.Inbound.ExternalID != "wamid.7" || inb.Inbound.Body != "olá" {
		t.Errorf("inbound event = %+v", inb.Inbound)
	}

	close(stop)
	_ = m.Shutdown(context.Background())
}

func TestSendRoutesToConnectedSession(t *testing.T) {
	ff := newFakeFactory()
	tenant := uuid.New()
	var gotTo, gotBody string
	dev := &fakeDevice{
		paired: true,
		connectFn: func(ctx context.Context, sink Sink, tn uuid.UUID, call int) error {
			sink.Emit(newStatusEvent(tn, StatusChange{To: StatusConnected}))
			<-ctx.Done()
			return ctx.Err()
		},
		sendFn: func(ctx context.Context, to, body string) (string, error) {
			gotTo, gotBody = to, body
			return "wamid.out", nil
		},
	}
	ff.set(tenant, dev)
	m := NewManager(ff, fastBackoff())
	stop := make(chan struct{})
	drain(m, stop)

	if err := m.StartSession(context.Background(), tenant); err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	waitFor(t, func() bool {
		st, _ := m.Status(tenant)
		return st == StatusConnected
	})

	id, err := m.Send(context.Background(), tenant, "5511888", "hi")
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if id != "wamid.out" || gotTo != "5511888" || gotBody != "hi" {
		t.Errorf("send routed wrong: id=%q to=%q body=%q", id, gotTo, gotBody)
	}

	close(stop)
	_ = m.Shutdown(context.Background())
}

func TestSendErrors(t *testing.T) {
	ff := newFakeFactory()
	tenant := uuid.New()
	ff.set(tenant, &fakeDevice{paired: true}) // Connect blocks: stays disconnected
	m := NewManager(ff, fastBackoff())
	stop := make(chan struct{})
	drain(m, stop)

	if _, err := m.Send(context.Background(), uuid.New(), "x", "y"); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("Send to unknown tenant = %v, want ErrSessionNotFound", err)
	}

	if err := m.StartSession(context.Background(), tenant); err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	waitFor(t, func() bool {
		st, ok := m.Status(tenant)
		return ok && st == StatusDisconnected
	})
	if _, err := m.Send(context.Background(), tenant, "x", "y"); !errors.Is(err, ErrNotConnected) {
		t.Fatalf("Send to disconnected session = %v, want ErrNotConnected", err)
	}

	close(stop)
	_ = m.Shutdown(context.Background())
}

func TestStopSession(t *testing.T) {
	ff := newFakeFactory()
	tenant := uuid.New()
	dev := &fakeDevice{paired: true}
	ff.set(tenant, dev)
	m := NewManager(ff, fastBackoff())
	stop := make(chan struct{})
	drain(m, stop)

	if err := m.StartSession(context.Background(), tenant); err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	waitFor(t, func() bool { return dev.calls() >= 1 })

	if err := m.StopSession(tenant); err != nil {
		t.Fatalf("StopSession: %v", err)
	}
	if _, ok := m.Status(tenant); ok {
		t.Error("session still present after StopSession")
	}
	if err := m.StopSession(tenant); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("second StopSession = %v, want ErrSessionNotFound", err)
	}

	close(stop)
	_ = m.Shutdown(context.Background())
}

func TestShutdownTimesOut(t *testing.T) {
	ff := newFakeFactory()
	tenant := uuid.New()
	// Device whose Connect ignores ctx cancellation so the supervisor cannot
	// stop in time; Shutdown must honour its own deadline.
	block := make(chan struct{})
	dev := &fakeDevice{
		paired: true,
		connectFn: func(ctx context.Context, sink Sink, tn uuid.UUID, call int) error {
			<-block
			return nil
		},
	}
	ff.set(tenant, dev)
	m := NewManager(ff, fastBackoff())
	stop := make(chan struct{})
	drain(m, stop)

	if err := m.StartSession(context.Background(), tenant); err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	waitFor(t, func() bool { return dev.calls() >= 1 })

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := m.Shutdown(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Shutdown err = %v, want DeadlineExceeded", err)
	}

	// Release the stuck goroutine and let the manager settle for goleak.
	close(block)
	close(stop)
	waitFor(t, func() bool { return dev.calls() >= 1 })
	// Drain the now-finished supervisor.
	if err := m.Shutdown(context.Background()); err != nil {
		t.Fatalf("final Shutdown: %v", err)
	}
}
