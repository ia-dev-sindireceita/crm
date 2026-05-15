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
