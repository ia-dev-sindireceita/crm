// Package whatsapp implements inbox.OutboundChannel against the Meta
// Cloud API (Graph endpoint /<phone_number_id>/messages).
//
// Two send paths are supported:
//
//   - text (freeform). Valid only inside the 24h "customer-service window"
//     after the last inbound from the contact. The Meta API enforces the
//     window server-side; we surface its rejection as ErrChannelRejected.
//   - template. Required to open a conversation outside the 24h window.
//     The OutboundChannel port carries a single Body field, so callers
//     signal "send a template" by passing a body of the form
//     "wa:template:<name>:<lang>" (see TemplatePrefix). Parameters are
//     not in Fase 1 scope.
//
// Reliability:
//
//   - 10s timeout per HTTP attempt.
//   - Retry with exponential backoff on 5xx and network errors, capped
//     at 3 attempts total. No retry on 4xx — those are domain errors and
//     the carrier will keep returning them.
//
// Security:
//
//   - The Meta system-user token (META_GRAPH_TOKEN) is held in the
//     Sender value and placed in the Authorization header only.
//     It is never logged, never embedded in returned error strings, and
//     never appears in the prometheus labels. The redacting slog handler
//     (SIN-62255) is a second line of defence.
//
// Observability:
//
//   - whatsapp_send_duration_seconds is observed once per SendMessage
//     call (including retries) so dashboards see per-call latency, not
//     per-attempt fan-out.
//   - whatsapp_send_total{outcome=success|rejected|transient|auth_failed|disabled}
//     counts terminal outcomes; the same set covers metric-driven alerts
//     and the SLO board.
package whatsapp

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

// DefaultBaseURL is the Meta Graph base URL the sender targets in
// production. Tests inject an httptest.Server URL via WithBaseURL.
const DefaultBaseURL = "https://graph.facebook.com/v18.0"

// DefaultTimeout caps each HTTP attempt. Meta latency is occasionally
// >2s; 10s leaves room for one slow attempt without the caller's overall
// context budget being eaten by the retry loop.
const DefaultTimeout = 10 * time.Second

// DefaultMaxAttempts caps the retry loop. Three attempts at 100ms /
// 200ms backoff stays within the 24h-window critical path while
// absorbing the typical transient burst.
const DefaultMaxAttempts = 3

// DefaultBackoffBase is the initial backoff between retries. Subsequent
// waits are 2^n * base (100ms → 200ms → 400ms).
const DefaultBackoffBase = 100 * time.Millisecond

// TemplatePrefix is the marker in OutboundMessage.Body that flags a
// template send. Format: "wa:template:<name>:<lang_code>". Anything not
// matching the prefix is sent as a freeform text body.
const TemplatePrefix = "wa:template:"

// TenantConfig is per-tenant configuration the sender needs to issue a
// Meta API call. Production wiring resolves it from the
// tenant_channel_associations table (PR8 work). Tests inject a stub.
type TenantConfig struct {
	// PhoneNumberID is the Meta phone_number_id the tenant's WhatsApp
	// Business Account is bound to. It becomes a path segment in the
	// Graph URL, so an empty value is a misconfiguration the sender
	// surfaces as ErrChannelAuthFailed rather than dialling a 404.
	PhoneNumberID string
	// Enabled is the per-tenant feature flag (feature.whatsapp.enabled).
	// When false, the sender returns ErrChannelDisabled without making
	// any outbound HTTP call.
	Enabled bool
}

// TenantConfigLookup resolves a tenant ID to its sender config. PR8
// wires this through a tenant_channel_associations adapter; PR7 tests
// inject a closure.
//
// The lookup runs on the send critical path — implementations MUST be
// fast (cached in-memory) and MUST be ctx-cancellation aware.
type TenantConfigLookup func(ctx context.Context, tenantID uuid.UUID) (TenantConfig, error)

// Sender is the Meta Cloud API outbound adapter. It implements
// inbox.OutboundChannel; the use case in internal/inbox/usecase invokes
// SendMessage and treats the returned channel-external-id as the
// authoritative wamid for the outbound row.
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

// WithHTTPClient overrides the default http.Client (10s timeout). Used
// by tests to inject a transport that records requests.
func WithHTTPClient(c *http.Client) Option {
	return func(s *Sender) {
		if c != nil {
			s.httpClient = c
		}
	}
}

// WithBaseURL overrides the Meta Graph base URL. Tests point this at
// an httptest.Server.
func WithBaseURL(url string) Option {
	return func(s *Sender) {
		if url != "" {
			s.baseURL = url
		}
	}
}

// WithMaxAttempts overrides DefaultMaxAttempts. Must be >= 1; values
// below 1 are ignored (the production default stays in force).
func WithMaxAttempts(n int) Option {
	return func(s *Sender) {
		if n >= 1 {
			s.maxAttempts = n
		}
	}
}

// WithBackoffBase overrides DefaultBackoffBase. Tests use a near-zero
// duration so retry assertions stay fast.
func WithBackoffBase(d time.Duration) Option {
	return func(s *Sender) {
		if d >= 0 {
			s.backoffBase = d
		}
	}
}

// New constructs a Sender. token, config and reg are required. reg
// registers the prometheus counters so /metrics exposes them — pass
// prometheus.DefaultRegisterer in cmd/server and a fresh
// prometheus.NewRegistry() in tests to avoid duplicate-registration
// panics.
func New(token string, config TenantConfigLookup, reg prometheus.Registerer, opts ...Option) (*Sender, error) {
	if token == "" {
		return nil, errors.New("whatsapp: META_GRAPH_TOKEN must not be empty")
	}
	if config == nil {
		return nil, errors.New("whatsapp: tenant config lookup must not be nil")
	}
	if reg == nil {
		return nil, errors.New("whatsapp: prometheus registerer must not be nil")
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
//
// Outcome is observed exactly once via the deferred metrics update —
// retries on transient failures do NOT inflate the counter, which keeps
// the success/transient ratio meaningful for SLO purposes.
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
	if cfg.PhoneNumberID == "" {
		outcome = outcomeAuth
		return "", fmt.Errorf("%w: tenant phone_number_id missing", inbox.ErrChannelAuthFailed)
	}

	payload, err := encodePayload(m)
	if err != nil {
		outcome = outcomeRejected
		return "", fmt.Errorf("%w: %v", inbox.ErrChannelRejected, err)
	}

	url := strings.TrimRight(s.baseURL, "/") + "/" + cfg.PhoneNumberID + "/messages"

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
		wamid, sendErr := s.doRequest(ctx, url, payload)
		if sendErr == nil {
			outcome = outcomeSuccess
			return wamid, nil
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

// doRequest performs a single Graph call and maps the carrier outcome
// onto an inbox.* sentinel. The token is only placed in the
// Authorization header; it is never echoed into the returned error
// because metaErrorMessage operates on the response body, which never
// contains the bearer.
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
		wamid, perr := parseWAMID(respBody)
		if perr != nil {
			return "", fmt.Errorf("%w: %v", inbox.ErrChannelRejected, perr)
		}
		return wamid, nil
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

// encodePayload renders an OutboundMessage into the Meta Graph JSON
// payload. The dispatch rule lives here so the rest of the sender is
// transport-shaped, not domain-shaped.
func encodePayload(m inbox.OutboundMessage) ([]byte, error) {
	if strings.TrimSpace(m.ToExternalID) == "" {
		return nil, errors.New("recipient (to_external_id) is empty")
	}
	envelope := map[string]any{
		"messaging_product": "whatsapp",
		"to":                m.ToExternalID,
	}
	if strings.HasPrefix(m.Body, TemplatePrefix) {
		name, lang, ok := parseTemplateRef(m.Body)
		if !ok {
			return nil, fmt.Errorf("invalid template body %q (expected %s<name>:<lang>)", m.Body, TemplatePrefix)
		}
		envelope["type"] = "template"
		envelope["template"] = map[string]any{
			"name":     name,
			"language": map[string]string{"code": lang},
		}
	} else {
		if strings.TrimSpace(m.Body) == "" {
			return nil, errors.New("text body is empty")
		}
		envelope["type"] = "text"
		envelope["text"] = map[string]string{"body": m.Body}
	}
	return json.Marshal(envelope)
}

// parseTemplateRef parses "wa:template:<name>:<lang_code>" into the
// (name, lang) pair. Returns ok=false when either field is missing or
// blank after trimming.
func parseTemplateRef(body string) (name, lang string, ok bool) {
	rest := strings.TrimPrefix(body, TemplatePrefix)
	parts := strings.SplitN(rest, ":", 2)
	if len(parts) != 2 {
		return "", "", false
	}
	name = strings.TrimSpace(parts[0])
	lang = strings.TrimSpace(parts[1])
	if name == "" || lang == "" {
		return "", "", false
	}
	return name, lang, true
}

// metaResponse is the minimal subset of the Meta Cloud send-message
// response we read. messages[].id is the wamid; error.message is the
// human-readable rejection reason we surface to the operator.
type metaResponse struct {
	Messages []struct {
		ID string `json:"id"`
	} `json:"messages"`
	Error *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
		Code    int    `json:"code"`
	} `json:"error"`
}

func parseWAMID(body []byte) (string, error) {
	var r metaResponse
	if err := json.Unmarshal(body, &r); err != nil {
		return "", fmt.Errorf("parse response: %v", err)
	}
	if len(r.Messages) == 0 || strings.TrimSpace(r.Messages[0].ID) == "" {
		return "", errors.New("response missing messages[0].id")
	}
	return r.Messages[0].ID, nil
}

// metaErrorMessage returns a bounded slice of the Meta error message —
// at most 256 chars — so SRE has a breadcrumb without flooding logs.
// The token is never in the body (Meta does not echo the bearer), so
// surfacing this is safe.
func metaErrorMessage(body []byte) string {
	const cap = 256
	var r metaResponse
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

// outcome is the prometheus label set for whatsapp_send_total. Five
// outcomes line up 1:1 with the inbox.Err* sentinels plus disabled
// (feature flag) so the SLO board has a single column to alert on.
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

// newSenderMetrics registers the two whatsapp_send_* metrics on reg.
// Reg is mandatory at construction time, so this never silently no-ops.
func newSenderMetrics(reg prometheus.Registerer) *senderMetrics {
	duration := prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "whatsapp_send_duration_seconds",
		Help:    "Duration of WhatsApp outbound SendMessage calls (one observation per logical send, including retries).",
		Buckets: []float64{0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10},
	})
	total := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "whatsapp_send_total",
		Help: "Total WhatsApp send outcomes (success | rejected | transient | auth_failed | disabled).",
	}, []string{"outcome"})
	reg.MustRegister(duration, total)
	return &senderMetrics{duration: duration, total: total}
}

// Compile-time assertion that Sender implements inbox.OutboundChannel.
// If the port shape changes upstream this build breaks immediately,
// rather than failing only at runtime through the use case.
var _ inbox.OutboundChannel = (*Sender)(nil)
