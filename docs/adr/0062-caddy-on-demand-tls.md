# ADR 0062 — Caddy on-demand-tls: configuration, rate limits, telemetry

- Status: Accepted
- Date: 2026-05-19
- Deciders: CTO
- Drives: [SIN-63074](/SIN/issues/SIN-63074) (this ADR), [SIN-62198](/SIN/issues/SIN-62198) (Fase 5 parent), [SIN-63081](/SIN/issues/SIN-63081) (producer)
- Builds on: [ADR 0079](./0079-custom-domain.md) (custom-domain ownership + Unbound)
- Closes: F45 architectural debt for Fase 5

## Context

Fase 5 ([SIN-62198](/SIN/issues/SIN-62198)) ships **custom-domain
self-service**: a tenant publishes the CRM under `crm.acme.com.br`
and the platform issues a TLS cert on the first request. Caddy's
`on_demand_tls` triggers ACME during the TLS handshake. Out-of-the-box
it issues for **any** SNI — a directly-weaponisable gun: flood random
SNIs and burn Let's Encrypt's per-account budget
(50/registered-domain/week; 300 orders/account/3 h), drive issuance
load, or monopolise LE budget from a single compromised tenant.

The deny-by-default decision and per-host limiter shipped under F45
([SIN-62243](/SIN/issues/SIN-62243), endpoint `/internal/tls/ask`).
Missing was the formal record of the configuration, the
**per-tenant** and **global** caps, the telemetry contract, and the
renewal-observation plan. This ADR pins all four.

## Decision

### §1 — Caddyfile pattern (current shape)

Caddy talks to the app's internal listener at `app:8081` to decide
whether to issue. The listener is unpublished — only the docker
bridge can reach it. The blocks live in
[`deploy/caddy/Caddyfile`](../../deploy/caddy/Caddyfile):

```caddyfile
{
    on_demand_tls {
        ask    http://app:8081/internal/tls/ask
        burst  3            # per-host issuance burst (Caddy-local)
    }
}

:443 {
    tls { on_demand }
    import security-headers.caddy
    reverse_proxy app:8080
}
```

DNS for ACME goes through the Unbound sidecar
([ADR 0079 §2](./0079-custom-domain.md#§2--dns-rebinding-in-acme--caddy));
HTTP-01 (`dns: ["unbound"]`) and the future DNS-01 plugin
(`tls.resolvers unbound:5353`) both route through it, so the issuance
path cannot rebind into private space.

### §2 — Endpoint contract: `/internal/tls/ask`

The contract is **deny-by-default**. Caddy treats only `200` as
permission to issue.

| Element             | Value                                                          |
|---------------------|----------------------------------------------------------------|
| Method              | `GET` (and `HEAD`); other methods → `405 method_not_allowed`. |
| Path                | `/internal/tls/ask` (constant `tlsask.Path`)                  |
| Query parameter     | `domain=<host>`                                                |
| Listener            | `app:8081`, unpublished (docker bridge only)                  |
| Auth                | Network-layer (closed listener); no auth header                |
| Latency target      | **p99 < 50 ms** under steady state                            |
| Allow               | `200 OK` JSON `{"status":"allow","host":"..."}`              |
| Deny                | `403 Forbidden` with `reason` (`not_found` / `not_verified` / `paused` / `invalid_host`) |
| Rate-limited        | `429 Too Many Requests` + `Retry-After: 60`                  |
| Globally disabled   | `503 Service Unavailable` (`customdomain.ask_enabled=false`)  |
| Port error          | `500 Internal Server Error` (Caddy retries on back-off)       |

The decision pipeline (`internal/customdomain/tls_ask/usecase.go`)
runs in fixed order:

1. **Syntactic host validation** (cheap, no I/O). RFC 1035 §2.3.4
   length cap (253 bytes), LDH labels, ≥ 2 labels. Rejections never
   touch downstream ports — nuisance SNIs are dropped at the boundary.
2. **Feature flag** (`customdomain.ask_enabled`). Global kill-switch;
   on flag error the decision is `error` so Caddy retries (existing
   issuances continue, no rollback storm).
3. **Per-host rate limiter** (Redis sliding window, §3). Checked
   BEFORE the DB so flooding a single host cannot drive DB load.
4. **Repository lookup** (Postgres `LOWER(host)` partial unique). The
   row must exist, `verified_at IS NOT NULL`, `tls_paused_at IS NULL`.

Any port error short-circuits to `error` (deny). A flag error,
limiter error, or repository error MUST NOT result in a `200`.

### §3 — Rate limits (per-host, per-tenant, global)

Three layers of defence in depth, each tightening a different
attack envelope:

| Layer       | Limit (default)      | Window  | Backend            | Acts on |
|-------------|----------------------|---------|--------------------|---------|
| Per-host    | **3 ask calls/min/host** | 60 s    | Redis sorted-set sliding window (`internal/customdomain/ratelimit/sliding`) | All SNIs |
| Per-tenant  | **5 cert emissions/h/tenant** | 60 min  | Redis sorted-set sliding window keyed on `tenant_id`, recorded ONLY when the ask handler returned `allow` for a tenant-owned host | All emissions for verified domains |
| Global      | **40 cert emissions/h/cluster** | 60 min  | Redis sorted-set sliding window, single key `customdomain:tls_emissions:global` | All emissions |

Rules:

- Per-host is unchanged from F45 (bumps on every ask call — dust
  control).
- Per-tenant trips AFTER the repository lookup resolves `tenant_id`;
  on trip → `429` + `Retry-After: 3600`, reason `tenant_rate_limited`.
- Global caps cluster emissions/h at **40**: comfortably below LE's
  300 orders/account/3 h cap and inside the 50/registered-domain/week
  envelope. On trip → `429` + `Retry-After: 3600`, reason
  `global_rate_limited`. The trip is paged (§4).
- All three share the sliding-window adapter; only the key shape
  differs (`host` / `tenant:<uuid>` / `global`). Redis is the source
  of truth — no Postgres counters on the hot path.
- **Fail-closed.** Any limiter error → deny; Caddy back-offs.
- Per-tenant + global counters bump **only on `allow`** (real
  issuance attempt). Per-host bumps on every call.

### §4 — Telemetry (metrics + alerts)

Metrics (Prometheus, exported by the app's `/metrics` endpoint):

| Metric                                                | Type      | Labels                          | Source                |
|-------------------------------------------------------|-----------|---------------------------------|-----------------------|
| `caddy_tls_emissions_total`                           | counter   | `tenant_id, result`             | ask handler `allow` path (`result=allow`); 429/403 mapped to `denied`/`rate_limited` |
| `caddy_tls_authorize_duration_seconds`                | histogram | `result`                         | wraps the full handler (boundary → response)  |
| `caddy_tls_ask_decisions_total`                       | counter   | `decision, reason`              | already emitted via structured log, mirrored to Prometheus |
| `caddy_certs_valid_total`                             | gauge     | (none — cluster total)          | scraped from Caddy's `/metrics` (`caddy_tls_certs` family) on the admin listener |
| `caddy_tls_ratelimit_trips_total`                     | counter   | `scope (host\|tenant\|global)`  | sliding-window adapter |

Alerts (Alertmanager rules under `deploy/alertmanager/`):

- **High failure rate.** `rate(caddy_tls_ask_decisions_total{decision="error"}[15m]) / rate(caddy_tls_ask_decisions_total[15m]) > 0.10` for 5 m → page `#sec`.
- **Global cap.** `increase(caddy_tls_ratelimit_trips_total{scope="global"}[15m]) > 0` → page `#sec` (we never expect to trip global in healthy operation).
- **Renewal failures.** `increase(caddy_tls_emissions_total{result="renew_failed"}[24h]) > 0` → ticket to `#ops`. Caddy emits renewal failures into its own logs; the app does NOT proxy renewals through `/internal/tls/ask`, so this signal comes from a log-based exporter on the Caddy container.

### §5 — Renewal observation

Caddy renews automatically (30 d before expiry by default). The ask
handler is **NOT** consulted on renewal — only on issuance. Therefore:

- Renewal failures are a Caddy-internal signal. We watch them via the
  log-based exporter (§4 metric `caddy_tls_emissions_total{result="renew_failed"}`).
- A tenant that becomes `tls_paused_at IS NOT NULL` AFTER issuance
  keeps its existing cert until expiry. To force a renewal denial
  the operator must additionally **revoke** the cert through Caddy's
  admin API; this is documented but not automated in Fase 5 (it is a
  Fase 6 follow-up — see §Out of scope).
- Cert validity is observable via `caddy_certs_valid_total` (§4).

### §6 — Certificate storage

Caddy persists ACME state under `/data`, bind-mounted to the named
volume `caddy-data` ([`deploy/compose/compose.yml`](../../deploy/compose/compose.yml)).
Decisions:

- **Single named volume, no S3 sync (Fase 5).** Single Caddy replica;
  host-level volume backup covers DR (Fase 6).
- **No `s3` storage backend.** Supply-chain cost ([ADR 0084](./0084-supply-chain.md))
  and ~ms-level latency would push the handshake budget over 50 ms.
- **Treat `caddy-data` as secret.** Contains private keys; deletion
  requires CEO authorisation.

## Consequences

- Ask handler stays small and fast: 4 ports, deterministic pipeline,
  p99 < 50 ms. Tenant/global layers are extra limiter calls on the
  `allow` branch only.
- Producer [SIN-63081](/SIN/issues/SIN-63081) owns the new limiter
  wiring, Prometheus surface, and Alertmanager rules. All three slot
  into existing shapes.
- Three Redis calls per `allow` — inside the 50 ms budget on local
  Redis. Fail-closed.
- Rollback: `CUSTOMDOMAIN_ASK_ENABLED=false` → every ask returns
  `503`, Caddy stops new issuance, existing certs serve until expiry.
  Fast, safe, no redeploy.

## Alternatives considered

- **Caddy `burst` alone.** Rejected: per-host AND per-process —
  multi-replica + tenant-level abuse fall outside its grasp.
- **Postgres-only counters.** Rejected: row-lock contention can blow
  the 50 ms hot-path budget. Redis sliding window is already in tree.
- **Counter in Caddy via `tls.issuers.acme.preferences`.** Rejected:
  too coarse, no per-tenant key, no Prom scraping on our terms.
- **Cluster-wide `s3` storage.** Rejected for Fase 5 — single replica
  is the boring-tech default; revisit in Fase 7 if multi-region.

## Out of scope

- Forced revocation when a tenant is paused mid-cert-life (Fase 6).
- Multi-replica Caddy with clustered storage (Fase 7+).
- DNS-01 challenge migration (`0079a-acme-dns01` follow-up).
- The TXT-challenge validator and ownership UX —
  [ADR 0079](./0079-custom-domain.md) and [ADR 0061 / SIN-63073](/SIN/issues/SIN-63073).
