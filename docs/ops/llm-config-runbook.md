# LLM configuration runbook (OpenRouter) — staging

SIN-65244 / product decision [SIN-65243]. This runbook is the operator
checklist for the two LLM call points the CRM ships and the single
shared model knob that drives both.

## The two LLM call points

| Point | What it does | Selected by | Activation posture |
| --- | --- | --- | --- |
| **Persona** (fake customer) | Drives the synthetic "Cliente Fake (LLM)" inbound replies in the `llmcustomer` inbox loop | `PERSONA_LLM_PROVIDER` | **HARD gate** — boot **aborts** if `openrouter` is selected without `OPENROUTER_API_KEY` |
| **AI-assist** (operator) | "Resumir + sugerir 3 respostas" button on the conversation view (`POST /inbox/conversations/{id}/ai-assist`) | presence of `OPENROUTER_API_KEY` | **SOFT degrade** — missing key leaves the feature off; boot continues, the route/button simply do not mount |

The contrast is deliberate. A misconfigured **persona** must fail loud
(it would otherwise spend money replying as a fake customer with the
wrong settings). The **AI-assist** feature is an opt-in operator
convenience: a missing key must never down the listener — it just means
the button does not appear.

## Environment variables (`.env.stg` on the VPS)

| Var | Default | Used by | Notes |
| --- | --- | --- | --- |
| `OPENROUTER_API_KEY` | _(unset)_ | both | OpenRouter bearer token. **Never logged.** Persona hard-refuses boot without it when `PERSONA_LLM_PROVIDER=openrouter`; AI-assist soft-degrades (off) without it. |
| `OPENROUTER_MODEL` | `google/gemini-2.0-flash` | both | **Central model knob.** Sets the default model for *both* points at once. |
| `AIASSIST_LLM_MODEL` | _(falls back to `OPENROUTER_MODEL`)_ | AI-assist | Per-point override for the operator Summarizer only. |
| `PERSONA_LLM_MODEL` | _(falls back to `OPENROUTER_MODEL`)_ | persona | Per-point override for the fake-customer persona only. |
| `PERSONA_LLM_PROVIDER` | `canned` | persona | `canned` (deterministic, no secrets) or `openrouter`. |
| `INBOX_CHANNEL_PROVIDER` | `disabled` | inbox | `disabled`, `llmcustomer`, or `real`. AI-assist is wired **inside the `llmcustomer` branch**, so the operator Summarizer is only reachable when `INBOX_CHANNEL_PROVIDER=llmcustomer`. Refused in production-tier `APP_ENV` (`production`, `staging-prod`). |

### Model resolution order (per point)

```
per-point override  →  OPENROUTER_MODEL  →  google/gemini-2.0-flash
```

- AI-assist: `AIASSIST_LLM_MODEL` → `OPENROUTER_MODEL` → `google/gemini-2.0-flash`
- Persona:   `PERSONA_LLM_MODEL`  → `OPENROUTER_MODEL` → `google/gemini-2.0-flash`

Leaving every model knob unset routes **both** points to
`google/gemini-2.0-flash` (the SIN-65243 "same model everywhere by
default" decision). Setting only `OPENROUTER_MODEL` moves both points
together; a per-point override peels one point off the shared default.

> **AI-assist model precedence caveat.** The per-tenant `ai_policy.model`
> column (set in the AI-policy settings UI) is forwarded by the
> Summarize use case and takes precedence over the env-resolved default
> for that tenant. `AIASSIST_LLM_MODEL` / `OPENROUTER_MODEL` set the
> *fallback* model used when the policy row leaves `model` blank — they
> make the previously hardcoded adapter default configurable without a
> code change.

## Enabling AI-assist on staging (operator steps)

`cd-stg` **does not propagate arbitrary env vars** — the SSH deploy
wrapper only runs `deploy | migrate-up | preflight` verbs and only
rewrites `APP_IMAGE` (see [SIN-64032] / the `cd_stg_does_not_propagate_env_vars`
note). The OpenRouter key therefore enters via `.env.stg` on the VPS;
this is an **operator action**, not something the deploy pipeline does.

1. SSH to the staging VPS.
2. Edit `/opt/crm/stg/.env.stg` and add (no quotes, no trailing space):
   ```
   INBOX_CHANNEL_PROVIDER=llmcustomer
   OPENROUTER_API_KEY=sk-or-...          # the board-provisioned key
   OPENROUTER_MODEL=google/gemini-2.0-flash   # optional; this is the default anyway
   # AIASSIST_LLM_MODEL=...               # optional per-point override
   # PERSONA_LLM_PROVIDER=openrouter      # optional; enables the LLM persona too
   ```
3. Recreate the app container so it re-reads the env:
   ```
   cd /opt/crm/stg && docker compose -f compose.stg.yml up -d --force-recreate app
   ```
4. Verify the boot log shows the wireup line (the key is **not** printed):
   ```
   docker logs crm-stg-app-1 2>&1 | grep "ai-assist operator summarizer"
   # wired:   crm: ai-assist operator summarizer wired (provider=openrouter, model=google/gemini-2.0-flash)
   # off:     crm: ai-assist operator summarizer disabled — OPENROUTER_API_KEY unset (soft-degrade; route + button stay off)
   ```
5. Run the smoke: `scripts/ci/stg-smoke-aiassist.sh` (see below).

### Rollback

Remove `OPENROUTER_API_KEY` from `.env.stg` and `--force-recreate app`.
The feature is default-OFF; the route + button stop mounting on the
next boot. Zero migrations, fully reversible.

## Security notes

- The API key is forwarded only as the OpenRouter `Authorization`
  header by the adapter; it is never logged, never placed in a URL, and
  never echoed by the smoke (`set +x`).
- The AI-assist route is auth-gated via `iamRoutes`
  (`RequireAction(iam.ActionTenantInboxRead)` + tenant scope + CSRF) —
  same gate as the rest of `/inbox`.
- The LGPD consent gate (anonymizer + per-tenant consent) is wired into
  the production Summarizer, so the first AI-assist call for a tenant
  surfaces the consent modal before any prompt reaches OpenRouter.
