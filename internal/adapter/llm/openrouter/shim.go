// Package openrouter adapts the in-tree adapters/openrouter HTTP
// client to the internal/aiassist.LLMClient port. The shim is the only
// thing that ever imports both the adapter package and the aiassist
// port — this keeps adapters/openrouter free of the W2C use-case import
// and keeps the use case free of any HTTP knowledge.
//
// The shim deliberately does NOT add retry, timeout, or idempotency
// logic. All three already live in adapters/openrouter (transport.go,
// client.go) and are exercised by their own test suite; re-implementing
// them here would only create a second policy that could drift from
// the validated one.
//
// What the shim DOES guarantee:
//
//   - Compile-time satisfaction of aiassist.LLMClient (see the
//     blank-identifier assertion below).
//   - Faithful field translation between aiassist.LLMRequest and
//     openrouter.CompleteRequest (and the reverse for responses).
//   - Context propagation: the caller's ctx is passed straight through
//     to the underlying client.
//   - errors.Is preservation: the underlying client returns wrapped
//     sentinel errors (ErrUpstream5xx, ErrRateLimited, ErrTimeout,
//     ErrBadRequest, ErrInvalidResponse); the shim returns them
//     unchanged so callers can branch on errors.Is.
//   - No prompt / no API-key logging: the shim never logs req.Prompt
//     and never touches OPENROUTER_API_KEY; secret handling stays in
//     the adapter (decision #8 / SIN-62904 W3A).
package openrouter

import (
	"context"

	openrouterclient "github.com/pericles-luz/crm/adapters/openrouter"
	"github.com/pericles-luz/crm/internal/aiassist"
)

// Compile-time check: the shim satisfies the W2C port. If the port
// signature drifts, this line is the first compile error and points
// directly at the translation layer that needs updating.
var _ aiassist.LLMClient = (*Shim)(nil)

// Shim adapts *openrouterclient.Client to aiassist.LLMClient.
//
// The zero value is NOT usable — callers must go through New so the
// underlying client is non-nil. Keeping the field unexported prevents
// downstream code from swapping the client out at runtime (which would
// also bypass the package's "no other dependency" invariant).
type Shim struct {
	client *openrouterclient.Client
}

// New constructs a Shim from an already-built *openrouterclient.Client.
// cmd/server owns the client construction (secret wiring, metrics
// registration, logger plumbing) and hands the ready-to-use client
// here. The shim does not validate c against nil at construction time
// because cmd/server is the only caller and is expected to fail boot
// when openrouter.New itself fails.
func New(c *openrouterclient.Client) *Shim {
	return &Shim{client: c}
}

// Complete satisfies aiassist.LLMClient. It is a pure translation
// layer: every behavioural concern (deadlines, retries, idempotency,
// metrics, logging) belongs to the underlying client.
//
// Translation rules:
//
//	aiassist.LLMRequest  → openrouter.CompleteRequest
//	  Prompt              → Prompt           (forwarded verbatim)
//	  Model               → Model            (empty means "adapter default")
//	  MaxTokens           → MaxTokens
//	  IdempotencyKey      → IdempotencyKey   (empty means "no header")
//
//	openrouter.CompleteResponse → aiassist.LLMResponse
//	  Text               → Text
//	  TokensIn           → TokensIn
//	  TokensOut          → TokensOut
//
// Errors are returned untouched so errors.Is against the openrouter
// sentinels (ErrUpstream5xx, ErrRateLimited, ErrTimeout, ErrBadRequest,
// ErrInvalidResponse) keeps working through this layer.
func (s *Shim) Complete(ctx context.Context, req aiassist.LLMRequest) (aiassist.LLMResponse, error) {
	resp, err := s.client.Complete(ctx, openrouterclient.CompleteRequest{
		Prompt:         req.Prompt,
		Model:          req.Model,
		MaxTokens:      req.MaxTokens,
		IdempotencyKey: req.IdempotencyKey,
	})
	if err != nil {
		return aiassist.LLMResponse{}, err
	}
	return aiassist.LLMResponse{
		Text:      resp.Text,
		TokensIn:  resp.TokensIn,
		TokensOut: resp.TokensOut,
	}, nil
}
