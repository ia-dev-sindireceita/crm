package aiassist

import "context"

// LLMRequest is the input the use case hands to LLMClient.Complete.
// Fields mirror the OpenRouter adapter (SIN-62904) but are owned by
// the domain so an alternative provider can be wired without rippling
// the use case.
type LLMRequest struct {
	// Prompt is the full text sent to the model. PII anonymisation
	// (SIN-62350 W3B) is layered between the use case and the adapter;
	// the use case sees the pre-anonymised text and pays for it.
	Prompt string

	// Model selects the upstream model. The use case forwards the
	// effective model resolved by the policy resolver, so an empty
	// string means "policy did not specify, adapter chooses default".
	Model string

	// MaxTokens caps the completion length. The use case sets this to
	// the output-budget portion of the reservation so the upstream
	// cannot exceed the reserved amount.
	MaxTokens int

	// IdempotencyKey is the boundary idempotency triple
	// (tenant_id, conversation_id, request_id) joined by colons. The
	// adapter forwards it as X-Idempotency-Key so a retry hits the
	// upstream cache.
	IdempotencyKey string
}

// LLMResponse is the LLMClient.Complete output. Tokens are reported
// separately so the use case can record the in/out split on the
// AISummary row (the wallet itself charges the sum).
type LLMResponse struct {
	Text      string
	TokensIn  int64
	TokensOut int64
}

// LLMClient is the port the use case calls to produce a completion.
// Implementations live in adapters/ (openrouter today; nothing else
// planned). The interface is intentionally a single method so the
// minimum mock surface for unit tests is trivial.
//
// Concrete implementations MUST:
//
//   - respect ctx (no synchronous calls without a deadline; the use case
//     supplies one when missing);
//   - return non-nil error on transport / decoding failure so the use
//     case can release the wallet reservation;
//   - never panic on empty Prompt — the use case validates upstream,
//     but defence in depth means adapters reject empty input too.
type LLMClient interface {
	Complete(ctx context.Context, req LLMRequest) (LLMResponse, error)
}
