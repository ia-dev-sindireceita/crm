// Package llmfake is a deterministic in-process adapter that satisfies
// aiassist.LLMClient without contacting any upstream LLM. It is selected
// by the composition root (cmd/server) only when no real LLM credential
// is configured, so non-production deployments can exercise the
// AI-assist flow end-to-end. Every response begins with the literal
// prefix "[FAKE LLM]" so any leaked output is human-recognisable.
package llmfake

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/pericles-luz/crm/internal/aiassist"
)

// defaultDelay matches the AC: a ~50ms latency so manual QA exercises
// the inbox loading state instead of seeing an instant response.
const defaultDelay = 50 * time.Millisecond

// ErrEmptyPrompt is returned when Complete is called with an empty
// LLMRequest.Prompt. It matches the defence-in-depth contract in
// internal/aiassist/llm.go: adapters MUST reject empty input even
// though the use case already validates upstream.
var ErrEmptyPrompt = errors.New("llmfake: empty prompt")

// Option configures a Client at construction time. Options compose via
// the functional-options pattern so callers can override only what they
// need; the zero Client (defaultDelay) is always valid.
type Option func(*Client)

// WithDelay overrides the artificial latency injected before returning.
// A non-positive value disables the delay entirely. The constructor
// default is ~50ms so manual QA sees the loading spinner.
func WithDelay(d time.Duration) Option {
	return func(c *Client) { c.delay = d }
}

// Client implements aiassist.LLMClient with deterministic canned
// responses. Safe for concurrent use; all state is set at construction.
type Client struct {
	delay time.Duration
}

// New returns a Client with the default ~50ms delay; options override.
func New(opts ...Option) *Client {
	c := &Client{delay: defaultDelay}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// Compile-time assertion that Client satisfies the domain port. If the
// interface changes upstream this fails to build, surfacing the drift
// at adapter compile time rather than at runtime.
var _ aiassist.LLMClient = (*Client)(nil)

// cannedBodies is the small fixed set of stub completions Complete picks
// from. Body index is derived from a stable SHA-256 of the idempotency
// key, so identical keys always produce identical text and different
// keys spread across the slots.
var cannedBodies = []string{
	"Resumo curto: o cliente confirmou o recebimento e pediu próximos passos.",
	"Resumo curto: o cliente solicitou orçamento para o serviço discutido.",
	"Resumo curto: o cliente reportou dúvida sobre a fatura do mês corrente.",
	"Resumo curto: o cliente agradeceu o atendimento e encerrou o contato.",
}

// Complete returns a deterministic canned response. The shape honours
// the AC in SIN-63801: text begins with "[FAKE LLM]", includes the
// requested model, and the body is selected by SHA-256(IdempotencyKey)
// so repeated calls with the same key are byte-identical. Tokens are
// reported with the OpenAI rough estimator (chars/4).
//
// The injected delay is interruptible: a cancelled context returns
// ctx.Err() so callers can drop the call instead of blocking on the
// fake.
func (c *Client) Complete(ctx context.Context, req aiassist.LLMRequest) (aiassist.LLMResponse, error) {
	if strings.TrimSpace(req.Prompt) == "" {
		return aiassist.LLMResponse{}, ErrEmptyPrompt
	}

	if c.delay > 0 {
		timer := time.NewTimer(c.delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return aiassist.LLMResponse{}, ctx.Err()
		case <-timer.C:
		}
	} else if err := ctx.Err(); err != nil {
		return aiassist.LLMResponse{}, err
	}

	text := fmt.Sprintf("[FAKE LLM] model=%s %s", req.Model, pickBody(req.IdempotencyKey))

	resp := aiassist.LLMResponse{
		Text:      text,
		TokensIn:  int64(len(req.Prompt) / 4),
		TokensOut: int64(req.MaxTokens / 2),
	}
	return resp, nil
}

// pickBody hashes the idempotency key and indexes into cannedBodies so
// repeated calls with the same key yield the same body, and different
// keys spread deterministically across the slots. The empty key still
// hashes to a fixed slot so callers without an idempotency key get a
// reproducible response too.
func pickBody(idempotencyKey string) string {
	sum := sha256.Sum256([]byte(idempotencyKey))
	idx := binary.BigEndian.Uint32(sum[:4]) % uint32(len(cannedBodies))
	return cannedBodies[idx]
}
