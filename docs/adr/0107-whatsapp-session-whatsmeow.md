# ADR 0107 — WhatsApp Session (whatsmeow): arquitetura, licença MPL-2.0, sessão por tenant

- Status: Accepted
- Date: 2026-06-29
- Drives: [SIN-66255](/SIN/issues/SIN-66255) (este ADR), [SIN-66252](/SIN/issues/SIN-66252) (plano rev 4)
- Lentes aplicadas: Hexagonal / Ports & Adapters, Boring technology, Secure-by-default, Least privilege

---

## Contexto

O board aprovou (2026-06-29) a adição de um canal WhatsApp não-oficial baseado em WhatsApp Web
(emulação de cliente móvel), coexistindo com o canal oficial Meta Cloud API já existente
(`internal/adapter/channels/whatsapp`). A decisão consta no plano rev 4 de
[SIN-66252](/SIN/issues/SIN-66252), Opção A: lib Go `whatsmeow` (`go.mau.fi/whatsmeow`), sem
sidecar Node.js.

O aceite de risco de ToS/banimento foi registrado explicitamente pelo board em 2026-06-29.
Este ADR ratifica as decisões de arquitetura antes da implementação.

---

## Decisões

### D1 — Biblioteca: `whatsmeow` (Go nativo), sem sidecar Node

**Decisão:** `go.mau.fi/whatsmeow` é a implementação escolhida. Sidecar Node.js
(`@whiskeysockets/baileys`, `@open-wa/wa-automate`, etc.) é descartado como opção primária;
pode ser revisitado apenas se uma feature obrigatória (não inbox v1) estiver ausente na lib Go.

**Rationale:**

1. **Go nativo.** O CRM é uma base 100% Go. Introduzir um sidecar Node adiciona um segundo
   runtime, um segundo contêiner, inter-process communication (HTTP ou gRPC), e surface de
   supply-chain duplicada — custo desproporcional para um inbox.
2. **whatsmeow é o estado da arte em Go.** Mantida ativamente pelo time do Mautrix (pontes de
   Matrix), com SQLite/Postgres store embutido, multi-device QR pairing, e suporte a mensagens
   de texto, mídia, reações e status. Recursos não necessários ao inbox v1 (chamadas, broadcast
   lists) não justificam trocar de runtime.
3. **Boring-tech.** Uma dependência Go adicional ao `go.mod` é muito menos invasiva do que um
   novo runtime de processo separado.
4. **Reversibilidade.** O adapter fica atrás de um port hexagonal
   (`WhatsAppSessionSender`/`WhatsAppSessionReceiver`). Trocar de lib exige apenas um novo
   pacote de adaptador, sem tocar domínio.

**Feature gaps conhecidos em whatsmeow (não necessários ao inbox v1):**

| Feature | whatsmeow | Necessário inbox v1 |
|---|---|---|
| Mensagens de texto | ✅ | ✅ |
| Mídia (imagem, documento) | ✅ | ✅ |
| Reações | ✅ | opcional |
| Chamadas de voz/vídeo | ❌ | ❌ |
| Broadcast lists | ❌ | ❌ |
| Status/Stories | ❌ | ❌ |

---

### D2 — Licença: MPL-2.0, uso como dependência, não obriga abertura do CRM

**Decisão:** O uso de `go.mau.fi/whatsmeow` (MPL-2.0) como dependência compilada **não** aciona
a obrigação de abertura do código-fonte do CRM. A análise abaixo é a posição técnica desta ADR;
o jurídico do board validou o aceite de risco em 2026-06-29.

**Análise MPL-2.0:**

A Mozilla Public License 2.0 é "copyleft fraco por arquivo" (_file-level copyleft_):

- Você é obrigado a abrir apenas os **arquivos que contenham código MPL-2.0** e que você
  **modificar**. Não se propaga para o restante do projeto.
- **Uso como dependência binária compilada** (i.e., `go get` sem fork/modificação dos arquivos
  da lib) **não** exige abertura de código. Você distribui o binário compilado; a lib é
  incorporada mas não "modificada" no sentido da MPL.
- A MPL-2.0, seção 3.3, permite combinar arquivos MPL com "Incompatible Source Code" sob
  licença proprietária — desde que os arquivos MPL em si sejam distribuíveis separadamente. Em
  Go, o binário estático satisfaz isso porque o código-fonte da lib continua disponível no
  repositório público upstream.
- **Obrigação real:** se o time **modificar** arquivos de `go.mau.fi/whatsmeow` (forks, patches
  locais via `replace` no `go.mod`), esses arquivos modificados devem ser disponibilizados
  publicamente. A solução é contribuir upstream ou manter o fork público; nunca produzir patches
  privados de arquivos MPL.

**Regra operacional:** uso de `whatsmeow` via `go get` sem `replace` no `go.mod` é livre.
Qualquer `replace` apontando para fork privado deve ser bloqueado em code review.

---

### D3 — Modelo de sessão: uma sessão WhatsApp Web por tenant, store em Postgres

**Decisão:** Cada tenant que ativar o canal WhatsApp Session tem **uma sessão WhatsApp Web**
(multi-device pairing). As credenciais de sessão (chaves de identidade, tokens de sessão,
pre-keys) são persistidas no **Postgres** via o `whatsmeow` SQL store (`whatsmeow/store/sqlstore`),
em schema isolado por tenant (`tenant_id` em todas as linhas ou schema separado).

**Rationale:**

1. **`whatsmeow` tem store SQL embutido.** `sqlstore` suporta Postgres e SQLite; escolhemos
   Postgres porque o CRM já opera Postgres e adicionar SQLite por tenant seria inviável em produção.
2. **Isolamento por `tenant_id`.** O store `whatsmeow` usa a tabela `whatsmeow_device` +
   adjacentes. Ao instanciar `sqlstore.New(db, sqlstore.Postgres)` com um schema específico
   por tenant — ou com `tenant_id` como coluna discriminante — garantimos que uma credencial
   vazada de um tenant não compromete outro (Defense in depth).
3. **Uma sessão por tenant.** WhatsApp Web multi-device permite até 4 devices vinculados a um
   número. A sessão do CRM ocupa um slot. O operador do tenant pode vincular/desvincular via
   QR-code exposto no painel master/tenant. Múltiplas sessões por tenant (N números) são
   fora de escopo no v1.
4. **Ciclo de vida da sessão.** Banimento ou logout pelo WhatsApp invalida a sessão; o adapter
   deve detectar `events.LoggedOut` / `events.QRScannedWithoutMultiDevice` e sinalizar ao tenant
   via flag + notificação de re-autenticação (operacional, não criptográfica).
5. **Sem SQLite por processo.** SQLite ficaria em disco local do pod — não sobrevive a
   restart/migração de pod. Postgres é a única opção durável no contexto do CRM.

**Schema canônico (whatsmeow cria automaticamente via `sqlstore.Upgrade`):**

```
whatsmeow_device (jid, registration_id, noise_key, identity_key, signed_pre_key, ...)
whatsmeow_pre_keys (jid, key_id, key, uploaded)
whatsmeow_sessions (our_jid, their_jid, session)
whatsmeow_contacts (our_jid, their_jid, first_name, ...)
...
```

Discriminante de tenant: coluna `tenant_id UUID NOT NULL` adicionada via migration antes de
deixar o `sqlstore` fazer o `Upgrade`. Alternativa: schema Postgres por tenant
(`search_path=tenant_<uuid>`). Decisão final na ADR da migration (child de implementação).

---

### D4 — Coexistência com canal oficial Meta Cloud API: `channel="whatsapp"`, roteado por `provider`

**Decisão:** O canal é registrado com `channel = "whatsapp"` (mesma string semântica) e
distinguido por `provider = "meta"` (canal oficial) ou `provider = "session"` (whatsmeow).
**Não** criamos um tipo de canal `"whatsapp_session"` separado.

**Rationale:**

1. **Identidade semântica.** Para o atendente e para o contato, ambos são "WhatsApp". Criar
   dois tipos de canal distintos exigiria duplicar regras de negócio (políticas de retenção,
   templates, consentimento LGPD) que são idênticas para ambos.
2. **Roteamento por `provider`.** O `OutboundChannel` port (hexagonal) já recebe contexto do
   tenant; o factory de adapter lê `provider` da `tenant_channel_association` e instancia o
   adapter correto. Do ponto de vista do domínio, `WhatsAppSender` é o port — `MetaSender` e
   `SessionSender` são adapters intercambiáveis.
3. **Retrocompatibilidade.** Tenants já no canal oficial `whatsapp` com `provider=meta` não são
   afetados. A migration que adiciona a coluna `provider` tem `DEFAULT 'meta'` (não-rompente).
4. **Tabela `tenant_channel_associations` — campo novo:**

```sql
ALTER TABLE tenant_channel_associations
  ADD COLUMN IF NOT EXISTS provider TEXT NOT NULL DEFAULT 'meta'
  CHECK (provider IN ('meta', 'session'));
```

5. **Discriminação no inbound.** O webhook Meta continua no path `/webhook/whatsapp` (Meta assina
   HMAC). O whatsmeow recebe eventos push (não polling) dentro do processo. O handler de inbound
   distingue a origem pela rota ou pelo campo `provider` na `tenant_channel_association`.

---

### D5 — Processo: in-process no servidor Go (não worker isolado), com goroutine supervisionada

**Decisão:** O `whatsmeow` client roda **in-process** no servidor Go principal, em goroutine
supervisionada por `errgroup` / supervisor com retry-backoff. **Não** criamos um processo/pod
worker separado no v1.

**Rationale:**

1. **Simplicidade operacional.** Um worker separado exige: segundo Dockerfile, deploy pipeline,
   variáveis de ambiente duplicadas, service-discovery, IPC entre servidor e worker. Para v1
   (número de tenants pequeno), o custo supera o benefício.
2. **whatsmeow é thread-safe.** A lib foi projetada para múltiplas conexões concorrentes no
   mesmo processo. O CTO revisará o uso de goroutines para garantir ausência de leaks
   (lente Idiomatic Go).
3. **Limites de conexão.** Cada cliente whatsmeow abre uma conexão WebSocket persistente com
   os servidores WhatsApp. Para N tenants no v1 (esperado <50 na Fase 0), o impacto em
   descritores de arquivo e memória é desprezível. Ao ultrapassar ~200 tenants, o isolamento
   em worker separado deve ser reavaliado (threshold registrado como dívida técnica).
4. **Isolamento de credenciais.** O isolamento de credenciais entre tenants é garantido pelo
   modelo de store (D3), não pelo isolamento de processo. Cada `whatsmeow.Client` instanciado
   carrega apenas as chaves do seu tenant — não há memória compartilhada de credenciais.
5. **Blast radius de falha.** Uma sessão whatsmeow que panics pode derrubaria o processo
   inteiro se não tratada. Mitigação: `recover()` no supervisor por goroutine-tenant + circuit
   breaker para retry exponencial. Goroutine leak detectado por `goleak` nos testes.

**Diagrama de componentes (in-process):**

```
cmd/server
  └── WhatsAppSessionManager (goroutine supervisor)
        ├── tenant-A: whatsmeow.Client → Postgres store (tenant_id=A)
        ├── tenant-B: whatsmeow.Client → Postgres store (tenant_id=B)
        └── ...
              │ events (inbound messages)
              ▼
        inbox.MessageReceiver port (domínio)
              │
        OutboundChannel port (envio)
              ▼
        whatsmeow.Client.SendMessage
```

---

### D6 — Aceite formal de risco ToS / banimento

**Decisão:** O board registra formalmente, neste ADR, o aceite dos seguintes riscos:

1. **Violação dos Termos de Serviço do WhatsApp (Meta):** o uso de clientes não-oficiais
   (WhatsApp Web emulado) viola os ToS do WhatsApp. A Meta pode banir números de telefone
   dos tenants sem aviso prévio.
2. **Instabilidade do protocolo:** mudanças no protocolo WhatsApp Web podem quebrar o
   `whatsmeow` sem aviso. A lib pode ficar desatualizada temporariamente.
3. **Sem SLA de entrega:** ao contrário da API oficial, não há garantias de entrega,
   leitura, ou conformidade com políticas de negócios da Meta (templates obrigatórios,
   opt-in explícito).
4. **Impacto limitado ao tenant opt-in:** apenas tenants que explicitamente ativarem
   `provider=session` estão sujeitos ao risco. O canal oficial `provider=meta` continua
   inalterado e sem exposição a esse risco.

**Aceite registrado por:** board Sindireceita, 2026-06-29.

**Mitigação operacional:** O painel master exibirá aviso proeminente de risco ToS no fluxo
de ativação do canal `provider=session`. Operadores de tenant devem confirmar ciência do
risco antes de concluir a configuração.

---

## Consequências

### Positivas

- Tenants sem acesso à API oficial Meta (sem CNPJ, sem conta Business verificada) podem usar
  o canal WhatsApp via número pessoal, com o mesmo inbox de atendimento.
- Zero sidecar — complexidade operacional não aumenta no v1.
- Isolamento hexagonal completo: domínio não sabe de `whatsmeow`, só de
  `WhatsAppSessionSender` / `WhatsAppSessionReceiver`.

### Negativas / Riscos residuais

- Risco ToS aceito pelo board (D6). Número de telefone do tenant pode ser banido.
- Dependência de protocolo privado do WhatsApp — atualizações upstream podem exigir upgrade
  de `whatsmeow` em curto prazo.
- Performance em escala (>200 tenants) exigirá migração para worker isolado (dívida técnica
  registrada).
- Patches locais em `whatsmeow` ficam proibidos sem fork público (MPL-2.0, D2).

### Dívida técnica registrada

| ID | Item | Threshold |
|---|---|---|
| DT-WA-01 | Migrar para worker isolado se >200 tenants ativos no canal session | >200 tenants |
| DT-WA-02 | Avaliar schema-por-tenant vs coluna tenant_id no store | ADR de migration |
| DT-WA-03 | Circuit breaker por tenant para restart de sessão | Fase 1 |

---

## Alternativas consideradas e descartadas

| Alternativa | Motivo de descarte |
|---|---|
| Sidecar Node (`baileys`) | Segundo runtime, IPC, supply chain duplicada, sem benefício claro para inbox v1 |
| Canal separado `whatsapp_session` | Duplicaria regras de negócio; `provider` é suficiente e menos invasivo |
| SQLite por tenant (store local) | Não sobrevive restart de pod; inviável em Postgres-only stack |
| Worker Kubernetes separado desde v1 | Over-engineering; threshold de necessidade é >200 tenants |
| Fork privado de whatsmeow | Viola MPL-2.0; manutenção de fork privado é dívida permanente |

---

## Addendum — 2026-06-29 (SIN-66267): `go.mau.fi/libsignal` é GPL-3.0

> Este addendum corrige uma premissa do ADR original. Refs:
> [SIN-66264](/SIN/issues/SIN-66264) (achado em review de supply-chain),
> [SIN-66265](/SIN/issues/SIN-66265) (decisão do CEO — Opção 1),
> [SIN-66266](/SIN/issues/SIN-66266), [SIN-66267](/SIN/issues/SIN-66267) (este fix).

**Achado.** A análise D2 trata corretamente a licença do **próprio** `go.mau.fi/whatsmeow`
(MPL-2.0). O que a premissa original **não** capturou: `whatsmeow` puxa transitivamente
`go.mau.fi/libsignal` **v0.2.2**, que é licenciada sob **GPL-3.0** — GPLv3 integral, **sem
exceção LGPL / de linking** (é um fork de `RadicalApp/libsignal-protocol-go`). GPLv3, ao
contrário de MPL-2.0, é copyleft **forte**: linkar estaticamente e **distribuir** o binário
combinado obriga todo o binário sob GPLv3.

**Distinção jurídica decisiva — GPLv3 ≠ AGPLv3.** Sob GPLv3, dar **acesso via rede** a um
software (SaaS) **não** é "conveying" (distribuição) e portanto **não** dispara a obrigação de
copyleft. É a AGPLv3 que estende o copyleft ao uso em rede — e `libsignal` é GPLv3, **não**
AGPLv3. Logo:

- **Shape SaaS (atual): seguro.** Servimos o CRM pela rede; não distribuímos o binário. Sem
  obrigação de abrir o código-fonte do CRM.
- **Distribuição / on-prem / download / imagem entregue ao tenant: NÃO permitido** enquanto o
  código que linka `libsignal` (o transporte WhatsApp/whatsmeow) fizer parte do binário. Isso
  obrigaria o binário combinado inteiro sob GPLv3.

**Decisão (CEO, SIN-66265 — Opção 1: risk-accept + guard-rail automatizado).** Aceitamos a
dependência GPL-3.0 **estritamente para o shape SaaS**, com dois mecanismos de enforcement:

1. **Gate de CI de licença** (`.github/workflows/license-scan.yml` +
   `scripts/ci/check-licenses.py`): falha o build em qualquer módulo da família GPL/AGPL, com
   **allowlist de uma única entrada** — `go.mau.fi/libsignal` — comentada inline com o link da
   decisão. Qualquer **nova** transitiva GPL/AGPL falha o build. É a layer 2 ao lado do
   `govulncheck` (que cobre só CVE, não licença).
2. **Política de deployment** (`docs/policy/deployment-licensing.md`): proíbe distribuir/
   shippar on-prem qualquer artefato que linke `libsignal` enquanto a isolação (Opção 2) não
   existir.

**Opção 2 (caminho planejado, se/quando um modelo distribuído ou on-prem for proposto):**
isolar o código que linka `whatsmeow`/`libsignal` em um **serviço separado, isolado por rede,
arms-length**, comunicando-se com o CRM via API — de modo que o binário distribuível do CRM
não linke GPLv3. Não implementado agora; pré-requisito para qualquer deployment não-SaaS.

### Dívida técnica registrada (adendo)

| ID | Item | Threshold |
|---|---|---|
| DT-WA-04 | Isolar transporte whatsmeow/libsignal em serviço arms-length (Opção 2) | Se/quando deployment distribuído ou on-prem for seriamente proposto |
