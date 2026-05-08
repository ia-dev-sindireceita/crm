# ADR 0086 — Política de inadimplência (dunning escalonado)

- Status: accepted
- Date: 2026-05-08
- Deciders: Board (CEO/Pericles), CTO
- Tickets: [SIN-62204](/SIN/issues/SIN-62204) (decisão D1), plan rev 2
  [SIN-62190](/SIN/issues/SIN-62190#document-plan), bloqueia
  [SIN-62197](/SIN/issues/SIN-62197) (Fase 4 — Cobrança PIX nativa)

## Contexto

Fase 4 introduz cobrança PIX nativa: cada tenant tem uma assinatura mensal
(plano + pacotes avulsos) faturada via PIX. Quando o pagamento de uma fatura
não compensa até o vencimento o tenant entra em estado `past_due` e o board
precisa de uma política técnica explícita para esse estado.

Duas alternativas foram apresentadas pelo CTO em [SIN-62204](/SIN/issues/SIN-62204):

- **(a) Bloqueio escalonado:** banner de aviso a partir do D+1, suspensão de
  envio outbound (mas mantém recepção) a partir do D+7, suspensão total
  (read-only) a partir do D+30.
- **(b) Apenas alerta:** banner permanente + email diário, sem qualquer
  suspensão técnica.

Trade-off: bloquear protege COGS (cada mensagem outbound consome canais e
LLM tokens, ambos pagos), mas pode prejudicar a operação do cliente final do
nosso cliente. Não bloquear é cliente-friendly mas vira inadimplência crônica
e alimenta abuso (tenant fica `past_due` indefinidamente consumindo nosso
saldo de canais).

## Decisão

Adotamos **opção (a) — bloqueio escalonado**, com gatilhos configuráveis por
plano e overrides explícitos via cortesia administrativa.

### Estados e gatilhos

A assinatura tem um campo `subscription.status` cujos valores relevantes para
dunning são:

- `active` — em dia, sem efeito visível.
- `past_due` — fatura vencida há ≥ 1 dia. Subdividido nas três bandas abaixo.
- `suspended` — read-only total (D+30+).
- `cancelled` — encerrado (mantém leitura por janela de retenção definida em
  ADR separada de retenção/LGPD).

Bandas de `past_due` (default; configuráveis por plano em `plans.dunning`):

| Banda           | A partir de | Efeito técnico                                           | Efeito UI                                |
| --------------- | ----------- | -------------------------------------------------------- | ---------------------------------------- |
| `past_due_warn` | D+1         | Nenhum — todas as operações continuam.                   | Banner amarelo no topo + email diário.   |
| `past_due_outbound_blocked` | D+7  | Outbound (envio para WhatsApp/email/SMS) suspenso.      | Banner laranja + email diário + bloqueio explícito ao tentar enviar campanha/mensagem. |
| `past_due_readonly` | D+30   | Suspensão total: outbound bloqueado, escrita em recursos críticos bloqueada (criar/editar campanhas, mudar pipeline, etc.). Leitura mantida. | Banner vermelho + email diário + lock visível no dashboard.                |

"Outbound" significa qualquer egress que custe COGS variável: mensagens em
canais terceirizados, chamadas LLM disparadas por automação, e qualquer
webhook saindo do tenant. Inbound (recebimento de mensagens, webhooks que
chegam) **continua funcionando** em todas as bandas até `cancelled` — não
queremos perder dados do cliente final do nosso cliente por inadimplência.

### Gatilho administrativo (CourtesyGrant)

Master pode estender prazo / pular bandas via
`CourtesyGrant.kind=free_subscription_period` apontando para o
`subscription.id` com `validUntil` explícito. Enquanto o grant está ativo a
assinatura é tratada como `active` para fins de dunning, independentemente do
status financeiro real da fatura. O grant é auditável (autor, motivo,
duração) e tem TTL — não é um bypass permanente.

### Reativação

Quando uma fatura `past_due` é compensada (PIX confirmado), a assinatura
volta para `active` no mesmo job que processa o webhook do PSP. Bandas
intermediárias são limpas atomicamente — não há "sai do D+30 e fica preso no
D+7". O job é idempotente: re-processar o mesmo webhook não muda o estado.

### Configurabilidade por plano

`plans.dunning` é JSON na tabela `plans` com este shape:

```json
{
  "warn_days": 1,
  "outbound_block_days": 7,
  "readonly_days": 30,
  "cancel_days": 90
}
```

Default global é `{1, 7, 30, 90}`. Planos enterprise podem aumentar prazos
(p.ex. `{3, 14, 45, 120}`); planos free-trial podem encurtar. Mudar essa
configuração não afeta tenants já em `past_due` — a banda em que o tenant
entrou é congelada no momento da transição (`subscription.dunning_snapshot`).

## Consequências

Positivas:

- COGS variável (canais + LLM) tem teto de 7 dias por tenant inadimplente — o
  bloqueio outbound zera o gasto principal antes que vire dívida significativa.
- Cliente final do nosso cliente continua recebendo dados (inbound) e podendo
  consultar histórico mesmo no D+30; a degradação é só na escrita/envio.
- Política é configurável: não amarra o time comercial a um SLA único.
- Master tem ferramenta auditável (`CourtesyGrant`) para casos legítimos de
  atraso de pagamento (cliente importante, problema bancário documentado).

Negativas / custos:

- Implementação não-trivial: precisa job de scan diário (cron), tabela de
  bandas, hooks no caminho de envio (`outbound_gateway` checa `subscription.status`
  antes de despachar), banners no frontend (HTMX server-side render baseado no
  contexto de sessão), email diário com template por banda.
- Cliente final pode reagir mal a uma campanha que parou de sair "do nada" no
  D+7 — UX precisa ser explícita ("seu cliente está com fatura em aberto há 7 dias,
  contato para regularizar"). Risco de reputação se o aviso D+1/D+7 não for
  visível o suficiente.
- Operação master fica responsável por atender chamados de tenants que querem
  destravar antes do D+7. Mitigar com auto-serviço (botão "renegociar" que
  gera 2ª via de cobrança PIX) na própria UI.

## Alternativas consideradas

- **Opção (b) — apenas alerta.** Rejeitada: COGS variável de mensagens e LLM
  cresce linearmente com volume; sem gatilho técnico, um único tenant
  inadimplente alto-volume sangra a margem do plano todo.
- **Bloqueio binário (D+1 já corta tudo).** Rejeitada por ser hostil ao
  cliente final e gerar churn imediato. O banner de 6 dias de janela para
  regularizar é suficiente para diferenciar atraso de inadimplência real.
- **Pré-pagamento por créditos consumíveis.** Considerada para Fase 4+; muda o
  modelo comercial e está fora do escopo desta decisão. Pode ser revisitada
  na decisão D4 (pacotes avulsos).

## Verificação (regressão)

Quando a implementação for criada (issues filhas de [SIN-62197](/SIN/issues/SIN-62197)):

1. Teste de domínio (unit) na regra de transição entre bandas: dado um
   `subscription.past_due_since` e `plans.dunning`, retornar a banda
   correta. Cobrir D-1, D+0, D+1, D+6, D+7, D+29, D+30, D+89, D+90.
2. Teste de domínio para `CourtesyGrant`: enquanto há grant ativo, a função
   de banda retorna `active` independente do status financeiro.
3. Teste de adapter no gateway de outbound: dado um tenant em
   `past_due_outbound_blocked`, qualquer `Send()` retorna
   `ErrSubscriptionOutboundBlocked` sem chamar o canal externo.
4. Teste de integração end-to-end: webhook PSP de pagamento confirmado em um
   tenant em `past_due_readonly` move o status para `active` e libera
   outbound + escrita em uma única transação.

## Out of scope (decisões separadas)

- **D2** (PSP de PIX) — [SIN-62205](/SIN/issues/SIN-62205).
- **D4** (custo dos pacotes avulsos de tokens) — [SIN-62207](/SIN/issues/SIN-62207).
- Janela de retenção de dados após `cancelled` — ADR LGPD separada.
- Cobrança automática multi-tentativa (retry policy do PSP) — issue
  específica dentro de Fase 4, depende da escolha do PSP em D2.

## Rollback

A política é configurada por plano em `plans.dunning`. Para suspender
temporariamente o bloqueio (incidente, falha do PSP, fim de mês comercial)
basta atualizar todos os planos com `outbound_block_days` e `readonly_days`
muito altos (p.ex. 9999) — efeito imediato, sem deploy. A flag pode ser
exposta como toggle global no painel master para casos emergenciais.
