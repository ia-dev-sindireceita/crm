# ADR 0108 — WhatsApp session (whatsmeow): credenciais at-rest — aceitar-com-controles + role least-priv

- Status: Accepted
- Date: 2026-06-29
- Drives: [SIN-66291](/SIN/issues/SIN-66291) (R1 — decisão), [SIN-66298](/SIN/issues/SIN-66298) (R1.2 — role + grants, este commit)
- Origem: revisão de segurança [SIN-66260](/SIN/issues/SIN-66260) (residual R1)
- Relaciona: [ADR 0107](0107-whatsapp-session-whatsmeow.md) (arquitetura whatsmeow, D3 store em Postgres, D6 no-log de credencial)
- Lentes aplicadas: Least privilege, Defense in depth, Secure-by-default, Boring technology, Hexagonal (a lib é o adapter; o domínio não toca credencial), Reversibilidade (DT-WA-05 com gatilho)

---

## Contexto / ameaça

As chaves de sessão whatsmeow (`whatsmeow_device.noise_key` / `identity_key` /
`signed_pre_key`, `whatsmeow_pre_keys`, `whatsmeow_sessions`, `whatsmeow_*_keys`)
são **bearer de takeover do número WhatsApp do tenant**: um vazamento permite
sessão completa e replayável. Hoje são persistidas pelo `sqlstore` do whatsmeow
**sem cifragem em nível de aplicação** — a confidencialidade depende inteiramente
de:

1. encryption-at-rest do Postgres (disco/volume + backups), e
2. controle de acesso ao banco.

Estado atual relevante:

- O store vive em **banco Postgres dedicado** apontado por `WA_SESSION_DATABASE_URL`,
  intencionalmente separado de `DATABASE_URL` (ADR 0107 D3 /
  `cmd/server/wa_session_wire.go`). As tabelas `whatsmeow_*` nunca caem no schema
  do app.
- O `sqlstore` do whatsmeow **possui e gerencia o próprio schema** e roda `Upgrade`
  (DDL) na conexão (`internal/wasession/whatsmeowdev/store.go`). O CRM **não**
  controla o caminho de read/write dessas colunas — ele está dentro da lib.
- Não há cifragem app-layer das credenciais. Há redaction de log
  (`internal/wasession/credential.go`) e `waLog.Noop` (ADR 0107 D6), que cobre
  *vazamento por log*, não *vazamento por leitura de DB/backup*.
- A lib não pode ser forkada/modificada: MPL-2.0 (ADR 0107 D2) +
  `go.mau.fi/libsignal` GPL-3.0 (ADR 0107 addendum). Patches privados em arquivos
  da lib são proibidos.

## Modelo de ameaça × controle

| Vetor | Controle que efetivamente fecha |
|---|---|
| Backup não-cifrado vazado (ameaça nomeada na R1) | encryption-at-rest do **backup** (SSE/volume cifrado) |
| Disco/volume roubado | encryption-at-rest do **volume** do Postgres |
| Leitura SQL por role do app comprometido | **role dedicado least-priv** — `app_runtime` não enxerga `whatsmeow_*`; DB separado |
| Dump completo de DB + filesystem **sem** acesso a KMS | **somente** envelope-encryption com KEK externo fecharia este (resíduo) |

## Opções consideradas

**A — Envelope-encryption app-layer das colunas de credencial (DEK por KEK em KMS).**
Rejeitada para v1. O `sqlstore` do whatsmeow é dono do read/write dessas colunas;
cifrar exigiria (a) forkar a lib — **proibido** (MPL/GPL), (b) reimplementar um
backend `store.Device` custom substituindo o `sqlstore` inteiro — esforço alto,
frágil a mudanças upstream, contraria as lentes Boring-tech e Hexagonal, ou (c)
cifragem de coluna via `pgcrypto` — exige a chave na sessão do DB, então **não
defende** contra um leitor de DB/backup, que é exatamente a ameaça. Custo
desproporcional para um resíduo que não bloqueia a Fase 5.

**B — Aceitar-com-controles (defense in depth) + diferir envelope-encryption. ✅ ESCOLHIDA.**
Aceitar que a lib persiste plaintext em nível de coluna e endurecer os controles
ao redor, que fecham as ameaças realmente nomeadas (backup/volume/leitura por role
do app):
- DB dedicado (já feito — `WA_SESSION_DATABASE_URL` ≠ `DATABASE_URL`).
- **Role dedicado least-priv** `wa_session_runtime`, distinto de
  `app_runtime`/`app_admin`, com DML-only sobre `whatsmeow_*` (R1.2).
- encryption-at-rest do volume **e** dos backups, TLS-only no DSN, confirmados com
  evidência (R1.1).
- Envelope-encryption registrada como dívida técnica **DT-WA-05** com gatilho
  explícito.

**C — Backend `store.Device` custom com cifragem.** Variante de A; mesmo veredito
(esforço/risco desproporcional ao resíduo).

## Decisão

**Adotar Opção B: aceitar-com-controles.** As credenciais permanecem em plaintext
de coluna dentro do **banco dedicado**, protegidas por defense-in-depth de quatro
camadas (DB separado + role least-priv + encryption-at-rest de volume e backup +
TLS). Envelope-encryption app-layer é **diferida** como DT-WA-05.

### Roles e split de privilégio (R1.2 — `migrations/wa_session/0001_wa_session_roles`)

Dois roles dedicados no cluster do WA session DB, ambos
`LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOBYPASSRLS`:

- **`wa_session_runtime`** — o role do DSN runtime do app (`WA_SESSION_DATABASE_URL`).
  `USAGE` no schema + `SELECT/INSERT/UPDATE/DELETE` somente nas tabelas
  `whatsmeow_*`. **Sem DDL**: não pode `CREATE`/`ALTER`/`DROP`. Se comprometido,
  o blast radius é ler/escrever as linhas de sessão — não destruir o schema nem
  escalar para o resto do cluster.
- **`wa_session_admin`** — role privilegiado que roda o `Upgrade` (DDL) do
  `sqlstore` num **passo de deploy separado**. `USAGE`+`CREATE` no schema; dono
  das tabelas `whatsmeow_*`.

`app_runtime`/`app_admin`/`app_master_ops`/`app_audit` **não têm acesso** ao WA
session DB. Cluster separado já garante isso; em cluster compartilhado, a migration
faz `REVOKE` explícito + isso é coberto por teste.

A migration usa `ALTER DEFAULT PRIVILEGES FOR ROLE wa_session_admin` para que cada
tabela `whatsmeow_*` criada pelo admin no `Upgrade` auto-conceda DML ao runtime —
permitindo que a migration de roles rode **antes** das tabelas existirem (primeiro
deploy). Um loop adicional concede DML às `whatsmeow_*` já existentes (re-deploy).

### Boot behaviour under the runtime role (split do `Upgrade`)

`internal/wasession/whatsmeowdev/store.go` abre o store com `sqlstore.New`, que
chama `container.Upgrade(ctx)` no boot. Auditoria do código da lib
(`go.mau.fi/whatsmeow .../sqlstore/container.go` + `go.mau.fi/util/dbutil/upgrades.go`,
versões em `go.mod`):

- Com o schema **já atualizado** (`version == len(UpgradeTable)`), `Upgrade` é
  **read-only — não emite DDL**: `checkDatabaseOwner` faz apenas leituras de
  catálogo (`Owner` vazio ⇒ retorna após checar tabelas estranhas); `getVersion`
  encontra a `whatsmeow_version` existente (sem `CREATE`/`ALTER`) e faz
  `SELECT version, compat FROM whatsmeow_version`; o loop de upgrade roda zero
  iterações. Todas essas operações cabem nos privilégios do `wa_session_runtime`
  (USAGE no schema + SELECT em `whatsmeow_*` + leitura de catálogo, sempre
  permitida). **Logo, bootar o app com `wa_session_runtime` num schema atual é
  seguro e não falha** — testado em `internal/adapter/db/postgres/wa_session_roles_migration_test.go`.
- Com schema **desatualizado** (DB novo, ou bump de versão da lib), `Upgrade`
  emite DDL (`CREATE TABLE whatsmeow_*`/`whatsmeow_version`, etc.), que o
  `wa_session_runtime` **não** pode rodar — por design. Por isso o `Upgrade` é
  **gated para o passo de deploy como `wa_session_admin`** (ver
  `migrations/wa_session/README.md` e `docs/deploy/staging.md` §5g): rode o
  `Upgrade`-como-admin **antes** de subir um build do app que carregue um schema
  whatsmeow mais novo. Isso mantém o caminho de boot livre de DDL.

> Gap pré-existente (fora do escopo desta R1.2, registrado para follow-up): o
> driver `database/sql` `"postgres"` que `sqlstore.New` exige **não está
> registrado** no grafo de build atual (nenhum import em branco de `lib/pq`/
> `pgx/stdlib`). O transporte está atrás de `FEATURE_WA_SESSION_ENABLED` e ainda
> não exercitado contra Postgres real; registrar o driver é pré-requisito para o
> primeiro boot real e deve ser tratado num ticket próprio.

### DT-WA-05 — gatilhos de reabertura

Qualquer um dispara reavaliação de envelope-encryption app-layer:

- deployment distribuído/on-prem/imagem entregue ao tenant (já vedado por GPL —
  ADR 0107 addendum DT-WA-04, sobrepõe);
- migração para Postgres gerenciado **sem** garantia de encryption-at-rest de
  volume + backup;
- requisito de compliance/contratual que exija cifragem app-layer das credenciais
  de canal;
- whatsmeow expor um hook oficial de cifragem de store (eliminaria o custo de fork).

## Consequências

- **Positivas.** Blast radius do credential store reduzido a um role DML-only
  isolado; um `app_runtime` comprometido não alcança as sessões; o schema não pode
  ser destruído pelo role de runtime; primeiro deploy e re-deploy cobertos por
  default-privileges + grant loop; decisão e gatilhos de reabertura registrados.
- **Negativas / resíduo.** Credenciais seguem em plaintext de coluna; um dump de
  DB + filesystem com acesso simultâneo a tudo ainda expõe sessões (fechado só por
  DT-WA-05). Operadores precisam rodar o `Upgrade`-como-admin como passo explícito
  de deploy antes de bumps de schema — documentado no runbook.

## Reversibilidade

`migrations/wa_session/0001_wa_session_roles.down.sql` revoga os grants e remove os
roles. Voltar ao posture pré-hardening (PUBLIC com acesso) é um passo manual
deliberado descrito na migration `down`.
