// Package aipanel is the HTTP adapter for the LGPD per-scope AI consent
// modal — Fase 3 decisão #8, SIN-62929.
//
// When an aiassist use-case (Summarize today, Suggest tomorrow) returns
// the typed *aiassist.ConsentRequired error, the caller renders this
// package's consent modal into the inbox right panel. The operator
// inspects the anonymized preview, version markers, and a short SHA-256
// fingerprint, then either Confirms or Cancels:
//
//   - Confirm → POST /aipanel/consent/accept persists the consent row
//     via aipolicy.ConsentService and emits an HX-Trigger so the
//     original assist request re-fires.
//   - Cancel  → POST /aipanel/consent/cancel records the metric outcome
//     and removes the modal via the HTMX outerHTML swap. The aiassist
//     call is NOT retried.
//
// The package has zero awareness of which use-case triggered the modal;
// scope, payload, anonymizer/prompt versions, and payload_hash travel
// over the wire so the adapter stays use-case-agnostic and the future
// Suggest caller (W4D) can reuse the same surface.
//
// Security posture:
//
//   - Both endpoints sit behind the project's auth + CSRF middleware
//     stack — actor_user_id is derived from the session, NEVER from the
//     request body (ADR 0093 / SIN-62225).
//   - The anonymized payload preview is HTML-escaped via Go template
//     auto-escape inside a <pre> block. There is zero template.HTML in
//     the preview path (mitigation F29 from SIN-62225).
//   - Anti-tampering: the accept handler recomputes SHA-256 over the
//     anonymized payload supplied with the form and rejects when the
//     digest does not match the payload_hash field. This catches a
//     client that swapped the payload while keeping the original hash.
package aipanel
