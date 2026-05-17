package rules

import "errors"

// ErrInvalidTenant is returned when a method receives uuid.Nil where a
// real tenant id is required. Mirrors the convention used by other
// bounded contexts in this codebase (campaigns, funnel).
var ErrInvalidTenant = errors.New("rules: invalid tenant")

// ErrInvalidRule is returned when a Rule fails the structural
// validation enforced by [NewRule] (missing trigger type, unknown
// scope, etc.). The error is intentionally coarse — callers that
// need to surface a precise reason should pre-validate.
var ErrInvalidRule = errors.New("rules: invalid rule")

// ErrUnknownTriggerType is returned when a Rule carries a
// TriggerType outside the documented enum. The application boundary
// (HTMX form / API handler) is expected to validate before
// constructing the rule; the resolver treats unknown types as
// non-deduplicating to remain forward-compatible without crashing.
var ErrUnknownTriggerType = errors.New("rules: unknown trigger type")

// ErrUnknownActionType mirrors ErrUnknownTriggerType for actions.
var ErrUnknownActionType = errors.New("rules: unknown action type")

// ErrNotFound is returned by [RuleAdminRepository.Get],
// [RuleAdminRepository.Update], [RuleAdminRepository.SetEnabled] and
// [RuleAdminRepository.Delete] when no row matches under the current
// tenant. Callers translate this into a 404.
var ErrNotFound = errors.New("rules: not found")
