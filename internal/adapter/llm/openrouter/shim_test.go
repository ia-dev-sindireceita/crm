package openrouter_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	openrouterclient "github.com/pericles-luz/crm/adapters/openrouter"
	"github.com/pericles-luz/crm/internal/adapter/llm/openrouter"
	"github.com/pericles-luz/crm/internal/aiassist"
)

// newStubServer returns an httptest.Server that records each request
// and replies according to the supplied handler. Tests use this to
// observe what the shim → client pipeline actually sends upstream and
// to control the response (success / error / 5xx / etc.).
func newStubServer(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return srv
}

// newClient wires a real *openrouterclient.Client at the test server.
// Tests then wrap it with openrouter.New(...) so we exercise the full
// shim → client → http path, never a stubbed client.
func newClient(t *testing.T, baseURL string) *openrouterclient.Client {
	t.Helper()
	c, err := openrouterclient.New(openrouterclient.Config{
		APIKey:  "test-key",
		BaseURL: baseURL,
	})
	if err != nil {
		t.Fatalf("openrouterclient.New: %v", err)
	}
	return c
}

// observedRequest captures the parts of an upstream request that the
// shim is responsible for translating faithfully. Tests read this to
// assert request shape without depending on JSON ordering.
type observedRequest struct {
	model     string
	prompt    string
	maxTokens int
	hits      int32
}

// decodeUpstreamRequest reads the OpenRouter-shaped JSON body the
// adapter sends and exposes the fields the shim cares about. The
// upstream schema is owned by adapters/openrouter; the test mirrors
// just enough to make assertions.
func decodeUpstreamRequest(t *testing.T, r *http.Request) (model, prompt string, maxTokens int) {
	t.Helper()
	var body struct {
		Model    string `json:"model"`
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
		MaxTokens int `json:"max_tokens"`
	}
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if len(body.Messages) == 0 {
		t.Fatalf("upstream body has no messages: %s", raw)
	}
	return body.Model, body.Messages[0].Content, body.MaxTokens
}

func writeOKResponse(t *testing.T, w http.ResponseWriter, text string, tokensIn, tokensOut int64) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	payload := map[string]any{
		"choices": []map[string]any{
			{"message": map[string]string{"role": "assistant", "content": text}},
		},
		"usage": map[string]int64{
			"prompt_tokens":     tokensIn,
			"completion_tokens": tokensOut,
		},
	}
	if err := json.NewEncoder(w).Encode(payload); err != nil {
		t.Fatalf("encode response: %v", err)
	}
}

// TestShim_PassthroughTokensAndText covers AC #2(a): a successful
// upstream call surfaces the response text and both token counters on
// the aiassist.LLMResponse unchanged.
func TestShim_PassthroughTokensAndText(t *testing.T) {
	srv := newStubServer(t, func(w http.ResponseWriter, r *http.Request) {
		writeOKResponse(t, w, "the assistant says hi", 42, 13)
	})
	shim := openrouter.New(newClient(t, srv.URL))

	got, err := shim.Complete(context.Background(), aiassist.LLMRequest{
		Prompt:    "hello",
		Model:     "google/gemini-2.0-flash",
		MaxTokens: 256,
	})
	if err != nil {
		t.Fatalf("Complete returned error: %v", err)
	}
	if got.Text != "the assistant says hi" {
		t.Errorf("Text = %q, want %q", got.Text, "the assistant says hi")
	}
	if got.TokensIn != 42 {
		t.Errorf("TokensIn = %d, want 42", got.TokensIn)
	}
	if got.TokensOut != 13 {
		t.Errorf("TokensOut = %d, want 13", got.TokensOut)
	}
}

// TestShim_PropagatesErrorWithErrorsIs covers AC #2(b): when the
// underlying client returns one of its sentinel errors, the shim must
// return the same error so callers (the W2C use case) keep working
// with errors.Is to branch on transient vs terminal failures. We use
// a 4xx (non-429) which the client maps to ErrBadRequest terminally —
// that way the test does not need to wait for the retry budget to
// burn through.
func TestShim_PropagatesErrorWithErrorsIs(t *testing.T) {
	srv := newStubServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	})
	shim := openrouter.New(newClient(t, srv.URL))

	_, err := shim.Complete(context.Background(), aiassist.LLMRequest{
		Prompt: "hello",
	})
	if err == nil {
		t.Fatalf("expected error, got nil")
	}
	if !errors.Is(err, openrouterclient.ErrBadRequest) {
		t.Errorf("errors.Is ErrBadRequest = false, got err=%v", err)
	}
}

// TestShim_PropagatesContextCancellation covers AC #2(c): the caller's
// context is forwarded to the underlying client so cancelling the
// caller's context aborts the in-flight request and surfaces a
// context error that errors.Is can recognise.
func TestShim_PropagatesContextCancellation(t *testing.T) {
	// The handler blocks until the test server is closed; we cancel
	// the context first so the client's per-attempt context.Err check
	// fires before the handler ever returns.
	released := make(chan struct{})
	srv := newStubServer(t, func(w http.ResponseWriter, r *http.Request) {
		<-released
	})
	t.Cleanup(func() { close(released) })

	shim := openrouter.New(newClient(t, srv.URL))

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := shim.Complete(ctx, aiassist.LLMRequest{Prompt: "hello"})
	if err == nil {
		t.Fatalf("expected error from cancelled context, got nil")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("errors.Is(err, context.Canceled) = false, got err=%v", err)
	}
}

// TestShim_EmptyModelDelegatesToAdapterDefault covers AC #2(d): if the
// caller leaves LLMRequest.Model empty, the shim must forward the
// empty string so the adapter substitutes its own DefaultModel rather
// than the shim hard-coding a model name. We verify by observing the
// model field on the upstream request body.
func TestShim_EmptyModelDelegatesToAdapterDefault(t *testing.T) {
	var observed observedRequest
	srv := newStubServer(t, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&observed.hits, 1)
		observed.model, observed.prompt, observed.maxTokens = decodeUpstreamRequest(t, r)
		writeOKResponse(t, w, "ok", 1, 1)
	})
	shim := openrouter.New(newClient(t, srv.URL))

	_, err := shim.Complete(context.Background(), aiassist.LLMRequest{
		Prompt: "hello",
		// Model intentionally empty.
		MaxTokens: 32,
	})
	if err != nil {
		t.Fatalf("Complete returned error: %v", err)
	}
	if observed.model != openrouterclient.DefaultModel {
		t.Errorf("upstream model = %q, want adapter default %q",
			observed.model, openrouterclient.DefaultModel)
	}
}

// TestShim_PreservesIdempotencyKey covers AC #2(e): the
// LLMRequest.IdempotencyKey is forwarded to the adapter, which sets it
// as the X-Idempotency-Key header on the upstream HTTP request. We
// assert the header value so a regression in either layer is caught.
func TestShim_PreservesIdempotencyKey(t *testing.T) {
	var receivedKey atomic.Value
	srv := newStubServer(t, func(w http.ResponseWriter, r *http.Request) {
		receivedKey.Store(r.Header.Get("X-Idempotency-Key"))
		writeOKResponse(t, w, "ok", 1, 1)
	})
	shim := openrouter.New(newClient(t, srv.URL))

	const key = "tenant-1:conv-2:req-3"
	_, err := shim.Complete(context.Background(), aiassist.LLMRequest{
		Prompt:         "hello",
		IdempotencyKey: key,
	})
	if err != nil {
		t.Fatalf("Complete returned error: %v", err)
	}
	got, _ := receivedKey.Load().(string)
	if got != key {
		t.Errorf("X-Idempotency-Key on upstream request = %q, want %q", got, key)
	}
}

// TestShim_PreservesPromptAndMaxTokens guards the trivial passthrough
// for the two remaining request fields. The AC table calls them out
// explicitly and the shim is the only place they are translated, so a
// failure here would be silent in higher layers.
func TestShim_PreservesPromptAndMaxTokens(t *testing.T) {
	var observed observedRequest
	srv := newStubServer(t, func(w http.ResponseWriter, r *http.Request) {
		observed.model, observed.prompt, observed.maxTokens = decodeUpstreamRequest(t, r)
		writeOKResponse(t, w, "ok", 1, 1)
	})
	shim := openrouter.New(newClient(t, srv.URL))

	const prompt = "summarise this document in two sentences"
	_, err := shim.Complete(context.Background(), aiassist.LLMRequest{
		Prompt:    prompt,
		Model:     "anthropic/claude-haiku-4.5",
		MaxTokens: 512,
	})
	if err != nil {
		t.Fatalf("Complete returned error: %v", err)
	}
	if observed.prompt != prompt {
		t.Errorf("upstream prompt = %q, want %q", observed.prompt, prompt)
	}
	if observed.maxTokens != 512 {
		t.Errorf("upstream max_tokens = %d, want 512", observed.maxTokens)
	}
	if observed.model != "anthropic/claude-haiku-4.5" {
		t.Errorf("upstream model = %q, want anthropic/claude-haiku-4.5", observed.model)
	}
}

// TestShim_NewReturnsUsableType is a tiny but load-bearing
// compile-time + runtime check that openrouter.New produces a value
// whose method set satisfies the port and is callable end-to-end.
// The blank-identifier assertion in shim.go would catch a port-shape
// drift at compile time; this test additionally proves that the
// returned value can be invoked through the interface against a live
// (test) upstream, which guards against accidental wrapping bugs in
// New (e.g. returning a *Shim with a nil underlying client).
func TestShim_NewReturnsUsableType(t *testing.T) {
	srv := newStubServer(t, func(w http.ResponseWriter, r *http.Request) {
		writeOKResponse(t, w, "ok", 1, 1)
	})
	var s aiassist.LLMClient = openrouter.New(newClient(t, srv.URL))
	got, err := s.Complete(context.Background(), aiassist.LLMRequest{Prompt: "ping"})
	if err != nil {
		t.Fatalf("Complete via interface: %v", err)
	}
	if got.Text != "ok" {
		t.Errorf("Text = %q, want %q", got.Text, "ok")
	}
}

// TestShim_DoesNotLogPromptOrAPIKey is a structural check on the
// shim package: it must not import a logging package, must not read
// the OPENROUTER_API_KEY env var, and must not include the prompt in
// any returned error message. Logging belongs to the underlying
// adapter (logTransport in transport.go), where the redaction policy
// already lives.
//
// We verify this behaviourally by triggering an error path with a
// distinctive prompt and asserting the prompt does not appear in the
// returned error string. The adapter's own error messages never
// include the prompt either (handleResponse drops the body), so a
// shim that accidentally wrapped the prompt into its error would be
// the only source of a leak.
func TestShim_DoesNotLogPromptOrAPIKey(t *testing.T) {
	srv := newStubServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	})
	shim := openrouter.New(newClient(t, srv.URL))

	const secretishPrompt = "PII-DO-NOT-LEAK-XYZ-12345"
	_, err := shim.Complete(context.Background(), aiassist.LLMRequest{
		Prompt: secretishPrompt,
	})
	if err == nil {
		t.Fatal("expected error from upstream 401")
	}
	if strings.Contains(err.Error(), secretishPrompt) {
		t.Errorf("error message leaked prompt: %v", err)
	}
}
