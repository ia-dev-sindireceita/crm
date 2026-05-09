# ADR 0073 — CSRF, session cookies, idle/hard timeouts and rate limit/lockout

- Status: Accepted
- Date: 2026-05-07
- Owners: Coder ([SIN-62337](/SIN/issues/SIN-62337)), CTO (review), SecurityEngineer (review)
- Related: [SIN-62223](/SIN/issues/SIN-62223) (parent — F12/F14/F15 bundle), [SIN-62192](/SIN/issues/SIN-62192) (Fase 0 plan), [security review](/SIN/issues/SIN-62220#document-security-review) (F13/F14/F17/F19 source findings)
- Lenses: **Defense in depth**, **Secure-by-default API**, **Reversibility & blast radius**

## Context

Phase 0 ships authenticated UI on two distinct subdomains: a `master.*` operator
console and per-tenant `{slug}.*` workspaces. The security review of
[SIN-62220](/SIN/issues/SIN-62220#document-security-review) flagged four
correlated findings that, if solved ad hoc per handler, guarantee inconsistent
middleware:

- **F13** (MEDIUM) — session cookie name/scope/flags not specified per audience.
- **F14** (HIGH) — CSRF undefined; HTMX makes naive per-request rotation unsafe
  (parallel `hx-swap` requests race on the token).
- **F17** (MEDIUM) — idle/hard timeouts and session-id rotation triggers absent.
- **F19** (MEDIUM) — login/2FA/password-reset have no rate limit or lockout;
  `/login` distinguishes known vs unknown emails (enumeration oracle).

These belong in one ADR because each control depends on the others: SameSite=Lax
on the tenant cookie is acceptable **only because** every state-changing endpoint
demands a CSRF token; rate limits without durable lockout die on a Redis flush.
Half-implementations are worse than none because they build the wrong intuition
(precedent: [ADR 0072](docs/adr/0072-rls-policies.md) on RLS vs FORCE RLS).

## Decision

### D1 — CSRF: per-session token, double-submit + Origin allowlist (F14)

A 32-byte CSPRNG token is minted on login (and on every session-id rotation per
D3) and stored as `session.csrf_token`. **Rotation is per-session, not
per-request** — per-request rotation loses the race against parallel HTMX
`hx-swap` requests on a single page (token N+1 wins the cookie write, token N
is rejected mid-flight). The token is re-minted only on events that already
invalidate the session (D3).

**Storage choice (SIN-62375 amendment).** The token lives in the existing
`sessions` table as a new column `csrf_token TEXT NOT NULL DEFAULT ''`
(migration 0011). The Redis-adjacent alternative was rejected because:

- the lifecycle is exactly the session row's lifecycle (no separate TTL to
  keep aligned),
- the four-policy RLS template on `sessions` already gates per-tenant access
  to the column, and
- a column survives a Redis flush — same precedent as the durable
  `account_lockout` row in §D4.

The empty-string sentinel on legacy rows pre-dating migration 0011 maps to
`csrf.session_token_missing` in the middleware (a programmer / migration
bug surfaced as 403, never silently accepted). `iam.Service.Login` always
mints via `iam/csrf.GenerateToken` so freshly issued sessions never carry
the sentinel; the HTTP layer mirrors the value into the `__Host-csrf`
cookie via `sessioncookie.SetCSRF` immediately after the session cookie.

**Delivery (three channels, two must match).**

| Channel       | Form                                                                | Read by             |
|---------------|---------------------------------------------------------------------|---------------------|
| Cookie        | `__Host-csrf` `Secure; SameSite=Strict; Path=/; HttpOnly=false`     | Browser + JS        |
| `<meta>` tag  | `<meta name="csrf-token" content="…">` in the authenticated layout  | HTMX `hx-headers`   |
| Header / form | `X-CSRF-Token` header (HTMX) **or** hidden `_csrf` input (`<form>`) | Server middleware   |

`HttpOnly=false` is intentional — HTMX and the form helper read it. The token
is not session material; theft of the token alone, without the session cookie
(D2, `HttpOnly`), achieves nothing.

**Templ helpers.** `csrfMeta()` renders the meta tag inside the authenticated
layout. `csrfFormToken()` wraps `<form>` and injects a hidden
`<input type="hidden" name="_csrf">`. HTMX picks up the meta value via a
global `hx-headers='{"X-CSRF-Token": …}'` on `<body>` of the layout.

**Middleware `RequireCSRF`** is mounted on every `POST/PATCH/PUT/DELETE` route
in an authenticated session. Decision tree:

1. `GET/HEAD/OPTIONS` → pass.
2. Webhook allowlist (HMAC-authed per [ADR 0075](docs/adr/0075-webhook-security.md))
   → pass. Allowlist is **explicit** (`RequireCSRF.Skip(routePath)`), not inferred.
3. Missing `__Host-csrf` cookie → 403 `csrf.cookie_missing`.
4. Missing `X-CSRF-Token` header **and** form `_csrf` → 403 `csrf.token_missing`.
5. Cookie ≠ presented value (constant-time compare) → 403 `csrf.token_mismatch`.
6. Presented value ≠ `session.csrf_token` → 403 `csrf.session_token_mismatch`.
   Guards against attacker who controls the cookie domain (e.g., subdomain
   takeover) but not the session record.

**Origin/Referer allowlist** is an independent layer behind the token check.
Read `Origin` (preferred) or `Referer` (fallback); match against
`{master.<host>, <slug>.<host> for the resolved tenant}`. Both missing → 403
`csrf.origin_missing`. Mismatch → 403 `csrf.origin_mismatch`. A stolen token
plus a stolen cookie still cannot be replayed from an attacker-controlled
origin.

### D2 — Session cookies: distinct names, distinct flags per audience (F13)

| Audience | Cookie name           | Flags                                       |
|----------|-----------------------|---------------------------------------------|
| Master   | `__Host-sess-master`  | `Secure; HttpOnly; SameSite=Strict; Path=/` |
| Tenant   | `__Host-sess-tenant`  | `Secure; HttpOnly; SameSite=Lax; Path=/`    |

`SameSite=Strict` on master is non-negotiable — there is no legitimate
cross-site navigation into a master action (impersonation grant, courtesy
password, feature flag write). Strict blocks the entire class of "operator
clicks attacker link → request fires with cookie attached".

`SameSite=Lax` on tenant is acceptable **only because** every state-changing
tenant endpoint requires a CSRF token (D1). Lax is the floor for usable UX:
an atendente clicking a deep link from internal email opens the tenant page
with the session attached.

Distinct **names** (not just paths) prevent cross-binding when an operator
holds both a master and a tenant account in the same browser. The `__Host-`
prefix forces `Secure; Path=/; no Domain` — each subdomain owns its own
cookie store, and distinct names let the browser hold both sessions in
parallel without ambiguity.

### D3 — Idle/hard timeouts and session-id rotation (F17)

Idle = inactivity since last request. Hard = wall-clock from session creation
regardless of activity. Both checked on every request.

| Audience / role             | Idle  | Hard | Re-MFA required for                                                                |
|-----------------------------|-------|------|------------------------------------------------------------------------------------|
| Master (any operator)       | 15min | 4h   | `master.grant_courtesy`, `master.impersonate.request`, `master.feature_flag.write` |
| Tenant gerente              | 30min | 8h   | (none beyond hard timeout)                                                         |
| Tenant atendente            | 60min | 12h  | (none beyond hard timeout)                                                         |
| Tenant common (other roles) | 30min | 8h   | (none beyond hard timeout)                                                         |

Atendente has the longest idle because shifts are multi-hour customer calls
with natural pauses; a 30min re-login destroys the workflow and trains
operators to leave the browser unlocked. The 12h hard cap bounds a stolen
device.

**Session-id rotation triggers** (mint new id, *invalidate* old, re-mint
CSRF token per D1):

- **Login** — always a new session.
- **Logout** — invalidates current.
- **Role change** — elevation, demotion, any RBAC change affecting the session.
- **2FA success** — swaps pre-MFA for post-MFA session-id.

**Same-user invalidation.** On logout-everywhere or password change, all
sessions for the user are invalidated by deleting `sess:user:{uid}:*` in
Redis **except** the current session, which is re-issued fresh (preserves
"I just changed my password — don't log me out of the tab I'm typing in").

Idle/hard windows are per-role config (D5), not constants. `__Host-sess-*`
cookie `Max-Age` is set to the hard timeout, so the browser also expires
the cookie if the server is unreachable when the hard window elapses.

### D4 — Rate limit + lockout (F19)

Two independent layers: Redis sliding-window counters for per-window
throttling, Postgres `account_lockout` for durable lockout state that
survives a Redis flush.

| Endpoint               | Per IP        | Per user / email / session   | Lockout                                              |
|------------------------|---------------|------------------------------|------------------------------------------------------|
| `POST /login`          | 5/min/IP      | 10/h/email                   | 15min lockout after 10 failures on the same email    |
| `POST /2fa/verify`     | —             | 5/min/session, 20/h/user     | session invalidated after 6 failures                 |
| `POST /password/reset` | 3/h/IP        | 3/h/email                    | —                                                    |
| `POST /m/login`        | 3/min/IP      | 5/h/email                    | 30min after 5 failures + immediate Slack alert       |
| `POST /m/2fa/verify`   | —             | 3/min/session, 10/h/user     | session invalidated after 5 failures + Slack alert   |

Master endpoints (`/m/*`) get tighter quotas and synchronous Slack alerts
because compromise is catastrophic; tenant endpoints get wider quotas
because the legitimate-failure rate is higher.

**Implementation.**

- Redis sliding-window counter for short windows; counters auto-expire at
  2× the window. Middleware `RateLimit("login", keys...)` takes the
  policy name and the keys (IP, email, session id, user id) to count.
- `account_lockout(user_id uuid PK, locked_until timestamptz, reason text, last_attempt_at timestamptz)`
  in Postgres. The login path checks this **before** verifying the
  password — locked accounts fail fast and cheap, surviving Redis flushes.
- Lockout duration is **fixed per policy**, not exponential. Exponential is
  harder to communicate and tends to drift upward after incidents.

**Anti-enumeration on `/login` and `/m/login`.** Unknown email and
known-email-wrong-password are indistinguishable: same `401`, same body
(`"Invalid credentials"`), same timing — the handler runs an Argon2id
verification against a constant dummy hash precomputed at process start
*even when the email does not exist*, reducing the side-channel to network
jitter. `account_lockout` increments only when the email is known, so an
attacker cannot lock out an arbitrary victim by flooding their email;
unknown-email floods are absorbed by the per-IP counter alone.

**Outcomes** (Prometheus `auth_attempts_total{outcome,endpoint}`):
`auth.login.success`, `auth.login.bad_password`, `auth.login.unknown_email`,
`auth.login.rate_limited`, `auth.login.locked`, plus `/m/*` and `/2fa/*`
parallels.

### D5 — Reversibility: per-bucket feature flags

Every numeric / enum decision above is wired through a config key, not a
constant:

- `auth.session.timeout.master.{idle,hard}` = 15m, 4h
- `auth.session.timeout.tenant.gerente.{idle,hard}` = 30m, 8h
- `auth.session.timeout.tenant.atendente.{idle,hard}` = 60m, 12h
- `auth.session.timeout.tenant.common.{idle,hard}` = 30m, 8h
- `auth.ratelimit.<endpoint>.<bucket>` for every D4 row
- `auth.lockout.<endpoint>.{threshold,duration}` for every D4 row
- `auth.csrf.required` (default `true`) — emergency rollback only; off in
  prod = page on-call

A tenant whose atendente workflow doesn't tolerate 30min idle dials it to
60min via config write — auditable in the master ops audit log, no deploy.
Blast radius of any single decision is one config row.

## Consequences

**Positive.**

- F13/F14/F17/F19 closed by independent, layered defenses. Bypass requires
  defeating all of them: CSRF token, Origin allowlist, SameSite,
  session-id rotation, rate limit, lockout.
- HTMX-friendly: per-session token avoids the `hx-swap` race; templ helpers
  make correct usage the default for both HTMX and plain forms.
- Boring tech: stdlib `crypto/rand`, `crypto/subtle`, Argon2id (already a
  dependency), Postgres for durable state, Redis for hot counters.
- Per-role timeouts are config — operators tune without a deploy.

**Negative / costs.**

- Every authenticated page **must** render via the layout that injects the
  global `hx-headers` and the `<meta>` tag. A page outside the layout fails
  every state-changing request — caught on first try by middleware, but a
  discipline cost. Mitigation: a single authenticated `templ` layout.
- `account_lockout` is one more table to migrate, GC, and back up. Cheap.
- Distinct cookie names mean an operator with both master and tenant
  accounts holds two parallel sessions in the same browser — intentional
  (security boundary) but occasionally surprising in dev.

**Residual risks (accepted).**

- **XSS still defeats CSRF.** CSRF is not an XSS defense — CSP, output
  escaping, and `templ`'s default-escaped interpolation are the controls
  for that.
- **Per-IP cap is bypassable behind a CGNAT.** A shared corporate proxy IP
  can starve legitimate users of the per-IP budget. Per-email lockout still
  fires; users retry from a different network. Operators can request a
  per-tenant IP-allowlist override if recurrent.
- **Lockout is itself a DoS vector** (attacker locks out a known target).
  Mitigated by the per-IP `/login` cap (5/min) — locking takes ≥ 2min on a
  single IP, and the email owner self-recovers via password reset (3/h).
  Accepted because the alternative (no lockout) is worse against
  credential stuffing.

## What this ADR does **not** decide

- Argon2id parameters and password storage schema — covered by ADR 0070
  ([SIN-62336](/SIN/issues/SIN-62336)).
- 2FA enrollment and verification mechanism (TOTP vs WebAuthn vs both) —
  separate ADR.
- Master operator audit log schema — separate ADR. This ADR only requires
  that re-MFA events on D3 actions land in it.
- Webhook auth — covered by [ADR 0075](docs/adr/0075-webhook-security.md).
  Webhooks are on the explicit CSRF skip-list (D1 step 2).
- Concrete Redis key schema for `sess:user:{uid}:*` and the sliding-window
  counters — implementation PR territory. This ADR fixes the invariants
  (rotation triggers, sliding-window per bucket, lockout in Postgres);
  the implementation PR picks key names consistent with operational
  conventions.
