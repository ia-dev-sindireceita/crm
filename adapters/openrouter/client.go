package openrouter

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// defaultBaseURL is the OpenRouter v1 API root. Tests override this
// via Config.BaseURL with the httptest.NewServer URL.
const defaultBaseURL = "https://openrouter.ai/api/v1"

// DefaultModel is the Gemini Flash model selected per Fase 3 decision
// #2 as the cost/latency default. Callers may override on a per-request
// basis via CompleteRequest.Model; W2C's policy resolver supplies the
// effective model.
const DefaultModel = "google/gemini-2.0-flash"

// FallbackModel is the Anthropic Haiku tier that ops can flip to if
// Gemini Flash is degraded. The adapter does not implement provider
// failover itself — it just exposes the constant so cmd/server and
// W2C use the same canonical string.
const FallbackModel = "anthropic/claude-haiku-4.5"

// defaultTimeout is the per-call deadline applied when the caller's
// context has no earlier deadline. 8s matches the ADR-0040 p99 budget;
// retries share the same wall-clock budget so a slow upstream cannot
// burn 8s × 3 attempts.
const defaultTimeout = 8 * time.Second

// backoffSchedule lists the wait between attempts. Length is the
// maximum number of retries (3 — first attempt + 3 retries = 4 total).
// Values match the issue spec: 250ms, 500ms, 1s.
var backoffSchedule = []time.Duration{
	250 * time.Millisecond,
	500 * time.Millisecond,
	1 * time.Second,
}

// CompleteRequest is the input to Client.Complete. Fields mirror the
// W2C LLMClient port spec: prompt + model + maxTokens + idempotencyKey.
// Defining the struct here (rather than importing it from
// internal/aiassist) lets W3A land before W2C: Go interface
// satisfaction is structural, so W2C's port can accept this concrete
// type or wrap it as it sees fit.
type CompleteRequest struct {
	// Prompt is the full user message sent to the model. The adapter
	// does NOT redact, anonymise, or otherwise transform the prompt —
	// that responsibility lives in the PII anonymizer (SIN-62350 W3B),
	// which sits between the use case and this adapter.
	Prompt string

	// Model selects which upstream model receives the request. Empty
	// falls back to DefaultModel so callers that don't have a policy
	// resolver wired (notably early integration tests) still work.
	Model string

	// MaxTokens caps the completion length. 0 means "use the model
	// default", which OpenRouter currently treats as an open-ended
	// budget — callers SHOULD set a positive value to bound cost.
	MaxTokens int

	// IdempotencyKey is the (tenant_id, conversation_id, request_id)
	// triple computed by the use case. It is forwarded as the
	// X-Idempotency-Key request header so that a retry of the same
	// logical request hits the same cached response upstream. Empty
	// means "no idempotency" — the header is omitted entirely.
	IdempotencyKey string
}

// CompleteResponse is the output from Client.Complete. Tokens are
// reported separately so the wallet debit (W2C step 6) can charge
// in + out at the correct rate when those rates diverge.
type CompleteResponse struct {
	Text      string
	TokensIn  int64
	TokensOut int64
}

// Config carries the construction-time knobs for a Client. APIKey is
// the only required field; the rest fall back to ADR-0040 defaults.
type Config struct {
	APIKey     string
	BaseURL    string        // default https://openrouter.ai/api/v1
	HTTPClient *http.Client  // default: a Client with Timeout=defaultTimeout
	Timeout    time.Duration // default 8s; ignored if HTTPClient is set
	Logger     *slog.Logger  // default slog.Default
	Metrics    *Metrics      // default nil — observe/addTokens are nil-safe
}

// Client is the OpenRouter chat-completions adapter. It is safe for
// concurrent use: all mutable state is owned by the underlying
// *http.Client, which is itself concurrency-safe.
//
// The zero value is NOT usable — callers must go through New.
type Client struct {
	httpClient *http.Client
	baseURL    string
	apiKey     string
	logger     *slog.Logger
	metrics    *Metrics
	timeout    time.Duration
	now        func() time.Time
}

// New constructs a Client from cfg. It validates that APIKey is set so
// missing-secret bugs surface at boot rather than on the first chat
// request. Returns an error rather than panicking so cmd/server can
// distinguish "secret not wired" from "binary not built".
func New(cfg Config) (*Client, error) {
	if strings.TrimSpace(cfg.APIKey) == "" {
		return nil, errors.New("openrouter: APIKey is required")
	}
	baseURL := cfg.BaseURL
	if baseURL == "" {
		baseURL = defaultBaseURL
	}
	if _, err := url.Parse(baseURL); err != nil {
		return nil, fmt.Errorf("openrouter: invalid BaseURL: %w", err)
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	httpClient := cfg.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{
			Timeout:   timeout,
			Transport: newLogTransport(http.DefaultTransport, logger),
		}
	} else if httpClient.Transport == nil {
		// Caller supplied an HTTPClient but no Transport — wrap the
		// default transport so logs/metrics still flow.
		httpClient.Transport = newLogTransport(http.DefaultTransport, logger)
	} else if _, alreadyWrapped := httpClient.Transport.(*logTransport); !alreadyWrapped {
		httpClient.Transport = newLogTransport(httpClient.Transport, logger)
	}
	return &Client{
		httpClient: httpClient,
		baseURL:    strings.TrimRight(baseURL, "/"),
		apiKey:     cfg.APIKey,
		logger:     logger,
		metrics:    cfg.Metrics,
		timeout:    timeout,
		now:        time.Now,
	}, nil
}

// openRouterChatRequest is the on-the-wire JSON the adapter sends.
// Field names match the OpenRouter chat-completions schema. Defining
// it locally (rather than vendoring an SDK) is the boring-tech default
// for this codebase.
type openRouterChatRequest struct {
	Model     string                  `json:"model"`
	Messages  []openRouterChatMessage `json:"messages"`
	MaxTokens int                     `json:"max_tokens,omitempty"`
}

type openRouterChatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type openRouterChatResponse struct {
	Choices []openRouterChatChoice `json:"choices"`
	Usage   openRouterChatUsage    `json:"usage"`
}

type openRouterChatChoice struct {
	Message openRouterChatMessage `json:"message"`
}

type openRouterChatUsage struct {
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
}

// Complete implements the W2C aiassist.LLMClient port: it sends the
// prompt to OpenRouter, retries transient failures with the configured
// backoff schedule, and returns the decoded completion plus token
// counts.
//
// Retry policy (per issue spec):
//
//   - 5xx and transient network errors: retry up to len(backoffSchedule)
//     extra attempts after the initial one.
//   - 429 Too Many Requests: same retry rule, but honour the
//     Retry-After header when present (clamped to the wall-clock
//     budget of the original context).
//   - 4xx (other than 429): no retry — return ErrBadRequest immediately.
//   - context.DeadlineExceeded / context.Canceled: no further retries.
//
// On success the wall-clock duration is observed under
// {model, outcome="ok"}, and the in/out token counts are added to
// openrouter_tokens_consumed_total. Failure paths still observe
// duration under the appropriate {model, outcome=<failure>} label so
// SLO panels can split latency by success vs failure without dragging
// in another metric.
func (c *Client) Complete(ctx context.Context, req CompleteRequest) (CompleteResponse, error) {
	if strings.TrimSpace(req.Prompt) == "" {
		return CompleteResponse{}, fmt.Errorf("%w: prompt is empty", ErrBadRequest)
	}
	model := req.Model
	if model == "" {
		model = DefaultModel
	}

	// Apply the per-call deadline if the caller didn't supply one
	// shorter than ours. The deadline covers ALL retry attempts so a
	// slow upstream cannot burn timeout × retries of wall-clock time.
	callCtx, cancel := contextWithTimeout(ctx, c.timeout)
	defer cancel()
	callCtx = context.WithValue(callCtx, requestModelCtxKey{}, model)

	body, err := json.Marshal(openRouterChatRequest{
		Model: model,
		Messages: []openRouterChatMessage{
			{Role: "user", Content: req.Prompt},
		},
		MaxTokens: req.MaxTokens,
	})
	if err != nil {
		// json.Marshal of a fixed-shape struct cannot fail in practice,
		// but returning the error is cheaper than ignoring it.
		return CompleteResponse{}, fmt.Errorf("openrouter: marshal request: %w", err)
	}

	endpoint := c.baseURL + "/chat/completions"
	maxAttempts := len(backoffSchedule) + 1
	start := c.now()
	var lastErr error

	for attempt := 0; attempt < maxAttempts; attempt++ {
		if err := callCtx.Err(); err != nil {
			c.metrics.observe(model, outcomeForCtx(err), c.now().Sub(start).Seconds())
			return CompleteResponse{}, c.wrapCtxErr(err)
		}

		resp, sendErr := c.sendOne(callCtx, endpoint, body, req.IdempotencyKey)
		if sendErr == nil {
			// Got an HTTP response — decide if it's terminal.
			outcome, terminal, decoded, errFromResp := c.handleResponse(resp, model)
			resp.Body.Close()
			if terminal {
				c.metrics.observe(model, outcome, c.now().Sub(start).Seconds())
				if errFromResp == nil {
					c.metrics.addTokens(model, decoded.TokensIn, decoded.TokensOut)
				}
				return decoded, errFromResp
			}
			// Retryable failure — store the error and continue.
			lastErr = errFromResp
			if err := c.waitBeforeRetry(callCtx, attempt, retryAfterFromResp(resp)); err != nil {
				c.metrics.observe(model, outcomeForCtx(err), c.now().Sub(start).Seconds())
				return CompleteResponse{}, c.wrapCtxErr(err)
			}
			continue
		}

		// Transport-level failure (DNS, connection refused, TLS, etc).
		// Distinguish context errors (terminal) from generic network
		// errors (retryable).
		if ctxErr := callCtx.Err(); ctxErr != nil {
			c.metrics.observe(model, outcomeForCtx(ctxErr), c.now().Sub(start).Seconds())
			return CompleteResponse{}, c.wrapCtxErr(ctxErr)
		}
		if !isTransientNetErr(sendErr) {
			c.metrics.observe(model, "bad_request", c.now().Sub(start).Seconds())
			return CompleteResponse{}, fmt.Errorf("openrouter: send: %w", sendErr)
		}
		lastErr = sendErr
		if err := c.waitBeforeRetry(callCtx, attempt, 0); err != nil {
			c.metrics.observe(model, outcomeForCtx(err), c.now().Sub(start).Seconds())
			return CompleteResponse{}, c.wrapCtxErr(err)
		}
	}

	// Retry budget exhausted: classify the last error so callers can
	// branch on errors.Is.
	c.metrics.observe(model, finalOutcomeFor(lastErr), c.now().Sub(start).Seconds())
	if lastErr != nil {
		if errors.Is(lastErr, ErrRateLimited) {
			return CompleteResponse{}, ErrRateLimited
		}
		return CompleteResponse{}, fmt.Errorf("%w: %v", ErrUpstream5xx, lastErr)
	}
	return CompleteResponse{}, ErrUpstream5xx
}

// sendOne builds and dispatches a single HTTP attempt. The body
// argument is the marshalled JSON; bytes.NewReader makes the body
// re-readable across retries without re-marshalling.
func (c *Client) sendOne(ctx context.Context, endpoint string, body []byte, idempotencyKey string) (*http.Response, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("openrouter: build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+c.apiKey)
	if idempotencyKey != "" {
		httpReq.Header.Set("X-Idempotency-Key", idempotencyKey)
	}
	return c.httpClient.Do(httpReq)
}

// handleResponse classifies an HTTP response and, on terminal outcomes,
// decodes the body. The boolean return is true when the loop should
// stop (success OR non-retryable failure). When it returns false, the
// caller is expected to back off and try again.
func (c *Client) handleResponse(resp *http.Response, model string) (outcome string, terminal bool, decoded CompleteResponse, err error) {
	switch {
	case resp.StatusCode == http.StatusOK:
		var payload openRouterChatResponse
		if decodeErr := json.NewDecoder(resp.Body).Decode(&payload); decodeErr != nil {
			return "invalid_response", true, CompleteResponse{}, fmt.Errorf("%w: %v", ErrInvalidResponse, decodeErr)
		}
		if len(payload.Choices) == 0 || payload.Choices[0].Message.Content == "" {
			return "invalid_response", true, CompleteResponse{}, ErrInvalidResponse
		}
		return "ok", true, CompleteResponse{
			Text:      payload.Choices[0].Message.Content,
			TokensIn:  payload.Usage.PromptTokens,
			TokensOut: payload.Usage.CompletionTokens,
		}, nil
	case resp.StatusCode == http.StatusTooManyRequests:
		// Drain and discard the body so the underlying connection can
		// be reused on the next attempt.
		_, _ = io.Copy(io.Discard, resp.Body)
		return "rate_limited", false, CompleteResponse{}, ErrRateLimited
	case resp.StatusCode >= 500:
		_, _ = io.Copy(io.Discard, resp.Body)
		return "upstream_5xx", false, CompleteResponse{}, fmt.Errorf("openrouter: upstream status %d", resp.StatusCode)
	default:
		// 4xx other than 429 — terminal, no retry. Drain body for
		// connection reuse; do NOT include body in the error message
		// because it may echo the prompt back.
		_, _ = io.Copy(io.Discard, resp.Body)
		return "bad_request", true, CompleteResponse{}, fmt.Errorf("%w: status %d", ErrBadRequest, resp.StatusCode)
	}
}

// waitBeforeRetry sleeps according to backoffSchedule[attempt] OR a
// honored Retry-After value, whichever is larger. attempt is the
// zero-based index of the attempt that JUST failed; we wait
// schedule[attempt] before trying attempt+1.
func (c *Client) waitBeforeRetry(ctx context.Context, attempt int, retryAfter time.Duration) error {
	if attempt >= len(backoffSchedule) {
		// Exhausted — the caller's loop will exit on the next iteration.
		return nil
	}
	wait := backoffSchedule[attempt]
	if retryAfter > wait {
		wait = retryAfter
	}
	// Clamp wait to the remaining deadline so we never sleep past it.
	if deadline, ok := ctx.Deadline(); ok {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return context.DeadlineExceeded
		}
		if wait > remaining {
			wait = remaining
		}
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// retryAfterFromResp parses the Retry-After response header (RFC 7231
// — either delta-seconds or HTTP-date). Returns 0 if absent or
// unparseable; the caller falls back to the configured backoff.
func retryAfterFromResp(resp *http.Response) time.Duration {
	if resp == nil {
		return 0
	}
	v := resp.Header.Get("Retry-After")
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && secs > 0 {
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		if delta := time.Until(t); delta > 0 {
			return delta
		}
	}
	return 0
}

// isTransientNetErr decides whether a transport-level error is worth
// retrying. Timeout-like errors and *net.OpError / DNS lookup errors
// are transient; URL parse errors and io.ErrUnexpectedEOF on the
// request body are not.
func isTransientNetErr(err error) bool {
	if err == nil {
		return false
	}
	// url.Error wraps transport-layer failures from http.Client.Do.
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		// Timeout-classified url.Errors are transient.
		if urlErr.Timeout() {
			return true
		}
		// Unwrap further: net.OpError, *net.DNSError, etc.
		err = urlErr.Err
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		// net.Error reports Timeout(); treat both timeout and
		// temporary (deprecated but still used by net package) as
		// transient.
		if netErr.Timeout() {
			return true
		}
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		return true
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return true
	}
	// EOF on connection setup is transient (server closed before
	// returning a status).
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	return false
}

// outcomeForCtx maps a context error into the metric outcome label.
// Today the context package only emits DeadlineExceeded and Canceled,
// so the default branch is defensive: if a future Go release adds a
// new context-error class we record it under "unknown" rather than
// silently mislabelling it as a timeout.
func outcomeForCtx(err error) string {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, context.Canceled):
		return "canceled"
	default:
		return "unknown"
	}
}

// finalOutcomeFor classifies the lastErr returned after the retry
// budget was exhausted so we can record a single terminal outcome.
func finalOutcomeFor(lastErr error) string {
	if errors.Is(lastErr, ErrRateLimited) {
		return "rate_limited"
	}
	return "upstream_5xx"
}

// wrapCtxErr lifts a context error into the package's sentinel so
// callers can branch on errors.Is(err, openrouter.ErrTimeout) without
// importing the context package directly.
func (c *Client) wrapCtxErr(err error) error {
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("%w: %v", ErrTimeout, err)
	}
	return err
}

// contextWithTimeout returns a child context with the smaller of the
// parent's existing deadline and the configured timeout. This avoids
// re-arming a deadline that has already been set tighter by the
// caller (e.g. an HTTP middleware with a per-request budget).
func contextWithTimeout(parent context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if timeout <= 0 {
		return context.WithCancel(parent)
	}
	if deadline, ok := parent.Deadline(); ok {
		// Parent's deadline is sooner — don't loosen it.
		if time.Until(deadline) <= timeout {
			return context.WithCancel(parent)
		}
	}
	return context.WithTimeout(parent, timeout)
}
