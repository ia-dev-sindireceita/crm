# Architecture Decision Records (ADRs)

Each ADR captures one architectural decision in MADR-ish format
(context → decision → alternatives → consequences). ADRs land
**before** the implementation they govern (e.g. ADR 0020/0021 precede
F2-06 and F2-11).

Filename convention: `NNNN-<kebab-case-title>.md`, with `NNNN` the next
sequential index. Numbers are permanent — supersedes/amendments live
inside the affected ADR, never via renumbering.

## Index (recent first)

### Phase 2 — Multi-canal + identidade + webchat

| ADR  | Title                                                                                                            |
|------|------------------------------------------------------------------------------------------------------------------|
| 0112 | [WhatsApp app-level single webhook](./0112-whatsapp-app-level-single-webhook.md) — one `/webhooks/whatsapp` per Meta app, multi-tenant fan-out by `phone_number_id` + go-live handshake (SIN-68299) |
| 0111 | [Image-carried compose deploy artifacts](./0111-image-carried-compose-deploy-artifacts.md) — compose extracted from the image at deploy |
| 0110 | [Backup age recipient injected at runtime](./0110-backup-age-recipient-runtime-mount.md) — host-mount the public recipient, image keeps the placeholder (resolves SIN-66536 gap) |
| 0109 | [Per-channel access — two-layer authz](./0109-per-channel-access-two-layer-authz.md) — surface-role gate + per-resource membership gate (extends 0090) |
| 0021 | [Webchat embed — segurança](./0021-webchat-embed-seguranca.md) — CSP/CORS/CSRF, assinatura de origem, rate limit |
| 0020 | [Merge de identidade](./0020-merge-de-identidade.md) — sinais, auto-merge vs `MergeProposal`, split              |

### Phase 0 / 1 — Platform, security, inbox

See sibling files `0002`, `0004`, `0070`–`0075`, `0078`–`0080`,
`0084`–`0095` in this directory. Open the file directly for status
and date.

## How to add an ADR

1. Pick the next free `NNNN` index (sequential from the highest in the
   directory).
2. Create `docs/adr/NNNN-<kebab-title>.md` using a recent ADR as a
   template — front-matter (status/date/owners/related), Context,
   Decision, Alternatives considered, Consequences, Rollback,
   Out of scope.
3. Cross-link from the issue/plan that motivated it, and link back
   from this index.
4. Land the ADR **before** the implementation PR it governs (or land
   them together when the implementation is the first instance).
