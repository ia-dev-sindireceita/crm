# Runbook — `AuthzProbingHorizontal`

**Severity:** warning · **Channel:** `#alerts` · **Source:** ADR 0004 §6 ([SIN-62254](/SIN/issues/SIN-62254))

The alert fires when **one actor accumulates more than 10 authorization denies in any 1-minute window**. Most legitimate users see fewer than one deny per session, so this volume almost always means one of:

1. **Probing / IDOR attempt** — an attendant of tenant A asking for `/conversations/{id_de_B}` repeatedly, hoping to find one that returns 200.
2. **Runaway client** — a misconfigured integration retrying a forbidden endpoint in a tight loop.
3. **Compromised credentials** — an outside actor with a valid session iterating identifiers.

The metric is `authz_user_deny_total{actor_user_id, tenant_id}`; the audit trail is `audit_log_security` rows with `event_type = 'authz_deny'`.

---

## 1. Triage (first 5 minutes)

1. Open the alert in Alertmanager / Slack `#alerts`. Note `actor_user_id` and `tenant_id` from the labels.
2. Pull the offending decisions:
   ```sql
   -- run as app_master_ops
   SELECT occurred_at,
          target->>'action'      AS action,
          target->>'reason_code' AS reason,
          target->>'target_kind' AS target_kind,
          target->>'target_id'   AS target_id
     FROM audit_log_security
    WHERE actor_user_id = '<actor_user_id>'
      AND event_type    = 'authz_deny'
      AND occurred_at > now() - interval '15 minutes'
    ORDER BY occurred_at DESC
    LIMIT 200;
   ```
3. Decide which scenario you are in:
   - **Diverse `target_id` values + same `action`** → almost certainly probing. Continue to §2.
   - **Same `target_id` repeated** → likely a runaway client. Continue to §3.
   - **Sudden `reason_code = denied_tenant_mismatch` burst from a single user** → cross-tenant attempt. Treat as probing. Continue to §2.

## 2. Probing — confirmed or suspected

1. **Suspend the session.** Use the master console to revoke all active sessions for the actor:
   ```sql
   -- run as app_master_ops
   DELETE FROM sessions
    WHERE user_id = '<actor_user_id>';
   ```
   The user will be forced to re-authenticate. This stops the probing loop without losing the audit trail.
2. **Reset the credential.** Set a password reset flag on the user (or rotate the credential out of band if the user is internal):
   ```sql
   UPDATE users SET must_reset_password = true WHERE id = '<actor_user_id>';
   ```
3. **Open an incident.** Create a Paperclip issue under the `Security` label with:
   - actor + tenant ids,
   - count + sample of the denies,
   - the action taken (session revoked, credential reset).
4. **Page the tenant's master contact** if you cannot reach the tenant admin within 10 minutes — this user may be an attacker who already exfiltrated session cookies; the tenant admin needs to know.

## 3. Runaway client

1. Identify the client. Cross-reference the `request_id` from a sample deny against the HTTP access log (`http_requests_total` panel + Loki query `{app="crm"} | json | actor_user_id="<id>"`).
2. If the client is an internal integration, **page the integration owner** with the action + sample. Do not suspend the session — that breaks legitimate work for the rest of the tenant.
3. If the client is the tenant's own browser tab, ask QA to reproduce; this is usually a UI bug that calls a forbidden endpoint after a role change.

## 4. False positive

If after triage the alert is benign (e.g. a load test, a known scraper test), document it inline on the incident issue, then:

- raise the threshold in `deploy/prometheus/alerts.yml` (the `> 10` expr) **only after a written CTO ack on the incident issue**, and
- consider scoping the alert with an additional label predicate (e.g. exclude a known synthetic tenant).

## 5. Closing the incident

Before resolving:

1. Confirm `authz_user_deny_total{actor_user_id="<id>"}` rate has returned to baseline for 10 minutes.
2. Confirm the audit trail captured every deny — `SELECT count(*) FROM audit_log_security WHERE actor_user_id = '<id>' AND occurred_at > '<incident start>'` should match the burst from §1.
3. Note in the incident issue what was done (revoked session, reset credential, paged owner) and any follow-ups (UI fix, scope tightening).
4. Mark the alert resolved in Alertmanager.

## Background

- **Why this matters:** [F10](/SIN/issues/SIN-62230) in the SecurityEngineer security review. Decisions to deny were invisible before [SIN-62254](/SIN/issues/SIN-62254); a horizontal probing burst could go unnoticed for hours.
- **Audit retention:** `audit_log_security` keeps rows for 24+ months and is **never** purged by the LGPD job, so post-incident forensics can reach back as far as the table goes. Allow rows are 1% sampled (deterministic by `request_id`) and live in the same table.
- **Metric semantics:** `authz_user_deny_total` is incremented once per deny only; allow decisions never bump it. The label set is `(actor_user_id, tenant_id)` so growth is bounded by the count of users who hit at least one deny in the retention window.
- **Tuning the threshold:** 10/min is the initial baseline before we have prod data. Plan to revisit after 30 days; the chosen value should sit at ~3σ above legitimate user behaviour.
