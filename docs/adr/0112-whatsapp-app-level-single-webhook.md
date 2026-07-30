# ADR 0112 — WhatsApp Cloud API: webhook único app-level, roteamento multi-tenant por `phone_number_id`

- Status: Accepted
- Date: 2026-07-30
- Drives: [SIN-68299](/SIN/issues/SIN-68299) (este ADR + handshake de go-live), origem em [SIN-67470](/SIN/issues/SIN-67470) (pergunta do board), parent [SIN-67138](/SIN/issues/SIN-67138) (WhatsApp API oficial)
- Ratifica implementação existente: [SIN-62731](/SIN/issues/SIN-62731) (intake), [SIN-62762](/SIN/issues/SIN-62762)/[SIN-62763](/SIN/issues/SIN-62763)/[SIN-62768](/SIN/issues/SIN-62768) (métricas/status/timestamp)
- Lentes aplicadas: Secure-by-default API, OWASP Top 10 (A01 broken access control, A10 SSRF/abuse), Least privilege, Defense in depth, Boring technology, Hexagonal / Ports & Adapters
- Relacionados: ADR 0075 (channel registry `channel`+`association`), ADR 0087 §D3 (idempotência inbound), ADR 0094 (janela de timestamp / replay), ADR 0109 (per-channel authz)

---

## Contexto

O board perguntou em [SIN-67470](/SIN/issues/SIN-67470) se não seria melhor "um webhook único
para todos os nossos usuários" em vez de um webhook por tenant. Esta ADR registra a decisão de
design e confirma que a plataforma **já opera exatamente assim** — não há rework pendente.

A pergunta tem uma resposta imposta pela própria Meta Cloud API, não uma escolha de arquitetura
nossa: **a Meta permite UMA única callback URL por app Meta.** Todas as WABAs (WhatsApp Business
Accounts) assinadas àquele app entregam eventos ao mesmo endpoint HTTP. Não existe, no modelo da
Meta, um webhook por número ou por tenant. Portanto "webhook por tenant" nunca foi uma opção
disponível — a plataforma tem de multiplexar.

---

## Decisões

### D1 — Um único endpoint público `/webhooks/whatsapp` (app-level)

**Decisão:** o CRM expõe exatamente um endpoint no listener público —
`GET /webhooks/whatsapp` (handshake de subscrição) + `POST /webhooks/whatsapp` (intake de
eventos). Registrado em `cmd/server/whatsapp_wire.go` (`assembleWhatsAppAdapter` → `adapter.Register(mux)`);
o handler está em `internal/adapter/channels/whatsapp/handler.go` / `challenge.go`.

**Rationale:** é a única topologia que a Meta suporta (uma callback URL por app). Um endpoint
único também minimiza a superfície pública (uma rota a proteger, um secret a rotacionar) —
Least privilege + Defense in depth.

### D2 — Roteamento multi-tenant por `phone_number_id` no payload

**Decisão:** o fan-out para o tenant correto é feito lendo
`entry[].changes[].value.metadata.phone_number_id` do envelope e resolvendo-o para o UUID do
tenant. A resolução vive atrás da porta `TenantResolver`
(`internal/adapter/channels/whatsapp/ports.go`), implementada por `pgTenantResolver` sobre a
tabela `tenant_channel_associations` via `ChannelAssociationLookup` (ADR 0075: `channel="whatsapp"`,
`association=<phone_number_id>` → `tenant UUID`). Ver `handler.go:237` (`change.Value.Metadata.PhoneNumberID`)
→ `handler.go:243` (`a.tenants.Resolve`).

**Rationale (Hexagonal):** o núcleo do handler não conhece SQL — depende só da porta
`TenantResolver`. A tradução do sentinel Postgres (`ErrAssociationUnknown`) para o erro de
domínio (`ErrUnknownPhoneNumberID`) acontece na composition root (`whatsapp_wire.go:162-168`),
mantendo a direção de dependência correta. Um `phone_number_id` sem associação é **drop
silencioso com 200** (`handler.go` log `whatsapp.unknown_phone_number_id`) — a Meta não
re-tenta e não vazamos existência de tenants.

### D3 — Segurança do modelo single-app

**Decisão:** por ser um app único, os segredos são únicos por app (não por tenant):

- **Autenticidade do POST:** HMAC-SHA256 no header `X-Hub-Signature-256`, verificado contra
  `META_APP_SECRET` com comparação em tempo constante — `handler.go:134`
  (`metashared.VerifySignature`). Corpo não-assinado ou assinatura inválida → rejeitado antes
  de qualquer parse/roteamento (Fail securely, A01/A08).
- **Handshake GET:** `hub.verify_token` comparado contra `META_VERIFY_TOKEN` com
  `subtle.ConstantTimeCompare`; mismatch → 403; `hub.mode != subscribe` → 400 (surface o erro
  no painel Meta durante setup) — `challenge.go`.
- **Anti-abuse:** rate-limit por `phone_number_id` aplicado **antes** de resolver o tenant
  (`handler.go:413`), de modo que um flood de `phone_number_id` desconhecidos não pode fazer
  DoS na resolução nem no Postgres (A10 / Defense in depth).
- **Idempotência:** dedup por `wamid` dentro da transação (`inbound_message_dedup`, ADR 0087 §D3);
  replays da Meta batem no UNIQUE e retornam 200 sem duplicar Message.
- **Janela de replay:** timestamps fora da janela de 24h (ADR 0094) são descartados e contados.

**Rationale:** o secret é do app, não do tenant, então comprometê-lo afeta todos os tenants —
por isso ele nunca é logado nem serializado além da composition root (`config.go` comentário de
invariante), e a rotação é um procedimento operacional único (runbook de secrets, ADR 0105).

### D4 — Fail-soft na inicialização

**Decisão:** se `META_APP_SECRET`, `META_VERIFY_TOKEN`, `DATABASE_URL` ou `REDIS_URL` faltarem,
ou se `FEATURE_WHATSAPP_ENABLED != "1"`, o wire loga uma linha `disabled` e retorna `nil` — o
listener público sobe **sem** as rotas WhatsApp, em vez de crashar (`whatsapp_wire.go:58-96`).
Habilitação por tenant é sobreposta por `FEATURE_WHATSAPP_TENANTS` (allowlist de UUIDs).

---

## Alternativas consideradas

1. **Webhook por tenant (a pergunta do board).** *Impossível* no modelo Meta — uma callback URL
   por app. Só seria viável com um app Meta por tenant, o que multiplica custo operacional
   (um App Review, um secret, uma verificação por tenant) sem benefício: o roteamento por
   `phone_number_id` já dá isolamento por tenant no mesmo endpoint. Rejeitada.
2. **Um app Meta por tenant + N endpoints.** Escala linearmente em toil operacional e
   superfície de secrets; contraria Least privilege e Boring technology. Reservado apenas para
   um eventual requisito de isolamento regulatório forte (não é o caso hoje). Rejeitada.
3. **Roteamento por path/subdomínio (`/webhooks/whatsapp/<tenant>`).** A Meta entrega tudo na
   URL configurada; ela não anexa o tenant ao path. Teríamos de derivar o tenant do payload de
   qualquer modo — o path seria decorativo e mais uma coisa a validar. Rejeitada.

---

## Consequências

- **Positivas:** uma rota, um par de secrets, um ponto de rotação; isolamento multi-tenant por
  dado do payload; superfície pública mínima; alinhado ao que a Meta suporta nativamente.
- **Custo:** a tabela `tenant_channel_associations` é o ponto único de verdade do roteamento —
  um `phone_number_id` não semeado = mensagens dropadas silenciosamente (por design, mas exige
  disciplina de seeding: ver checklist §Go-live). O `META_APP_SECRET` é blast-radius global.
- **Observabilidade:** `whatsapp_handler_elapsed_seconds`, `whatsapp_status_total`,
  `whatsapp_status_lag_seconds`, `webhook_timestamp_window_drop_total` no `/metrics`
  (runbook `docs/runbooks/whatsapp-inbound-latency.md`). Logs incluem `phone_number_id`
  (identificador Meta, não PII de cliente) mas nunca o corpo nem os secrets.

---

## Rollback

Setar `FEATURE_WHATSAPP_ENABLED=0` (ou remover `META_APP_SECRET`) desmonta a rota no próximo
boot sem migração — o listener sobe sem as rotas WhatsApp (D4). Nenhum schema muda; nenhum passo
de migração é necessário para desligar. Reversível em um restart.

---

## Checklist de handshake de go-live

Owner do handshake: CTO (verificação). Seeding das associações é delegável a engenharia se
virar migration/seed. Ordem:

1. **Boot.** Subir instância com `META_APP_SECRET` + `META_VERIFY_TOKEN` (+ `DATABASE_URL`,
   `REDIS_URL`, `FEATURE_WHATSAPP_ENABLED=1`). Confirmar o log
   `crm: whatsapp intake mounted on public listener`. (Ausência = alguma dep faltando; a linha
   `disabled — <motivo>` diz qual.)
2. **Painel Meta.** Configurar callback URL `https://<host>/webhooks/whatsapp` + o mesmo
   `META_VERIFY_TOKEN`. Confirmar que a Meta recebe **200** no GET challenge (mode=subscribe).
3. **Assinatura de campos.** Assinar o app à(s) WABA(s) e ao campo `messages` (e `message_status`
   se quiser telemetria de entrega/leitura — o adapter já reconcilia `statuses[]`).
4. **Seeding.** Popular `tenant_channel_associations` (channel=`whatsapp`,
   association=`<phone_number_id>` → tenant UUID) para cada número. Sem esta linha o inbound é
   dropado silenciosamente (D2). Habilitar o tenant via `FEATURE_WHATSAPP_TENANTS` se a
   allowlist estiver em uso.
5. **E2E inbound.** Enviar mensagem real → o endpoint responde **200** → a conversa aparece no
   inbox do tenant correto. Verificar `whatsapp_handler_elapsed_seconds` incrementando e nenhum
   `whatsapp.unknown_phone_number_id` no log para o número testado.

---

## Out of scope

- Canal WhatsApp **não-oficial** (whatsmeow / WhatsApp Web) — ADR 0107. Esta ADR cobre só o
  canal oficial Meta Cloud API.
- Outbound / templates HSM e billing de conversas Meta — fora do escopo do intake.
- Feature-flag DB-backed (hoje env-based `EnvFeatureFlag`) — follow-up já anotado em `config.go`.
