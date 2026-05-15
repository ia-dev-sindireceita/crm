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

// WithMetricsRegistry registers the inbound-handler latency histogram
// (whatsapp_handler_elapsed_seconds) on reg. Pass
// prometheus.DefaultRegisterer in cmd/server; pass
// prometheus.NewRegistry() inside tests to avoid duplicate-registration
// panics. Omitting this option leaves the histogram unregistered —
// every observe() call becomes a no-op so the adapter remains
// functional without metrics. See
// docs/runbooks/whatsapp-inbound-latency.md for the operational rule
// the histogram drives (SIN-62762).
func WithMetricsRegistry(reg prometheus.Registerer) Option {
	return func(a *Adapter) {
		if reg != nil {
			a.handlerMetrics = newHandlerMetrics(reg)
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
		cfg:     cfg,
		inbox:   in,
		tenants: t,
		flag:    f,
		rate:    r,
		clock:   systemClock{},
		logger:  slog.Default(),
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
