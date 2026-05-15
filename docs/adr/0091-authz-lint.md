# ADR 0091 — Authorization wireup lint and public-route allowlist

- Status: Accepted (allowlist + middleware land in PR-A;
  AST lint lands in PR-B)
- Date: 2026-05-14
- Drives: [SIN-62728](/SIN/issues/SIN-62728)
- Pairs with: [ADR 0090](0090-rbac-matrix.md)

## Context

[ADR 0090](0090-rbac-matrix.md) defines the deterministic policy:
`Authorizer.Can` returns a typed `Decision` per `(Principal, Action,
Resource)` tuple. That policy is useful only when handlers actually
consult it — otherwise a new route can ship without any gate at all
and the deny-by-default property of the system silently degrades.
This ADR addresses three things that defend the wireup:

1. A canonical `RequireAuth` middleware (deny-by-default) that lifts
   the validated `iam.Session` into an `iam.Principal`.
2. A canonical `RequireAction(authz, action, resolve)` middleware that
   consults the Authorizer and emits a typed Decision.
3. A **declarative allowlist** of routes that are permitted to skip
   `RequireAuth` (login form, health, prometheus scrape, etc.), plus a
   static-analysis lint (`cmd/authzlint`, PR-B) that enforces:

       Every chi-registered route is in the allowlist
       OR is wrapped in RequireAuth + RequireAction.

The lint is the build-time backstop. Even with `RequireAuth` widely
adopted, a future PR could mount a handler outside the authed group
and re-introduce an unauthenticated path. The lint catches that in CI
before it ships.

## Decision

### L1. `internal/adapter/httpapi/middleware.RequireAuth` is canonical.

`RequireAuth` is the named gate the lint searches for. It:

- reads `iam.Session` from context (set by the existing
  `middleware.Auth`),
- constructs an `iam.Principal` via
  `iam.PrincipalFromSession(session, masterImpersonating,
  mfaVerifiedAt)`,
- attaches the Principal to the request context via
  `iam.WithPrincipal`,
- responds `401 Unauthorized` (closed message) when the session is
  missing — defense-in-depth against a wireup bug that mounts an
  authenticated route without `middleware.Auth`.

The existing `middleware.Auth` keeps responsibility for the cookie →
session lookup; `RequireAuth` is the lifter and the lint anchor.

### L2. `RequireAction(authz, action, resolve)` is the second mandatory link.

`RequireAction`:

- reads the `Principal` from context (set by `RequireAuth`),
- resolves the `iam.Resource` via the optional `ResourceResolver`,
- calls `authz.Can(ctx, principal, action, resource)`,
- attaches the `Decision` to context via `WithDecision` so audit
  middleware ([SIN-62254](/SIN/issues/SIN-62254)) can emit it without
  re-calling the Authorizer,
- responds `403 Forbidden` with a generic `forbidden` body when the
  decision is deny — the `ReasonCode` is intentionally NOT echoed on
  the wire because policy names (`denied_master_pii_step_up`,
  `denied_rbac`, …) leak the existence and shape of internal
  authorization gates to external tenants
  ([SIN-62756](/SIN/issues/SIN-62756)). The full `ReasonCode` still
  rides the audit trail via `WithDecision`, so internal troubleshooting
  is unaffected.

### L3. The public-route allowlist is declarative.

`internal/adapter/httpapi.PublicRoutes()` returns the closed set of
`(method, pattern, reason)` rows that MAY skip `RequireAuth`. The list
is small and well-justified — every entry is an audit decision. The
Fase 1 contents:

- `GET /health` — liveness; the LB reaches it before tenant resolves.
- `GET /metrics` — Prometheus; access control at the network edge.
- `POST /internal/test-alert` — smoke-alert seam; the prod build
  serves 404, only `-tags test` runs the body.
- `GET|POST /login`, `GET|POST /m/login`, `GET /m/logout` — credential
  surfaces; the session does not exist yet.

Adding an entry requires an issue-thread justification on
[SIN-62728](/SIN/issues/SIN-62728) or a successor ADR — this list is
exactly the kind of drift the lint exists to catch.

### L4. `cmd/authzlint` is the AST analyzer (PR-B follow-up).

A `golang.org/x/tools/go/analysis` analyzer mounted via
`make lint` and pre-commit. It:

1. parses every package under `internal/adapter/httpapi/...`,
2. identifies chi route registrations (`r.Get`, `r.Post`, `r.Method`,
   `r.Mount`, etc.),
3. for each `(method, pattern)`, walks back the chi.Group context to
   determine which middleware wraps it,
4. fails when the route is **not** in `PublicRoutes()` **and**
   `RequireAuth` + `RequireAction` are not present on the chain.

The analyzer is intentionally conservative: an unrecognised middleware
between `RequireAuth` and the handler does not falsify the chain, but
a missing `RequireAuth` does. PR-B (split per the
[SIN-62728](/SIN/issues/SIN-62728) PR strategy) lands the analyzer
and a regression test that adds a deliberately mis-wired route and
asserts `make lint` exits non-zero.

### L5. Defense-in-depth layering.

The three layers — lint at build time, RequireAuth + RequireAction at
request time, RBAC matrix + PII gate at policy time — are independent.
Failing one of them does not silently grant access:

- A handler that forgets `RequireAuth`: lint fails build.
- A `RequireAuth`-gated handler with no `RequireAction`: lint fails.
- A handler that chains both but uses an unknown Action: Authorizer
  denies with `ReasonDeniedUnknownAction`.
- A handler that chains both and uses a known Action but the
  Principal's role does not cover it: Authorizer denies with
  `ReasonDeniedRBAC`.

## Consequences

- Wireup mistakes fail in CI rather than at runtime, before a request
  ever touches an unauthenticated handler.
- The allowlist is one file; review of "what is reachable without
  auth" is a single-screen exercise.
- Every authz check emits a typed `Decision` so deny logging
  ([SIN-62254](/SIN/issues/SIN-62254)) is uniform across the codebase.

## Alternatives considered

- **Manual checklist on PR review.** Rejected: drift is invisible in
  diff hunks and reviewer fatigue is real.
- **`golangci-lint` `revive` rules.** Rejected: matching chi `r.Method`
  patterns reliably needs the type-checked AST, which `revive` doesn't
  give us at the granularity we need.
- **Tagged comments on handlers (`//authz:public`).** Rejected:
  comments diverge from the wireup, defeating the purpose.

## References

- [ADR 0090 RBAC matrix](0090-rbac-matrix.md)
- [SIN-62254 (downstream)](/SIN/issues/SIN-62254)
- [SIN-62222 ADR 0074 (absorbed source)](/SIN/issues/SIN-62222#document-adr-0074-authz-lint)
