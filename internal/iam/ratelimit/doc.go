// Package ratelimit holds the rate-limit and account-lockout domain ports
// used by the authentication paths (SIN-62341, ADR 0073 D4).
//
// This package is the domain core for the F19 controls: per-window
// counters that throttle abuse on /login, /2fa/verify, /password/reset,
// /m/login and /m/2fa/verify, plus a durable per-user lockout that
// outlives a Redis flush.
//
// Two ports live here:
//
//   - RateLimiter — counts hits over a sliding window. Implemented by
//     a Redis adapter (internal/adapter/ratelimit/redis); the contract
//     is intentionally storage-agnostic so a future Postgres-backed
//     emergency fallback can satisfy the same surface.
//   - Lockouts — durable "this principal is locked until T" state.
//     Implemented by Postgres adapters (internal/adapter/db/postgres),
//     one per scope (tenant / master) so each adapter knows which
//     transactional helper (WithTenant / WithMasterOps) to use.
//
// The two ports are independent on purpose: a Redis flush wipes the
// short-window counters but Lockouts keeps the long-tail penalty
// (Defense in depth — ADR 0073 §D4). The login path checks Lockouts
// FIRST so locked accounts fail fast before any password verification.
//
// Per the project's hexagonal rule, this package MUST NOT import the
// Redis SDK or database/sql. Acceptance criterion #8 of SIN-62341.
package ratelimit
