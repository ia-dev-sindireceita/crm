// Package consent implements a generic LGPD consent ledger for
// non-AI purposes: terms of service, privacy policy, marketing
// communications, and analytics cookies.
//
// The package exposes:
//
//   - ConsentRecord — aggregate that mirrors one consent_record row.
//   - ConsentRegistry — the storage port (Record/Latest/History/Revoke).
//   - RecordingRegistry — decorator that wraps any ConsentRegistry
//     and emits one audit_log_data event per Record/Revoke.
//
// The pgx-backed adapter lives in
// internal/iam/consent/pgconsent and implements ConsentRegistry on
// top of migration 0107.
//
// AI-specific consent (per-IA-call payload acceptance) is NOT in
// this package. That use-case has different invariants
// (anonymizer/prompt version pivots, single active row per scope)
// and continues to live in internal/aipolicy. See ADR 0101.
//
// SIN-63185 / Fase 6 PR2.
package consent
