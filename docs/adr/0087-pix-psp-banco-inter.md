# ADR 0087 — PSP de cobrança PIX (Banco Inter API)

- Status: accepted
- Date: 2026-05-09
- Deciders: Board (CEO/Pericles), CTO
- Tickets: [SIN-62205](/SIN/issues/SIN-62205) (decisão D2), plan rev 3
  [SIN-62190](/SIN/issues/SIN-62190#document-plan), bloqueia
  [SIN-62197](/SIN/issues/SIN-62197) (Fase 4 — Cobrança PIX nativa)

## Contexto

A Fase 4 do roadmap introduz cobrança PIX nativa: cada tenant tem assinatura
mensal (plano + pacotes avulsos) faturada via PIX, com webhook de conciliação
fechando a fatura quando o pagamento compensa. Antes de implementar é preciso
escolher o **PSP (Payment Service Provider)** que ficará por trás do port
`PixCharger` (definido no plan §6, em `adapters/pix/`).

A decisão D2 do plan rev 2/3 ([SIN-62190#document-plan](/SIN/issues/SIN-62190#document-plan))
pré-selecionou duas alternativas equivalentes do ponto de vista de API:

- **Asaas** — API REST ampla, sandbox bom, onboarding rápido (~1 dia para PJ
  cadastrada). Custo por cobrança em torno de R$ 1,99 por PIX dependendo do
  plano contratado.
- **Banco Inter API** — sem custo por cobrança PIX (PJ Inter já existente), em
  troca de onboarding mais formal (abertura/uso de conta PJ no Inter,
  habilitação da API com chaves, certificado mTLS).

O critério prioritário a fixar era **custo vs onboarding**:

- (a) custo zero por cobrança vence → Banco Inter
- (b) onboarding rápido vence → Asaas

O board aceitou a interaction `request_confirmation`
[`de74901f`](/SIN/issues/SIN-62205) em 2026-05-09 fixando o critério (a) —
**custo zero por cobrança**.

## Decisão

Adotamos **Banco Inter API** como PSP de PIX para a Fase 4 e seguintes,
acessada exclusivamente atrás do port `PixCharger` em
`internal/billing` / `adapters/pix/banco_inter/`.

### Escopo do adapter

O adapter `adapters/pix/banco_inter/` implementa o port `PixCharger` com no
mínimo as seguintes operações (assinaturas finais a serem confirmadas no
design da Fase 4):

- `CreateCharge(ctx, ChargeRequest) (Charge, error)` — emite cobrança PIX com
  valor, vencimento, `txid` idempotente e identificação do tenant/fatura. Retorna
  `qr_code_payload`, `qr_code_image_ref`, `expires_at`, `psp_charge_id`.
- `GetCharge(ctx, txid) (Charge, error)` — consulta estado atual (idempotente,
  usado por reconciliação).
- `RefundCharge(ctx, txid, RefundRequest) (Refund, error)` — opcional na Fase 4
  inicial; entra quando dunning + master grants exigirem estorno explícito.
- Webhook handler — recebe notificação assinada do Inter, valida origem, atualiza
  `PIXCharge.status` e dispara o caminho de conciliação descrito no
  ADR 0086 (dunning).

O domain core (`internal/billing`) **não importa** o cliente Inter — toda a
dependência fica isolada no adapter. Trocar o PSP não exige alterar regra de
negócio (ver §"Reversibilidade").

### Autenticação e segredos

O Inter exige autenticação OAuth2 client-credentials com **certificado mTLS**.
Os segredos vivem em variáveis de ambiente carregadas pelo `platform/config`:

- `PIX_INTER_CLIENT_ID`
- `PIX_INTER_CLIENT_SECRET`
- `PIX_INTER_CERT_PATH` (PEM cliente)
- `PIX_INTER_KEY_PATH` (PEM chave)
- `PIX_INTER_WEBHOOK_SECRET` (assinatura HMAC dos webhooks, se suportada;
  caso contrário lista de IPs allow-list documentada na config)

Nada disso pode aparecer em log, URL ou mensagem de erro propagada ao
tenant. O middleware de redaction global (definido em `platform/logging`)
deve cobrir essas chaves.

### Reversibilidade

O port `PixCharger` permanece swappable por design (decisões #5/#6 do plan).
Trocar para Asaas (ou outro PSP brasileiro com API REST + webhook) exige:

1. Implementar novo adapter `adapters/pix/<psp>/` contra o mesmo port.
2. Trocar a injeção em `cmd/server` (uma linha de wiring).
3. Migração de dados: nenhuma para cobranças novas; cobranças `pending` em
   aberto continuam liquidando contra Inter até expirarem (`expires_at`),
   evitando dual-write durante o cutover.
4. Uma flag de config (`pix_provider`) pode coexistir por uma janela curta
   se quisermos testar o novo PSP em produção com tráfego mínimo antes do
   cutover.

Esse caminho de saída é parte da avaliação que justificou aceitar o
onboarding mais lento do Inter em troca do custo zero — quando o volume
cair fora da curva favorável (ver §"Quando reavaliar"), a troca é
operacionalmente barata.

## Consequências

Positivas:

- **Custo marginal de cobrança ≈ zero.** Em volume alto (Fase 6+) Asaas
  custaria 1,99 × N cobranças mensais, o que cresce linear com a base de
  tenants e come margem de planos baixos. Inter remove esse vetor de COGS.
- **Conta PJ no Inter já permite saída de fluxo** (recebe direto na conta
  operacional, sem PSP intermediário), simplificando reconciliação contábil.
- **API documentada e estável** com sandbox e webhook assinado, atendendo
  os critérios técnicos do plan (REST, sandbox, webhook, suporte pt-BR).
- **Sem lock-in via dependência única.** Port + adapter mantém Asaas como
  plano B realista (~1 sprint de implementação se preciso).

Negativas / custos:

- **Onboarding mais lento.** Habilitar a API Inter exige PJ cadastrada,
  emissão de certificado mTLS no internet banking PJ, e ativação manual da
  cobrança PIX por API. Estimativa: 3–7 dias úteis de calendário, contra
  ~1 dia da Asaas. Impacta data de início real da Fase 4 e precisa entrar
  no cronograma.
- **mTLS no client.** Operacionalmente é uma dependência a mais (rotação
  de certificado, monitoramento de expiração). Mitigação: alarme em
  `cert.expires_at - 30d` na pipeline de observabilidade (slog/OTel já
  prontos pelo SIN-62218).
- **Banco único como vendor.** Se o Inter tiver indisponibilidade
  prolongada da API (já houve incidentes públicos no setor), cobrança
  trava. Mitigação: detecção via SLO no PSP, fallback manual de QR Code
  estático com `txid` correlacionável para conciliação tardia, e plano B
  documentado de cutover para Asaas (port mantém isso barato).
- **Curva de aprendizado.** Documentação Inter é boa mas mais "bancária"
  que de PSP-puro como Asaas. Esperar 1–2 dias de ramp-up do engenheiro
  designado para a integração.

## Alternativas consideradas

- **Asaas.** Rejeitada como PSP inicial pelo critério de custo. Mantida
  como plano B realista (mesmo port `PixCharger`); pode ser reativada se
  Inter falhar nos critérios operacionais ou se a Fase 6+ mostrar que a
  dor de onboarding bancário recorrente não justifica a economia.
- **Pagar.me / Mercado Pago / Cora / Banco do Brasil API.** Pré-listadas
  no plan §11. Não foram pré-selecionadas pelo CTO porque ou (i) custo por
  cobrança comparável a Asaas sem a vantagem de onboarding (Pagar.me, MP),
  ou (ii) onboarding tão formal quanto Inter sem a vantagem de custo zero
  (Cora, BB) na faixa de volume da Fase 4 inicial. Podem ser revisitadas
  via novo ADR caso a comparação mude.
- **Multi-PSP simultâneo (roteamento por preço/disponibilidade).**
  Rejeitada para Fase 4: dobra superfície de bugs de webhook + reconciliação
  para resolver problema que não temos. Pode entrar como decisão separada
  se uma indisponibilidade real de PSP único justificar.

## Quando reavaliar

Reabrir esta decisão por novo ADR se algum gatilho disparar:

- **Volume:** > 5.000 cobranças/mês começa a justificar absolver custo
  Asaas em troca de onboarding sem fricção bancária — só vale a pena se a
  fricção do Inter virar problema operacional repetido.
- **Disponibilidade:** ≥ 2 incidentes de API Inter > 4h em janela de 90
  dias com impacto em conciliação.
- **Mudança contratual:** se Inter introduzir cobrança por PIX corporativo
  na faixa que usamos, custo deixa de ser zero e premissa cai.
- **Mudança comercial:** se entrarmos em segmento self-serve onde o tenant
  faz onboarding em minutos (vs dias úteis hoje), o overhead bancário do
  Inter precisa ser revisitado.

## Verificação (regressão)

Quando a implementação for criada (issues filhas de [SIN-62197](/SIN/issues/SIN-62197)):

1. **Domínio (unit, sem PSP).** Regras de transição de estado de
   `PIXCharge` (`pending` → `paid`/`expired`/`refunded`) são puras e
   testadas contra um fake `PixCharger` em memória.
2. **Adapter `banco_inter` (integração contra sandbox).** Testes rodam
   contra a sandbox Inter com credenciais de teste. Cobrir: criar
   cobrança válida, criar cobrança inválida (4xx), consultar cobrança,
   webhook assinado válido, webhook assinado inválido.
3. **Webhook end-to-end.** Webhook simulado dispara reconciliação
   atomicamente: `PIXCharge.status=paid` + `Invoice.paid_at` + saída de
   `subscription.past_due` (gancho com ADR 0086) em uma transação
   idempotente. Re-enviar o mesmo webhook não move estado.
4. **Falha do PSP (resilience).** Adapter retorna erro mapeado de timeout/
   5xx do Inter sem vazar detalhes do banco; cliente HTTP tem timeout
   curto, retry exponencial limitado, e métrica `pix_psp_errors_total`
   exposta para alerta.

## Out of scope (decisões separadas)

- **D1** (política de inadimplência) — resolvida em ADR 0086
  ([SIN-62204](/SIN/issues/SIN-62204)).
- **D4** (preço dos pacotes avulsos de tokens) — [SIN-62207](/SIN/issues/SIN-62207).
- Política de retry de cobrança automática multi-tentativa — issue
  específica dentro de Fase 4, depende deste ADR.
- DPA + LGPD do Inter como sub-processador — checklist de compliance
  dentro de Fase 4; não faz parte deste ADR.

## Rollback

Como descrito em §"Reversibilidade": trocar o PSP é alterar a injeção em
`cmd/server` para um adapter alternativo já testado. O port `PixCharger`
não muda. Cobranças `pending` em aberto continuam fluindo no PSP antigo
até expirarem; nenhuma migração de dados é necessária. O config flag
`pix_provider` pode coexistir durante o cutover para liberar tráfego
gradual.
