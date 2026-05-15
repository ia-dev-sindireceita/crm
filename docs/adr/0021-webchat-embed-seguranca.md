# ADR 0021 — Webchat embed: CSP, CORS, CSRF, assinatura de origem, rate limit

- Status: Accepted
- Date: 2026-05-15
- Owners: Coder ([SIN-62789](/SIN/issues/SIN-62789)), CTO (review), SecurityEngineer (review)
- Related: [SIN-62194](/SIN/issues/SIN-62194) (Fase 2 parent), [SIN-62798](/SIN/issues/SIN-62798) (F2-11 — backend webchat), [SIN-62800](/SIN/issues/SIN-62800) (F2-14 — widget JS), [ADR 0020](./0020-merge-de-identidade.md) (Identity hook)
- Antecedentes: [ADR 0073](./0073-csrf-and-session.md) (CSRF interno), [ADR 0075](./0075-webhook-security.md) (HMAC+replay), [ADR 0079](./0079-custom-domain.md) (custom domain), CSP middleware em `internal/http/middleware/csp`
- Lentes: **Defense in depth**, **Secure-by-default API**, **Boring technology**, **Reversibility**, **Least privilege**, OWASP A01/A02/A05/A07

## Context

Webchat é o 4º canal Fase 2. AC #6: o site do tenant carrega
`<script src="https://<tenant>.crm.<host>/widget.js">`. O widget chama
`POST /widget/v1/session`, `POST /widget/v1/message`, e abre SSE em
`GET /widget/v1/stream`.

Diferente dos canais Meta (server-to-server, ADR 0075) e do CRM interno
(autenticado, ADR 0073), aqui o cliente é **anônimo**, os endpoints são
**públicos cross-origin**, o widget executa JS no contexto do site
cliente (XSS lá se comprometido), e SSE long-poll esgota pool se
abusado. Este ADR fixa o contrato antes de F2-11/F2-14.

## Decision

### D1 — CSP do widget (Defense in depth, OWASP A05)

- `GET /widget.js`: `Content-Security-Policy: default-src 'none'`;
  cache `public, max-age=300`.
- `POST /widget/v1/{session,message}` e `GET /widget/v1/stream`:
  `default-src 'none'` (JSON / SSE; nenhum HTML).
- Widget **nunca** usa `innerHTML`, `eval`, `Function()`, `setTimeout("string")`.
  Render via `document.createElement` + `textContent`/`setAttribute`.
- Painel vive num **shadow DOM aberto** com CSS scoped: herda zero do
  site cliente e não vaza. CSP do site cliente é responsabilidade dele;
  recomendamos `script-src https://<tenant>.crm.<host>` em
  `docs/widget/README.md`.
- Lint custom (`internal/lint/widget.go`, F2-14) bloqueia `innerHTML` /
  `eval` no bundle. Fallback `<form action="/widget/v1/message">` sem
  JS, degradação graciosa (AC F2-14).

### D2 — CORS allowlist por tenant (Least privilege)

`tenant_settings.webchat_allowed_origins` (JSONB array de strings).

- Lista origens completas (`https://acme.com.br`,
  `https://www.acme.com.br`). **Sem wildcard de subdomínio** na Fase 2.
- `POST /widget/v1/session` valida `Origin` header:
  - Ausente → 400.
  - Fora da allowlist → 403 + audit
    `webchat.origin_blocked{tenant_id, origin_hash}`.
  - Dentro → `Access-Control-Allow-Origin: <origin echoed>` (não `*`),
    `Access-Control-Allow-Credentials: true`, `Vary: Origin`.
- Lista vazia = **fail-closed** (403 universal). Tenant precisa popular
  antes de habilitar o canal.

### D3 — Sessão anônima + CSRF (Defense in depth, OWASP A01)

`POST /widget/v1/session` devolve:

```json
{ "session_id": "<uuid v7>", "csrf_token": "<32-byte base64url>",
  "expires_at": "<RFC3339; default 30min idle>" }
```

Persistido em `webchat_session(tenant_id, session_id, csrf_token_hash,
created_at, last_activity_at, origin_signature, ip_hash, expires_at)`.
`csrf_token_hash = sha256(token)` — plaintext nunca dorme no DB (padrão
do ADR 0075 §C3 `webhook_tokens`).

**Double-submit por header** (não cookie):

- Widget guarda token em `sessionStorage` (não `localStorage` — escopo
  de aba só).
- Todo `POST /message` envia `X-Webchat-CSRF: <token>` +
  `X-Webchat-Session: <session_id>`. Server valida
  `sha256(header) == csrf_token_hash`.
- `fetch` com `credentials: 'omit'`. **Sem cookies** — `SameSite=Strict`
  quebraria cross-origin; `SameSite=None` reabriria CSRF clássico.
- Refresh em `POST /widget/v1/session/refresh` quando `expires_at <
  now()+5min`. Requer token vigente.

### D4 — Assinatura de origem (Defense in depth, OWASP A07)

`origin_signature = HMAC-SHA256(tenant_origin_secret, canonical_origin)`
com `canonical_origin = "<scheme>://<host>:<port>"` (lowercase, porta
default omitida). `tenant_origin_secret` é per-tenant em
`tenant_settings.webchat_origin_secret` (bytea, rotacionável, redacted
no audit log).

- O handler que serve `widget.js` injeta uma **lookup table** de
  `{origin → hmac}` para cada origem da allowlist; widget escolhe a
  entrada onde `key == window.location.origin`. HMAC é one-way.
- `POST /widget/v1/session` exige `X-Webchat-Origin-Signature`. Server
  recomputa e compara em constant-time (`hmac.Equal`); mismatch → 403 +
  `webchat.origin_signature_fail{tenant_id}`. Sessão armazena a
  assinatura; mudou mid-sessão → 403 e widget cria sessão nova.

D4 não é controle primário (D2 é). É camada extra para "alguém clonou
`widget.js` num site fora da allowlist".

### D5 — Rate limit por IP+tenant (Boring tech, OWASP A04)

Token bucket in-memory (`internal/http/middleware/ratelimit`). Buckets
conferidos **antes** de CSRF/HMAC (filtro barato primeiro):

| Bucket                                    | Limite        | Janela | Ação                  |
|-------------------------------------------|---------------|--------|------------------------|
| `webchat.session.create{tenant_id, ip}`   | 10 sessions   | 1 min  | 429, `Retry-After: 30` |
| `webchat.session.create{tenant_id, /24}`  | 200 sessions  | 1 min  | 429 (anti-sybil)       |
| `webchat.message{tenant_id, session_id}`  | 60 msgs       | 1 min  | 429, `Retry-After: 5`  |

Chave hash-only (`ip_hash = sha256(ip + tenant_id)`); IP plaintext não
persiste — LGPD-safe nos audit logs.

SSE (`GET /widget/v1/stream`): **1 conexão por session_id**, **5 por
tenant × IP**. Excesso = 429 imediato. Multi-instance sync via Redis
fica para Fase 3 (Fase 2 single-instance — aceitável).

### D6 — Identity hook (cruza com ADR 0020)

Primeiro `POST /widget/v1/message` chama
`IdentityRepository.Resolve(channel='webchat', external_id=session_id,
phone=nil, email=nil)` → ADR 0020 D2 caminho 1 cria identity+contact+link
`external_id`.

Quando o widget colhe email/telefone do visitante (campos opcionais),
o backend re-chama `Resolve` com sinais preenchidos. Aí ADR 0020 D3
pode disparar merge (auto ou `MergeProposal`). Visitante anônimo nunca
atravessa para contato existente — só converge quando preenche sinal
forte.

### D7 — Idempotência e feature flag

`POST /widget/v1/message` carrega `client_msg_id` (uuid v7 do widget);
idempotência por `(session_id, client_msg_id)` via
`inbound_message_dedup` (mesma tabela do ADR 0087, `channel='webchat'`).

`feature.channel.webchat.enabled` per-tenant, default `false`. Flag-off
→ `/widget/v1/*` retorna 404 (não 503 — não vaza existência do tenant).

## Modelo de ameaças (resumo)

| # | Ameaça                            | Mitigação primária + extra              |
|---|-----------------------------------|------------------------------------------|
| T1| Site malicioso embeda widget      | D2 (CORS allowlist) + D4 (origin signature) |
| T2| CSRF entre origens                | D3 (header double-submit) — sem cookies  |
| T3| Flood de sessões                  | D5 (rate limit IP+tenant + /24 anti-sybil) |
| T4| XSS via mensagem inbound          | D1 (`textContent` + CSP `'none'`)        |
| T5| SSE pool exhaustion               | D5 (1/session, 5/IP) + idle timeout 4min |
| T6| Enumeração de tenant              | flag-off = 404; ADR 0079 rate limit      |
| T7| Replay de body                    | D7 + ADR 0087 dedup                      |
| T8| Origin secret vazado              | rotação + audit em `tenant_settings`     |

## Alternativas consideradas

- **Cookie `__Host-webchat-csrf`.** Rejeitada: cross-origin por desenho;
  `SameSite=Strict` quebra, `SameSite=None` reabre CSRF. Header
  double-submit é a saída idiomática.
- **JWT no widget.** Rejeitada: já precisamos de session store em DB
  (`expires_at`, `ip_hash`); JWT vira 2ª fonte da verdade e complica
  revogação.
- **Sem origin signature, só CORS allowlist.** Rejeitada: CORS é
  controle do **browser**; cliente fabricado (`curl`) ignora. D4 exige
  conhecer `tenant_origin_secret`.
- **WebSocket em vez de SSE.** Rejeitada: SSE atravessa qualquer proxy
  e cobre 100% do caso (server→cliente).
- **`widget.js` de CDN público.** Rejeitada: D4 é per-tenant, exige
  serve dinâmico; `max-age=300` resolve eficiência.

## Consequences

**Positivas.** 4 camadas independentes contra cross-origin abuse
(allowlist + origin signature + CSRF + rate limit). Boring tech (HTTP +
SSE + HMAC + Postgres; sem WebSocket, JWT, Redis na Fase 2). Rollout
granular: flag + allowlist vazia = canal "off".

**Negativas.** 5 sub-controles = mais surface que canais Meta (só HMAC).
F2-11 precisa cobertura por D1–D7. `webchat_session` cresce com
visitantes — sweep diário (`DELETE WHERE expires_at < now()-1d`). Leak
de `tenant_origin_secret` (DB/log) bypassa D4 (mitigação: rotação ~90d
+ redact no audit).

**Risco residual.** Cliente fabricado + secret vazado bypassa D4
(mitigação: alert em `webchat.session.create_total{tenant_id,
origin_hash}` fora da allowlist). Shadow DOM em browsers antigos →
fallback `<form>` plain. Anonimato deliberado é aceitável (LGPD-friendly).

## Rollback

- Flag-off no tenant → `/widget/v1/*` retorna 404.
- Origin secret vazado → rotar `tenant_settings.webchat_origin_secret`
  via UI master/admin; sessões antigas falham na próxima request,
  widget cria sessão nova. Recupera em < 1 expiração.
- CSP estourando → CSP de `/widget/v1/*` é independente da do site
  cliente; worst case revert do shadow DOM para `<iframe sandbox>`.

## Out of scope

- Webchat mobile nativo (SDK iOS/Android).
- Push notifications a visitante anônimo.
- i18n do widget (F2-14 entrega `pt-BR`).
- Read receipts visitante→operador (ADR 0095 cobre só o oposto).
- Compliance LGPD do site cliente (banner de cookies, opt-in
  email/telefone) — responsabilidade do site; ADR 0089 cobre retenção
  de message body.
