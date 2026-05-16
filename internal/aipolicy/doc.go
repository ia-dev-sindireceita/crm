// Package aipolicy is the bounded context for per-scope AI assist
// configuration: which model is called, whether payloads are
// anonymised, and whether the tenant has opted in at all.
//
// The package is the domain core: it imports neither database/sql nor
// pgx, neither net/http nor any vendor SDK. Storage lives behind the
// Repository port; the Postgres adapter ships in
// internal/adapter/db/postgres/aipolicy.
//
// The Resolver implements the cascade contract from ADR-0042
// (channel > team > tenant > default, all-or-nothing). The first
// matching row wins in full; there is no field-level merge. A tenant
// that has never been configured falls through to DefaultPolicy(),
// which has AIEnabled = false so the use-case denies the call (LGPD
// opt-in, ADR-0041).
//
// The audit pipeline (SIN-62353 / decisão #8) plugs in via the
// AuditLogger port plus the RecordingRepository decorator: every
// Upsert that goes through RecordingRepository.Upsert emits one
// AuditEvent per changed field (or one FieldCreated event when no
// prior row existed), attributing the change to the Actor pulled
// from request context. The pgx adapter for AuditLogger lives in
// internal/adapter/db/postgres/aipolicy (migration 0099 backs it).
//
// SIN-62351 (Fase 3 W2A, child of SIN-62196) + SIN-62353 (Fase 3 H1).
package aipolicy
