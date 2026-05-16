# Acordo de Processamento de Dados (DPA) — CRM Sindireceita

Este documento descreve as obrigações da Sindireceita (Operadora) no
tratamento de dados pessoais de titulares dos seus clientes (Controladores
B2B), nos termos da Lei nº 13.709/2018 (LGPD).

A versão canônica deste documento é controlada pela constante
`DPAVersion` exposta pelo pacote `internal/legal`. O número da versão e o
timestamp da geração ficam impressos no rodapé da página
`/settings/privacy` e no nome do arquivo baixado.

## 1. Definições

- **Operadora**: Sindireceita, fornecedora do CRM.
- **Controlador**: empresa cliente da Sindireceita que opera uma
  instância white-label do CRM (referido como "tenant").
- **Sub-processador**: terceiro contratado pela Operadora que realiza
  parte do tratamento de dados pessoais em nome do Controlador.

## 2. Finalidade e bases legais

Os dados pessoais (nome, telefone, e-mail, conteúdo de conversas) são
tratados exclusivamente para:

- Centralizar atendimento multi-canal (WhatsApp, Instagram, Facebook,
  chatbot, e-mail).
- Resumir conversas e sugerir argumentação de venda via LLM
  (sub-processador OpenRouter — ver §4).
- Emitir cobranças, faturas e contratos de cessão de uso.

Base legal aplicável: execução de contrato (LGPD art. 7º, V) e legítimo
interesse para finalidades operacionais auxiliares (LGPD art. 7º, IX),
mediante balanceamento documentado.

## 3. Retenção e direito de eliminação

- Retenção padrão de dados de conversa: **12 meses** (configurável pelo
  Controlador entre 6 e 60 meses).
- Pedidos de eliminação chegam via `POST /api/data-erasure`. A Operadora
  remove (não apenas marca como inativo) registros de conversas, mídias
  associadas e cache de resumos de IA em até 30 dias.
- Cópias em backups encriptados são retidas por mais 30 dias após a
  remoção em produção e então rotacionadas.

## 4. Sub-processadores

A Operadora declara abaixo a relação de sub-processadores ativos. A
versão atual desta lista vive em `internal/legal/Subprocessors()` e é
exibida em `/settings/privacy`. Mudanças exigem nova `DPAVersion` e
notificação ao Controlador com **30 dias de antecedência**.

### 4.1 OpenRouter (decisão #8 / SIN-62203)

| Atributo | Valor |
|---|---|
| Finalidade | Resumir conversa e sugerir argumentação de venda. |
| Dados tratados | Mensagens com PII estruturada mascarada (telefone, e-mail e CPF substituídos por tokens; nomes mantidos por necessidade conversacional). |
| Localização | EUA / multi-região conforme o modelo selecionado. |
| Base contratual | Contratação direta entre Operadora e OpenRouter, Inc. |
| Anonimização aplicada | Sim — `internal/aiassist/anonymizer` (ADR-0041) executa máscara antes do envio quando `policy.opt_in=true` e `policy.anonymize=true` (default). |
| Política de privacidade | <https://openrouter.ai/privacy> |

**Opt-in obrigatório**: nenhum dado é enviado ao OpenRouter para um
escopo (canal/equipe/tenant) cujo `ai_policy.opt_in=false`. O default na
criação do tenant é `false` (LGPD posture, ADR-0041).

### 4.2 Meta Platforms (WhatsApp / Instagram / Facebook)

| Atributo | Valor |
|---|---|
| Finalidade | Transporte de mensagens via WhatsApp Cloud API, Instagram Graph API e Messenger Graph API. |
| Dados tratados | Mensagens, mídias e metadados de identificação do cliente final no canal correspondente. |
| Base contratual | Termos de Processamento de Dados da Meta para Negócios. |
| Política de privacidade | <https://www.facebook.com/legal/terms/dataprocessingterms> |

### 4.3 Mailgun Technologies

| Atributo | Valor |
|---|---|
| Finalidade | Entrega transacional de e-mail (notificações de cobrança, recuperação de senha, convites). |
| Dados tratados | Endereço de e-mail e conteúdo da mensagem transacional. |
| Base contratual | DPA Mailgun. |
| Política de privacidade | <https://www.mailgun.com/dpa/> |

### 4.4 PSP de PIX (a definir)

| Atributo | Valor |
|---|---|
| Finalidade | Conciliação de cobranças PIX. |
| Status | Provedor a ser ratificado na decisão D2; até lá, o módulo de cobrança PIX opera com sandbox. |
| Política de privacidade | A ser publicada quando o provedor for ratificado. |

## 5. Segurança

- Trânsito: TLS 1.2+ obrigatório nos endpoints públicos (ADR-0073).
- Repouso: encriptação em volume gerenciado pelo VPS (LUKS/dm-crypt) +
  encriptação aplicacional para colunas marcadas como sensíveis.
- Controle de acesso: RBAC (`atendente`, `líder de equipe`, `gerente`,
  `master`, `master_supporter`, `master_observer`) com auditoria por
  ação em `audit_log_security`.
- Resposta a incidente: 72h para comunicação ao Controlador (LGPD art. 48).

## 6. Direitos do titular

A Operadora apoia o Controlador no atendimento aos direitos do titular
(LGPD art. 18) através de:

- Endpoint `POST /api/data-erasure` (erasure).
- Exportação por usuário em formato JSON via UI do Controlador.
- Anonimização irreversível de dados de auditoria após o prazo de
  retenção.

## 7. Transferência internacional

OpenRouter (EUA) é o único sub-processador com transferência
internacional rotineira. A base legal é a hipótese do LGPD art. 33, V
(cumprimento de obrigação contratual de execução do serviço), reforçada
pela anonimização prévia descrita em §4.1.

## 8. Auditoria

O Controlador pode solicitar evidências de conformidade (logs de acesso
agregados, relatórios de pen-test, certificações dos sub-processadores)
com aviso de 15 dias. Atos de auditoria são executados em janela
agendada e sob acordo de confidencialidade.

## 9. Vigência

Este DPA é vigente enquanto houver contrato ativo entre Operadora e
Controlador, e por mais 30 dias após sua rescisão para fins de
liquidação técnica (exportação final, eliminação certificada).

---

_Documento gerado pelo CRM Sindireceita. Versão exibida e baixada pela
página `/settings/privacy`. Para a versão canônica em vigor, consulte
sempre o cabeçalho/rodapé com o número da versão e o timestamp._
