# Gitleaks — secret-scanning gate

> **Source:** [SIN-62299](/SIN/issues/SIN-62299).
> **Trigger:** every `pull_request`, every `push` to `main`.
> **Action on finding:** job fails (`--exit-code=1`); merge blocked.

## Why this gate exists

The pre-handoff `gitleaks detect --log-opts=--all` run on the SIN-62297 bridge tip was clean — but **only because there was no `.gitleaks.toml` in the repo**, so gitleaks ran with default rules. The default ruleset catches AWS keys, GitHub PATs, and other generic API-key shapes, but **not** the secret families this CRM actually handles. This file documents the project-specific rules and the deployment posture.

## Rules

The custom ruleset lives at [`.gitleaks.toml`](../../.gitleaks.toml) and extends gitleaks defaults via `[extend] useDefault = true`. Five custom rules cover Sindireceita-specific high-sensitivity tokens:

| Rule ID | What it catches | Why it matters |
|---|---|---|
| `meta-cloud-user-token` | `EAA[A-Za-z0-9]{200,}` Meta Cloud API user/system tokens | Lets an attacker post to a tenant's WhatsApp Business number, read inbound messages, impersonate the operator. Inbound-channel HIGH. |
| `meta-cloud-long-lived-token` | `EAAG[A-Za-z0-9]{200,}` long-lived graph access tokens | Same blast radius as the user token, but extended lifetime raises rotation cost on a leak. |
| `instagram-graph-token` | `IGQV[A-Za-z0-9_\-]{100,}` Instagram Graph API tokens | Same blast-radius profile as Meta Cloud tokens. |
| `meta-app-secret` | 32-hex Meta App Secret, **context-anchored** to `META_APP_SECRET` / `app_secret` / `appsecret` adjacency | Used to verify inbound webhook signatures (HMAC-SHA256). A leak nullifies signature verification — attackers can forge inbound channel messages, including inbound prompt-injection-bearing payloads (LLM Top 10). |
| `whatsapp-verify-token` | Operator-chosen verify_token, **context-anchored** to `whatsapp` / `wa` / `meta` prefix | Combined with a Cloud API token leak, lets an attacker register their own URL as the webhook callback in the operator's Meta dashboard — full webhook takeover. |

### Context anchoring (false-positive control)

The `meta-app-secret` and `whatsapp-verify-token` rules are deliberately **context-anchored** — they require the variable name to be adjacent to the value. This is by design:

- A bare 32-hex string matches every UUID-without-dashes and every MD5 hash in the codebase. Without context, this rule would be unusable.
- A bare `verify_token` keyword fires on JWT, OAuth, and many unrelated webhook handshakes. Only the `whatsapp_*` / `wa_*` / `meta_*` family is in scope here.

If a future leak vector emerges that bypasses the anchor (e.g., a config file that puts the secret in a JSON key not in our keyword list), tighten the rule before extending the allowlist.

### Separator class — must NOT span lines ([SIN-62758](/SIN/issues/SIN-62758))

The separator character class between the keyword and the captured value is `["' \t:=]{1,8}` in both context-anchored rules. The earlier form `["'\s:=]{1,8}` was incorrect: `\s` includes `\n`, which let the engine consume the newline after the keyword and then capture an unrelated identifier from the *next* line as the "secret". A const block like

```go
const (
    EnvAppSecret   = "META_APP_SECRET"
    EnvVerifyToken = "META_VERIFY_TOKEN"
    EnvWhatsAppEnabled = "FEATURE_WHATSAPP_ENABLED"
)
```

fired `whatsapp-verify-token` because the engine matched `META_VERIFY_TOKEN`, consumed `"\n\t` (3 separator chars), and captured `EnvWhatsAppEnabled` (18 chars). Entropy 3.7, false positive.

When editing either rule's regex, do **not** reintroduce `\s` (or any character class that contains `\n` or `\r`) into the separator. Keep it explicit: `["' \t:=]`. The keyword and the value must be on the **same line**.

### Regression bed (positive + negative per rule)

Future rule edits must be sanity-checked against this minimum set before merging. The exemplar lines below intentionally use placeholder words instead of patterns that would themselves trip the rule, so this file does not depend on the allowlist for its own contents — but the allowlist still exempts `docs/security/gitleaks.md` as defence in depth.

#### `whatsapp-verify-token`

- **Positive (must fire):** a single line of the shape `KEYWORD = "VALUE"` where `KEYWORD` is one of `whatsapp_verify_token` / `wa_verify_token` / `meta_verify_token` (any case, `-` or `_` separators) and `VALUE` matches `[A-Za-z0-9_\-]{12,}`. The rule must catch this; if it does not, the regex was over-tightened.
- **Negative (must NOT fire):** the Go const-block fixture above. After this fix, the engine cannot consume the newline between `EnvVerifyToken = "META_VERIFY_TOKEN"` and the next line, so the rule terminates at end-of-line and finds no `{12,}` capture on the same line.

#### `meta-app-secret`

- **Positive (must fire):** a single line of the shape `KEYWORD = "HEX32"` where `KEYWORD` is one of `meta_app_secret` / `app_secret` / `appsecret` (any case, `-` or `_` separators) and `HEX32` matches `[a-f0-9]{32}`.
- **Negative (must NOT fire):** a const block where `EnvAppSecret = "META_APP_SECRET"` is followed on the next line by a name that happens to be 32 hex characters (for example a SHA-256 digest literal declared right after the env-var-name constant). With newline excluded from the separator, the match cannot reach the next line.

If you change either regex, recompute these four cases locally (`gitleaks detect --no-git --source=<scratch-dir>` against a small fixture directory) before opening the PR.

## Allowlist

Paths suppressed by `[allowlist]`:

| Pattern | Reason |
|---|---|
| `.*_test\.go$` | Go unit/integration tests with placeholder fixtures. |
| `.*/testdata/.*` | Go testdata convention (skipped by build, used by analyzers like `tools/lint/nosecrets`). |
| `docs/adr/.*\.md$` | ADR example payloads (e.g., `META_APP_SECRET=example...`). |
| `docs/runbooks/.*` | Runbooks may contain redacted log samples (`EAA****redacted****`). |
| `docs/security/gitleaks\.md$` | This file references the rule patterns themselves. |
| `\.gitleaks\.toml$` | The ruleset is its own self-reference. |

The allowlist is the **last** lever to pull. If a real secret is showing up because of a path in the allowlist, fix the path scope or the rule; do not wave new secrets through.

## CI workflow

Defined in [`.github/workflows/gitleaks.yml`](../../.github/workflows/gitleaks.yml):

- Runs on `pull_request` and `push` to `main`.
- Pins gitleaks to a specific version (`v8.21.2`) and verifies the tarball SHA-256 before extracting — protects against a compromised upstream release.
- Invokes `gitleaks detect --log-opts=--all --exit-code=1`. The `--all` is non-negotiable: it ensures orphan-history branches merged via `--allow-unrelated-histories` cannot smuggle leaks past the gate (see SIN-62297).
- Uploads SARIF report as a workflow artifact for triage.

### Why we run the binary directly instead of `gitleaks/gitleaks-action@v2`

The `v2` action wrapper hardcodes `--exit-code=2` and on `pull_request` / `push` events scopes `--log-opts` to the diff range. Neither matches this gate's spec. Direct invocation pins the version, makes the args reviewable in-repo, and guarantees the orphan-history sweep runs on every event.

### Why we SHA-pin third-party GitHub-hosted actions

All third-party actions referenced from `gitleaks.yml` (and `govulncheck.yml`) are pinned by 40-char commit SHA, with the resolved version in a trailing `# v<version>` comment — never by a moving tag like `@v4`. A mutable tag can be re-pointed by the upstream owner (or by an attacker who compromises the publish flow) at any commit; pinning to a SHA freezes the exact bytes of the action's code, so a tag re-publish cannot smuggle malicious workflow code into our CI without a corresponding repo-side change being reviewed and merged. This is an application of **OWASP A06: Vulnerable & Outdated Components** and **CIS GitHub Actions Benchmark §3** to the workflow-supply-chain layer that sits beneath the runtime-binary supply-chain layer (where the `gitleaks` tarball is already sha256-verified). When upgrading an action, look up the new release's commit SHA, update **both** the `@<sha>` and the trailing version comment in the same edit, and re-run the workflow on a PR before merging — never edit only the comment. See [SIN-62303](/SIN/issues/SIN-62303) for the introduction of this convention.

## Operating posture

- **Fail-closed.** A finding fails the job. There is no soft-fail mode and no `continue-on-error`.
- **No bypass.** Do not add `if: false`, do not edit `paths` to skip files containing secrets, do not introduce `GITLEAKS_DISABLED` gates. If the rule is wrong, fix the rule.
- **No retroactive deletion.** If a secret leaks, rotation is the priority. Removing the commit does not invalidate the credential — only the issuer can revoke it. Open a separate operational ticket for rotation.

## Future layers (not in this ticket)

- **Layer 3 — GitHub native secret scanning** (separate ticket): repo-level setting that catches widely-known token formats Meta/etc. publish to GitHub. Complementary to this gate.
- **trufflehog / gitleaks-pro**: out of scope until Layer 3 baseline is in.
- **Pre-commit hook**: developer-side defense in depth (run gitleaks before push). Optional, separate ticket.

## Triage runbook (when the gate fires)

1. Read the finding in the workflow log. `--redact` redacts the value, but the file path, line, and rule ID are visible.
2. Determine: real secret or false positive?
   - **Real secret:** rotate the credential immediately (separate operational task), then remove from history (`git filter-repo` or BFG; coordinate with CTO). Do not just delete the commit — pushed history is already mirrored.
   - **False positive:** tighten the rule's regex with a more specific anchor, or extend `[allowlist].paths` with a narrowly-scoped path (never a broad pattern). Open a follow-up issue if the rule needs systemic rework.
3. Document the decision in the PR thread and on the originating Paperclip ticket.
