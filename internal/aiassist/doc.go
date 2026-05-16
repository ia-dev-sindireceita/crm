// Package aiassist is the AI-assist domain (SIN-62903 / Fase 3 W2C).
//
// The package owns the Summary aggregate, the LLMClient port, the token
// estimator, and the orchestration that ties them to the wallet's atomic
// reserve→commit/rollback flow (ADR-0043). Domain code MUST stay free of
// database/sql, pgx, net/http, and vendor SDK imports — storage lives
// behind SummaryRepository, the LLM call lives behind LLMClient, and the
// token wallet is consumed through WalletClient (a subset of the wallet
// use-case service). Concrete adapters live under
// internal/adapter/db/postgres/aiassist (Summary) and adapters/openrouter
// (LLM).
//
// Concurrency / debit contract (ADR-0043, AC #4 of SIN-62196):
//
//   - EstimateTokens computes an upper bound for (prompt + max output).
//     The use case calls wallet.Reserve(estimate) BEFORE the LLM round
//     trip so concurrent callers race at the wallet, not at the LLM.
//   - wallet.Reserve is atomic per F30 (SELECT … FOR UPDATE + UNIQUE
//     (wallet_id, idempotency_key)). A failed Reserve never charges the
//     wallet; the use case maps wallet.ErrInsufficientFunds to
//     ErrInsufficientBalance so the caller can render a "buy more
//     credits" affordance without leaking the internal sentinel.
//   - On LLM success the use case calls wallet.Commit(actual) where
//     actual = tokensIn + tokensOut clamped to the reservation upper
//     bound. The clamp is defensive: the OpenRouter adapter caps
//     MaxTokens so the upstream cannot exceed the reservation, but a
//     tokenizer mismatch (our /4 estimator vs. upstream's BPE) could
//     in theory drift; clamping rather than failing keeps the user-
//     visible request from erroring after the LLM has already done the
//     work, and emits an observability signal in the structured log.
//   - On LLM error the use case calls wallet.Release(reservation) so
//     the reservation does not silently age out into the F37 reaper's
//     backstop.
//
// Idempotency:
//
//   - The boundary idempotency key is (tenant_id, conversation_id,
//     request_id). The use case builds the per-step keys by appending
//     ":reserve", ":commit", and ":release" so the wallet ledger has
//     distinct rows for each phase but the same logical request.
//   - Re-running Summarize with the same request_id returns the prior
//     summary when the wallet's idempotency short-circuit fires (see
//     internal/wallet/usecase/usecase.go for the underlying semantics).
//
// Cache (AC #6):
//
//   - Summary.IsValid combines TTL (expires_at) and explicit
//     invalidation (invalidated_at). When a new message lands on the
//     conversation, the inbox use case calls Invalidate so the next
//     Summarize call regenerates rather than serving a stale summary.
//
// Out of scope here:
//
//   - PII anonymizer (SIN-62350 W3B) — wraps the LLMClient adapter in
//     cmd/server wiring.
//   - OpenRouter HTTP adapter (SIN-62904 W3A) — already on fork/main.
//   - HTMX UI (SIN-62908 W4D).
package aiassist
