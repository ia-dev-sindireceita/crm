# ADR 0094 — Webhook timestamp `PastWindow` vs Meta 24h retry budget (accepted)

> **Status:** Accepted — SecurityEngineer + CTO sign-off complete per [SIN-62761](/SIN/issues/SIN-62761) AC §4.
> **Source review:** SecurityEngineer note on [SIN-62731](/SIN/issues/SIN-62731) PR [#86](https://github.com/ia-dev-sindireceita/crm/pull/86).
> **Antecedents:** [ADR 0075 §D3](./0075-webhook-security.md#d3) (5min/1min window), [ADR 0087](./0087-webhook-idempotency.md) (two-layer dedup).
> **Bundle severity:** MEDIUM (availability, silent-loss class). Not actively exploitable in prod (Fase 1 not on); blocks Fase 1 prod-on.
> **Lenses applied:** Defense in depth, Fail-securely (failure-closed), Reliability under partition, OWASP API A04 Insecure design, STRIDE Information Disclosure → reframed as Information Loss.
> **Numbering note:** drafted as "ADR 0093" in [SIN-62761#document-plan](/SIN/issues/SIN-62761#document-plan); landed at 0094 because SIN-62730 (courtesy-grant) consumed 0093 between draft and merge.

## 1. Context

ADR 0075 §D3 set a `PastWindow = 5 min` envelope-freshness check on `entry[].time` for the WhatsApp webhook adapter. The check was specified in the same breath as the HMAC verify and the `webhook_idempotency` row insert — three independent defenses against replay. At the time of writing, the relative weight of each defense was not analysed against Meta's documented retry semantics.

Meta Cloud API retries failed deliveries on an exponential schedule **for up to 24 hours**. Retries reuse the original `entry[].time` (the event time), not the retry attempt time. Concretely, the following sequence is documented behaviour for Meta:

1. Event happens at `t0`. Meta POSTs `entry[].time = t0`.
2. CRM is unreachable (Caddy down, VPS reboot, NATS partition, deploy window, AZ flap). Meta receives no 2xx.
3. Meta backs off and retries with the **same body** (and therefore the same `entry[].time = t0`) at `t1 = t0 + Δ` where Δ grows up to ~24h.
4. CRM recovers at `t2`. Retry arrives at `t3`.
5. If `t3 − t0 > 5 min`, the current handler drops the retry silently with 200 OK.
6. Meta marks the message **delivered** (Meta got 200) → no further retries → **message permanently lost, no operator signal**.

The 5-minute window is **safe** in the sense that no attacker can use the timestamp check itself to amplify damage. It is **expensive** in the sense that any partition longer than 5 minutes converts Meta's retry SLA into silent customer message loss with no alert.

### What the other defenses already prove

The "what does the 5-min window buy us, given the other defenses?" decomposition:

| Threat                                                  | Defended by                                                                                  | Window helps? |
|---------------------------------------------------------|----------------------------------------------------------------------------------------------|---------------|
| Forged body                                             | HMAC-SHA256 over body bytes (`Adapter.verifySignature`, ADR 0075 §D2/D4)                     | No            |
| Captured-body replay creating duplicate `Message`       | `inbound_message_dedup(channel, channel_external_id)` UNIQUE (ADR 0087 §D2)                  | No            |
| Captured-body replay creating duplicate `raw_event`     | `webhook_idempotency(tenant_id, channel, sha256(body))` PK (ADR 0075 §D2)                    | No            |
| Cross-tenant misrouting via leaked URL/token            | `BodyTenantAssociation` cross-check (ADR 0075 §D4 F-12)                                       | No            |
| Replay with "fresh" wall-clock to appear current        | `OccurredAt = parseMetaTimestamp(msg.Timestamp)` — inbox stores the message's own timestamp  | No            |
| Clock-drift abuse (future-pivoted payload, extends dedup retention horizon) | `FutureSkew = 1 min` future bound                                       | **Yes**       |
| Long-archive replay (year-old body, post-30d-GC of `webhook_idempotency`)  | None today; would create one `Message` with year-old `OccurredAt`        | Marginal      |

The past window provides marginal defense only against the **last** row. All other replay classes are defended by HMAC + dedup + `OccurredAt` preservation independently of the window size. The "year-old archived body" attack is itself low-value: the resulting `Message` shows up in the inbox with its original `OccurredAt`, sorts at the bottom of any time-ordered view, and is one row — not a flood.

The 1-minute future skew is the only timestamp check that defends a unique threat (future-pivoting to extend the dedup-GC horizon — see [§5 Non-objectives](#5-nao-objetivos)).

## 2. Options

### (a) Widen `PastWindow` from 5min to 24h (match Meta retry budget)

- ✅ Eliminates silent-loss class for partitions/outages up to Meta's documented retry SLA.
- ✅ Preserves all replay defenses (HMAC, transport dedup, domain dedup, body↔tenant cross-check) unchanged.
- ✅ Aligns CRM behaviour with Meta's retry envelope — operationally predictable.
- ❌ A captured body can now be force-presented within 24h of its original timestamp. Already dedup'd. Marginal cost: one row in `webhook_idempotency` per replay attempt. No `Message` created.
- ❌ Long-archive replay (24h–30d): still hits `webhook_idempotency` until GC, then would produce one `Message` with old `OccurredAt`. Same risk profile as today, just shifted by 23h55m.

### (b) Drop the past-window check entirely (rely on HMAC + dedup)

- ✅ Zero false drops, ever. Maximum availability.
- ✅ Simplest code path.
- ❌ Contradicts ADR 0075 §D3 documented invariant — diverges from a recently-reviewed security posture.
- ❌ Year-old archived bodies (post-30d-GC) would produce one `Message` each. The mitigation (`OccurredAt` preservation) still makes them low-impact, but there's no defense-in-depth left.
- ❌ Removes a forensic signal: drops outside any plausible Meta retry window are interesting. Losing the signal loses the ability to alert on it.

### (c) Keep 5min window, add metric/log on drop

- ✅ Documented invariant preserved.
- ✅ Operators can see drops in Grafana.
- ❌ **Does not solve the original problem.** Partition >5min still loses messages. Operator now *sees* the loss; can manually re-process from `raw_event`, but `raw_event` retention is 30d — survivable. However, requires human intervention to recover from every >5min outage.

## 3. Decision

**Option (a), augmented with the metric from (c).**

| Param            | Current      | Proposed     | Rationale                                                              |
|------------------|--------------|--------------|------------------------------------------------------------------------|
| `PastWindow`     | 5 minutes    | **24 hours** | Matches Meta retry budget. Marginal security cost ≈ 0 given HMAC+dedup. |
| `FutureSkew`     | 1 minute     | **1 minute** | Unchanged. Defends dedup-GC horizon abuse — different threat class.    |
| Drop observability | none (log only) | **structured log + Prometheus counter** | Defense in depth on the new wider window. |

### 3.1 — Code changes

- `internal/adapter/channels/whatsapp/config.go`: `defaultReplayWindowPast = 24 * time.Hour` (was `5 * time.Minute`).
- `internal/adapter/channels/whatsapp/handler.go`: in `handlePost`, when `timestampWindowDirection` returns a non-empty direction, increment the `webhook_timestamp_window_drop_total` counter labelled `direction=past` vs `direction=future`. The existing structured-log line gains a `direction` field so SIEM dashboards can filter without recomputing the boundary in promQL.
- `internal/obs/metrics.go`: adds `WebhookTimestampWindowDrops *prometheus.CounterVec` with name `webhook_timestamp_window_drop_total`, help text, and labels `{channel, direction}`. Wired the same way as `AuthRateLimitDenies`. No per-tenant label (cardinality + the `tenant_id` is not authenticated at this point in the handler anyway — ADR 0075 §D4 invariant). Package-level helper `obs.WebhookTimestampWindowDrop(channel, direction)` mirrors `obs.IncRLSMiss` so adapters can wire the increment as a callback without taking a hard dependency on a concrete `*Metrics`.
- `deploy/prometheus/webhook-rules.yml`: adds `WebhookTimestampWindowDropPastBurst` (`rate(webhook_timestamp_window_drop_total{direction="past"}[15m]) > 0.05`, sustained 15min, page on-call) and the symmetric `WebhookTimestampWindowDropFutureBurst` rule for clock-skew detection (§3.2).
- Tests in `internal/adapter/channels/whatsapp/handler_test.go`:
  - `TestPost_TimestampStale_Drops` (existing): updated to use a timestamp **>24h** in the past, not >5min. Asserts the counter incremented by 1 with `direction="past"`.
  - **New** `TestPost_TimestampInsidePastWindow_PersistsAt23h`: timestamp 23h59m in the past → 200 + `inbox.PersistedCount() == 1`. Encodes the new behaviour (regression-test against any future narrowing back to 5min).
  - `TestPost_TimestampFutureBeyondSkew_Drops` (existing): adds counter-increment assertion with `direction="future"`.
  - `TestPost_TimestampInsideFutureSkew_Persists` (existing): unchanged, regression-test that `FutureSkew` did **not** get touched.

### 3.2 — Why the metric, even on option (a)

Even with the 24h window, three failure modes can still produce drops worth seeing:

1. **Meta retry budget exceeded** (Meta's 24h SLO is best-effort, not contractual). If Meta retries land >24h late, we want to know — it means another extension is needed OR an outage exceeded Meta's own retry budget.
2. **Captured-body replay attempts at scale** (rate-limit canary). A spike on `direction=past` without correlated outage signal is interesting.
3. **Clock skew on our side** (rare but real on cheap cloud hardware). `direction=future` spikes catch CRM-clock-drift bugs early.

A counter is the smallest possible observability change that makes those events visible. The alert rule keeps drop-rate ≪ 0.05/sec under normal operation — drops at all are abnormal; sustained drops are a signal.

### 3.3 — Why not a wider `FutureSkew`

The future-skew check at 1 minute defends a real distinct threat: a payload with a timestamp **in the future** would extend the row's effective age in `inbound_message_dedup` GC. ADR 0089 sets retention to 30 days from `inserted_at`, but a future-pivoted timestamp could be replayed close to the GC boundary to slip past it. The 1-minute future bound caps that pivot to negligible.

Widening `FutureSkew` would weaken this without buying anything operationally — Meta does not send timestamps in the future under normal operation. **Keep `FutureSkew = 1 min`.**

## 4. Acceptance criteria mapping (issue [SIN-62761](/SIN/issues/SIN-62761))

| AC | Status                                                                                                       |
|----|---------------------------------------------------------------------------------------------------------------|
| 1  | ADR landed at `docs/adr/0094-webhook-timestamp-past-window.md` (renumbered from 0093 due to SIN-62730 race). |
| 2  | `handler.go` + `handler_test.go` updates shipped in [SIN-62763](/SIN/issues/SIN-62763).                       |
| 3  | Counter + log field + alert rule shipped per §3.1; covers Option (a) **with** Option (c) observability.      |
| 4  | SecurityEngineer + CTO sign-off captured in §9 below.                                                         |

## 5. Não-objetivos

- **HMAC verify is not touched.** ADR 0075 §D2 stands as-is.
- **`webhook_idempotency` is not touched.** ADR 0075 §D2 stands.
- **`inbound_message_dedup` is not touched.** ADR 0087 stands.
- **`FutureSkew` is not widened.** §3.3.
- **Retention (`webhook_idempotency` GC, `raw_event` partitioning, `inbound_message_dedup` GC) is not touched.** ADR 0089 stands.

## 6. Residual risk

- **Captured-body replay within 24h, post-30d GC of `webhook_idempotency`.** A body captured at `t0` and replayed at `t0 + 23.9h` AND after the `(tenant_id, channel, sha256(body))` row was GC'd from `webhook_idempotency`. Impossible by construction today: dedup retention is 30d, well over the new 24h window. The two retentions must stay in `dedup_retention > PastWindow` ordering; we add an inline comment in `config.go` documenting that invariant. If a future ADR ever wants to drop dedup retention below 24h, this constraint must be reopened.
- **Long-archive replay (>30d, post-`webhook_idempotency`-GC).** Year-old body would produce one `Message` with year-old `OccurredAt`. Same risk as today; not regressed. Mitigation remains: inbox UI sorts by `OccurredAt`, ancient replays sink to the bottom; alerting on the new metric flags abnormal `direction=past` rate.
- **Meta extends retry budget beyond 24h.** Possible. The alert in §3.1 catches it; the next ADR widens the window.

## 7. Rollback

- `defaultReplayWindowPast` is a config value. Operator can override via env if 24h ever turns out to be wrong in production. No migration, no schema change.
- Reverting commit re-introduces the silent-loss class but is purely-local (no data destroyed).
- Counter + alert rule are additive — removing them has no functional impact.

## 8. Lentes citadas

- **Defense in depth.** Five replay defenses (HMAC + transport dedup + domain dedup + body↔tenant cross-check + `OccurredAt` preservation). Past-window is the sixth; narrowing it to 5min adds nothing the other five don't already cover for replay; widening it to 24h removes a silent-loss failure mode that the other five don't address.
- **Fail-securely.** Failure-closed semantics preserved: the action on drop is still "drop + log + 200" (anti-enumeration). The new metric does not change the failure-closed property.
- **Reliability under partition.** Aligning with the carrier's retry budget is the textbook fix for partition-induced delivery loss.
- **OWASP API A04 Insecure design.** The original 5-min choice was conservative-by-default but did not weigh the availability dimension against the carrier's documented behaviour — exactly the design gap A04 is about. This ADR closes it.
- **STRIDE Information Loss (extension).** The classical STRIDE Information **Disclosure** lens (which the 5min window was implicitly defending) is dominated by HMAC + dedup; the dual lens — Information **Loss** induced by overzealous time-window — is the one we are now addressing.

## 9. Sign-off

- [x] **SecurityEngineer** (author): position confirmed, no further changes requested.
- [x] **CTO**: confirmation captured via `request_confirmation` interaction on [SIN-62761](/SIN/issues/SIN-62761).

Implementation landed in [SIN-62763](/SIN/issues/SIN-62763).
