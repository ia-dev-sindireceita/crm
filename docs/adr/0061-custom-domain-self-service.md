# ADR 0061 — Custom-domain self-service (end-to-end flow & Fase 5 supersession)

- Status: Proposed (pending CEO confirmation on supersession)
- Date: 2026-05-19
- Issue: [SIN-63073](/SIN/issues/SIN-63073) — Fase 5 / ADR-0061
- Parent goal: [SIN-62198](/SIN/issues/SIN-62198) — Fase 5 (White-label avançado)
- Supersedes (proposed): the Fase-5 sub-plan that called for a parallel
  re-implementation under `internal/tenancy/custom_domain.*` ([SIN-63077](/SIN/issues/SIN-63077))
  and a separate `tenant_custom_domain` (singular) migration ([SIN-63075](/SIN/issues/SIN-63075)).
- Related: [ADR-0079](./0079-custom-domain.md) — ownership validation, anti-rebinding, rate limits.

## Context

Fase 5 was scoped before the F43–F53 custom-domain hardening sprint ran
its course. The Fase-5 plan ([SIN-62198 AC §3–§6](/SIN/issues/SIN-62198))
describes the user-visible flow (cadastro → instrução DNS → propagação
→ ativação) and asks for an entity `CustomDomain` under
`internal/tenancy` plus a new migration `tenant_custom_domain`.

Between the Fase-5 plan and today, the F45 / F51 / F53 stack shipped a
working custom-domain self-service end-to-end:

| Fase-5 piece                             | Where it lives today                                           |
|------------------------------------------|----------------------------------------------------------------|
| Entity + lifecycle                       | `internal/customdomain/management` (`Domain`, `UseCase`)       |
| State machine                            | Derived from timestamps in `management.StatusOf`               |
| Uniqueness                               | `migrations/0010_tenant_custom_domains.up.sql` (partial idx)   |
| TXT-challenge validation                 | `internal/customdomain/validation` ([ADR-0079 §1](./0079-custom-domain.md)) |
| Anti-rebinding / IP allowlist            | Unbound sidecar ([ADR-0079 §2](./0079-custom-domain.md))       |
| Handler rate limits                      | `internal/customdomain/ratelimit` ([ADR-0079 §3](./0079-custom-domain.md)) |
| Per-tenant enrollment quotas             | `internal/customdomain/enrollment` ([ADR-0079 §4](./0079-custom-domain.md)) |
| LE circuit breaker                       | `internal/customdomain/circuitbreaker`                          |
| On-demand TLS issuance hook              | `internal/customdomain/tls_ask`                                 |
| Feature flag                             | `internal/customdomain/featureflag`                             |
| Slug reservation on delete (12-month)    | `internal/slugreservation`                                      |
| Soft-delete + audit                      | `tenant_custom_domains.deleted_at` + `audit_log`                |

ADR-0079 captures the security/anti-rebinding contract but does not
narrate the **product flow**: what the tenant sees, the four-state
badge, the abuse-limit numbers the tenant hits, and the teardown
behaviour. This ADR fills that gap, and folds the Fase-5 ACs into
the existing implementation rather than duplicating it.

## Decision

### §1 — End-to-end flow (canonical)

```
┌────────────────────────────────────────────────────────────────────┐
│ Tenant admin clicks "Adicionar domínio" in /settings/dominios      │
│                                                                    │
│   POST /api/tenant/custom-domains  { host: "crm.acme.com.br" }     │
│                                                                    │
│   management.Enroll:                                               │
│     1. normalize+validate (FQDN, length, blocklist apex)           │
│     2. enrollment.Gate.Check (rate-limit + hard-cap + breaker)     │
│     3. tokenGen → 32-byte hex verificationToken                    │
│     4. store.Create (partial UNIQUE on LOWER(host) WHERE           │
│        deleted_at IS NULL → 409 on collision)                      │
│     5. audit_log{customdomain_enrolled}                            │
│                                                                    │
│   UI shows DNS instructions:                                       │
│     TXT _crm-verify.crm.acme.com.br = "<token>"                    │
│                                                                    │
│       (status = Pending — verified_at IS NULL)                     │
│                                                                    │
│ Tenant publishes TXT in their DNS                                  │
│                                                                    │
│   POST /api/tenant/custom-domains/{id}/verify                      │
│                                                                    │
│   management.Verify:                                               │
│     1. validation.Validate (TXT-only, IP allowlist) — ADR-0079 §1  │
│     2. on success → store.MarkVerified(verified_at, dnssec_bit)    │
│        + dns_resolution_log row pinning the resolved IP            │
│     3. audit_log{customdomain_verified}                            │
│                                                                    │
│       (status = Verified)                                          │
│                                                                    │
│ First request to https://crm.acme.com.br                           │
│                                                                    │
│   Caddy → /internal/tls/ask?domain=crm.acme.com.br                 │
│   tls_ask.UseCase returns 200 iff:                                 │
│     verified_at IS NOT NULL AND tls_paused_at IS NULL              │
│     AND deleted_at IS NULL                                         │
│   Caddy issues ACME cert via Unbound (ADR-0079 §2)                 │
│                                                                    │
│       (operationally Active — same Verified row, certificate now   │
│        cached by Caddy)                                            │
│                                                                    │
│ Tenant deletes the domain                                          │
│                                                                    │
│   DELETE /api/tenant/custom-domains/{id}                           │
│     → store.SoftDelete (sets deleted_at)                           │
│     → slugreservation lock 12 months on the host                   │
│     → audit_log{customdomain_deleted}                              │
└────────────────────────────────────────────────────────────────────┘
```

The default subdomain (`<tenant>.crm.<host-base>`) is never affected by
custom-domain teardown — both vhosts always coexist.

### §2 — State model (timestamps, not enum)

The ORIGINAL Fase-5 plan called for a `state` enum column with values
`pending_dns | verified | active | failed`. We KEEP the existing
**derive-from-timestamps** model in `tenant_custom_domains`:

| Field                | Meaning                                       |
|----------------------|-----------------------------------------------|
| `verified_at`        | NULL = pending; NOT NULL = ownership proven   |
| `verified_with_dnssec` | TRUE iff every TXT answer carried AD bit    |
| `tls_paused_at`      | NULL = TLS issuance allowed; ops freeze point |
| `deleted_at`         | NULL = active row; soft-delete tombstone      |
| `dns_resolution_log_id` | FK to the IP-pinning record at verify time |

The UI badge is derived in `management.StatusOf`:

```
TLSPausedAt != nil           → StatusPaused
VerifiedAt != nil            → StatusVerified
lastVerifyErr != nil         → StatusError
otherwise                    → StatusPending
```

The `failed` enum state from the Fase-5 plan is observationally
expressed as `StatusError`: it carries the reason code from the most
recent verify attempt and is computed from the `customdomain_validate_*`
audit-log row, not stored on the domain row itself.

**Why we chose this over an explicit enum:**

- The four states the UI actually paints (`pending`, `verified`,
  `paused`, `error`) are not the same four states the original plan
  envisaged (`pending_dns`, `verified`, `active`, `failed`). The
  `active` state in particular is operational, not a row state — once
  `verified_at` is set, "active" is whatever Caddy has cached.
- A persisted `state` column drifts: every transition needs a write,
  and divergence between `verified_at != NULL` and `state = "verified"`
  becomes its own bug class.
- Migration risk: shipping an enum column on the already-populated
  `tenant_custom_domains` table requires a backfill + check constraint
  with no production benefit.

### §3 — Validation choice (TXT only)

We DO NOT accept "CNAME already pointing at the tenant subdomain" as
an alternative proof. The Fase-5 plan listed CNAME-pre-existence as a
defence-in-depth OR; the F43/F44 review rejected it for two reasons:

1. CNAME existence is observable by anyone resolving the host, so an
   attacker who controls a similar-looking domain can satisfy it
   passively.
2. The validator must be a **pure function of (host, expectedToken,
   resolver answer)**. A CNAME check that asks "does this name resolve
   somewhere reasonable" is not pure — different resolvers see different
   answers, and the Unbound chokepoint (ADR-0079 §2) only protects the
   IP allowlist, not the CNAME chain.

TXT-only is therefore the contract:

- Record: `_crm-verify.<host>`
- Value: 32-byte hex token issued by `management.Enroll` (cryptographic
  RNG via `crypto/rand`, never reused across enrolments).
- Validator: `internal/customdomain/validation/validate.go`, must not
  import `net/http` (enforced at compile time by
  `internal/lint/customdomainnet`).

### §4 — Uniqueness & blacklist

Uniqueness is enforced at the database, not at the application:

```sql
CREATE UNIQUE INDEX uq_tenant_custom_domains_active_host
    ON tenant_custom_domains(LOWER(host))
    WHERE deleted_at IS NULL;
```

The partial predicate `WHERE deleted_at IS NULL` deliberately lets a
soft-deleted host be re-claimed by the same or a different tenant once
the slug-reservation lock expires (12 months). Application code catches
`pq.ErrCode == "23505"` (unique_violation) and translates to HTTP 409
with PT-BR copy in `internal/customdomain/management/copy_pt_br.go`.

**Blacklist (apex-protected hosts):**

- The CRM platform's own apex (`sindireceita.com.br` and any
  `*.sindireceita.com.br` reserved by ops). Configured in
  `customdomain.validation.blocklist` adjacent to the IP CIDR list, NOT
  hard-coded in Go — operators MUST be able to add a host without a
  redeploy.
- Public-suffix-list apex hosts (PSL leaf domains for free providers:
  vercel.app, netlify.app, blogspot.com, etc.). Rejected because a
  tenant cannot prove control of the apex.
- `localhost`, IP literals, single-label hosts. Rejected at the
  normalize step before any DNS lookup.

### §5 — Abuse mitigations (numeric limits)

All numbers come from the production-wired configuration in ADR-0079
§3 and §4. This ADR ratifies them as the Fase-5 default.

| Layer                                       | Limit                          | Source             |
|---------------------------------------------|--------------------------------|--------------------|
| Handler — per source IP                     | 10 calls / min                 | ADR-0079 §3        |
| Handler — per tenant                        | 60 calls / hour                | ADR-0079 §3        |
| Enrollment — new domains per tenant         | 5 / hour, 20 / day, 50 / month | ADR-0079 §4        |
| Enrollment — active domains per tenant      | 25 (hard cap)                  | ADR-0079 §4        |
| Let's Encrypt circuit breaker — per tenant  | 5 failures / 1 h → 24 h freeze | ADR-0079 §4        |
| TLS-ask handler — per host                  | 3 / min                        | ADR-0079 §4        |
| Slug-reservation lock on teardown           | 12 months                      | SIN-62244          |

Every layer is independent (defense in depth). Breakers persist across
restarts (Redis-backed in production via `redisstate`); placeholder
in-memory adapters exist only on the test path and are unreachable from
production by the boot-time `EnrollmentRedisRequired` gate.

### §6 — Audit log

Every state-changing operation writes one `audit_log` row before
returning. The events the schema MUST carry are:

- `customdomain_enrolled` — host claimed by tenant
- `customdomain_verified` — TXT validation passed
- `customdomain_paused` / `customdomain_resumed` — ops toggle on
  `tls_paused_at`
- `customdomain_deleted` — soft-delete + slug-reservation lock
- `customdomain_validate_blocked_ssrf` — §3 IP-allowlist hit
- `customdomain_validate_token_mismatch` — wrong TXT value
- `customdomain_breaker_tripped` / `customdomain_breaker_reset`

The audit row carries `tenant_id`, `actor_user_id`, `host`, and the
`dns_resolution_log_id` when applicable. Retention follows the platform
default (90 days hot, archival per ADR-0089).

### §7 — Reversibility

- Soft-delete via `deleted_at` retains the row indefinitely; the
  partial unique index frees the host immediately for re-claim *after*
  the slug-reservation lock (12 months) elapses.
- `CUSTOM_DOMAIN_UI_ENABLED=0` retires the public flow entirely; the
  Caddy `/internal/tls/ask` route is gated on
  `tenant_custom_domains.verified_at`, so previously-active domains
  stop renewing once their cert expires. No data migration is required.
- Pausing a single host: ops sets `tls_paused_at` via the management
  use-case; the on-demand handler refuses issuance but the row stays.

## Alternatives considered

### A. Re-implement under `internal/tenancy/custom_domain.*` (Fase-5 plan literal)

Rejected. Duplicating `internal/customdomain/management.UseCase` under
`internal/tenancy` would:

1. Create two production code paths for the same product feature,
   requiring lockstep changes for every future evolution.
2. Force a redundant migration `tenant_custom_domain` (singular)
   alongside the live `tenant_custom_domains` (plural). The two-table
   split breaks the partial unique index and the slug-reservation
   contract.
3. Reset the test coverage we already have (the F45 stack has unit +
   integration + harness coverage above the 85% bar).

This violates the **Boring technology budget** and **Reversibility**
lenses simultaneously.

### B. Migrate state to an explicit enum column

Rejected. See §2 — the persisted-state model adds drift surface without
unlocking any UI or operational capability. The four-state badge the
Fase-5 plan asks for is already produced by `management.StatusOf`
derived from timestamps.

### C. Accept both TXT and CNAME-pre-existence

Rejected. See §3 — CNAME existence is observable to outsiders and the
validator must be pure. The F43/F44 review explicitly closed this door.

### D. Allow tenant to choose the default-subdomain CNAME pattern

Out of scope for this ADR; recorded as a follow-up product question on
Fase 5.

## Consequences

**Positive**

- One source of truth for the custom-domain product flow, mapping
  Fase-5 ACs to shipped code.
- No new package, no duplicated entity, no parallel migration.
- ADR-0061 anchors the user-visible numbers (5/h, 20/d, 50/mo, 25 hard
  cap, 12-month lock) so support and CS have a stable reference.

**Negative**

- The Fase-5 plan's literal task list (`internal/tenancy/custom_domain.go`,
  `tenant_custom_domain` migration) becomes obsolete; SIN-63077 and
  SIN-63075's custom-domain leg need a board-level supersession decision
  before they are cancelled. This ADR proposes that supersession but
  does not unilaterally close those issues.
- The "active" state visible in operator dashboards is derived
  (Caddy-cache-aware), not a row column. Any future analytics that
  expects a literal `state = "active"` row needs to query
  `verified_at IS NOT NULL AND tls_paused_at IS NULL AND deleted_at IS NULL`
  with an optional join on the Caddy cert log.

**Open follow-ups (post-confirmation)**

- UI cadastro page + DNS-instructions screen wiring — the use-case is
  ready; the HTMX surface in `internal/adapter/transport/http/customdomain`
  needs the Fase-5 product copy reviewed against
  `management/copy_pt_br.go`.
- Operator dashboard view of pending domains (count by tenant, age in
  hours). Nice-to-have; not blocking Fase 5 close-out.
- Confirm SIN-63075's `tenant_palette` leg stays in scope — only the
  `tenant_custom_domain` leg is superseded by ADR-0061.

## Out of scope

- White-label palette extraction (ADR-0060 / [SIN-63072](/SIN/issues/SIN-63072)).
- Email transactional white-label `from` rewriting (Fase 5 AC #8;
  separate ADR when that work starts).
- Per-tenant operator alerting on breaker trips (LGPD-adjacent, Fase 6).
