package webchat

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/pericles-luz/crm/internal/inbox"
)

// Adapter is the Webchat inbound HTTP boundary. It is constructed
// once at startup and is safe for concurrent use across HTTP goroutines.
type Adapter struct {
	inbox    inbox.InboundChannel
	sessions SessionStore
	origins  OriginValidator
	flag     FeatureFlag
	rl       RateLimiter
	contacts ContactSignalUpdater // optional; nil skips email/phone re-resolve
	broker   *Broker
	logger   *slog.Logger
}

// New validates required dependencies and returns a ready Adapter.
func New(
	in inbox.InboundChannel,
	sessions SessionStore,
	origins OriginValidator,
	flag FeatureFlag,
	rl RateLimiter,
	broker *Broker,
	contacts ContactSignalUpdater,
) (*Adapter, error) {
	if in == nil {
		return nil, errors.New("webchat: InboundChannel is nil")
	}
	if sessions == nil {
		return nil, errors.New("webchat: SessionStore is nil")
	}
	if origins == nil {
		return nil, errors.New("webchat: OriginValidator is nil")
	}
	if flag == nil {
		return nil, errors.New("webchat: FeatureFlag is nil")
	}
	if rl == nil {
		return nil, errors.New("webchat: RateLimiter is nil")
	}
	if broker == nil {
		return nil, errors.New("webchat: Broker is nil")
	}
	return &Adapter{
		inbox:    in,
		sessions: sessions,
		origins:  origins,
		flag:     flag,
		rl:       rl,
		contacts: contacts,
		broker:   broker,
		logger:   slog.Default(),
	}, nil
}

// Register attaches the three widget endpoints to mux.
func (a *Adapter) Register(mux *http.ServeMux) {
	mux.HandleFunc("POST /widget/v1/session", a.handleSession)
	mux.HandleFunc("POST /widget/v1/message", a.handleMessage)
	mux.HandleFunc("GET /widget/v1/stream", a.handleStream)
}
