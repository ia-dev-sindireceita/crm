# ADR 0020 — Merge de identidade (Fase 2)

- Status: Accepted
- Date: 2026-05-15
- Owners: Coder ([SIN-62789](/SIN/issues/SIN-62789)), CTO (review)
- Related: [SIN-62194](/SIN/issues/SIN-62194) (Fase 2 parent), [SIN-62790](/SIN/issues/SIN-62790) (migration 0092 — `identity` + `contact_identity_link` + `assignment_history`), [SIN-62794](/SIN/issues/SIN-62794) (F2-06 — domínio `internal/contacts`), [SIN-62799](/SIN/issues/SIN-62799) (F2-13 — UI HTMX de split)
- Antecedentes: [ADR 0072](./0072-rls-policies.md) (RLS por tenant), [ADR 0089](./0089-message-retention.md) (LGPD erasure)
- Lentes: **DDD-lite**, **Secure-by-default**, **Reversibility**, **Defense in depth**, **Observability**

## Context

Fase 2 ([SIN-62194](/SIN/issues/SIN-62194)) ativa 4 canais (WhatsApp, IG,
Messenger, Webchat). AC #2: "mesmo telefone em WhatsApp e Webchat = um
contato (merge automático)". A migration 0092 já criou `identity`
(raiz por tenant, `merged_into_id` self-FK), `contact_identity_link`
(`UNIQUE(contact_id)`, `link_reason ∈ {phone,email,external_id,manual}`)
e backfill 1:1.

Faltam três decisões antes de F2-06 desenhar o porto
`IdentityRepository`: quais sinais disparam merge automático, quando o
merge exige confirmação humana, e como reverter um merge errado.

## Decision

### D1 — Sinais e seu peso

| Sinal       | Normalização                                | Vínculo      | Auto-merge? |
|-------------|---------------------------------------------|--------------|-------------|
| `phone`     | E.164; reject se não normalizável            | `'phone'`    | sim (D3)    |
| `email`     | `ToLower(TrimSpace(x))`                     | `'email'`    | sim (D3)    |
| `(channel, external_id)` | par opaco já UNIQUE em `contact_channel_identity` | `'external_id'` | nunca cross-channel |
| `manual`    | operação humana via UI                       | `'manual'`   | n/a         |

`(channel, external_id)` por si só **não atravessa canais** (um PSID
Messenger nunca colide com um `wa_id` WhatsApp). É só garantia de
estabilidade dentro do canal.

`display_name` **não é sinal de merge** — colisões frequentes
demais. Reservado para "sugerir unificação" futura com aprovação manual.

### D2 — Resolver: ordem fixa

`IdentityRepository.Resolve(ctx, tenantID, channel, externalID,
phoneE164*, emailLower*) → *Identity`:

1. **`(channel, external_id)` exato** → devolve identity do contact
   correspondente (hot path; sem merge).
2. **`phone` match** → devolve identity do contact que carrega o telefone
   (em `contact_channel_identity` do canal WhatsApp ou em campos opcionais
   do webchat — Fase 2 não adiciona coluna `email`/`phone` em `contact`;
   isso é Fase 2.5 / SIN-62195).
3. **`email` match** → análogo a phone.
4. **Falha** → cria identity + contact + links na mesma transação.

`Resolve` é idempotente por sinal: dois inbounds concorrentes do mesmo
`(channel, external_id)` produzem a mesma identity (UNIQUE global em
`contact_channel_identity` + `SELECT … FOR UPDATE` na transação
serializam).

Hot path #1 é puro lookup; #2/#3 abrem transação com `FOR UPDATE` no
mesmo `tenant_id` para evitar race com outro resolver tocando os
mesmos contatos.

### D3 — Auto-merge vs `MergeProposal`

Dois contatos colidem quando um sinal forte (phone/email) liga duas
identities distintas no mesmo tenant. Regras:

- **Nenhuma das duas tem `conversation` com líder atribuído (via
  `assignment_history` ou `assignment`)** → **auto-merge**. Alvo =
  identity mais antiga; source = mais nova. `source.merged_into_id =
  target.id`; `contact_identity_link.identity_id` da source repontado
  para target via UPDATE.
- **Qualquer das duas tem líder** → cria `merge_proposal(tenant_id,
  source_id, target_id, reason, created_at, resolved_at, resolution)`
  (nova tabela em F2-06). Bloqueia auto-merge; UI de F2-13 expõe
  proposta. Operador escolhe: confirmar, manter separados, ou
  re-rotear a conversation antes de mergear.

Heurística "líder atribuído = veto" erra para o lado seguro:
embaralhar o owner de um lead é o incidente #1 de CRM. Custo: tempo
operacional. Reavaliar quando tivermos dados de F2-07 + F2-13 em
produção.

### D4 — Split (reversão)

`Split(ctx, tenantID, linkID, reason)`:

1. Cria identity nova (`merged_into_id = NULL`).
2. UPDATE no `contact_identity_link.identity_id` apontando para a nova.
3. Identity original mantém `merged_into_id` (histórico preservado).
4. Registra em `merge_proposal` com `resolution='split'`.

Conversations e `assignment_history` **não migram** — ficam onde estavam
no merge. Split é per-link (3 contatos mergeados = 2 splits). Verbose,
mas óbvio.

### D5 — Feature flag, locking, auditoria

`feature.identity_merge.enabled` per-tenant (default `true` após F2-06
land); kill switch, não undo. `Merge` abre `SELECT … FOR UPDATE`
ordenado por id (anti-deadlock). Auditoria master-side via
`master_ops_audit_trigger` (já em 0092).

## Métricas

`identity_merge_total{tenant_id, mode ∈ {auto, proposal_created,
manual_confirmed, split}}`; `identity_resolve_duration_seconds` (budget
p95 < 20ms no hot path D2#1); `identity_merge_proposal_open_total`
(alerta se > 50 abertas por > 24h).

## Alternativas consideradas

- **Auto-merge sempre.** Rejeitada: muda owner de leads sem confirmação;
  causa #1 de "perdi meu lead" em CRMs.
- **Sempre `MergeProposal`.** Rejeitada: AC #2 exige merge automático no
  caso comum (visitante anônimo recém-chegado). Pedir UI para cada
  novo contato é caro.
- **Merge por `display_name`.** Rejeitada: falsos positivos > falsos
  negativos. Vira feature manual futura.
- **`contact_channel_identity` UNIQUE já basta.** Rejeitada: a UNIQUE
  agrupa só *dentro* do canal. Sem camada `identity` em cima, não há
  porta cross-canal.

## Consequences

**Positivas.** Inbound multi-canal do mesmo humano = uma identity. Veto
por líder impede embaralhamento silencioso. Split per-link reversível.
Tudo auditável.

**Negativas.** `MergeProposal` é fila operacional — sem UI (até F2-13),
veto degrada para "duas identities separadas até alguém revisar".
`identity` cresce monotonicamente (1.5–2x contacts ao longo do tenant);
ADR 0089 reduz quando aplicável.

**Risco residual.** Phone normalização errada → match incorreto
(mitigação: `nyaruka/phonenumbers` com região default `BR`, tabela de
testes E.164). Operador mergeia errado → `Split` recupera; auditoria
permanece.

## Rollback

- Flag-off no tenant afetado congela operações; existentes permanecem.
- Revert do porto `IdentityRepository` no router é feature-flag flip.
- Heurística D3 errada → amendment ADR, sem migration (decisão vive no
  domínio).

## Out of scope

- Email/phone first-class em `contact` (Fase 2.5 / SIN-62195).
- Fuzzy match por nome (feature manual futura).
- Cross-tenant identity (RLS de ADR 0072 garante isolamento).
- Re-rotear conversation no merge (F2-07 / UI).
- Bulk merge / importação CSV.
