// Package messenger implements inbox.OutboundChannel against the Meta
// Graph API send-message endpoint for the Messenger channel.
//
// The endpoint is POST /v18.0/<page_id>/messages. Unlike WhatsApp, the
// recipient is a page-scoped user ID (PSID) and there are no HSM
// templates — every send is freeform. Meta enforces the 24h conversation
// window server-side and rejects out-of-window freeform messages with a
// 4xx; we surface that as ErrChannelRejected (retries won't help).
//
// Three message types are supported:
//
//   - text — plain UTF-8 body
//   - image / video / file — attachment by URL (prefix "ms:image:", "ms:video:",
//     "ms:file:" followed by the URL)
//
// Reliability: 10s timeout per attempt, exponential backoff, 3 attempts max.
// No retry on 4xx — those are domain / policy errors.
//
// Security: META_GRAPH_TOKEN is placed in the Authorization header only.
// It is never logged, never embedded in error strings, and never appears
// in prometheus labels.
//
// SecretScope: AppLevel. The same system-user token signs all Messenger
// page calls; per-page scoping is done via the page_id path segment, not
// per-token.
package messenger

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/pericles-luz/crm/internal/inbox"
)

// DefaultBaseURL is the Meta Graph base URL targeted in production.
const DefaultBaseURL = "https://graph.facebook.com/v18.0"

// DefaultTimeout caps each HTTP attempt.
const DefaultTimeout = 10 * time.Second

// DefaultMaxAttempts caps the retry loop.
const DefaultMaxAttempts = 3

// DefaultBackoffBase is the initial backoff between retries.
const DefaultBackoffBase = 100 * time.Millisecond

// MediaPrefixes maps a body prefix to the Messenger attachment type.
// Format: "ms:image:<url>", "ms:video:<url>", "ms:file:<url>".
const (
	ImagePrefix = "ms:image:"
	VideoPrefix = "ms:video:"
	FilePrefix  = "ms:file:"
)

// TenantConfig is per-tenant configuration the sender needs.
type TenantConfig struct {
	// PageID is the Facebook Page ID whose messages API path segment is
	// /<page_id>/messages. An empty value is a misconfiguration exposed
	// as ErrChannelAuthFailed.
	PageID string
	// Enabled is the per-tenant feature flag (feature.messenger.enabled).
	// When false the sender returns ErrChannelDisabled without an HTTP call.
	Enabled bool
}

// TenantConfigLookup resolves a tenant ID to its sender config.
type TenantConfigLookup func(ctx context.Context, tenantID uuid.UUID) (TenantConfig, error)

// Sender is the Meta Graph API outbound adapter for the Messenger channel.
// It implements inbox.OutboundChannel.
type Sender struct {
	httpClient  *http.Client
	baseURL     string
	token       string
	config      TenantConfigLookup
	maxAttempts int
	backoffBase time.Duration
	metrics     *senderMetrics
}

// Option configures a Sender.
type Option func(*Sender)

// WithHTTPClient overrides the default http.Client.
func WithHTTPClient(c *http.Client) Option {
	return func(s *Sender) {
		if c != nil {
			s.httpClient = c
		}
	}
}

// WithBaseURL overrides the Meta Graph base URL. Tests point this at an
// httptest.Server.
func WithBaseURL(url string) Option {
	return func(s *Sender) {
		if url != "" {
			s.baseURL = url
		}
	}
}

// WithMaxAttempts overrides DefaultMaxAttempts. Values below 1 are ignored.
func WithMaxAttempts(n int) Option {
	return func(s *Sender) {
		if n >= 1 {
			s.maxAttempts = n
		}
	}
}

// WithBackoffBase overrides DefaultBackoffBase.
func WithBackoffBase(d time.Duration) Option {
	return func(s *Sender) {
		if d >= 0 {
			s.backoffBase = d
		}
	}
}

// New constructs a Sender. token, config, and reg are required.
func New(token string, config TenantConfigLookup, reg prometheus.Registerer, opts ...Option) (*Sender, error) {
	if token == "" {
		return nil, errors.New("messenger: META_GRAPH_TOKEN must not be empty")
	}
	if config == nil {
		return nil, errors.New("messenger: tenant config lookup must not be nil")
	}
	if reg == nil {
		return nil, errors.New("messenger: prometheus registerer must not be nil")
	}
	s := &Sender{
		httpClient:  &http.Client{Timeout: DefaultTimeout},
		baseURL:     DefaultBaseURL,
		token:       token,
		config:      config,
		maxAttempts: DefaultMaxAttempts,
		backoffBase: DefaultBackoffBase,
		metrics:     newSenderMetrics(reg),
	}
	for _, opt := range opts {
		opt(s)
	}
	return s, nil
}

// SendMessage implements inbox.OutboundChannel.
func (s *Sender) SendMessage(ctx context.Context, m inbox.OutboundMessage) (string, error) {
	start := time.Now()
	outcome := outcomeSuccess
	defer func() {
		s.metrics.duration.Observe(time.Since(start).Seconds())
		s.metrics.total.WithLabelValues(string(outcome)).Inc()
	}()

	cfg, err := s.config(ctx, m.TenantID)
	if err != nil {
		outcome = outcomeTransient
		return "", fmt.Errorf("%w: tenant config lookup: %v", inbox.ErrChannelTransient, err)
	}
	if !cfg.Enabled {
		outcome = outcomeDisabled
		return "", inbox.ErrChannelDisabled
	}
	if cfg.PageID == "" {
		outcome = outcomeAuth
		return "", fmt.Errorf("%w: tenant page_id missing", inbox.ErrChannelAuthFailed)
	}

	payload, err := encodePayload(m)
	if err != nil {
		outcome = outcomeRejected
		return "", fmt.Errorf("%w: %v", inbox.ErrChannelRejected, err)
	}

	url := strings.TrimRight(s.baseURL, "/") + "/" + cfg.PageID + "/messages"

	var lastErr error
	for attempt := 0; attempt < s.maxAttempts; attempt++ {
		if attempt > 0 {
			delay := s.backoffBase << uint(attempt-1)
			select {
			case <-ctx.Done():
				outcome = outcomeTransient
				return "", fmt.Errorf("%w: %v", inbox.ErrChannelTransient, ctx.Err())
			case <-time.After(delay):
			}
		}
		mid, sendErr := s.doRequest(ctx, url, payload)
		if sendErr == nil {
			outcome = outcomeSuccess
			return mid, nil
		}
		lastErr = sendErr
		if !errors.Is(sendErr, inbox.ErrChannelTransient) {
			switch {
			case errors.Is(sendErr, inbox.ErrChannelAuthFailed):
				outcome = outcomeAuth
			default:
				outcome = outcomeRejected
			}
			return "", sendErr
		}
	}
	outcome = outcomeTransient
	return "", lastErr
}

// doRequest performs a single Graph call and maps the HTTP outcome to an
// inbox.* sentinel.
func (s *Sender) doRequest(ctx context.Context, url string, body []byte) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("%w: build request: %v", inbox.ErrChannelTransient, err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+s.token)

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("%w: %v", inbox.ErrChannelTransient, err)
	}
	defer resp.Body.Close()

	respBody, readErr := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if readErr != nil {
		return "", fmt.Errorf("%w: read response: %v", inbox.ErrChannelTransient, readErr)
	}

	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		mid, perr := parseMID(respBody)
		if perr != nil {
			return "", fmt.Errorf("%w: %v", inbox.ErrChannelRejected, perr)
		}
		return mid, nil
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return "", fmt.Errorf("%w: status %d: %s", inbox.ErrChannelAuthFailed, resp.StatusCode, metaErrorMessage(respBody))
	case resp.StatusCode >= 400 && resp.StatusCode < 500:
		return "", fmt.Errorf("%w: status %d: %s", inbox.ErrChannelRejected, resp.StatusCode, metaErrorMessage(respBody))
	case resp.StatusCode >= 500:
		return "", fmt.Errorf("%w: status %d", inbox.ErrChannelTransient, resp.StatusCode)
	default:
		return "", fmt.Errorf("%w: unexpected status %d", inbox.ErrChannelTransient, resp.StatusCode)
	}
}

// encodePayload renders an OutboundMessage into the Meta Messenger send-API
// JSON payload. Attachment sends use a URL-based payload; no binary upload.
func encodePayload(m inbox.OutboundMessage) ([]byte, error) {
	if strings.TrimSpace(m.ToExternalID) == "" {
		return nil, errors.New("recipient (to_external_id/psid) is empty")
	}

	envelope := map[string]any{
		"recipient": map[string]string{"id": m.ToExternalID},
	}

	attType, url, isMedia := parseMediaBody(m.Body)
	if isMedia {
		if url == "" {
			return nil, fmt.Errorf("media body %q has empty URL", m.Body)
		}
		envelope["message"] = map[string]any{
			"attachment": map[string]any{
				"type":    attType,
				"payload": map[string]any{"url": url, "is_reusable": true},
			},
		}
	} else {
		body := strings.TrimSpace(m.Body)
		if body == "" {
			return nil, errors.New("text body is empty")
		}
		envelope["message"] = map[string]string{"text": body}
	}

	return json.Marshal(envelope)
}

// parseMediaBody checks for ms:image:, ms:video:, ms:file: prefixes.
// Returns (attachmentType, url, true) when a prefix is found.
func parseMediaBody(body string) (attType, url string, ok bool) {
	for prefix, typ := range map[string]string{
		ImagePrefix: "image",
		VideoPrefix: "video",
		FilePrefix:  "file",
	} {
		if strings.HasPrefix(body, prefix) {
			return typ, strings.TrimPrefix(body, prefix), true
		}
	}
	return "", "", false
}

// messengerResponse is the minimal subset of the Messenger send response.
type messengerResponse struct {
	MessageID string `json:"message_id"`
	Error     *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
		Code    int    `json:"code"`
	} `json:"error"`
}

func parseMID(body []byte) (string, error) {
	var r messengerResponse
	if err := json.Unmarshal(body, &r); err != nil {
		return "", fmt.Errorf("parse response: %v", err)
	}
	if strings.TrimSpace(r.MessageID) == "" {
		return "", errors.New("response missing message_id")
	}
	return r.MessageID, nil
}

func metaErrorMessage(body []byte) string {
	const cap = 256
	var r messengerResponse
	if err := json.Unmarshal(body, &r); err == nil && r.Error != nil {
		msg := r.Error.Message
		if len(msg) > cap {
			msg = msg[:cap]
		}
		return msg
	}
	if len(body) > cap {
		return string(body[:cap])
	}
	return string(body)
}

type outcome string

const (
	outcomeSuccess   outcome = "success"
	outcomeRejected  outcome = "rejected"
	outcomeTransient outcome = "transient"
	outcomeAuth      outcome = "auth_failed"
	outcomeDisabled  outcome = "disabled"
)

type senderMetrics struct {
	duration prometheus.Histogram
	total    *prometheus.CounterVec
}

func newSenderMetrics(reg prometheus.Registerer) *senderMetrics {
	duration := prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "messenger_send_duration_seconds",
		Help:    "Duration of Messenger outbound SendMessage calls (one observation per logical send, including retries).",
		Buckets: []float64{0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10},
	})
	total := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "messenger_send_total",
		Help: "Total Messenger send outcomes (success | rejected | transient | auth_failed | disabled).",
	}, []string{"outcome"})
	reg.MustRegister(duration, total)
	return &senderMetrics{duration: duration, total: total}
}

// Compile-time assertion.
var _ inbox.OutboundChannel = (*Sender)(nil)
