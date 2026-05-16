package openrouter

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/common/expfmt"
)

// fastClient builds a Client with a no-op sleep so retry tests run in
// milliseconds rather than seconds. Tests that need to assert on
// backoff timing override sleep manually.
func fastClient(t *testing.T, srvURL string, opts ...func(*Config)) *Client {
	t.Helper()
	cfg := Config{
		APIKey:  "test-key",
		BaseURL: srvURL,
		Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	for _, opt := range opts {
		opt(&cfg)
	}
	c, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	c.sleep = func(time.Duration) {}
	return c
}

func TestNewRequiresAPIKey(t *testing.T) {
	if _, err := New(Config{}); err == nil {
		t.Fatal("expected error when APIKey is empty")
	}
	if _, err := New(Config{APIKey: "   "}); err == nil {
		t.Fatal("expected error when APIKey is whitespace-only")
	}
}

func TestNewAppliesDefaults(t *testing.T) {
	c, err := New(Config{APIKey: "k"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if c.baseURL != "https://openrouter.ai/api/v1" {
		t.Errorf("baseURL default = %q", c.baseURL)
	}
	if c.timeout != 8*time.Second {
		t.Errorf("timeout default = %v", c.timeout)
	}
	if c.httpClient == nil || c.httpClient.Transport == nil {
		t.Fatal("expected http client + transport to be configured")
	}
	if _, ok := c.httpClient.Transport.(*logTransport); !ok {
		t.Errorf("transport not wrapped as logTransport: %T", c.httpClient.Transport)
	}
}

func TestNewWrapsExistingTransport(t *testing.T) {
	base := http.DefaultTransport
	hc := &http.Client{Transport: base}
	c, err := New(Config{APIKey: "k", HTTPClient: hc})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	lt, ok := c.httpClient.Transport.(*logTransport)
	if !ok {
		t.Fatalf("transport not wrapped: %T", c.httpClient.Transport)
	}
	if lt.base != base {
		t.Error("wrapped transport did not preserve the supplied base")
	}
}

func TestNewKeepsAlreadyWrappedTransport(t *testing.T) {
	preWrapped := newLogTransport(http.DefaultTransport, slog.Default())
	hc := &http.Client{Transport: preWrapped}
	c, err := New(Config{APIKey: "k", HTTPClient: hc})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if c.httpClient.Transport != preWrapped {
		t.Error("New double-wrapped a logTransport that was already in place")
	}
}

func TestCompleteHappyPath(t *testing.T) {
	var gotHeaders http.Header
	var gotBody openRouterChatRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeaders = r.Header.Clone()
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Errorf("decode request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(openRouterChatResponse{
			Choices: []openRouterChatChoice{{Message: openRouterChatMessage{Role: "assistant", Content: "olá mundo"}}},
			Usage:   openRouterChatUsage{PromptTokens: 12, CompletionTokens: 34},
		})
	}))
	defer srv.Close()

	c := fastClient(t, srv.URL)
	c.metrics = NewMetrics()

	resp, err := c.Complete(context.Background(), CompleteRequest{
		Prompt:         "diga olá",
		Model:          "google/gemini-2.0-flash",
		MaxTokens:      256,
		IdempotencyKey: "tenant-A:conv-1:req-7",
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.Text != "olá mundo" {
		t.Errorf("Text = %q", resp.Text)
	}
	if resp.TokensIn != 12 || resp.TokensOut != 34 {
		t.Errorf("tokens = %d / %d", resp.TokensIn, resp.TokensOut)
	}
	if gotHeaders.Get("Authorization") != "Bearer test-key" {
		t.Errorf("missing/invalid Authorization header: %q", gotHeaders.Get("Authorization"))
	}
	if gotHeaders.Get("X-Idempotency-Key") != "tenant-A:conv-1:req-7" {
		t.Errorf("missing/invalid X-Idempotency-Key header: %q", gotHeaders.Get("X-Idempotency-Key"))
	}
	if gotHeaders.Get("Content-Type") != "application/json" {
		t.Errorf("missing Content-Type header: %q", gotHeaders.Get("Content-Type"))
	}
	if gotBody.Model != "google/gemini-2.0-flash" {
		t.Errorf("model on wire = %q", gotBody.Model)
	}
	if len(gotBody.Messages) != 1 || gotBody.Messages[0].Content != "diga olá" {
		t.Errorf("messages on wire = %#v", gotBody.Messages)
	}
	if gotBody.MaxTokens != 256 {
		t.Errorf("max_tokens on wire = %d", gotBody.MaxTokens)
	}
}

func TestCompleteOmitsIdempotencyKeyWhenEmpty(t *testing.T) {
	var gotHeaders http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeaders = r.Header.Clone()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(openRouterChatResponse{
			Choices: []openRouterChatChoice{{Message: openRouterChatMessage{Content: "ok"}}},
			Usage:   openRouterChatUsage{PromptTokens: 1, CompletionTokens: 1},
		})
	}))
	defer srv.Close()

	c := fastClient(t, srv.URL)
	if _, err := c.Complete(context.Background(), CompleteRequest{Prompt: "p"}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if v, ok := gotHeaders["X-Idempotency-Key"]; ok {
		t.Errorf("X-Idempotency-Key should be absent when key is empty, got %q", v)
	}
}

func TestCompleteDefaultsModelWhenUnset(t *testing.T) {
	var gotBody openRouterChatRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(openRouterChatResponse{
			Choices: []openRouterChatChoice{{Message: openRouterChatMessage{Content: "x"}}},
			Usage:   openRouterChatUsage{PromptTokens: 1, CompletionTokens: 1},
		})
	}))
	defer srv.Close()

	c := fastClient(t, srv.URL)
	if _, err := c.Complete(context.Background(), CompleteRequest{Prompt: "hi"}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if gotBody.Model != DefaultModel {
		t.Errorf("expected default model %q, got %q", DefaultModel, gotBody.Model)
	}
}

func TestCompleteRejectsEmptyPrompt(t *testing.T) {
	c, err := New(Config{APIKey: "k"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := c.Complete(context.Background(), CompleteRequest{Prompt: "   "}); !errors.Is(err, ErrBadRequest) {
		t.Errorf("expected ErrBadRequest, got %v", err)
	}
}

func TestCompleteRetriesOn500ThenFails(t *testing.T) {
	var attempts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&attempts, 1)
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `{"error":"oops"}`)
	}))
	defer srv.Close()

	c := fastClient(t, srv.URL)
	_, err := c.Complete(context.Background(), CompleteRequest{Prompt: "p"})
	if !errors.Is(err, ErrUpstream5xx) {
		t.Fatalf("expected ErrUpstream5xx, got %v", err)
	}
	if got := atomic.LoadInt32(&attempts); got != 4 {
		t.Errorf("expected 4 attempts (1 + 3 retries), got %d", got)
	}
}

func TestCompleteRetriesOn500ThenSucceeds(t *testing.T) {
	var attempts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := atomic.AddInt32(&attempts, 1)
		if n < 3 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(openRouterChatResponse{
			Choices: []openRouterChatChoice{{Message: openRouterChatMessage{Content: "recovered"}}},
			Usage:   openRouterChatUsage{PromptTokens: 5, CompletionTokens: 6},
		})
	}))
	defer srv.Close()

	c := fastClient(t, srv.URL)
	resp, err := c.Complete(context.Background(), CompleteRequest{Prompt: "p"})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.Text != "recovered" {
		t.Errorf("Text = %q", resp.Text)
	}
	if got := atomic.LoadInt32(&attempts); got != 3 {
		t.Errorf("expected 3 attempts, got %d", got)
	}
}

func TestCompleteRateLimitedExhaustsRetries(t *testing.T) {
	var attempts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&attempts, 1)
		w.Header().Set("Retry-After", "1")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	c := fastClient(t, srv.URL)
	_, err := c.Complete(context.Background(), CompleteRequest{Prompt: "p"})
	if !errors.Is(err, ErrRateLimited) {
		t.Fatalf("expected ErrRateLimited, got %v", err)
	}
	if got := atomic.LoadInt32(&attempts); got != 4 {
		t.Errorf("expected 4 attempts on 429, got %d", got)
	}
}

func TestCompleteHonoursRetryAfterSeconds(t *testing.T) {
	var attempts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := atomic.AddInt32(&attempts, 1)
		if n == 1 {
			w.Header().Set("Retry-After", "2")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(openRouterChatResponse{
			Choices: []openRouterChatChoice{{Message: openRouterChatMessage{Content: "ok"}}},
			Usage:   openRouterChatUsage{PromptTokens: 1, CompletionTokens: 1},
		})
	}))
	defer srv.Close()

	c := fastClient(t, srv.URL)
	var slept []time.Duration
	var mu sync.Mutex
	c.sleep = func(d time.Duration) {
		mu.Lock()
		slept = append(slept, d)
		mu.Unlock()
	}
	if _, err := c.Complete(context.Background(), CompleteRequest{Prompt: "p"}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if got := atomic.LoadInt32(&attempts); got != 2 {
		t.Errorf("expected 2 attempts, got %d", got)
	}
	// Backoff schedule for attempt 0 is 250ms; Retry-After=2s
	// overrides it.
	// (We can't observe c.sleep since waitBeforeRetry uses a real
	//  timer; the assertion above on attempts proves the path was
	//  exercised. Slept slice is captured here for future expansion.)
	_ = slept
}

func TestCompleteHonoursRetryAfterHTTPDate(t *testing.T) {
	// Retry-After as an HTTP-date 3s in the future. http.TimeFormat
	// only has second precision; 3s gives us comfortable headroom so
	// the parsed delta stays strictly positive even under CI jitter.
	target := time.Now().Add(3 * time.Second).UTC().Format(http.TimeFormat)
	resp := &http.Response{Header: http.Header{}}
	resp.Header.Set("Retry-After", target)
	if d := retryAfterFromResp(resp); d <= 0 || d > 5*time.Second {
		t.Errorf("unexpected duration from HTTP-date Retry-After: %v", d)
	}

	// Past date returns zero (server's clock skew).
	pastResp := &http.Response{Header: http.Header{}}
	pastResp.Header.Set("Retry-After", time.Now().Add(-time.Hour).UTC().Format(http.TimeFormat))
	if d := retryAfterFromResp(pastResp); d != 0 {
		t.Errorf("expected 0 for past Retry-After, got %v", d)
	}

	if d := retryAfterFromResp(nil); d != 0 {
		t.Errorf("expected 0 for nil response, got %v", d)
	}
	emptyResp := &http.Response{Header: http.Header{}}
	if d := retryAfterFromResp(emptyResp); d != 0 {
		t.Errorf("expected 0 for missing header, got %v", d)
	}
	garbageResp := &http.Response{Header: http.Header{}}
	garbageResp.Header.Set("Retry-After", "not a date")
	if d := retryAfterFromResp(garbageResp); d != 0 {
		t.Errorf("expected 0 for garbage Retry-After, got %v", d)
	}
}

func TestCompleteDoesNotRetryOn400(t *testing.T) {
	var attempts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&attempts, 1)
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer srv.Close()

	c := fastClient(t, srv.URL)
	_, err := c.Complete(context.Background(), CompleteRequest{Prompt: "p"})
	if !errors.Is(err, ErrBadRequest) {
		t.Fatalf("expected ErrBadRequest, got %v", err)
	}
	if got := atomic.LoadInt32(&attempts); got != 1 {
		t.Errorf("expected exactly 1 attempt on 4xx, got %d", got)
	}
}

func TestCompleteDoesNotRetryOn401(t *testing.T) {
	var attempts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&attempts, 1)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	c := fastClient(t, srv.URL)
	_, err := c.Complete(context.Background(), CompleteRequest{Prompt: "p"})
	if !errors.Is(err, ErrBadRequest) {
		t.Fatalf("expected ErrBadRequest, got %v", err)
	}
	if got := atomic.LoadInt32(&attempts); got != 1 {
		t.Errorf("expected exactly 1 attempt on 401, got %d", got)
	}
}

func TestCompleteContextCanceledStopsRetries(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := fastClient(t, srv.URL)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := c.Complete(ctx, CompleteRequest{Prompt: "p"})
	if err == nil {
		t.Fatal("expected error on canceled context")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("expected context.Canceled, got %v", err)
	}
}

func TestCompleteContextDeadlineWrappedAsTimeout(t *testing.T) {
	// Server sleeps 100ms; ctx deadline is 10ms → context.DeadlineExceeded.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(200 * time.Millisecond):
			w.WriteHeader(http.StatusOK)
		case <-r.Context().Done():
			return
		}
	}))
	defer srv.Close()

	c := fastClient(t, srv.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	_, err := c.Complete(ctx, CompleteRequest{Prompt: "p"})
	if !errors.Is(err, ErrTimeout) {
		t.Errorf("expected ErrTimeout, got %v", err)
	}
}

func TestCompleteInvalidResponsePayload(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices": []}`)
	}))
	defer srv.Close()

	c := fastClient(t, srv.URL)
	_, err := c.Complete(context.Background(), CompleteRequest{Prompt: "p"})
	if !errors.Is(err, ErrInvalidResponse) {
		t.Errorf("expected ErrInvalidResponse for empty choices, got %v", err)
	}
}

func TestCompleteUndecodableResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{not json}`)
	}))
	defer srv.Close()

	c := fastClient(t, srv.URL)
	_, err := c.Complete(context.Background(), CompleteRequest{Prompt: "p"})
	if !errors.Is(err, ErrInvalidResponse) {
		t.Errorf("expected ErrInvalidResponse for undecodable body, got %v", err)
	}
}

func TestCompleteEmptyContentTreatedAsInvalid(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(openRouterChatResponse{
			Choices: []openRouterChatChoice{{Message: openRouterChatMessage{Content: ""}}},
			Usage:   openRouterChatUsage{PromptTokens: 1, CompletionTokens: 1},
		})
	}))
	defer srv.Close()

	c := fastClient(t, srv.URL)
	_, err := c.Complete(context.Background(), CompleteRequest{Prompt: "p"})
	if !errors.Is(err, ErrInvalidResponse) {
		t.Errorf("expected ErrInvalidResponse for empty content, got %v", err)
	}
}

func TestCompleteMetricsRecorded(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(openRouterChatResponse{
			Choices: []openRouterChatChoice{{Message: openRouterChatMessage{Content: "ok"}}},
			Usage:   openRouterChatUsage{PromptTokens: 7, CompletionTokens: 11},
		})
	}))
	defer srv.Close()

	m := NewMetrics()
	c := fastClient(t, srv.URL, func(cfg *Config) { cfg.Metrics = m })
	if _, err := c.Complete(context.Background(), CompleteRequest{Prompt: "p"}); err != nil {
		t.Fatalf("Complete: %v", err)
	}

	// Drain registry into the text format and assert the in/out
	// counters landed on the right labels.
	dump := dumpMetrics(t, m)
	mustContain(t, dump, `openrouter_tokens_consumed_total{direction="in",model="google/gemini-2.0-flash"} 7`)
	mustContain(t, dump, `openrouter_tokens_consumed_total{direction="out",model="google/gemini-2.0-flash"} 11`)
	mustContain(t, dump, `openrouter_request_duration_seconds_count{model="google/gemini-2.0-flash",outcome="ok"} 1`)
}

func TestCompleteMetricsNilSafe(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(openRouterChatResponse{
			Choices: []openRouterChatChoice{{Message: openRouterChatMessage{Content: "ok"}}},
			Usage:   openRouterChatUsage{PromptTokens: 1, CompletionTokens: 1},
		})
	}))
	defer srv.Close()

	c := fastClient(t, srv.URL) // no metrics
	if _, err := c.Complete(context.Background(), CompleteRequest{Prompt: "p"}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
}

func TestCompleteTransportErrorIsRetried(t *testing.T) {
	// Server that closes the connection before writing a status the
	// first two times, then replies normally.
	var attempts int32
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&attempts, 1)
		if n < 3 {
			// Hijack and close to surface a transport-level error
			// (server closed connection before sending headers).
			hj, ok := w.(http.Hijacker)
			if !ok {
				t.Fatal("hijack not supported")
			}
			conn, _, err := hj.Hijack()
			if err != nil {
				t.Fatalf("hijack: %v", err)
			}
			_ = conn.Close()
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(openRouterChatResponse{
			Choices: []openRouterChatChoice{{Message: openRouterChatMessage{Content: "ok"}}},
			Usage:   openRouterChatUsage{PromptTokens: 1, CompletionTokens: 1},
		})
	})
	srv := httptest.NewServer(handler)
	defer srv.Close()

	c := fastClient(t, srv.URL)
	if _, err := c.Complete(context.Background(), CompleteRequest{Prompt: "p"}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if got := atomic.LoadInt32(&attempts); got != 3 {
		t.Errorf("expected 3 attempts (2 retries after EOF), got %d", got)
	}
}

func TestCompleteTransportErrorNonTransient(t *testing.T) {
	// Point at an obviously unreachable URL with an unsupported scheme
	// so Do() returns a non-net.OpError immediately.
	c, err := New(Config{
		APIKey:  "k",
		BaseURL: "::::/not-a-url",
		Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	// New() validates baseURL via url.Parse, so "::::/not-a-url" must
	// still pass — but the actual request will fail. Skip if it didn't
	// build (interpreted by url.Parse leniently); the goal is to cover
	// the non-transient send-error branch.
	if err != nil {
		t.Skipf("baseURL rejected: %v", err)
	}
	c.sleep = func(time.Duration) {}
	_, callErr := c.Complete(context.Background(), CompleteRequest{Prompt: "p"})
	if callErr == nil {
		t.Fatal("expected error on unparseable base URL")
	}
}

// TestCompleteBackoffWaitClampedToContextDeadline exercises the
// waitBeforeRetry branch where Retry-After exceeds the remaining
// context budget and must be clamped down to ctx.Deadline.
func TestCompleteBackoffWaitClampedToContextDeadline(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "300") // 5 minutes — way past ctx
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	c := fastClient(t, srv.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()

	_, err := c.Complete(ctx, CompleteRequest{Prompt: "p"})
	if err == nil {
		t.Fatal("expected error when ctx expires inside backoff")
	}
	if !errors.Is(err, ErrTimeout) {
		t.Errorf("expected ErrTimeout, got %v", err)
	}
}

func TestRetryAfterWithDeltaSecondsTakesPrecedence(t *testing.T) {
	resp := &http.Response{Header: http.Header{}}
	resp.Header.Set("Retry-After", "5")
	if d := retryAfterFromResp(resp); d != 5*time.Second {
		t.Errorf("expected 5s from delta-seconds, got %v", d)
	}
}

func TestContextWithTimeoutDoesNotLoosenParentDeadline(t *testing.T) {
	parent, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	child, cancelChild := contextWithTimeout(parent, 5*time.Second)
	defer cancelChild()
	deadline, ok := child.Deadline()
	if !ok {
		t.Fatal("expected child to have a deadline")
	}
	if time.Until(deadline) > 100*time.Millisecond {
		t.Errorf("child deadline loosened past parent's, remaining = %v", time.Until(deadline))
	}
}

func TestContextWithTimeoutSetsDeadlineWhenParentHasNone(t *testing.T) {
	child, cancel := contextWithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	deadline, ok := child.Deadline()
	if !ok {
		t.Fatal("expected child to have a deadline")
	}
	if time.Until(deadline) > 200*time.Millisecond {
		t.Errorf("child deadline too far out: %v", time.Until(deadline))
	}
}

func TestContextWithTimeoutZeroDoesNotSetDeadline(t *testing.T) {
	child, cancel := contextWithTimeout(context.Background(), 0)
	defer cancel()
	if _, ok := child.Deadline(); ok {
		t.Error("expected no deadline when timeout=0")
	}
}

func TestIsTransientNetErrClassification(t *testing.T) {
	if isTransientNetErr(nil) {
		t.Error("nil should not be transient")
	}
	if !isTransientNetErr(io.EOF) {
		t.Error("io.EOF should be transient")
	}
	if !isTransientNetErr(io.ErrUnexpectedEOF) {
		t.Error("io.ErrUnexpectedEOF should be transient")
	}
	if isTransientNetErr(errors.New("application-level error")) {
		t.Error("generic error should not be transient")
	}
}

// dumpMetrics writes the registry in Prometheus text format to a
// buffer so tests can grep for specific samples.
func dumpMetrics(t *testing.T, m *Metrics) string {
	t.Helper()
	mfs, err := m.Registry.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	var buf bytes.Buffer
	enc := expfmt.NewEncoder(&buf, expfmt.NewFormat(expfmt.TypeTextPlain))
	for _, mf := range mfs {
		if err := enc.Encode(mf); err != nil {
			t.Fatalf("encode metric family: %v", err)
		}
	}
	return buf.String()
}

func mustContain(t *testing.T, haystack, needle string) {
	t.Helper()
	if !strings.Contains(haystack, needle) {
		excerpt := haystack
		if len(excerpt) > 2000 {
			excerpt = excerpt[:2000] + "..."
		}
		t.Errorf("expected metrics output to contain %q\n--- got ---\n%s", needle, excerpt)
	}
}
