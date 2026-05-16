// Package instagram implements the Meta Graph API outbound sender for
// the Instagram Direct channel.
//
// The endpoint is POST /v18.0/{ig-business-id}/messages. Per-tenant
// Graph access tokens are resolved via a TokenSource port and placed in
// the URL as the `access_token` query parameter — never logged, never
// embedded in returned errors.
//
// Two message types are supported:
//
//   - text — plain UTF-8 body via {"message":{"text":"..."}}
//   - media — image or video attachment by URL via
//     {"message":{"attachment":{"type":"image|video","payload":{"url":"..."}}}}
//
// Meta enforces a 24h customer-care window (since the last inbound
// message from the user). The sender mirrors the window check on the
// client side so an expired send is dropped before any HTTP call rather
// than relying on a Meta 4xx rejection.
//
// Reliability: 10s timeout per attempt; exponential backoff at 100ms
// base; 3 attempts on transport / 5xx; no retries on 4xx.
//
// Security: access tokens never appear in returned errors, log
// messages, or metric labels. net/http error strings would otherwise
// leak the URL (and therefore the query-string token) — the package
// classifies and scrubs every transport error before returning.
package instagram

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
)

// DefaultBaseURL is the Meta Graph base URL targeted in production.
const DefaultBaseURL = "https://graph.facebook.com/v18.0"

// DefaultTimeout caps each HTTP attempt.
const DefaultTimeout = 10 * time.Second

// DefaultMaxAttempts caps the retry loop.
const DefaultMaxAttempts = 3

// DefaultBackoffBase is the initial backoff between retries.
const DefaultBackoffBase = 100 * time.Millisecond

// DefaultOutboundWindow is the Meta 24h customer-care window used when
// SenderConfig.OutboundWindow is zero.
const DefaultOutboundWindow = 24 * time.Hour

// Sentinel errors. Callers use errors.Is to classify.
var (
	// ErrOutsideWindow indicates the 24h customer-care window has
	// expired (now - lastInboundAt > cfg.OutboundWindow). Delivering
	// outside the window requires a Message Tag flow (not modelled in
	// Fase 2).
	ErrOutsideWindow = errors.New("instagram: outside customer-care window")

	// ErrTokenUnknown indicates the TokenSource has no Graph access
	// token registered for the tenant. The send is dropped before any
	// HTTP call so an unconfigured tenant cannot accidentally trigger a
	// 401 against Meta.
	ErrTokenUnknown = errors.New("instagram: no access token for tenant")

	// ErrUnsupportedAttachment indicates SendMedia was called with an
	// attachment type other than image or video.
	ErrUnsupportedAttachment = errors.New("instagram: unsupported attachment type")

	// ErrEmptyBody / ErrEmptyURL guard against silent no-ops at the
	// boundary.
	ErrEmptyBody = errors.New("instagram: empty text body")
	ErrEmptyURL  = errors.New("instagram: empty media URL")

	// ErrTransport / ErrUpstream split retryable transport / 5xx from
	// terminal 4xx so callers can decide retry policy.
	ErrTransport = errors.New("instagram: transport error")
	ErrUpstream  = errors.New("instagram: upstream rejected")
)

// AttachmentType enumerates the Instagram Direct media types supported
// in Fase 2 (audio / file deferred).
type AttachmentType string

const (
	AttachmentImage AttachmentType = "image"
	AttachmentVideo AttachmentType = "video"
)

// Media bundles an attachment type with the URL Meta will fetch.
type Media struct {
	Type AttachmentType
	URL  string
}

// TokenSource resolves a per-tenant Graph API access token. Production
// wires a postgres-backed lookup against tenant_channel_associations
// (channel="instagram"); tests inject a stub.
//
// Returning ErrTokenUnknown is the documented "no token" signal. An
// empty string with a nil error is treated the same.
type TokenSource interface {
	AccessToken(ctx context.Context, tenantID uuid.UUID) (string, error)
}

// Clock decouples time.Now from the window check so unit tests can pin
// "now" without sleeping.
type Clock interface {
	Now() time.Time
}

type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now().UTC() }

// SenderConfig bundles per-process Sender settings. Zero values fall
// back to package defaults.
type SenderConfig struct {
	BaseURL        string
	HTTPClient     *http.Client
	MaxAttempts    int
	BackoffBase    time.Duration
	OutboundWindow time.Duration
	Logger         *slog.Logger
	Clock          Clock
}

// Sender posts Instagram Direct messages via the Meta Graph API. Safe
// for concurrent use.
type Sender struct {
	httpClient  *http.Client
	baseURL     string
	tokens      TokenSource
	window      time.Duration
	clock       Clock
	logger      *slog.Logger
	maxAttempts int
	backoffBase time.Duration
}

// NewSender constructs a Sender. tokens is required.
func NewSender(tokens TokenSource, cfg SenderConfig) (*Sender, error) {
	if tokens == nil {
		return nil, errors.New("instagram: TokenSource is nil")
	}
	s := &Sender{
		httpClient:  cfg.HTTPClient,
		baseURL:     strings.TrimRight(cfg.BaseURL, "/"),
		tokens:      tokens,
		window:      cfg.OutboundWindow,
		clock:       cfg.Clock,
		logger:      cfg.Logger,
		maxAttempts: cfg.MaxAttempts,
		backoffBase: cfg.BackoffBase,
	}
	if s.httpClient == nil {
		s.httpClient = &http.Client{Timeout: DefaultTimeout}
	}
	if s.baseURL == "" {
		s.baseURL = DefaultBaseURL
	}
	if s.window <= 0 {
		s.window = DefaultOutboundWindow
	}
	if s.clock == nil {
		s.clock = systemClock{}
	}
	if s.logger == nil {
		s.logger = slog.Default()
	}
	if s.maxAttempts < 1 {
		s.maxAttempts = DefaultMaxAttempts
	}
	if s.backoffBase <= 0 {
		s.backoffBase = DefaultBackoffBase
	}
	return s, nil
}

// SendText posts a text message and returns the Graph API message_id on
// success.
func (s *Sender) SendText(ctx context.Context, tenantID uuid.UUID, igsid, igBusinessID, text string, lastInboundAt time.Time) (string, error) {
	if strings.TrimSpace(text) == "" {
		return "", ErrEmptyBody
	}
	payload := map[string]any{
		"recipient": map[string]string{"id": igsid},
		"message":   map[string]string{"text": text},
	}
	return s.send(ctx, tenantID, igBusinessID, lastInboundAt, payload)
}

// SendMedia posts an image or video attachment and returns the Graph
// API message_id on success.
func (s *Sender) SendMedia(ctx context.Context, tenantID uuid.UUID, igsid, igBusinessID string, m Media, lastInboundAt time.Time) (string, error) {
	if m.Type != AttachmentImage && m.Type != AttachmentVideo {
		return "", fmt.Errorf("%w: %q", ErrUnsupportedAttachment, m.Type)
	}
	if strings.TrimSpace(m.URL) == "" {
		return "", ErrEmptyURL
	}
	payload := map[string]any{
		"recipient": map[string]string{"id": igsid},
		"message": map[string]any{
			"attachment": map[string]any{
				"type":    string(m.Type),
				"payload": map[string]any{"url": m.URL},
			},
		},
	}
	return s.send(ctx, tenantID, igBusinessID, lastInboundAt, payload)
}

func (s *Sender) send(ctx context.Context, tenantID uuid.UUID, igBusinessID string, lastInboundAt time.Time, payload map[string]any) (string, error) {
	if s.clock.Now().Sub(lastInboundAt) > s.window {
		return "", ErrOutsideWindow
	}
	token, err := s.tokens.AccessToken(ctx, tenantID)
	if err != nil {
		if errors.Is(err, ErrTokenUnknown) {
			return "", ErrTokenUnknown
		}
		return "", fmt.Errorf("instagram: token lookup: %w", err)
	}
	if token == "" {
		return "", ErrTokenUnknown
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("instagram: marshal payload: %w", err)
	}
	endpoint, err := s.endpointURL(igBusinessID, token)
	if err != nil {
		return "", err
	}
	var lastErr error
	for attempt := 0; attempt < s.maxAttempts; attempt++ {
		if attempt > 0 {
			delay := s.backoffBase << uint(attempt-1)
			select {
			case <-ctx.Done():
				return "", fmt.Errorf("%w: %v", ErrTransport, ctx.Err())
			case <-time.After(delay):
			}
		}
		mid, status, sendErr := s.doRequest(ctx, endpoint, body)
		s.logger.LogAttrs(ctx, slog.LevelDebug, "instagram.send",
			slog.Int("status", status),
			slog.String("ig_business_id", igBusinessID),
			slog.Int("attempt", attempt+1),
		)
		if sendErr == nil {
			return mid, nil
		}
		lastErr = sendErr
		if !errors.Is(sendErr, ErrTransport) {
			return "", sendErr
		}
	}
	return "", lastErr
}

// endpointURL composes the Graph URL. The token never leaves this
// function except inside the returned URL string used by the request
// builder.
func (s *Sender) endpointURL(igBusinessID, token string) (string, error) {
	if strings.TrimSpace(igBusinessID) == "" {
		return "", errors.New("instagram: ig_business_id is empty")
	}
	q := url.Values{}
	q.Set("access_token", token)
	return s.baseURL + "/" + igBusinessID + "/messages?" + q.Encode(), nil
}

// doRequest performs one HTTP POST and maps the outcome.
func (s *Sender) doRequest(ctx context.Context, endpoint string, body []byte) (string, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return "", 0, fmt.Errorf("%w: build request", ErrTransport)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.httpClient.Do(req)
	if err != nil {
		// net/http embeds the URL (and therefore the access_token query
		// param) in err.Error(); classify+scrub instead of wrapping.
		return "", 0, fmt.Errorf("%w: %s", ErrTransport, classifyHTTPErr(err))
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		mid, perr := parseMID(respBody)
		if perr != nil {
			return "", resp.StatusCode, fmt.Errorf("%w: %v", ErrUpstream, perr)
		}
		return mid, resp.StatusCode, nil
	case resp.StatusCode >= 500:
		return "", resp.StatusCode, fmt.Errorf("%w: status %d", ErrTransport, resp.StatusCode)
	default:
		return "", resp.StatusCode, fmt.Errorf("%w: status %d", ErrUpstream, resp.StatusCode)
	}
}

func classifyHTTPErr(err error) string {
	if err == nil {
		return ""
	}
	msg := err.Error()
	switch {
	case strings.Contains(msg, "deadline"), strings.Contains(msg, "timeout"):
		return "timeout"
	case strings.Contains(msg, "canceled"):
		return "canceled"
	case strings.Contains(msg, "refused"):
		return "connection refused"
	default:
		return "transport error"
	}
}

type sendResponse struct {
	MessageID string `json:"message_id"`
}

func parseMID(body []byte) (string, error) {
	var r sendResponse
	if err := json.Unmarshal(body, &r); err != nil {
		return "", fmt.Errorf("parse response: %v", err)
	}
	if strings.TrimSpace(r.MessageID) == "" {
		return "", errors.New("response missing message_id")
	}
	return r.MessageID, nil
}
