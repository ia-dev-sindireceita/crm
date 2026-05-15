// Package metashared bundles the cross-channel primitives every Meta
// product family (WhatsApp, Instagram, Messenger) needs at the webhook
// boundary: HMAC-SHA256 signature verification, the Meta-published
// source-IP allowlist, and the Deduper port that backs idempotent
// inbound delivery.
//
// The package is intentionally infrastructure-only. It MUST NOT import
// any specific channel adapter (whatsapp / instagram / messenger) nor
// any domain package (inbox, contacts). Specific channels depend on
// metashared, never the reverse. SIN-62791.
package metashared
