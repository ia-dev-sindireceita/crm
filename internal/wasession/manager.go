package wasession

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"
)

// Manager owns the per-tenant WhatsApp Web sessions (ADR 0107 D5): it
// supervises each session in its own goroutine, reconnecting with capped
// exponential backoff, stops auto-reconnect once a session is banned, and
// fans every inbound message / status / QR event out on a single channel.
//
// Manager implements Sink: each Device emits its events here, so the Device
// never needs a back-reference to the Manager beyond this narrow port.
//
// The events channel is never closed — Manager is wired once at server scope
// and lives for the process. Consumers select on the channel together with
// their own shutdown signal.
type Manager struct {
	factory DeviceFactory
	logger  *slog.Logger
	backoff Backoff

	baseCtx    context.Context
	baseCancel context.CancelFunc
	closeCh    chan struct{}

	mu       sync.Mutex
	sessions map[uuid.UUID]*session
	closed   bool
	wg       sync.WaitGroup

	events chan Event
}

type session struct {
	tenantID uuid.UUID
	device   Device
	cancel   context.CancelFunc
	done     chan struct{}
	status   Status // guarded by Manager.mu
}

// Option configures a Manager.
type Option func(*Manager)

// WithLogger sets the structured logger. The Manager only ever logs tenant
// ids and status — never message bodies, phone numbers or credentials.
func WithLogger(l *slog.Logger) Option {
	return func(m *Manager) {
		if l != nil {
			m.logger = l
		}
	}
}

// WithBackoff sets the reconnect backoff policy.
func WithBackoff(b Backoff) Option { return func(m *Manager) { m.backoff = b } }

// WithEventBuffer sets the buffer size of the events channel (default 64).
func WithEventBuffer(n int) Option {
	return func(m *Manager) {
		if n >= 0 {
			m.events = make(chan Event, n)
		}
	}
}

// NewManager builds a Manager over the given device factory.
func NewManager(factory DeviceFactory, opts ...Option) *Manager {
	ctx, cancel := context.WithCancel(context.Background())
	m := &Manager{
		factory:    factory,
		logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		backoff:    DefaultBackoff,
		baseCtx:    ctx,
		baseCancel: cancel,
		closeCh:    make(chan struct{}),
		sessions:   make(map[uuid.UUID]*session),
		events:     make(chan Event, 64),
	}
	for _, o := range opts {
		o(m)
	}
	return m
}

// Events returns the read side of the fan-out channel. It is never closed.
func (m *Manager) Events() <-chan Event { return m.events }

// Emit implements Sink. Devices call it (possibly concurrently) to report
// events. Status events update the tracked session state before fan-out so
// the supervisor's ban check and Send's connected check see fresh state.
func (m *Manager) Emit(ev Event) {
	m.mu.Lock()
	if ev.Kind == EventStatus && ev.Status != nil {
		if s, ok := m.sessions[ev.TenantID]; ok {
			s.status = ev.Status.To
		}
	}
	closed := m.closed
	m.mu.Unlock()
	if closed {
		return
	}
	select {
	case m.events <- ev:
	case <-m.closeCh:
	}
}

// StartSession creates and supervises a session for tenantID. It is an error
// to start a session that already exists or to start one after Shutdown.
func (m *Manager) StartSession(ctx context.Context, tenantID uuid.UUID) error {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return ErrManagerClosed
	}
	if _, ok := m.sessions[tenantID]; ok {
		m.mu.Unlock()
		return ErrSessionExists
	}
	m.mu.Unlock()

	device, err := m.factory.NewDevice(ctx, tenantID, m)
	if err != nil {
		return err
	}

	initial := StatusUnpaired
	if device.Paired() {
		initial = StatusDisconnected
	}

	sctx, cancel := context.WithCancel(m.baseCtx)
	sess := &session{
		tenantID: tenantID,
		device:   device,
		cancel:   cancel,
		done:     make(chan struct{}),
		status:   initial,
	}

	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		cancel()
		device.Disconnect()
		return ErrManagerClosed
	}
	if _, ok := m.sessions[tenantID]; ok {
		m.mu.Unlock()
		cancel()
		device.Disconnect()
		return ErrSessionExists
	}
	m.sessions[tenantID] = sess
	m.wg.Add(1)
	m.mu.Unlock()

	// Announce the starting state so consumers see the initial status.
	m.Emit(newStatusEvent(tenantID, StatusChange{From: "", To: initial, Reason: "session started"}))

	go m.supervise(sctx, sess)
	return nil
}

// supervise runs a session: it connects, and on every disconnect reconnects
// with backoff until the context is cancelled (clean stop) or the session is
// banned (terminal). A clean connection that later drops retries quickly;
// repeated connect failures grow the delay.
func (m *Manager) supervise(ctx context.Context, sess *session) {
	defer m.wg.Done()
	defer close(sess.done)

	failures := 0
	for {
		if ctx.Err() != nil {
			return
		}
		err := sess.device.Connect(ctx)
		if ctx.Err() != nil {
			return
		}
		if m.statusOf(sess).Terminal() {
			m.logger.Warn("wa session banned; not reconnecting", "tenant", sess.tenantID)
			return
		}
		if err != nil {
			failures++
			m.logger.Warn("wa session connect ended with error; will retry",
				"tenant", sess.tenantID, "failures", failures)
		} else {
			failures = 0
			m.logger.Info("wa session disconnected; will reconnect", "tenant", sess.tenantID)
		}
		delay := m.backoff.Delay(failures + 1)
		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
	}
}

func (m *Manager) statusOf(sess *session) Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	return sess.status
}

// Status returns the current status of a tenant's session.
func (m *Manager) Status(tenantID uuid.UUID) (Status, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[tenantID]
	if !ok {
		return "", false
	}
	return s.status, true
}

// Send delivers a plain-text outbound message through a tenant's session.
// The session must exist and be connected.
func (m *Manager) Send(ctx context.Context, tenantID uuid.UUID, toE164, body string) (string, error) {
	m.mu.Lock()
	s, ok := m.sessions[tenantID]
	if !ok {
		m.mu.Unlock()
		return "", ErrSessionNotFound
	}
	connected := s.status.Live()
	device := s.device
	m.mu.Unlock()

	if !connected {
		return "", ErrNotConnected
	}
	return device.SendText(ctx, toE164, body)
}

// StopSession stops and removes a tenant's session, waiting for its
// supervisor goroutine to exit.
func (m *Manager) StopSession(tenantID uuid.UUID) error {
	m.mu.Lock()
	s, ok := m.sessions[tenantID]
	if !ok {
		m.mu.Unlock()
		return ErrSessionNotFound
	}
	delete(m.sessions, tenantID)
	m.mu.Unlock()

	s.cancel()
	s.device.Disconnect()
	<-s.done
	return nil
}

// Shutdown stops every session and releases resources. It blocks until all
// supervisor goroutines exit or ctx is done. After Shutdown the Manager
// rejects new sessions and stops fanning out events.
func (m *Manager) Shutdown(ctx context.Context) error {
	m.mu.Lock()
	first := !m.closed
	var sessions []*session
	if first {
		m.closed = true
		close(m.closeCh)
		sessions = make([]*session, 0, len(m.sessions))
		for _, s := range m.sessions {
			sessions = append(sessions, s)
		}
		m.sessions = make(map[uuid.UUID]*session)
	}
	m.mu.Unlock()

	if first {
		m.baseCancel()
		for _, s := range sessions {
			s.device.Disconnect()
		}
	}

	// Always wait for the supervisors to exit, even on a repeat call after a
	// timed-out first Shutdown, so callers can block until fully drained.
	waited := make(chan struct{})
	go func() {
		m.wg.Wait()
		close(waited)
	}()
	select {
	case <-waited:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
