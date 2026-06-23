# ADR 0079 — Custom-domain ownership validation, anti-rebinding, rate limits

- Status: Accepted
- Date: 2026-05-03
- Issue: [SIN-62242](/SIN/issues/SIN-62242) — F43 (CRITICAL) + F44 (HIGH) of the
  custom-domain security review ([SIN-62220](/SIN/issues/SIN-62220#document-security-review))
- Plan: [SIN-62226 doc decisions §1-§2](/SIN/issues/SIN-62226#document-decisions)

## Context

Pitho lets a tenant publish their CRM under a custom hostname
(e.g. `crm.acme.com.br`). Once a tenant proves they own the hostname,
Caddy issues an ACME certificate for it and routes the vhost to the
tenant's slug. Two attack classes blocked merge of any custom-domain
feature:

- **F43 (SSRF via TLS issuance):** the older HTTP+DNS validator did a
  parallel HTTP fetch of `http://<host>/.well-known/pitho-verify`
  AND a TXT lookup. Either side passing was treated as ownership proof.
  An attacker who pointed `evil.test` at a CRM internal IP could then
  serve the verification file from inside our network and pass.
- **F44 (DNS rebinding through ACME HTTP-01):** even with HTTP fetch
  removed, Caddy itself does an HTTP-01 self-check during issuance, and
  Caddy's resolver is the OS resolver. An authoritative server controlled
  by the attacker can return a public IP to our validator and `127.0.0.1`
  to the in-cluster ACME challenge, and Caddy will happily fetch the
  challenge from itself.

Both are textbook SSRF / OWASP A10 problems; they have to be closed at
multiple layers — the use-case, the resolver, and the recursive cache.
This ADR pins the contract for all three.

## Decision

### §1 — DNS-only ownership validation with IP allowlist

The validator is **DNS-only**. There is no HTTP leg. The contract is:

1. Look up A and AAAA for the hostname.
2. If ANY answer falls inside the blocked CIDR list (below), reject with
   `ErrPrivateIP` and emit `audit_log{event=customdomain_validate_blocked_ssrf}`.
   The mixed-answer case (one public, one private) is a textbook
   rebinding setup — it is rejected too.
3. Look up TXT under `_crm-verify.<host>`. If no record carries the
   expected token, reject with `ErrTokenMismatch` and emit
   `audit_log{event=customdomain_validate_token_mismatch}`.
4. On success, pin the **first non-blocked IP** as
   `dns_resolution_log.pinned_ip` and persist
   `verified_with_dnssec = (every answer carried the AD bit)`.

The blocked CIDR list (mirrors `internal/customdomain/validation/blocklist.go`
and `infra/caddy/unbound.conf`):

| Range            | Reason                                |
|------------------|---------------------------------------|
| `10.0.0.0/8`     | RFC 1918 private                      |
| `172.16.0.0/12`  | RFC 1918 private                      |
| `192.168.0.0/16` | RFC 1918 private                      |
| `127.0.0.0/8`    | IPv4 loopback                         |
| `169.254.0.0/16` | RFC 3927 link-local incl. cloud IMDS  |
| `100.64.0.0/10`  | RFC 6598 CGNAT                        |
| `0.0.0.0/8`      | RFC 1122 "this network"               |
| `224.0.0.0/4`    | IPv4 multicast                        |
| `::1/128`        | IPv6 loopback                         |
| `fc00::/7`       | RFC 4193 IPv6 unique-local            |
| `fe80::/10`      | IPv6 link-local                       |

The validator is implemented as a pure-domain hexagonal use-case at
`internal/customdomain/validation`. It MUST NOT import `net/http`; the
custom analyzer at `internal/lint/customdomainnet` enforces this at
compile time and the CI workflow `.github/workflows/customdomainnet-lint.yml`
fails the build on violation.

### §2 — DNS rebinding in ACME / Caddy

A second leg of the F44 attack remains even after §1: Caddy's internal
ACME issuance does its own DNS lookups. The mitigation is structural:
**Caddy never talks to a public recursive resolver directly. It talks to
the Unbound sidecar at `infra/caddy/unbound.conf`.**

Unbound is configured with:

- `private-address: …` — drops every RR matching the §1 blocklist.
- `deny-answer-address: …` — refuses the WHOLE answer if any RR matches,
  closing the "good A + bad AAAA" rebinding case.
- `harden-glue: yes` and `harden-referral-path: yes` — defends against
  parent-zone glue stuffing.
- DNSSEC validation is on; the AD bit Caddy receives is the AD bit
  Unbound itself validated.

Wiring: `deploy/compose/compose.yml` adds the sidecar service and pins
`caddy.dns: ["unbound"]` so the OS resolver inside the Caddy container
is also Unbound. Site blocks `import secure-resolvers` so the (future)
DNS-01 challenge plugin uses the same path.

The miekg-based application resolver (`adapters/dnsresolver/miekg`) is
ALSO pointed at Unbound at boot in production wiring (issue B,
[SIN-62243](/SIN/issues/SIN-62243)). That gives us a single chokepoint
to upgrade — change Unbound, both validators move together.

### §3 — Rate limits (handler-level, defers to issue B)

Token-guessing and SSRF-scan brute-forcing share the same shape: many
fast Validate calls per source. Rate limiting is enforced at the handler
layer (issue [SIN-62243](/SIN/issues/SIN-62243), F45). The buckets
defined there apply BEFORE the handler reaches the validator:

- 10 calls per minute per source IP.
- 60 calls per hour per tenant.
- A circuit breaker on `EventResolverError` so a single misbehaving
  upstream resolver cannot stall every tenant's setup flow.

The validator stays unaware of rate limits — it must remain a pure
function of (host, expectedToken, resolver answer) for testability.

### §4 — Production gates (SIN-62334 F53)

The per-tenant enrollment quotas (5/h, 20/d, 50/mo new domains, 25
active hard cap) and the Let's Encrypt circuit breaker (5 failures /
1 h → 24 h freeze) are independent layers of defense in depth alongside
the §3 handler-level rate limits. They MUST be wired against the
production Redis-backed adapters before the public flag is flipped.

**Hard-error boot contract.** When `CUSTOM_DOMAIN_UI_ENABLED=1` is set
without `REDIS_URL`, `cmd/server` MUST refuse to boot. The check fires
in `cmd/server.runWith` via `EnrollmentRedisRequired(getenv)` BEFORE
`buildCustomDomainHandler`; if the gate is misconfigured the process
exits non-zero so the orchestrator restarts and the operator sees the
failed boot. A startup `WARN` is NOT acceptable — WARN is not a
control. With the placeholders in the same binary, the only brake
against LE quota exhaustion at scale would be the 3/min/host rate
limiter on `/internal/tls/ask`, and a single attacker rotating across
hosts can blow through the upstream LE issuance budget.

**Single env-var contract.** `REDIS_URL` (the same env var the internal
listener already consumes) drives the swap. There is no second toggle
like `CUSTOM_DOMAIN_QUOTA_BACKEND=redis` — Redis presence IS the
configuration.

**Adapter wiring.** The three ports plug in as follows:

- `enrollment.CountStore` → `pgstore.NewEnrollmentCountStore` (Postgres
  `COUNT(*)` against `tenant_custom_domains WHERE deleted_at IS NULL`,
  the same partial unique index the management UI reads from). Source
  of truth IS the relational data; a Redis-set adapter would require
  lock-step `SADD`/`SREM` with every soft-delete and silently bypass
  the cap on Redis loss.
- `enrollment.WindowCounter` → `rediswindow.New` (sorted-set sliding
  window per `(tenantID, window)` key, atomic via Lua, TTL = window +
  60 s grace).
- `circuitbreaker.State` → `redisstate.New` (sliding-window failure
  log + `SET … PX freezeMs` frozen flag, persisted across server
  restarts so a 24h freeze survives a deploy and is enforced
  consistently across all replicas — multi-replica safe by
  construction).

The placeholder types (`zeroCount`, `passWindowCounter`, `zeroBreaker`)
remain in `cmd/server/customdomain_wire.go` for the test path that
drives `buildCustomDomainHandler` without Redis. They are unreachable
from production because `runWith` errors out before the function is
called.

**Reversibility.** Unset `CUSTOM_DOMAIN_UI_ENABLED` to retire the
public flow; the wire-up returns nil and the public listener serves
only `/health`. No data migration; quota state ages out on its TTLs.

## Consequences

- The validator is small (1 use-case file, 1 blocklist file, 1 ports
  file) and 100% covered by `go test`. The 85% acceptance gate is
  comfortably exceeded.
- Adding a CIDR to the blocklist requires editing TWO places:
  `blocklist.go` AND `infra/caddy/unbound.conf`. A follow-up issue
  (`0079b-blocklist-drift-test`) will automate the comparison.
- Caddy's local-only stack now requires the Unbound image. The compose
  healthcheck depends on `drill`; the chosen image (`mvance/unbound`)
  ships it.
- Tenants who publish IPv6-only without DNSSEC see
  `verified_with_dnssec = false` in `dns_resolution_log` but still pass
  validation. This matches the security review's recommendation
  (observe, don't block; bumping to a hard requirement waits for
  customer education).

## HTTP wiring (SIN-62331 F51)

A independent re-review do bundle F43–F49 ([SIN-62328](/SIN/issues/SIN-62328))
constatou que o domínio do `slugreservation` (F46 / OWASP A01) só
defende em runtime se a boundary HTTP chamar suas três peças.
SIN-62331 fechou esse gap em `cmd/server/slugreservation_wire.go`:

- **`buildSlugReservationWiring(ctx, getenv)`** instancia
  `slugreservation.Service` apoiado em `pgstore.NewSlugReservationStore`
  + `pgstore.NewSlugRedirectStore` quando `DATABASE_URL` está setado, e
  devolve no-ops caso contrário. O modo no-op deixa o middleware
  passar tudo (a regra de produção é não habilitar signup sem banco).
- **`registerSlugReservationRoutes`** monta:
  - `POST /api/master/slug-reservations/{slug}/release` — `OverrideHandler`
    embrulhado em `masterAuthMiddleware`, **deny-by-default** quando
    `MASTER_API_TOKEN` está vazio (responde 401). Quando configurado,
    valida `Authorization: Bearer <token>` em tempo constante via
    `crypto/subtle.ConstantTimeCompare`. O master ID é derivado por
    UUIDv5 estável, e a flag MFA cai para `true` quando o token está
    setado (ou `MASTER_REQUIRE_MFA=1` o impõe). SIN-62342 substitui
    esse middleware pelo `RequireMasterMFA` real.
  - `POST /signup` e `PATCH /tenants/{slug}/slug` — placeholders 501
    embrulhados em `RequireSlugAvailable`, de forma que o curto-circuito
    409 do F46 já dispara antes que o handler real (em outro ticket)
    pouse.
- **`slugWiring.redirect(...)`** é aplicado em `runWith` por fora do
  `csp.Middleware` para que toda request com Host `<old>.<primary>`
  responda **301 + `Clear-Site-Data: "cookies"`** antes de qualquer
  rota normal. `primaryHost` é lido de `PRIMARY_DOMAIN` (default
  `exemplo.com`).

**Smoke tests** em `cmd/server/slugreservation_wire_test.go`:

- 409 em `/signup` quando o slug está em reserva ativa.
- 501 (placeholder) em `/signup` quando o slug está livre — prova que o
  middleware passou.
- 409 em `PATCH /tenants/{slug}/slug` para slug reservado.
- 401 em `POST /api/master/slug-reservations/{slug}/release` sem token.
- 401 quando o bearer é diferente do `MASTER_API_TOKEN`.
- Auth passa quando bearer bate; OverrideHandler executa.
- Redirect 301 + `Clear-Site-Data` para Host `<old>.exemplo.com`.
- Pass-through quando Host = primary ou Host = subdomínio aninhado.

**Reversibility.** Desconfigurar `DATABASE_URL` zera o wire-up
(o middleware vira no-op, override fica nil) — a boundary não
serve signup/rename sem banco. Para retirar o master mux, basta
deixar `MASTER_API_TOKEN` vazio: o handler permanece montado mas
responde 401 a todas as requests (deny-by-default).

## Out of scope

- The `/internal/tls/ask` Caddy on-demand handler ([SIN-62243](/SIN/issues/SIN-62243)).
- Slug reservation rules ([SIN-62244](/SIN/issues/SIN-62244)).
- The DNS-01 plugin migration (`0079a-acme-dns01`) — non-blocking
  follow-up; the current contract works with HTTP-01 because the
  Unbound sidecar already covers that path.
