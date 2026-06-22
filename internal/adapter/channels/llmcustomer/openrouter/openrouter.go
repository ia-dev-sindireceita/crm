// Package openrouter is the OpenRouter-backed PersonaLLM implementation
// used by the llmcustomer fake-channel adapter when an operator wants
// the synthetic customer to be driven by a real LLM rather than the
// deterministic canned script.
//
// The package is a sibling of llmcustomer/canned and plugs into the
// same PersonaLLM port (SIN-63793 W2 — see
// internal/adapter/channels/llmcustomer/persona_llm.go). cmd/server's
// W5 selector decides which implementation to wire based on
// PERSONA_LLM_PROVIDER.
//
// Why this package does NOT import internal/aiassist:
//
// The aiassist port (SIN-63797) is shaped for the operator-side AI
// drafting flow: it carries idempotency keys, token budget, consent,
// and a wallet debit. None of those concerns apply to the fake-customer
// simulator, which is a single-tenant single-persona dev/staging tool
// with no per-tenant cost accounting and no PII to consent to.
//
// Importing aiassist here would also break the dependency rule that
// internal/adapter/channels/* must stay free of internal/aiassist —
// the two surfaces have to be composable in cmd/server without dragging
// the aiassist port through the channels graph.
//
// What this package does reuse: nothing from internal/adapter/llm/* —
// the persona impl makes a small, direct chat-completions call against
// OpenRouter using the caller-supplied *http.Client. The "no second HTTP
// client" boring-tech constraint is honoured by accepting the
// *http.Client via Config; transport policy (timeouts, TLS, proxy) is
// owned by the caller.
//
// Failure mode: any non-2xx, empty completion, or transport error is
// returned to the caller unchanged. There is no silent canned fallback —
// staging is expected to surface the failure loudly so an operator
// reaches for the rollback (`PERSONA_LLM_PROVIDER=canned`).
package openrouter

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

	"github.com/pericles-luz/crm/internal/adapter/channels/llmcustomer"
)

const (
	// defaultBaseURL is the OpenRouter v1 API root. Tests override via
	// Config.BaseURL using httptest.NewServer; production wiring leaves
	// it empty and gets this value.
	defaultBaseURL = "https://openrouter.ai/api/v1"

	// DefaultModel is the persona-side default routed model when the
	// operator has not set PERSONA_LLM_MODEL. Gemini Flash is already
	// the operator-AI default (see adapters/openrouter.DefaultModel),
	// has a strong pt-BR profile, and is the cheapest tier OpenRouter
	// publishes — appropriate for a staging-only simulator.
	DefaultModel = "google/gemini-2.5-flash-lite"

	// defaultTimeout caps a single chat-completion attempt. Persona
	// replies are short (one to three sentences per the PersonaV1 prompt
	// in llmcustomer.PersonaV1) so 15s is generous; the caller's ctx
	// deadline still wins when it is shorter.
	defaultTimeout = 15 * time.Second

	// maxResponseSize caps the upstream response body the decoder will
	// read. Defense-in-depth against a hostile or runaway upstream;
	// 1 MiB is far above a typical chat completion (~few KB) yet bounds
	// the persona-side memory footprint per request.
	maxResponseSize = 1 << 20

	// roleUser / roleAssistant are the only two roles the persona impl
	// emits in the messages array. operator turns are forwarded as
	// "user" (the side calling the model) and prior customer turns as
	// "assistant" (the side the model plays). The system prompt is
	// always the persona text.
	roleUser      = "user"
	roleAssistant = "assistant"
	roleSystem    = "system"
)

// Persona is the PersonaLLM implementation backed by OpenRouter's
// chat-completions API. It is safe for concurrent use — every
// goroutine builds its own request body and reads its own response
// stream; the underlying *http.Client is the only shared state and is
// itself concurrency-safe.
//
// The zero value is NOT usable — callers must go through New so the
// invariants on apiKey, model, and httpClient are enforced.
type Persona struct {
	apiKey     string
	model      string
	baseURL    string
	httpClient *http.Client
}

// Compile-time check: Persona satisfies the W2 PersonaLLM port. If the
// port signature drifts, this line is the first compile error and
// points directly at the persona impl that needs updating.
var _ llmcustomer.PersonaLLM = (*Persona)(nil)

// Config carries the construction-time knobs for a Persona. APIKey is
// the only required field; the rest fall back to documented defaults.
type Config struct {
	// APIKey is the OpenRouter bearer token. Required: New returns an
	// error when this is empty so a missing-secret bug surfaces at boot
	// rather than on the first persona reply. The key never appears in
	// logs and never leaves this struct (only the Authorization header
	// of outgoing requests carries it).
	APIKey string

	// Model selects the upstream routed model. Empty falls back to
	// DefaultModel. The caller is expected to pick a pt-BR-capable
	// model for the PersonaV1 prompt; the package does NOT validate the
	// string against an allow-list because OpenRouter's model catalogue
	// changes faster than this code does.
	Model string

	// BaseURL overrides the OpenRouter API root. Empty means
	// defaultBaseURL. Tests use this to point at an httptest.NewServer
	// so the persona impl is exercised without any external network
	// dependency.
	BaseURL string

	// HTTPClient lets the caller inject transport policy (timeouts,
	// TLS, proxy, metrics). Empty means a fresh *http.Client with
	// Timeout=defaultTimeout. Note: when this is supplied, the caller
	// is responsible for setting Timeout — the persona impl does NOT
	// retrofit one onto a caller-owned client.
	HTTPClient *http.Client
}

// New constructs a Persona from cfg. Returns an error rather than
// panicking on missing APIKey so cmd/server can distinguish "secret
// not wired" from "binary not built" in the boot log.
func New(cfg Config) (*Persona, error) {
	if strings.TrimSpace(cfg.APIKey) == "" {
		return nil, errors.New("openrouter persona: APIKey is required")
	}
	model := strings.TrimSpace(cfg.Model)
	if model == "" {
		model = DefaultModel
	}
	baseURL := strings.TrimSpace(cfg.BaseURL)
	if baseURL == "" {
		baseURL = defaultBaseURL
	}
	httpClient := cfg.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: defaultTimeout}
	}
	return &Persona{
		apiKey:     cfg.APIKey,
		model:      model,
		baseURL:    strings.TrimRight(baseURL, "/"),
		httpClient: httpClient,
	}, nil
}

// openRouterChatRequest is the on-the-wire JSON the persona impl sends.
// Field names match OpenRouter's chat-completions schema. Defining the
// struct locally (rather than vendoring an SDK) is the boring-tech
// default for this codebase.
type openRouterChatRequest struct {
	Model    string                  `json:"model"`
	Messages []openRouterChatMessage `json:"messages"`
}

type openRouterChatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type openRouterChatResponse struct {
	Choices []openRouterChatChoice `json:"choices"`
}

type openRouterChatChoice struct {
	Message openRouterChatMessage `json:"message"`
}

// NextCustomerMessage implements llmcustomer.PersonaLLM. It builds an
// OpenRouter chat-completions request with:
//
//   - messages[0] = {role: "system", content: persona} so the model
//     adopts the persona for the whole conversation;
//   - messages[1..] = history mapped to the chat schema, where
//     llmcustomer.TurnRoleOperator → "user" and TurnRoleCustomer →
//     "assistant" (the model plays the customer side, so the model's
//     own prior turns are "assistant").
//
// The reply is the trimmed content of the first choice. Empty choices
// or empty content are treated as errors — staging surfaces the
// failure loudly so an operator reaches for the rollback. No retry
// policy lives here; the caller can configure cfg.HTTPClient to add
// transport-level retries if it really needs them.
//
// ctx is honoured for cancellation and deadlines. When ctx has no
// deadline shorter than defaultTimeout, the impl applies its own
// 15s timeout via context.WithTimeout.
func (p *Persona) NextCustomerMessage(ctx context.Context, persona string, history []llmcustomer.Turn) (string, error) {
	if strings.TrimSpace(persona) == "" {
		return "", errors.New("openrouter persona: persona prompt is empty")
	}
	messages := make([]openRouterChatMessage, 0, len(history)+1)
	messages = append(messages, openRouterChatMessage{Role: roleSystem, Content: persona})
	for _, t := range history {
		messages = append(messages, openRouterChatMessage{
			Role:    mapRole(t.Role),
			Content: t.Body,
		})
	}
	body, err := json.Marshal(openRouterChatRequest{Model: p.model, Messages: messages})
	if err != nil {
		// json.Marshal of a fixed-shape struct cannot fail in practice,
		// but returning the error is cheaper than ignoring it.
		return "", fmt.Errorf("openrouter persona: marshal request: %w", err)
	}

	callCtx, cancel := contextWithTimeout(ctx, defaultTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(callCtx, http.MethodPost, p.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("openrouter persona: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+p.apiKey)

	resp, err := p.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("openrouter persona: send request: %w", err)
	}
	defer resp.Body.Close()

	limited := io.LimitReader(resp.Body, maxResponseSize)

	if resp.StatusCode != http.StatusOK {
		// Drain (bounded) so the underlying connection can be reused on
		// a future request. Do NOT include the body in the error — it
		// may echo the persona prompt or history back.
		_, _ = io.Copy(io.Discard, limited)
		return "", fmt.Errorf("openrouter persona: upstream status %d", resp.StatusCode)
	}

	var payload openRouterChatResponse
	if err := json.NewDecoder(limited).Decode(&payload); err != nil {
		return "", fmt.Errorf("openrouter persona: decode response: %w", err)
	}
	if len(payload.Choices) == 0 {
		return "", errors.New("openrouter persona: response has no choices")
	}
	msg := strings.TrimSpace(payload.Choices[0].Message.Content)
	if msg == "" {
		return "", errors.New("openrouter persona: completion is empty")
	}
	return msg, nil
}

// mapRole translates an llmcustomer turn role into the OpenRouter chat
// role schema:
//
//   - llmcustomer.TurnRoleOperator → "user"       (the side calling the model)
//   - llmcustomer.TurnRoleCustomer → "assistant"  (the side the model plays)
//
// Any other role string is forwarded as "user" — the safer default
// because the model treats unknown content as input rather than
// fabricating it as its own prior output. The contract on
// llmcustomer.Turn.Role is "opaque text", so the persona impl avoids
// crashing on a role the upstream did not emit.
func mapRole(role string) string {
	if role == llmcustomer.TurnRoleCustomer {
		return roleAssistant
	}
	return roleUser
}

// contextWithTimeout returns a child context with the smaller of the
// caller's existing deadline and the configured persona timeout. This
// avoids loosening a tighter caller deadline (e.g. an HTTP middleware
// with a per-request budget) while still capping unbounded contexts at
// 15s.
func contextWithTimeout(parent context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if timeout <= 0 {
		return context.WithCancel(parent)
	}
	if deadline, ok := parent.Deadline(); ok {
		if time.Until(deadline) <= timeout {
			return context.WithCancel(parent)
		}
	}
	return context.WithTimeout(parent, timeout)
}
