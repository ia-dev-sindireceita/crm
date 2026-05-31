package openrouter

// SIN-63826 / SIN-63793 W3 — table-driven tests for the OpenRouter
// PersonaLLM impl.
//
// The unit tests never reach the real OpenRouter API. Every test points
// the impl at an httptest.NewServer that captures the outbound request
// shape (model, system prompt, role mapping, auth header) and replays a
// fixed response so the test asserts both directions of the translation
// without flake / cost / network.

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/pericles-luz/crm/internal/adapter/channels/llmcustomer"
)

// --- Constructor --------------------------------------------------------

func TestNew_RequiresAPIKey(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		key  string
	}{
		{name: "empty", key: ""},
		{name: "whitespace only", key: "   "},
		{name: "tab + newline", key: "\t\n"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := New(Config{APIKey: tc.key})
			if err == nil {
				t.Fatal("expected error for empty APIKey, got nil")
			}
			if !strings.Contains(err.Error(), "APIKey") {
				t.Fatalf("err = %q; want it to mention APIKey", err.Error())
			}
		})
	}
}

func TestNew_DefaultModelAndBaseURL(t *testing.T) {
	t.Parallel()
	p, err := New(Config{APIKey: "sk-test"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if p.model != DefaultModel {
		t.Fatalf("model = %q; want %q (DefaultModel)", p.model, DefaultModel)
	}
	if p.baseURL != strings.TrimRight(defaultBaseURL, "/") {
		t.Fatalf("baseURL = %q; want %q", p.baseURL, defaultBaseURL)
	}
	if p.httpClient == nil {
		t.Fatal("httpClient = nil; want default client")
	}
	if p.httpClient.Timeout != defaultTimeout {
		t.Fatalf("default httpClient.Timeout = %s; want %s", p.httpClient.Timeout, defaultTimeout)
	}
}

func TestNew_HonoursCustomConfig(t *testing.T) {
	t.Parallel()
	custom := &http.Client{Timeout: 3 * time.Second}
	p, err := New(Config{
		APIKey:     "sk-test",
		Model:      "anthropic/claude-haiku-4.5",
		BaseURL:    "https://example.com/api/v1/",
		HTTPClient: custom,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if p.model != "anthropic/claude-haiku-4.5" {
		t.Fatalf("model = %q; want anthropic/claude-haiku-4.5", p.model)
	}
	// Trailing slash is trimmed so the path join produces /api/v1/chat/completions.
	if p.baseURL != "https://example.com/api/v1" {
		t.Fatalf("baseURL = %q; want trailing-slash-trimmed form", p.baseURL)
	}
	if p.httpClient != custom {
		t.Fatal("httpClient was replaced; want the injected client to be preserved")
	}
}

// --- Compile-time port satisfaction ------------------------------------

func TestPersonaSatisfiesPort(t *testing.T) {
	t.Parallel()
	var _ llmcustomer.PersonaLLM = (*Persona)(nil)
}

// --- Happy path: request shape + response parsing ----------------------

// capturedRequest holds the fields the stub server snapshots on each
// inbound call. Tests assert against the latest capture.
type capturedRequest struct {
	method     string
	path       string
	authHeader string
	contentTyp string
	body       openRouterChatRequest
}

// newStubServer returns an httptest.NewServer that records each
// inbound request and responds with the supplied (status, body) pair.
// Callers point Persona.Config.BaseURL at server.URL.
func newStubServer(t *testing.T, status int, response any) (*httptest.Server, *capturedRequest) {
	t.Helper()
	captured := &capturedRequest{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured.method = r.Method
		captured.path = r.URL.Path
		captured.authHeader = r.Header.Get("Authorization")
		captured.contentTyp = r.Header.Get("Content-Type")
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &captured.body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if response != nil {
			_ = json.NewEncoder(w).Encode(response)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, captured
}

func TestNextCustomerMessage_HappyPath_SendsSystemAndHistory(t *testing.T) {
	t.Parallel()
	srv, captured := newStubServer(t, http.StatusOK, openRouterChatResponse{
		Choices: []openRouterChatChoice{
			{Message: openRouterChatMessage{Role: roleAssistant, Content: "Oi! Posso ajudar?"}},
		},
	})
	p, err := New(Config{
		APIKey:  "sk-test-secret",
		Model:   "anthropic/claude-haiku-4.5",
		BaseURL: srv.URL,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	history := []llmcustomer.Turn{
		{Role: llmcustomer.TurnRoleOperator, Body: "Olá, em que posso ajudar?"},
		{Role: llmcustomer.TurnRoleCustomer, Body: "Recebi a fatura mais alta."},
		{Role: llmcustomer.TurnRoleOperator, Body: "Deixe-me verificar."},
	}
	reply, err := p.NextCustomerMessage(context.Background(), "persona-prompt", history)
	if err != nil {
		t.Fatalf("NextCustomerMessage: %v", err)
	}
	if reply != "Oi! Posso ajudar?" {
		t.Fatalf("reply = %q; want %q", reply, "Oi! Posso ajudar?")
	}

	// --- Request shape ---
	if captured.method != http.MethodPost {
		t.Fatalf("method = %q; want POST", captured.method)
	}
	if captured.path != "/chat/completions" {
		t.Fatalf("path = %q; want /chat/completions", captured.path)
	}
	if captured.authHeader != "Bearer sk-test-secret" {
		t.Fatalf("Authorization = %q; want Bearer sk-test-secret", captured.authHeader)
	}
	if captured.contentTyp != "application/json" {
		t.Fatalf("Content-Type = %q; want application/json", captured.contentTyp)
	}
	if captured.body.Model != "anthropic/claude-haiku-4.5" {
		t.Fatalf("body.model = %q; want anthropic/claude-haiku-4.5", captured.body.Model)
	}

	// messages[0] = system + persona
	if len(captured.body.Messages) != 4 {
		t.Fatalf("len(messages) = %d; want 4 (1 system + 3 history)", len(captured.body.Messages))
	}
	if captured.body.Messages[0].Role != roleSystem {
		t.Fatalf("messages[0].role = %q; want system", captured.body.Messages[0].Role)
	}
	if captured.body.Messages[0].Content != "persona-prompt" {
		t.Fatalf("messages[0].content = %q; want persona-prompt", captured.body.Messages[0].Content)
	}
	// messages[1] = operator → user
	if captured.body.Messages[1].Role != roleUser {
		t.Fatalf("messages[1].role = %q; want user (operator)", captured.body.Messages[1].Role)
	}
	if captured.body.Messages[1].Content != "Olá, em que posso ajudar?" {
		t.Fatalf("messages[1].content = %q; want operator body", captured.body.Messages[1].Content)
	}
	// messages[2] = customer → assistant
	if captured.body.Messages[2].Role != roleAssistant {
		t.Fatalf("messages[2].role = %q; want assistant (customer)", captured.body.Messages[2].Role)
	}
	if captured.body.Messages[2].Content != "Recebi a fatura mais alta." {
		t.Fatalf("messages[2].content = %q; want customer body", captured.body.Messages[2].Content)
	}
	// messages[3] = operator → user
	if captured.body.Messages[3].Role != roleUser {
		t.Fatalf("messages[3].role = %q; want user (operator)", captured.body.Messages[3].Role)
	}
}

func TestNextCustomerMessage_EmptyHistorySendsSystemOnly(t *testing.T) {
	t.Parallel()
	srv, captured := newStubServer(t, http.StatusOK, openRouterChatResponse{
		Choices: []openRouterChatChoice{
			{Message: openRouterChatMessage{Role: roleAssistant, Content: "First customer line."}},
		},
	})
	p, err := New(Config{APIKey: "sk-test", BaseURL: srv.URL})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	reply, err := p.NextCustomerMessage(context.Background(), "persona", nil)
	if err != nil {
		t.Fatalf("NextCustomerMessage: %v", err)
	}
	if reply != "First customer line." {
		t.Fatalf("reply = %q", reply)
	}
	if len(captured.body.Messages) != 1 {
		t.Fatalf("len(messages) = %d; want 1 (system only)", len(captured.body.Messages))
	}
	if captured.body.Messages[0].Role != roleSystem {
		t.Fatalf("messages[0].role = %q; want system", captured.body.Messages[0].Role)
	}
}

func TestNextCustomerMessage_TrimsCompletionWhitespace(t *testing.T) {
	t.Parallel()
	srv, _ := newStubServer(t, http.StatusOK, openRouterChatResponse{
		Choices: []openRouterChatChoice{
			{Message: openRouterChatMessage{Content: "  \n  Oi tudo bem?\n\n  "}},
		},
	})
	p, err := New(Config{APIKey: "sk", BaseURL: srv.URL})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	reply, err := p.NextCustomerMessage(context.Background(), "persona", nil)
	if err != nil {
		t.Fatalf("NextCustomerMessage: %v", err)
	}
	if reply != "Oi tudo bem?" {
		t.Fatalf("reply = %q; want trimmed", reply)
	}
}

// --- Error paths -------------------------------------------------------

func TestNextCustomerMessage_EmptyPersonaIsRejected(t *testing.T) {
	t.Parallel()
	// No server needed — the early-return fires before any HTTP call.
	p, err := New(Config{APIKey: "sk", BaseURL: "http://127.0.0.1:1"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for _, prompt := range []string{"", "   ", "\n\t"} {
		_, err := p.NextCustomerMessage(context.Background(), prompt, nil)
		if err == nil {
			t.Fatalf("expected error for empty persona %q, got nil", prompt)
		}
		if !strings.Contains(err.Error(), "persona prompt is empty") {
			t.Fatalf("err = %q; want it to mention 'persona prompt is empty'", err.Error())
		}
	}
}

func TestNextCustomerMessage_Non200ReturnsError(t *testing.T) {
	t.Parallel()
	srv, _ := newStubServer(t, http.StatusInternalServerError, map[string]string{
		"error": "upstream boom — must not leak into the returned error string",
	})
	p, err := New(Config{APIKey: "sk", BaseURL: srv.URL})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = p.NextCustomerMessage(context.Background(), "persona", nil)
	if err == nil {
		t.Fatal("expected error for 500 status, got nil")
	}
	if !strings.Contains(err.Error(), "upstream status 500") {
		t.Fatalf("err = %q; want 'upstream status 500'", err.Error())
	}
	// Defense in depth: the body must not be echoed in the error string.
	if strings.Contains(err.Error(), "boom") {
		t.Fatalf("err = %q; must not echo upstream body", err.Error())
	}
}

func TestNextCustomerMessage_4xxReturnsError(t *testing.T) {
	t.Parallel()
	srv, _ := newStubServer(t, http.StatusUnauthorized, nil)
	p, err := New(Config{APIKey: "sk", BaseURL: srv.URL})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = p.NextCustomerMessage(context.Background(), "persona", nil)
	if err == nil {
		t.Fatal("expected error for 401 status, got nil")
	}
	if !strings.Contains(err.Error(), "401") {
		t.Fatalf("err = %q; want it to mention status 401", err.Error())
	}
}

func TestNextCustomerMessage_EmptyChoicesReturnsError(t *testing.T) {
	t.Parallel()
	srv, _ := newStubServer(t, http.StatusOK, openRouterChatResponse{Choices: nil})
	p, err := New(Config{APIKey: "sk", BaseURL: srv.URL})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = p.NextCustomerMessage(context.Background(), "persona", nil)
	if err == nil {
		t.Fatal("expected error for empty choices, got nil")
	}
	if !strings.Contains(err.Error(), "no choices") {
		t.Fatalf("err = %q; want 'no choices'", err.Error())
	}
}

func TestNextCustomerMessage_EmptyContentReturnsError(t *testing.T) {
	t.Parallel()
	srv, _ := newStubServer(t, http.StatusOK, openRouterChatResponse{
		Choices: []openRouterChatChoice{
			{Message: openRouterChatMessage{Content: "   "}},
		},
	})
	p, err := New(Config{APIKey: "sk", BaseURL: srv.URL})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = p.NextCustomerMessage(context.Background(), "persona", nil)
	if err == nil {
		t.Fatal("expected error for empty content, got nil")
	}
	if !strings.Contains(err.Error(), "completion is empty") {
		t.Fatalf("err = %q; want 'completion is empty'", err.Error())
	}
}

func TestNextCustomerMessage_MalformedJSONReturnsError(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{not valid json`))
	}))
	t.Cleanup(srv.Close)
	p, err := New(Config{APIKey: "sk", BaseURL: srv.URL})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = p.NextCustomerMessage(context.Background(), "persona", nil)
	if err == nil {
		t.Fatal("expected decode error, got nil")
	}
	if !strings.Contains(err.Error(), "decode response") {
		t.Fatalf("err = %q; want 'decode response'", err.Error())
	}
}

func TestNextCustomerMessage_TransportErrorReturnsError(t *testing.T) {
	t.Parallel()
	// Point at a closed port — net.Dial will refuse the connection.
	p, err := New(Config{
		APIKey:  "sk",
		BaseURL: "http://127.0.0.1:1",
		HTTPClient: &http.Client{
			Timeout: 200 * time.Millisecond,
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = p.NextCustomerMessage(context.Background(), "persona", nil)
	if err == nil {
		t.Fatal("expected transport error, got nil")
	}
	if !strings.Contains(err.Error(), "send request") {
		t.Fatalf("err = %q; want 'send request'", err.Error())
	}
}

func TestNextCustomerMessage_ContextCancelled(t *testing.T) {
	t.Parallel()
	// Stub server blocks indefinitely so the only path out is the ctx
	// cancel below.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	t.Cleanup(srv.Close)
	p, err := New(Config{APIKey: "sk", BaseURL: srv.URL})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled — Do returns immediately
	_, err = p.NextCustomerMessage(ctx, "persona", nil)
	if err == nil {
		t.Fatal("expected error from cancelled ctx, got nil")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v; want errors.Is(err, context.Canceled)", err)
	}
}

// --- mapRole ----------------------------------------------------------

func TestMapRole(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   string
		want string
	}{
		{in: llmcustomer.TurnRoleCustomer, want: roleAssistant},
		{in: llmcustomer.TurnRoleOperator, want: roleUser},
		{in: "", want: roleUser},
		{in: "system", want: roleUser},
		{in: "unknown-future-role", want: roleUser},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.in, func(t *testing.T) {
			t.Parallel()
			if got := mapRole(tc.in); got != tc.want {
				t.Fatalf("mapRole(%q) = %q; want %q", tc.in, got, tc.want)
			}
		})
	}
}

// --- contextWithTimeout -----------------------------------------------

func TestContextWithTimeout_RespectsShorterParentDeadline(t *testing.T) {
	t.Parallel()
	parent, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	t.Cleanup(cancel)
	ctx, cancel2 := contextWithTimeout(parent, 1*time.Hour)
	t.Cleanup(cancel2)
	deadline, ok := ctx.Deadline()
	if !ok {
		t.Fatal("child ctx has no deadline")
	}
	if d := time.Until(deadline); d > 60*time.Millisecond {
		t.Fatalf("child deadline = %s away; parent should have won", d)
	}
}

func TestContextWithTimeout_AppliesOwnTimeoutWhenParentHasNone(t *testing.T) {
	t.Parallel()
	ctx, cancel := contextWithTimeout(context.Background(), 100*time.Millisecond)
	t.Cleanup(cancel)
	deadline, ok := ctx.Deadline()
	if !ok {
		t.Fatal("child ctx has no deadline; expected 100ms applied")
	}
	if d := time.Until(deadline); d > 200*time.Millisecond {
		t.Fatalf("child deadline too far: %s", d)
	}
}

func TestContextWithTimeout_ZeroTimeoutMeansNoOwnDeadline(t *testing.T) {
	t.Parallel()
	ctx, cancel := contextWithTimeout(context.Background(), 0)
	t.Cleanup(cancel)
	if _, ok := ctx.Deadline(); ok {
		t.Fatal("child has a deadline; want none when timeout=0 and parent has none")
	}
}
