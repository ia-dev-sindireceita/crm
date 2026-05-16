// Package openrouter is the HTTP adapter for the OpenRouter chat-completions
// API. It implements the LLM client port consumed by internal/aiassist
// (W2C, SIN-62903): a single Complete(ctx, req) method that returns
// generated text plus the in/out token counts charged by the upstream
// provider.
//
// Design constraints (SIN-62904 / Fase 3 W3A):
//
//   - Boring tech: net/http stdlib + time.Sleep backoff. No retry library.
//   - Defense in depth: log redaction (no prompt, no response body) +
//     8s context timeout + capped retry + idempotency-key header.
//   - Observability before optimisation: every request emits a structured
//     log line and Prometheus metrics (duration histogram + tokens
//     counter) before any latency tuning happens.
//   - Idiomatic Go: small surface (Complete only), typed errors
//     (ErrUpstream5xx, ErrTimeout, ErrRateLimited) so callers can branch
//     with errors.Is.
//
// Secrets: the API key is supplied at construction time (cmd/server reads
// it from an env var). It is set on the Authorization header and is never
// emitted in logs or metrics.
//
// Out of scope: PII anonymizer (SIN-62350 W3B) and cmd/server wiring
// (SIN-62908 W4D).
package openrouter
