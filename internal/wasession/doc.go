// Package wasession is the hexagonal core of the unofficial WhatsApp Web
// channel (ADR 0107). It maintains one WhatsApp Web session per tenant:
// QR pairing, credential persistence, reconnection and status signalling
// (connected / disconnected / banned), and it fans inbound message and
// status events out to a single consumer.
//
// The package is transport-agnostic: it never imports go.mau.fi/whatsmeow.
// The concrete whatsmeow client and its Postgres credential store live in
// the whatsmeowdev sub-package behind the Device / DeviceFactory ports
// declared here. This keeps the session lifecycle logic fully unit-testable
// with fakes and lets the carrier library be swapped without touching the
// orchestration (ADR 0107 D1/D4).
//
// Security bar (ADR 0107 D6): session credentials are secret. Pairing codes
// are wrapped in Credential, whose String/Format/JSON renderings redact the
// value, and the Manager only ever logs the tenant id and status — never a
// message body, phone number or credential.
package wasession
