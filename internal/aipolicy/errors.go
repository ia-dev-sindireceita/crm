package aipolicy

import "errors"

// ErrInvalidTenant is returned when TenantID is uuid.Nil. Every policy
// row is tenant-scoped; an anonymous Resolve call is a programming
// bug that the resolver refuses to translate into a SourceDefault fallback.
var ErrInvalidTenant = errors.New("aipolicy: invalid tenant id")

// ErrInvalidScopeType is returned when a Policy passed to Upsert
// carries a ScopeType outside {tenant, team, channel}. The same
// CHECK constraint exists in Postgres, but the domain rejects the
// value before the adapter so callers see a typed error instead of
// a SQL state code.
var ErrInvalidScopeType = errors.New("aipolicy: invalid scope type")

// ErrInvalidScopeID is returned when ScopeID is blank after trimming.
// scope_id is NOT NULL in migration 0098; the domain pre-validates so
// the adapter does not have to translate a generic NOT NULL violation.
var ErrInvalidScopeID = errors.New("aipolicy: invalid scope id")
