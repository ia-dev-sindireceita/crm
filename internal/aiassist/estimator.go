package aiassist

import "strings"

// minBytesPerToken is the lower bound the per-model heuristic clamps to.
// 1 byte per token is a hard floor: even the most pessimistic tokenizer
// emits at least one byte of source text per token. Falling below 1
// would cause int64 overflow on extreme inputs and erodes the safety
// margin the reservation provides over the actual LLM cost.
const minBytesPerToken = 1

// defaultBytesPerToken is the divisor used when the caller's model is
// unknown to the heuristic. 4 bytes per token matches the conventional
// /4 estimator used in the SIN-62903 spec; it is conservative for
// English-heavy chat (which tends to run closer to /5) and slightly
// optimistic for languages that pack multi-byte UTF-8 (pt-BR, hi).
// The clamp at 1 byte per token keeps the reservation safe against the
// adversarial case.
const defaultBytesPerToken = 4

// minimumTokens is the floor on EstimateTokens output. A 0-byte input
// would otherwise produce 0 tokens reserved, which the wallet rejects
// (Reserve checks amount > 0). Returning 1 keeps the contract that
// every non-empty estimation surfaces at least one reservation token
// without surprising the caller.
const minimumTokens = 1

// modelDivisor returns the bytes-per-token divisor for a given model.
// The table is intentionally small: model-specific tuning is observability
// territory, not domain territory. Adding entries here lets ops nudge
// the safety margin when a new model lands without rewiring the use case.
//
// Convention: the returned divisor is always >= minBytesPerToken. Unknown
// models fall through to defaultBytesPerToken so the estimator is
// deterministic regardless of catalogue changes upstream.
func modelDivisor(model string) int64 {
	m := strings.ToLower(strings.TrimSpace(model))
	switch {
	case strings.HasPrefix(m, "anthropic/claude"):
		// Claude's tokenizer packs slightly more bytes per token than
		// GPT-style BPEs; /3 is the documented heuristic for English.
		// We stay conservative at /3 (worst case among Claude tiers).
		return 3
	case strings.HasPrefix(m, "google/gemini"):
		// Gemini Flash / Pro behave similarly to GPT-3.5/4 on English;
		// /4 is the safe default.
		return defaultBytesPerToken
	case strings.HasPrefix(m, "openai/gpt"):
		return defaultBytesPerToken
	default:
		return defaultBytesPerToken
	}
}

// EstimateTokens returns the conservative upper-bound token count for
// the supplied prompt text under model. The estimator is intentionally
// pessimistic: it is wired into wallet.Reserve, which charges the
// caller for the upper bound, so any overestimate becomes a refund
// when the actual LLM response lands (wallet.Commit charges only what
// was actually consumed).
//
// The function is pure: same (text, model) → same output, no
// allocations beyond the strings.ToLower transient. That makes it
// testable without infra and safe to call from any code path.
//
// The lower bound is minimumTokens; the upper bound is len(text)
// divided by the per-model divisor, never less than minBytesPerToken.
func EstimateTokens(text, model string) int64 {
	div := modelDivisor(model)
	if div < minBytesPerToken {
		div = minBytesPerToken
	}
	// int64 conversion is safe: Go strings can be up to (2^63 - 1) bytes,
	// and the division by div >= 1 cannot overflow. We use byte length
	// (not rune count) because tokenizers operate on bytes / BPE merges,
	// not codepoints — and byte length is the conservative bound for
	// multi-byte UTF-8 input.
	n := int64(len(text)) / div
	if n < minimumTokens {
		return minimumTokens
	}
	return n
}

// EstimateReservation returns the reservation amount the use case should
// pass to wallet.Reserve before issuing an LLM call. The amount is the
// sum of EstimateTokens(prompt, model) and maxOutputTokens — both the
// prompt and the bounded output participate in the upstream charge, and
// the wallet's reservation must cover both so a successful response can
// always commit without exceeding the ceiling.
//
// maxOutputTokens <= 0 is treated as "no output budget" and contributes
// 0 to the estimate; the caller MUST pass a positive cap in practice or
// the wallet's clamp logic will refuse a commit larger than the
// reserved input estimate.
func EstimateReservation(prompt, model string, maxOutputTokens int64) int64 {
	in := EstimateTokens(prompt, model)
	if maxOutputTokens <= 0 {
		return in
	}
	return in + maxOutputTokens
}
