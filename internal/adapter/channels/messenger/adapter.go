package messenger

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/pericles-luz/crm/internal/inbox"
)

// ErrUnknownPageID is returned by TenantResolver implementations when
// no tenant_channel_associations row matches the supplied page id. The
// handler treats this as a silent drop.
var ErrUnknownPageID = errors.New("messenger: unknown page_id")

// Adapter is the Messenger inbound HTTP boundary. It owns the
// HMAC-verified POST handler and the GET subscription-verify
// handshake; both are registered on the public mux via Register.
//
// The struct is constructed once at startup from validated env config;
// no field is mutated after construction so the adapter is safe to
// share across the request goroutines spawned by net/http.
type Adapter struct {
	cfg     Config
	inbox   inbox.InboundChannel
	tenants TenantResolver
	flag    FeatureFlag
	media   MediaScanPublisher
	clock   Clock
	logger  *slog.Logger
}

// Option mutates an Adapter at construction time. Tests use options to
// inject a fake Clock or a no-op Logger; production wires the system
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

// WithMediaScanPublisher wires the MediaScanPublisher the handler
// invokes when an inbound message carries attachments. Optional: when
// nil, the handler still persists the placeholder body but no
// `media.scan.requested` envelope is emitted (logged at warn). The
// composition root passes a NATS-backed publisher in production.
func WithMediaScanPublisher(p MediaScanPublisher) Option {
	return func(a *Adapter) {
		if p != nil {
			a.media = p
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

// New constructs an Adapter. Required dependencies are validated up
// front so a misconfigured composition root panics at startup rather
// than emitting a 500 on the first webhook delivery.
func New(cfg Config, in inbox.InboundChannel, t TenantResolver, f FeatureFlag, opts ...Option) (*Adapter, error) {
	if cfg.AppSecret == "" {
		return nil, errors.New("messenger: AppSecret is empty")
	}
	if cfg.VerifyToken == "" {
		return nil, errors.New("messenger: VerifyToken is empty")
	}
	if in == nil {
		return nil, errors.New("messenger: InboundChannel is nil")
	}
	if t == nil {
		return nil, errors.New("messenger: TenantResolver is nil")
	}
	if f == nil {
		return nil, errors.New("messenger: FeatureFlag is nil")
	}
	a := &Adapter{
		cfg:     cfg,
		inbox:   in,
		tenants: t,
		flag:    f,
		clock:   systemClock{},
		logger:  slog.Default(),
	}
	for _, opt := range opts {
		opt(a)
	}
	return a, nil
}

// Register attaches both webhook routes onto an existing stdlib mux at
// the canonical paths. The routes are intentionally unauthenticated at
// the HTTP layer — HMAC + verify-token replace bearer auth here.
func (a *Adapter) Register(mux *http.ServeMux) {
	mux.HandleFunc("POST /webhooks/messenger", a.handlePost)
	mux.HandleFunc("GET /webhooks/messenger", a.handleChallenge)
}
