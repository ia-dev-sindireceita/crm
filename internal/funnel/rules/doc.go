// Package rules holds the funnel-automation rule domain: the [Rule]
// entity, the [RuleRepository] port, and the cascade [Resolver] that
// chooses the effective rule set for an event scope.
//
// Architectural decision: the cascade order is channel > team > tenant,
// applied per-trigger first-match-wins. See
// [ADR-0099 / ex-ADR-0088 (C2)](../../../docs/adr/0099-funnel-rules-cascade.md)
// for the full rationale (the originating Paperclip issue
// [SIN-62953](/SIN/issues/SIN-62953) proposed ADR-0088; that index was
// already taken by `0088-wallet-basic.md`, so the same ADR landed under
// the next free index, 0099 — the cascade decision is unchanged).
//
// Storage shape: every persisted rule lives in funnel_rules (migration
// 0102) with two NULLABLE columns — channel (text) and team_id (uuid)
// — that together encode the scope:
//
//   - channel non-empty           → channel scope (most specific)
//   - channel empty, team_id set  → team scope
//   - both NULL                   → tenant scope (least specific)
//
// The Resolver collapses cross-scope conflicts on the same trigger by
// adopting the most-specific rule and shadowing the rest. Distinct
// triggers across scopes all survive — see the ADR D2/D3 worked
// example for the dedup semantics.
//
// The package is pure domain: it does NOT import database/sql, pgx,
// net/http, or any vendor SDK. Storage lives behind [RuleRepository];
// the adapter is in internal/adapter/db/postgres/funnelrules.
//
// SIN-62955 (Fase 4 / child of [SIN-62197](/SIN/issues/SIN-62197)).
package rules
