# ADR 0090 — RBAC matrix and Authorizer interface

- Status: Accepted
- Date: 2026-05-14
- Drives: [SIN-62728](/SIN/issues/SIN-62728), [SIN-62254](/SIN/issues/SIN-62254)
- Supersedes: none — formalises the matrix that was implicit in
  [SIN-62192](/SIN/issues/SIN-62192) Fase 0 gate and the `internal/iam.Role`
  enum from [ADR 0073](0073-csrf-and-session.md).

## Context

Fase 0 closed with `iam.Role` exporting four values (`master`,
`tenant_gerente`, `tenant_atendente`, `tenant_common`), but no
deterministic mapping from those roles to authorizable actions. The
absorbed-AC list for Fase 0 ([SIN-62222](/SIN/issues/SIN-62222) ADR
0074) committed to landing six artefacts (RequireAuth middleware,
public-route allowlist, Authorizer, AST lint, contract tests, ADRs).
Six of seven did not enter code during the 20 re-landing batches.
[SIN-62728](/SIN/issues/SIN-62728) re-opens that gap; this ADR is the
deterministic policy half (the lint that enforces wireup lives in
[ADR 0091](0091-authz-lint.md)).

The downstream consumer is [SIN-62254](/SIN/issues/SIN-62254) — deny
logging 100%, allow sampling 1%, Prometheus rule, runbook. That work
requires a stable `Decision{Allow, ReasonCode, TargetKind, TargetID}`
shape so `audit.WriteSecurity` and the Prometheus deny counter can
read uniform fields off every authz check.

## Decision

### M1. Authorizer is an interface in `internal/iam`.

```go
type Authorizer interface {
    Can(ctx context.Context, p Principal, a Action, r Resource) Decision
}
```

The interface lives in the domain (`internal/iam`). HTTP handlers and
the `RequireAction` middleware depend on the interface, never on a
concrete implementation. Alternative policies (ABAC, OPA, signed
attestations) implement the same shape and are swapped at wireup.

### M2. The production Authorizer is `RBACAuthorizer`.

`RBACAuthorizer` is deterministic: a closed table maps `Action` to the
set of `Role` values that may invoke it. The table is built at
construction (`NewRBACAuthorizer`), not in a package-level `var`, so
tests can build alternative instances without mutating shared state.

The matrix (Fase 1 starting set + Fase 2.5 billing/wallet delta from
[SIN-62880](/SIN/issues/SIN-62880); extensions follow the issue-thread
process below):

| Action                                            | master | gerente | atendente | common |
|---------------------------------------------------|:-:|:-:|:-:|:-:|
| `tenant.contact.read`                             |   | ✓ | ✓ | ✓ |
| `tenant.contact.read_pii` (PII)                   |   | ✓ |   |   |
| `tenant.contact.create`                           |   | ✓ |   |   |
| `tenant.contact.update`                           |   | ✓ | ✓ |   |
| `tenant.contact.delete`                           |   | ✓ |   |   |
| `tenant.conversation.read`                        |   | ✓ | ✓ | ✓ |
| `tenant.message.read`                             |   | ✓ | ✓ | ✓ |
| `tenant.message.send`                             |   | ✓ | ✓ |   |
| `tenant.billing.view`                             |   | ✓ |   |   |
| `tenant.wallet.view_ledger`                       |   | ✓ |   |   |
| `master.tenant.create`                            | ✓ |   |   |   |
| `master.tenant.read`                              | ✓ |   |   |   |
| `master.tenant.update`                            | ✓ |   |   |   |
| `master.tenant.delete`                            | ✓ |   |   |   |
| `master.tenant.impersonate`                       | ✓ |   |   |   |
| `master.subscription.assign_plan`                 | ✓ |   |   |   |
| `master.subscription.cancel`                      | ✓ |   |   |   |
| `master.grant_courtesy.free_subscription_period`  | ✓ |   |   |   |
| `master.grant_courtesy.extra_tokens`              | ✓ |   |   |   |
| `master.grant_courtesy.revoke`                    | ✓ |   |   |   |

An `Action` absent from the map denies for every role (deny by
default) and returns `ReasonDeniedUnknownAction`.

### M3. Master tenant-PII gate.

A master operator impersonating a tenant (`Principal.MasterImpersonating
== true`) requesting a PII action (`tenant.contact.read_pii` and any
future `_pii`-suffixed action listed in `piiActions`) is **denied**
unless `Principal.MFAVerifiedAt` is within
`RBACConfig.MasterPIIStepUpWindow` of the configured `Now`. The gate
fires **before** the RBAC check, so even a master who carries a tenant
role on the request is bounced when the step-up is stale.

The window is wired at process startup (production: 5 minutes; tests
pin it via `RBACConfig.Now`).

The denial returns `ReasonDeniedMasterPIIStepUp`, distinct from
`ReasonDeniedRBAC`, so [SIN-62254](/SIN/issues/SIN-62254) can route
PII-gate denials to a higher-priority Slack alert than ordinary RBAC
denials.

### M4. Decision fields are part of the audit contract.

```go
type Decision struct {
    Allow      bool
    ReasonCode ReasonCode
    TargetKind string
    TargetID   string
}
```

`ReasonCode` is a closed set (see `internal/iam/authorizer.go`).
`TargetKind`/`TargetID` mirror the `Resource` so audit writers do not
re-resolve them. The string values of `ReasonCode` and `Action` are
audit-stable: renaming requires an ADR delta.

### M5. Per-resource tenant scoping.

A `Resource.TenantID` different from `Principal.TenantID` is denied
with `ReasonDeniedTenantMismatch`, even for actions the role would
otherwise permit. This is defense-in-depth on top of RLS
([ADR 0072](0072-rls-policies.md)).

### M6. Extension process.

Adding an `Action`, a `ReasonCode`, or a row to the matrix requires:

1. an entry in this ADR's matrix table,
2. a new constant in `internal/iam/authorizer.go`,
3. a row in `internal/adapter/httpapi/authz_contract_test.go`.

A PR that ships one without the other two fails CI: contract test
breaks if (1) and (2) disagree; lint ([ADR 0091](0091-authz-lint.md))
breaks if a handler references an unknown `Action`.

### M7. Route → Action wireup pattern (first protected route).

[SIN-62767](/SIN/issues/SIN-62767) mounts the first production
`RequireAction` gate on `GET /hello-tenant` with
`iam.ActionTenantContactRead` via the
[SIN-62765](/SIN/issues/SIN-62765) audited `Deps.Authorizer` seam.
Route additions follow the same shape: chain
`middleware.RequireAuth → middleware.RequireAction(deps.Authorizer,
<action>, <resourceResolver>)` inside the authenticated tenant group
in `internal/adapter/httpapi/router.go`. The audited authorizer is
the single seam — handlers MUST NOT take a bare `*RBACAuthorizer`,
or `audit_log_security` writes silently miss the route.

Action choice for the first-route is `ActionTenantContactRead` —
the most-permissive tenant-scope read entry, so the three tenant
roles keep their pre-PR access while empty-role / cross-role probes
(the F10 horizontal-probing pattern) produce 403 + audit row +
`authz_user_deny_total` increment. Future routes pick the action
that matches the handler's actual operation; the matrix in M2
remains authoritative.

## Consequences

- Every authz check produces a `Decision` with a closed-set
  `ReasonCode`, so [SIN-62254](/SIN/issues/SIN-62254) can index audit
  rows and Prometheus labels without an open enum.
- The `ReasonCode` is internal-only: it rides the `Decision` through
  context to audit and Prometheus, but the HTTP `403` body is the
  generic string `forbidden` so policy names do not leak to external
  tenants ([SIN-62756](/SIN/issues/SIN-62756)). Operators read the
  reason in audit logs and Prometheus, never on the wire.
- The PII gate is independent of role: a master with a tenant role on
  the request still hits the gate, so a step-up bypass requires
  forging both Principal.MasterImpersonating and Principal.MFAVerifiedAt.
- The matrix is small on purpose. Adding new actions is cheap (one
  constant + one row + one test cell) — small surface keeps the audit
  cardinality bounded.
- Alternative policies (e.g. attribute-based) can plug in at wireup
  without changing handlers, because handlers depend on the
  `Authorizer` interface, not on `RBACAuthorizer`.

## Alternatives considered

- **Per-handler `if role == "gerente" {...}`.** Rejected: scatters the
  matrix across files, no audit shape, no lint enforcement, regressions
  invisible in code review.
- **OPA / Rego.** Rejected for now (boring-tech budget): a 60-line Go
  table is enough for Fase 1; an OPA bridge can re-implement
  `Authorizer.Can` later without changing handlers.
- **ABAC on tenant + role + resource attributes.** Rejected: not needed
  for Fase 1's matrix; the cost is more rule complexity per action,
  which lengthens the audit and lint surface.

## References

- [SIN-62222 ADR 0074 (absorbed)](/SIN/issues/SIN-62222#document-adr-0074-authz-lint)
- [ADR 0072 RLS policies](0072-rls-policies.md)
- [ADR 0073 CSRF and session](0073-csrf-and-session.md)
