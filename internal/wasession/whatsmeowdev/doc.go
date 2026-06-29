// Package whatsmeowdev is the concrete carrier binding for the unofficial
// WhatsApp Web channel: it implements the wasession.Device and
// wasession.DeviceFactory ports on top of go.mau.fi/whatsmeow and its
// Postgres credential store (ADR 0107 D1/D3).
//
// It is the only package in the tree that imports whatsmeow. All session
// lifecycle orchestration lives in the parent wasession package behind the
// ports; this package translates between whatsmeow's event/JID types and the
// carrier-agnostic wasession types, and drives the whatsmeow client.
//
// Testability: the whatsmeow *Client is used through the small waClient
// interface, so the device's pairing / send / event-dispatch logic is
// exercised with an in-memory fake transport. The factory is exercised with
// whatsmeow's in-memory store (store.NewDevice) via a StoreProvider seam. The
// only code that requires a live Postgres / WhatsApp connection is Open and
// the sqlstore-backed StoreProvider in store.go, which is integration-only.
//
// Per-tenant credential isolation: ADR 0107 D3 records that the final store
// layout (tenant_id column vs schema-per-tenant) is settled in a follow-up
// migration ADR (DT-WA-02). This package keeps store resolution behind the
// StoreProvider seam so that decision can land without touching the device or
// the domain.
package whatsmeowdev
