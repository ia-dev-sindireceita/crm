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
	"testing"
	"time"
)

// TestLogTransportRedactsPromptAndResponse is the smoking-gun assertion
// for SIN-62904 AC #3 (logging redacta prompt e resposta): a full
// Complete() round-trip must not leak the prompt content, the LLM
// response content, or the bearer token into log output.
//
// We use a real httptest server so the request body actually flows
// through the wrapped RoundTripper rather than asserting on the
// transport in isolation.
func TestLogTransportRedactsPromptAndResponse(t *testing.T) {
	const sensitivePrompt = "TOPSECRET_PROMPT_PII=cpf:123.456.789-00"
	const sensitiveResponse = "SECRET_REPLY_FROM_LLM_paciente_diabetico"
	const apiKey = "sk-or-v1-DO_NOT_LEAK_ME"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Drain the body so we don't leave a goroutine dangling.
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(openRouterChatResponse{
			Choices: []openRouterChatChoice{{Message: openRouterChatMessage{Content: sensitiveResponse}}},
			Usage:   openRouterChatUsage{PromptTokens: 7, CompletionTokens: 11},
		})
	}))
	defer srv.Close()

	var logBuf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	c, err := New(Config{
		APIKey:  apiKey,
		BaseURL: srv.URL,
		Logger:  logger,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	resp, err := c.Complete(context.Background(), CompleteRequest{
		Prompt:         sensitivePrompt,
		Model:          "google/gemini-2.0-flash",
		IdempotencyKey: "t:c:r",
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.Text != sensitiveResponse {
		t.Fatalf("test setup: expected response %q, got %q", sensitiveResponse, resp.Text)
	}

	logs := logBuf.String()
	// Assert: model + duration + status are present.
	for _, needle := range []string{`"model":"google/gemini-2.0-flash"`, `"duration_ms":`, `"status":200`, `"msg":"openrouter request"`} {
		if !strings.Contains(logs, needle) {
			t.Errorf("expected log line to contain %q\nlogs:\n%s", needle, logs)
		}
	}
	// Assert: redacted fields MUST NOT be in logs.
	for _, secret := range []string{sensitivePrompt, sensitiveResponse, apiKey, "Bearer", "TOPSECRET_PROMPT_PII", "SECRET_REPLY_FROM_LLM"} {
		if strings.Contains(logs, secret) {
			t.Errorf("log line leaked redacted material %q\nlogs:\n%s", secret, logs)
		}
	}
}

// TestLogTransportLogsTransportFailure exercises the warn-level
// branch where the underlying transport returns an error (server
// closes the connection mid-request) so we observe duration_ms +
// error label without leaking the prompt.
func TestLogTransportLogsTransportFailure(t *testing.T) {
	const sensitive = "PROMPT_THAT_MUST_NOT_LEAK"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Fatal("hijack unsupported")
		}
		conn, _, err := hj.Hijack()
		if err != nil {
			t.Fatalf("hijack: %v", err)
		}
		_ = conn.Close()
	}))
	defer srv.Close()

	var logBuf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	c, err := New(Config{
		APIKey:  "k",
		BaseURL: srv.URL,
		Logger:  logger,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	c.sleep = func(time.Duration) {}
	_, callErr := c.Complete(context.Background(), CompleteRequest{Prompt: sensitive})
	if callErr == nil {
		t.Fatal("expected error from closed connection")
	}

	logs := logBuf.String()
	if !strings.Contains(logs, `"msg":"openrouter request failed"`) {
		t.Errorf("expected failure log line\nlogs:\n%s", logs)
	}
	if strings.Contains(logs, sensitive) {
		t.Errorf("failure log line leaked prompt\nlogs:\n%s", logs)
	}
}

func TestModelFromRequestNilContext(t *testing.T) {
	if got := modelFromRequest(nil); got != "unknown" {
		t.Errorf("expected 'unknown', got %q", got)
	}
}

func TestModelFromRequestNoModelKey(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	if got := modelFromRequest(req); got != "unknown" {
		t.Errorf("expected 'unknown' when ctx has no model, got %q", got)
	}
}

func TestModelFromRequestWithModelKey(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	ctx := context.WithValue(req.Context(), requestModelCtxKey{}, "model-X")
	req = req.WithContext(ctx)
	if got := modelFromRequest(req); got != "model-X" {
		t.Errorf("expected 'model-X', got %q", got)
	}
}

func TestNewLogTransportNilBaseFallsBackToDefault(t *testing.T) {
	lt := newLogTransport(nil, nil)
	if lt.base != http.DefaultTransport {
		t.Error("expected nil base to fall back to http.DefaultTransport")
	}
	if lt.logger == nil {
		t.Error("expected nil logger to fall back to slog.Default")
	}
}

// TestLogTransportRoundTripPassesThrough ensures the wrapper does not
// alter the response surface that the caller observes — only logs.
func TestLogTransportRoundTripPassesThrough(t *testing.T) {
	body := "hello"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, body)
	}))
	defer srv.Close()

	lt := newLogTransport(nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	req, _ := http.NewRequest(http.MethodGet, srv.URL, nil)
	resp, err := lt.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d", resp.StatusCode)
	}
	got, _ := io.ReadAll(resp.Body)
	if string(got) != body {
		t.Errorf("body = %q", got)
	}
}

func TestErrorsAreDistinct(t *testing.T) {
	for _, e := range []error{ErrUpstream5xx, ErrRateLimited, ErrTimeout, ErrBadRequest, ErrInvalidResponse} {
		if e == nil {
			t.Error("sentinel must be non-nil")
		}
	}
	if errors.Is(ErrUpstream5xx, ErrRateLimited) {
		t.Error("ErrUpstream5xx and ErrRateLimited must be distinct")
	}
}
