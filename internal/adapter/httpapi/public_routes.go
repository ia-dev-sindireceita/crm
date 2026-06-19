// Package httpapi public_routes — declarative allowlist of routes that
// MAY be reached without RequireAuth. Anything not in this list is
// expected to chain RequireAuth + RequireAction.
//
// The list is the single source of truth consumed by:
//
//  1. The router wireup (regression test asserts every public route
//     handler is mounted without middleware.Auth — to prove the spec
//     and the wireup agree).
//  2. The cmd/authzlint analyzer (PR-B, [SIN-62728]§4 follow-up) —
//     it parses chi route registrations and fails when a route is
//     neither in this list nor wrapped in RequireAuth + RequireAction.
//
// Patterns are stored as (Method, Pattern) tuples. Pattern strings
// follow chi's path-template syntax (no leading host, no query). Each
// row carries a Reason so the audit trail and the lint diagnostic can
// quote a load-bearing justification.
//
// Adding a new entry requires a comment in the issue thread (or an
// ADR delta) — declarative-allowlist drift is exactly the regression
// the lint exists to catch.
package httpapi

import "net/http"

// PublicRoute describes one allowlisted route entry. Method "" means
// "every method" (rare — preserved for chi.Method(MethodAny, ...)
// patterns). Pattern is matched verbatim against the chi-registered
// path template; trailing slashes are significant.
type PublicRoute struct {
	Method  string
	Pattern string
	Reason  string
}

// publicRoutes is the declarative allowlist (ADR 0090 §Public list).
// Keep this list short and well-justified — every entry is a place an
// unauthenticated request reaches an application handler.
var publicRoutes = []PublicRoute{
	{Method: http.MethodGet, Pattern: "/health", Reason: "liveness probe; LB reaches by IP before tenant resolves"},
	{Method: http.MethodGet, Pattern: "/metrics", Reason: "prometheus scrape; access control at network edge"},
	{Method: http.MethodPost, Pattern: "/internal/test-alert", Reason: "smoke-alert seam; test build tag only in prod"},
	{Method: http.MethodGet, Pattern: "/login", Reason: "login form render (tenant scope, no session yet)"},
	{Method: http.MethodPost, Pattern: "/login", Reason: "credential submit (mints the session)"},
	{Method: http.MethodGet, Pattern: "/m/login", Reason: "master login form (no session yet)"},
	{Method: http.MethodPost, Pattern: "/m/login", Reason: "master credential submit (mints the master session)"},
	{Method: http.MethodPost, Pattern: "/m/logout", Reason: "master logout submit (clears cookie); POST-only to defeat forced-logout CSRF — SIN-65232"},
}

// PublicRoutes returns a copy of the declarative allowlist. Callers
// (router wireup, lint analyzer, tests) MUST treat it as read-only;
// the function copies to defend against in-place mutation that would
// otherwise affect every caller in the process.
func PublicRoutes() []PublicRoute {
	out := make([]PublicRoute, len(publicRoutes))
	copy(out, publicRoutes)
	return out
}

// IsPublic reports whether the (method, pattern) pair is in the
// allowlist. The lint analyzer uses this to check each chi route
// registration; tests call it for explicit-table assertions.
func IsPublic(method, pattern string) bool {
	for _, r := range publicRoutes {
		if (r.Method == "" || r.Method == method) && r.Pattern == pattern {
			return true
		}
	}
	return false
}
