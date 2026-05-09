# ADR 0074 — 2FA TOTP de master em Fase 0 (enroll + recovery codes)

- Status: Accepted
- Data: 2026-05-07
- Origem: Decisão §2 + §5 do [`decisions` doc rev b133d026](/SIN/issues/SIN-62223#document-decisions),
  aprovada pelo board em 2026-05-07 via confirmation `973a176c-…`
  ([SIN-62223](/SIN/issues/SIN-62223)).
- Issue: [SIN-62338](/SIN/issues/SIN-62338) (filha de [SIN-62223](/SIN/issues/SIN-62223))
- Escopo afeta: [SIN-62192](/SIN/issues/SIN-62192) (Fase 0 bootstrap — adendo
  de critério de aceitação 10 + PR de implementação)
- Compõe com: ADR 0070 (Argon2id helper, F12 + F18) e ADR `0073-csrf-and-session.md`
  (CSRF + sessão + rate limit, F13 + F14 + F17 + F19)
- Substitui: —
- Substituído por: —

> **Nota de numeração.** O `decisions` doc nomeou este ADR como `0071-master-mfa-phase0.md`
> antes de 2026-05-02. ADR 0071 ([postgres-roles](0071-postgres-roles.md)) já tinha sido
> mergido nessa data, e ADR 0072 ([rls-policies](0072-rls-policies.md)) também ocupou seu
> slot. O conteúdo é idêntico ao planejado; só o número desliza para 0074, mantendo o
> cluster de segurança contíguo (0070 Argon2id, 0073 CSRF/sessão, **0074 master MFA**).

## Contexto

O painel master controla operações cross-tenant críticas: criação de tenant,
impersonation de usuário, configuração de canal, override de feature flag,
billing rollups, etc. Hoje, o plano original de [SIN-62192](/SIN/issues/SIN-62192)
trata 2FA de master como entrega da **Fase 2.5**. Nas Fases 0–2.5 o master opera
**só com senha** — phishing ou credential-stuffing de uma única conta master é
breach total cross-tenant.

A janela de exposição se mantivermos o plano original é estimada em 10–13
semanas calendário entre o primeiro login real de master e a Fase 2.5. Inaceitável
dado que a Fase 0 já entrega impersonation funcional. Por isso o board aprovou a
promoção de 2FA TOTP de master para a **Fase 0** (decisão §2 do `decisions` doc),
e este ADR fixa a forma da entrega.

**Lentes aplicadas:**

- **Defense in depth.** Senha sozinha é uma camada; TOTP + recovery code
  hash-at-rest + rate limit + lockout + alerta Slack são camadas independentes.
  Comprometer uma não compromete a próxima.
- **Least privilege.** Master sem TOTP enrolled não pode tocar nenhuma rota
  privilegiada; o middleware `RequireMasterMFA` é deny-by-default e cobre
  toda a árvore `master.*`.
- **Secure-by-default API.** Toda rota nova em `master.*` herda o middleware
  por composição no router; não é necessário lembrar de aplicá-lo manualmente
  em cada handler — o default é denied.

## Decisão

### 1. Enroll de TOTP

- **Quando.** Obrigatório no **primeiro login do primeiro master**. Master sem
  TOTP enrolled que tente acessar qualquer rota `master.*` fora do enroll é
  redirecionado para `GET /m/2fa/enroll` (HTMX-first, server-rendered).
- **Seed TOTP.** 32 bytes de `crypto/rand`, codificados em base32 sem padding
  (RFC 4648) para o `otpauth://` URI exibido como QR code. Algoritmo `SHA1`,
  `digits=6`, `period=30s` (defaults RFC 6238 — todos os clientes TOTP
  comuns suportam; alterar exige retroativo).
- **Confirmação.** Antes de marcar `master.totp_enrolled = true`, o servidor
  exige um código TOTP válido digitado pelo usuário com a janela
  `±1 step (30s)`. Sem código válido, a seed é descartada e o enroll
  recomeça.
- **Persistência da seed.** Coluna `master_user.totp_seed_encrypted` cifrada
  com chave simétrica do app (mesma chave usada pelo helper de criptografia
  de pepper de F12, ver ADR 0070). O *seed em claro nunca toca disco*; só
  fica em memória durante a transação de enroll.
- **Audit.** `event=master_totp_enrolled` em `audit_log` com `actor`, `ts`,
  `tenant=null`, `resource=master_user.{id}`.

### 2. Recovery codes (composição com F16)

- **Quantos.** **10 códigos** gerados junto com a seed TOTP no enroll.
- **Formato.** `crypto/rand` → 10 bytes → base32 (RFC 4648) sem padding,
  truncado para **10 caracteres** (50 bits de entropia, suficiente para
  single-use). Dashes inseridos só na renderização (`XXXXX-XXXXX`); o
  banco guarda o hash do valor sem dash, então a comparação é insensível
  a separador.
- **Apresentação.** Mostrados **uma única vez** na tela de enroll, com:
  - botão "copiar todos para clipboard";
  - botão "baixar `.txt`" (server-rendered, `Content-Disposition: attachment`);
  - warning explícito de print/save offline e que **não serão mostrados de
    novo**, com confirmação obrigatória ("já guardei meus códigos") antes
    do redirect pós-enroll.
- **Armazenamento.** Cada código é hashed com **Argon2id** usando o helper
  do ADR 0070 (`password.Hash`, parâmetros `m=64MB, t=3, p=1`). A coluna
  `master_recovery_code.code_hash` armazena o output Argon2id, **nunca o
  código em claro**, com flag `consumed_at TIMESTAMPTZ NULL` (single-use).
- **Lookup.** O verify percorre os 10 hashes do master e chama
  `password.Verify` em cada um — overhead linear desprezível (≤ 10
  verificações de Argon2id, ainda dentro do budget de 1 login). Não há
  índice por hash porque Argon2id usa salt único por hash; lookup direto
  por igualdade de bytes não funcionaria.
- **Regeneração.** Endpoint `POST /m/2fa/recovery/regenerate` (atrás de
  `RequireMasterMFA`):
  - `UPDATE master_recovery_code SET consumed_at=now() WHERE master_user_id=$1
    AND consumed_at IS NULL` (invalida set anterior em massa);
  - insere 10 novos códigos hashed;
  - emite `event=master_recovery_regenerated` em `audit_log` + alerta
    Slack `#alerts`;
  - retorna a tela "mostrar uma vez" idêntica ao enroll inicial.
- **Recovery codes nunca trafegam por email ou SMS.** Mostrados só na tela
  de enroll/regen.

### 3. Bloqueio de ações privilegiadas — middleware `RequireMasterMFA`

- **Onde mora.** Adapter HTTP (`adapters/transport/http/master/mfa`); domain
  core em `internal/iam/mfa` define só os ports (`Verifier`,
  `RecoveryConsumer`).
- **Onde aplica.** Composto no router master sobre **todo** o subtree
  `master.*` — criação de tenant, impersonation (request + execute),
  configuração de canal, override de feature flag, billing rollups, GDPR
  delete, qualquer rota futura sob esse prefixo.
- **Critérios para passar:**
  1. Sessão master autenticada (já validada pelo middleware de sessão).
  2. `master_user.totp_enrolled = true`.
  3. Sessão tem flag `mfa_verified_at` setado *no login corrente* (ver §4).
- **Falhas:**
  - Não enrolled → `303 See Other` para `/m/2fa/enroll` (preserva o destino
    em `?return=...` validado contra allowlist server-side).
  - Enrolled mas sessão sem `mfa_verified_at` corrente → `303` para
    `/m/2fa/verify` com mesmo padrão de `?return=`.
  - `RequireMasterMFA` é **deny-by-default**: composição no router master
    garante que esquecer de aplicá-lo em uma rota nova retorna 401.
- **Observabilidade.** Cada bloqueio emite `event=master_mfa_required` em
  `audit_log` com `route`, `actor`, `reason ∈ {not_enrolled, not_verified}`.

### 4. Verificação de TOTP por login master

- **Quando.** A cada login de master. **Sem "trust this device"** na entrega
  inicial (revisitamos em Fase 4 se UX pesar — ver Trade-offs).
- **Fluxo.** Após senha válida em `/m/login`, sessão é criada com
  `mfa_verified_at = NULL`; redirect para `/m/2fa/verify`. Submissão válida
  marca `mfa_verified_at = now()` na sessão. Sessões expiradas/idle exigem
  novo verify (não só novo login com senha).
- **Re-MFA explícita.** Operações `master.grant_courtesy`,
  `master.impersonate.request`, `master.feature_flag.write` exigem
  re-verify mesmo com sessão recente — composição com F17 (ADR 0073 §
  Sessão).

### 5. Recovery flow

- **Entrada.** `/m/2fa/recover` aceita 1 código de recovery em vez de TOTP.
- **Efeito do consume:**
  1. Marca o código como `consumed_at = now()`.
  2. Marca a sessão como `mfa_verified_at = now()` para o login corrente
     (deixa o usuário entrar **uma vez** para se reorganizar).
  3. Marca `master_user.totp_reenroll_required = true` — qualquer login
     futuro força o fluxo de enroll **novo** (nova seed, novos 10 códigos,
     set anterior invalidado).
  4. Emite `event=master_recovery_used` em `audit_log` com `actor`,
     `code_index`, `ip`, `user_agent`.
  5. Dispara alerta **imediato** em Slack `#alerts` via adapter
     `notifier/slack` (mesmo adapter usado por F19 lockouts).
- **Sem entrega de recovery code via email ou SMS.**

### 6. Rate limit + lockout (compõe com ADR 0073 §F19)

| Endpoint               | Por IP            | Por user/sessão            | Lockout / efeito                             |
|------------------------|-------------------|-----------------------------|----------------------------------------------|
| `POST /m/login`        | 3/min/IP          | 5/h/email                   | 30 min após **5 falhas** no mesmo email; alerta Slack `#alerts` imediato |
| `POST /m/2fa/verify`   | —                 | 3/min/session, 10/h/user    | **session invalidada** após 5 falhas; alerta Slack `#alerts` imediato     |
| `POST /m/2fa/recover`  | —                 | 3/min/session, 10/h/user    | session invalidada após 5 falhas; alerta Slack `#alerts` imediato         |

- **Implementação.** Middleware `RateLimit("master_login", "ip", "email")` e
  `RateLimit("master_2fa_verify", "session", "user")` reutilizam o adapter
  Redis sliding-window de F19. **Lockout state** vive em Postgres
  (`account_lockout`), não em Redis, para sobreviver a restart.
- **Defesa contra enumeração.** `/m/login` com email inexistente faz dummy
  Argon2id hash + retorna mesma mensagem genérica e mesmo perfil de timing
  que email existente (idêntico ao tratamento de tenant em F19).
- **Alertas Slack.** Lockout master — qualquer um — dispara `#alerts`
  imediatamente, incluindo `actor_email` (não masked, só master é alvo
  desse alerta), `ip`, `user_agent`, `route`. Lockouts de tenant não vão
  para `#alerts` (volume).

### 7. Schema delta

```sql
ALTER TABLE master_user
  ADD COLUMN totp_enrolled BOOLEAN NOT NULL DEFAULT FALSE,
  ADD COLUMN totp_seed_encrypted BYTEA NULL,
  ADD COLUMN totp_reenroll_required BOOLEAN NOT NULL DEFAULT FALSE,
  ADD COLUMN totp_enrolled_at TIMESTAMPTZ NULL;

CREATE TABLE master_recovery_code (
  id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  master_user_id UUID NOT NULL REFERENCES master_user(id) ON DELETE CASCADE,
  code_hash     TEXT NOT NULL,           -- "argon2id$v=19$m=65536,t=3,p=1$..."
  consumed_at   TIMESTAMPTZ NULL,
  generated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX master_recovery_code_active_idx
  ON master_recovery_code (master_user_id)
  WHERE consumed_at IS NULL;
```

A coluna `totp_seed_encrypted` é cifrada com a chave do app (helper de
F12 / ADR 0070); não é armazenada em texto. Migration roda como
`app_admin` (ADR 0071) — `master_recovery_code` é tenant-less, RLS não
se aplica.

## Consequências

**Positivas:**

- Janela de exposição cross-tenant fecha em Fase 0 — deixa de ser 10–13
  semanas e vira zero antes do primeiro tenant produtivo.
- Phishing de senha master perde valor sozinho: atacante precisa também
  do dispositivo TOTP **e** da chance de cair em janela curta sem
  recovery code consumido.
- O middleware `RequireMasterMFA` é deny-by-default — esquecer de
  aplicá-lo em uma rota nova é rota 401, não rota privilegiada aberta.
- Recovery codes Argon2id-hashed: dump do banco não dá ao atacante a
  chance de bypass de TOTP.
- Alertas Slack imediatos em lockout master detectam tentativa de
  brute force em segundos.

**Negativas / custos:**

- **+1 PR** em Fase 0 (~1–2 dias de Coder) — adendo a [SIN-62192](/SIN/issues/SIN-62192).
- UX adicional: master precisa configurar TOTP no primeiro login. Documentado
  em runbook ops + tela de enroll com instruções para Authy/1Password/Google
  Authenticator.
- Verificação TOTP a cada login (sem trust-this-device) é fricção real.
  Aceitável por agora — base de master é pequena (≤ 5 contas previstas no
  primeiro ano), e a perda de UX é menor do que a perda em segurança em
  uma fase inicial onde estamos descobrindo casos de uso.
- 10 hashes Argon2id de verify no recovery flow custam ~2.5s (10 × 250ms).
  Aceitável: recovery é caminho frio e o alerta Slack é o sinal real.

**Trade-offs explicitamente recusados:**

- **WebAuthn / passkeys em vez de TOTP.** Mais seguro, mas exige hardware
  conhecido em todos os masters, fluxo de fallback complexo, e está
  fora do orçamento "boring tech" da Fase 0. Re-considerar em Fase 4.
- **SMS / email como fator.** Phishing/SIM-swap vulnerabilidades
  conhecidas; OWASP recomenda contra. Recovery codes one-shot cumprem o
  papel de fallback offline.
- **"Trust this device" desde já.** Adia para Fase 4 conforme decisão §2
  do `decisions` doc — UX inicial não dói o suficiente para justificar
  superfície adicional (cookie de device + binding + revogação) na
  Fase 0.
- **Recovery codes em email.** Re-introduz canal phishable; recusado
  explicitamente em F16.
- **Plain-text recovery codes em disco** (mesmo cifrados a nível
  container). Argon2id é overkill em CPU mas simétrico: o mesmo helper
  já existe no projeto, custo zero de adoção.

## Implementação

- **Domain core:** `internal/iam/mfa`
  - Port `TOTPVerifier`: `Verify(seed, code, ts) (bool, error)`.
  - Port `RecoveryConsumer`: `Consume(ctx, masterUserID, plain) (bool, error)`.
  - Use-case `EnrollMaster(ctx, masterUserID) (Result, error)` — orquestra
    geração de seed, geração de 10 códigos, persistência via repositório,
    retorno do `otpauth://` URI + códigos em claro (uma única vez).
  - Use-case `VerifyMaster(ctx, sessionID, code) error`.
- **Adapters:**
  - `adapters/totp/rfc6238` — wrapper sobre `github.com/pquerna/otp/totp`
    (boring tech, único projeto Go bem mantido para RFC 6238) ou
    re-implementado com stdlib + `crypto/hmac` se quisermos zero
    deps adicionais. **Decisão de dep fica para o PR de implementação**
    com sign-off CTO; a port é o que conta para este ADR.
  - `adapters/transport/http/master/mfa` — handlers `/m/2fa/enroll`,
    `/m/2fa/verify`, `/m/2fa/recover`, `/m/2fa/recovery/regenerate` +
    middleware `RequireMasterMFA`.
  - `adapters/notifier/slack` — já existe; reuso para alerta `#alerts`.
- **Persistência:** repositório em `adapters/store/postgres/masterauth`
  com queries puras, todas hexadecimal-clean (sem `database/sql` no
  domain).
- **Testes:**
  - Unit: vetor TOTP fixo (RFC 6238 §test vectors), recovery code
    hash + verify roundtrip, dummy-hash anti-enumeration mantém timing
    profile.
  - Integration: enroll → verify → consume recovery → re-enroll forçado
    (boundary E2E em Postgres real).
  - Regression: 6ª falha de TOTP em sessão invalida sessão (cookie
    rotacionado, queries subsequentes 401).
  - Lockout: 6ª tentativa de `/m/login` em janela retorna 429 + alerta
    Slack mockado disparou.

## Como validar este ADR está respeitado

- Convention test em `internal/iam/mfa` que falha o build se algum handler
  em `adapters/transport/http/master/*` não estiver coberto pelo
  `RequireMasterMFA` (varre o router e confere mounts).
- Migration check: `BYPASSRLS` permanece ausente em `app_runtime` (já
  coberto pelo `TestRolesArePostureCorrect` da ADR 0071).
- Audit fixture: cada um dos `event=master_*` aparece exatamente nos
  caminhos esperados em testes E2E (sem audit silencioso).

## Referências

- [`decisions` doc rev b133d026](/SIN/issues/SIN-62223#document-decisions) §2 + §5 (origem + escopo).
- Confirmation `973a176c-…` sobre rev `b133d026` ([SIN-62223](/SIN/issues/SIN-62223)) — aprovação board 2026-05-07.
- [SIN-62223](/SIN/issues/SIN-62223) — Security: Password hashing + CSRF + master 2FA em Fase 0 (epic).
- [SIN-62192](/SIN/issues/SIN-62192) — Fase 0 bootstrap (recebe critério de aceitação 10 + PR de implementação).
- [SIN-62338](/SIN/issues/SIN-62338) — issue desta ADR.
- ADR 0070 — Argon2id helper (F12 + F18) — provedor de `password.Hash`/`Verify` para os recovery codes.
- ADR `0073-csrf-and-session.md` — CSRF, sessão, idle/hard timeout, rate limit (F13/F14/F17/F19) — provedor de `RateLimit` middleware e cookies de sessão master.
- OWASP ASVS V2 (Authentication) — TOTP, recovery codes, lockout.
- RFC 6238 (TOTP) e RFC 4648 (base32).
