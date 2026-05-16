package instagram

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/pericles-luz/crm/internal/inbox"
)

// Adapter is the Instagram Direct inbound HTTP boundary. It owns the
// HMAC-verified POST handler and the GET subscription-verify
// handshake; both register on the public mux via Register.
//
// The struct is constructed once at startup from validated config; no
// field is mutated after construction so the adapter is safe to share
// across the request goroutines net/http spawns.
type Adapter struct {
	cfg     Config
	inbox   inbox.InboundChannel
	tenants TenantResolver
	flag    FeatureFlag
	rate    RateLimiter
	media   MediaScanPublisher
	clock   Clock
	logger  *slog.Logger
}

// Option mutates an Adapter at construction time.
type Option func(*Adapter)

// WithClock replaces the default systemClock — pinned by unit tests.
func WithClock(c Clock) Option {
	return func(a *Adapter) {
		if c != nil {
			a.clock = c
		}
	}
}

// WithLogger replaces the default slog.Default logger.
func WithLogger(l *slog.Logger) Option {
	return func(a *Adapter) {
		if l != nil {
			a.logger = l
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

// New constructs an Adapter. Required dependencies are validated up
// front so a misconfigured composition root panics at startup rather
// than emitting a 500 on the first webhook delivery.
func New(cfg Config, in inbox.InboundChannel, t TenantResolver, f FeatureFlag, r RateLimiter, opts ...Option) (*Adapter, error) {
	if cfg.AppSecret == "" {
		return nil, errors.New("instagram: AppSecret is empty")
	}
	if cfg.VerifyToken == "" {
		return nil, errors.New("instagram: VerifyToken is empty")
	}
	if in == nil {
		return nil, errors.New("instagram: InboundChannel is nil")
	}
	if t == nil {
		return nil, errors.New("instagram: TenantResolver is nil")
	}
	if f == nil {
		return nil, errors.New("instagram: FeatureFlag is nil")
	}
	if r == nil {
		return nil, errors.New("instagram: RateLimiter is nil")
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

// Register attaches both webhook routes onto an existing stdlib mux.
// Callers MUST mount on the public listener — the routes are
// intentionally unauthenticated at the HTTP layer (HMAC + verify-token
// replace bearer auth here).
func (a *Adapter) Register(mux *http.ServeMux) {
	mux.HandleFunc("POST /webhooks/instagram", a.handlePost)
	mux.HandleFunc("GET /webhooks/instagram", a.handleChallenge)
}
