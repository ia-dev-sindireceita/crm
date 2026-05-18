// Package pix is the HTTP boundary for the PIX webhook receivers.
//
// Today the single receiver lives at POST /webhooks/pix/inter (SIN-62964).
// The handler is the only place that touches net/http; everything
// downstream (signature verify, body parse, reconciliation) goes
// through pix.* ports + the per-PSP adapter packages.
//
// Defense-in-depth chain per SIN-62964 AC: IP allowlist → per-IP rate
// limit → HMAC signature → per-event rate limit → idempotent
// reconciler. Each layer is independently observable via the
// structured Logger port; each rejection maps to its own Outcome label
// so dashboards can attribute the rejection reason without parsing
// free-text messages.
package pix
