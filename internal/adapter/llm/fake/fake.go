// Package fake provides a deterministic aiassist.LLMClient adapter for
// local development, smoke tests, and demo environments where calling
// the real upstream LLM is undesirable (cost, latency, or because the
// machine is offline).
//
// The adapter answers every request with a fixed RESUMO + three SUGESTAO
// block in the exact shape the inbox panel's parseAssistText expects, so
// the rendered HTMX panel stays visually realistic without an upstream
// dependency.
//
// PII safety: the prompt body is never logged, never written to disk,
// and never echoed back beyond the bounded prefix derived from the
// caller's IdempotencyKey. Only deterministic shape and the first eight
// characters of the tenant UUID surface in the response.
//
// Wiring belongs to the composition root: env-var lookup (e.g.
// AIASSIST_FAKE_LLM_DELAY_MS) is the caller's responsibility — the
// adapter only accepts a typed Config so the package can be tested
// without process state.
package fake

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/pericles-luz/crm/internal/aiassist"
)

// tokenDivisor matches the /4 heuristic used by aiassist.EstimateTokens
// for unknown models. Keeping it as a local constant decouples the fake
// from the estimator's safety clamps: the fake is purely a shape
// generator and never participates in wallet reservation paths.
const tokenDivisor = 4

// promptToMessages is the divisor that turns prompt length into the
// "N mensagens" figure embedded in the deterministic RESUMO line. The
// value is arbitrary but stable across calls, which keeps snapshot
// tests in downstream packages reproducible.
const promptToMessages = 80

// minMessages and maxMessages clamp the derived message count so the
// rendered line stays human-readable even on extreme prompt sizes.
const (
	minMessages = 1
	maxMessages = 50
)

// tenantPrefixLen is the number of leading characters of the tenant
// UUID that the RESUMO line surfaces. Eight characters is enough to
// disambiguate tenants in a developer's eyeball test while keeping the
// rest of the UUID out of any place a human might read it.
const tenantPrefixLen = 8

// Config tunes the deterministic adapter. The only knob today is an
// optional Delay that the composition root can set to mimic upstream
// latency in local QA flows; it is wired from process env at the
// boundary (e.g. AIASSIST_FAKE_LLM_DELAY_MS) so the package itself
// stays free of os.Getenv calls.
type Config struct {
	// Delay sleeps before returning the canned response. Zero (the
	// default) responds immediately. The sleep is interruptible by the
	// supplied context — see (*LLM).Complete.
	Delay time.Duration
}

// LLM is the deterministic LLMClient implementation.
type LLM struct {
	delay time.Duration
}

// Compile-time assertion: the fake satisfies the domain port. If the
// aiassist.LLMClient signature drifts, compilation here fails before
// the use case starts panicking at runtime.
var _ aiassist.LLMClient = (*LLM)(nil)

// New builds an LLM from cfg. A negative Delay is treated as zero so a
// misconfigured environment cannot block the heartbeat indefinitely on
// a single request.
func New(cfg Config) *LLM {
	d := cfg.Delay
	if d < 0 {
		d = 0
	}
	return &LLM{delay: d}
}

// Complete returns the fixed shape described in the package doc. The
// response is deterministic per (Prompt length, tenant prefix) and
// honours context cancellation while waiting on the configured delay.
//
// The function never reads or writes the prompt body to anything but a
// length count — see the PII note in the package doc.
func (l *LLM) Complete(ctx context.Context, req aiassist.LLMRequest) (aiassist.LLMResponse, error) {
	if err := ctx.Err(); err != nil {
		return aiassist.LLMResponse{}, err
	}
	if l.delay > 0 {
		timer := time.NewTimer(l.delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return aiassist.LLMResponse{}, ctx.Err()
		case <-timer.C:
		}
	}

	prefix := tenantPrefix(req.IdempotencyKey)
	messages := messageCount(len(req.Prompt))
	text := renderResponse(prefix, messages)

	return aiassist.LLMResponse{
		Text:      text,
		TokensIn:  int64(len(req.Prompt) / tokenDivisor),
		TokensOut: int64(len(text) / tokenDivisor),
	}, nil
}

// renderResponse builds the exact RESUMO + 3 SUGESTAO block the inbox
// parser (internal/web/inbox/ai_assist.go parseAssistText) extracts.
// The marker layout is part of the contract; do not reorder lines.
func renderResponse(tenantPrefix string, messages int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "RESUMO: [fake-llm] resumo do tenant %s, %d mensagens\n", tenantPrefix, messages)
	b.WriteString("SUGESTAO 1: Obrigado pelo contato. Em que posso ajudar?\n")
	b.WriteString("SUGESTAO 2: Pode me passar mais detalhes para entender melhor?\n")
	b.WriteString("SUGESTAO 3: Vou verificar isso para você e retorno em instantes.\n")
	return b.String()
}

// tenantPrefix extracts the first tenantPrefixLen characters of the
// tenant UUID from the boundary IdempotencyKey. The key format is
// tenantID:conversationID:requestID per the use case. Anything shorter
// than the prefix (empty, malformed, or a UUID smaller than expected)
// is returned verbatim so the response still parses and the developer
// can see "the key was unusual" without surfacing PII or panicking.
func tenantPrefix(idempotencyKey string) string {
	tenant := idempotencyKey
	if i := strings.IndexByte(tenant, ':'); i >= 0 {
		tenant = tenant[:i]
	}
	if len(tenant) > tenantPrefixLen {
		return tenant[:tenantPrefixLen]
	}
	return tenant
}

// messageCount maps prompt length to the bounded "N mensagens" figure
// embedded in the RESUMO line. The clamp keeps the rendered text within
// the [minMessages, maxMessages] band so the visible value stays a
// believable conversation-count regardless of input size.
func messageCount(promptLen int) int {
	n := promptLen / promptToMessages
	if n < minMessages {
		return minMessages
	}
	if n > maxMessages {
		return maxMessages
	}
	return n
}
