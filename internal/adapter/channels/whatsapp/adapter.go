package whatsapp

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/pericles-luz/crm/internal/inbox"
)

// Adapter is the WhatsApp Cloud-API inbound HTTP boundary. It owns
// the HMAC-verified POST handler and the GET subscription-verify
// handshake; both are registered on the public mux via Register.
//
// The struct is constructed once at startup from validated env config;
// no field is mutated after construction so the adapter is safe to
// share across the request goroutines spawned by net/http.
type Adapter struct {
	cfg            Config
	inbox          inbox.InboundChannel
	tenants        TenantResolver
	flag           FeatureFlag
	rate           RateLimiter
	clock          Clock
	logger         *slog.Logger
	handlerMetrics *handlerMetrics
	statusUpdater  inbox.MessageStatusUpdater
	statusMetrics  *statusMetrics
	// timestampWindowDrop is invoked when handlePost drops a webhook
	// because its envelope timestamp fell outside [now-PastWindow,
	// now+FutureSkew]. Production wires obs.Metrics.WebhookTimestampWindowDrop
	// (ADR 0094). Default is a no-op so tests that don't care about
	// the metric stay terse.
	timestampWindowDrop func(channel, direction string)
}

// Option mutates an Adapter at construction time. Tests use options to
// inject a fake Clock or a no-op Logger; production wires a system
// clock and a configured slog handler implicitly.
type Option func(*Adapter)

// WithClock replaces the default systemClock — used by unit tests to
// pin "now" for the timestamp-window check without sleeping.
func WithClock(c Clock) Option {
	return func(a *Adapter) {
		if c != nil {
			a.clock = c
		}
	}
}

// WithLogger replaces the default slog.Default logger. The logger is
// the only sink for per-event observability; the adapter never writes
// to stderr or stdout directly.
func WithLogger(l *slog.Logger) Option {
	return func(a *Adapter) {
		if l != nil {
			a.logger = l
		}
	}
}

// WithStatusUpdater wires the inbox.MessageStatusUpdater the adapter
// uses to reconcile carrier status updates (sent / delivered / read /
// failed). It is optional: without it the adapter still services
// inbound messages, and any statuses[] in the payload are logged +
// counted under outcome="dropped". Production wires the concrete
// UpdateMessageStatus use case in PR8 onward (SIN-62734).
func WithStatusUpdater(u inbox.MessageStatusUpdater) Option {
	return func(a *Adapter) {
		if u != nil {
			a.statusUpdater = u
		}
	}
}

// WithMetricsRegistry registers every Prometheus instrument the
// adapter owns onto reg in a single call:
//
//   - whatsapp_handler_elapsed_seconds (SIN-62762, handler-latency
//     histogram partitioned by terminal result)
//   - whatsapp_status_total (SIN-62734, status-update counter)
//   - whatsapp_status_lag_seconds (SIN-62734, Meta-timestamp lag)
//
// reg MUST be non-nil; production wires prometheus.DefaultRegisterer
// and tests inject a fresh prometheus.NewRegistry() to avoid
// duplicate-registration panics. Omitting this option leaves every
// instrument unregistered — every observe() call becomes a no-op so
// the adapter remains functional without metrics. See
// docs/runbooks/whatsapp-inbound-latency.md for the operational rule
// the handler histogram drives.
func WithMetricsRegistry(reg prometheus.Registerer) Option {
	return func(a *Adapter) {
		if reg != nil {
			a.handlerMetrics = newHandlerMetrics(reg)
			a.statusMetrics = newStatusMetrics(reg)
		}
	}
}

// WithTimestampWindowDropCounter wires the callback handlePost invokes
// when an envelope is dropped because its timestamp fell outside the
// configured replay window. cmd/server passes
// obs.Metrics.WebhookTimestampWindowDrop so the
// webhook_timestamp_window_drop_total counter (ADR 0094) increments
// per drop. A nil fn is accepted and treated as a no-op.
func WithTimestampWindowDropCounter(fn func(channel, direction string)) Option {
	return func(a *Adapter) {
		if fn != nil {
			a.timestampWindowDrop = fn
		}
	}
}

// New constructs an Adapter. Required dependencies are validated up
// front so a misconfigured composition root panics at startup rather
// than emitting a 500 on the first webhook delivery.
func New(cfg Config, in inbox.InboundChannel, t TenantResolver, f FeatureFlag, r RateLimiter, opts ...Option) (*Adapter, error) {
	if cfg.AppSecret == "" {
		return nil, errors.New("whatsapp: AppSecret is empty")
	}
	if cfg.VerifyToken == "" {
		return nil, errors.New("whatsapp: VerifyToken is empty")
	}
	if in == nil {
		return nil, errors.New("whatsapp: InboundChannel is nil")
	}
	if t == nil {
		return nil, errors.New("whatsapp: TenantResolver is nil")
	}
	if f == nil {
		return nil, errors.New("whatsapp: FeatureFlag is nil")
	}
	if r == nil {
		return nil, errors.New("whatsapp: RateLimiter is nil")
	}
	a := &Adapter{
		cfg:                 cfg,
		inbox:               in,
		tenants:             t,
		flag:                f,
		rate:                r,
		clock:               systemClock{},
		logger:              slog.Default(),
		timestampWindowDrop: func(string, string) {},
	}
	for _, opt := range opts {
		opt(a)
	}
	return a, nil
}

// Register attaches both webhook routes onto an existing stdlib mux at
// the canonical paths. Callers MUST register the adapter on the public
// listener — the routes are intentionally unauthenticated at the HTTP
// layer (HMAC + verify-token replace bearer auth here).
func (a *Adapter) Register(mux *http.ServeMux) {
	mux.HandleFunc("POST /webhooks/whatsapp", a.handlePost)
	mux.HandleFunc("GET /webhooks/whatsapp", a.handleChallenge)
}
