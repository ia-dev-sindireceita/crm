# ADR 0041 — Anonymizer: PII masking before LLM dispatch

- Status: Accepted
- Date: 2026-05-16
- Deciders: CTO, SecurityEngineer (review in-thread on PR)
- Drives: [SIN-62901](/SIN/issues/SIN-62901) (this ADR), [SIN-62196](/SIN/issues/SIN-62196) (Fase 3 parent)
- Cross-links: Board decision #8 on [SIN-62203](/SIN/issues/SIN-62203) (default `policy.anonymize=true`), [ADR 0040](./0040-llm-port-retry-idempotency.md) (the port the anonymizer sits in front of), [SIN-62225](/SIN/issues/SIN-62225) §3.4-§3.8 (prompt isolation framework)

## Context

Operator-facing AI assist ([SIN-62196](/SIN/issues/SIN-62196))
forwards a conversation snippet to a third-party LLM (OpenRouter at
first; possibly others). Brazilian CRM conversations routinely
contain personally identifying data: phone numbers in
`+55XXXXXXXXXXX` form, email addresses, CPF numbers
(`XXX.XXX.XXX-XX`). Forwarding raw PII to an external model is a
problem for three independent reasons:

- **Regulatory.** LGPD restricts cross-border transfer of personal
  data, especially to a sub-processor that has not been disclosed to
  the data subject. Even with a DPA, minimising the data sent is a
  required practice.
- **Operational.** The provider's logs, abuse-review systems, and
  fine-tuning corpora are not under our control. A leak in their
  infrastructure leaks our customers' customers.
- **Functional.** The model does not need the PII to do its job. A
  summary of "what did the customer ask about" is more useful when
  the customer's CPF has been replaced with `<cpf>` — the model
  cannot confidently regurgitate the masked value, which is exactly
  what we want.

Board decision #8 on [SIN-62203](/SIN/issues/SIN-62203) made
anonymisation **default-on**: `policy.anonymize = true` unless the
tenant explicitly opts out for a specific scope (and even then, the
opt-out is audited and time-limited).

The existing AI surface already has multiple safety layers:

- Prompt isolation (the framework decided in
  [SIN-62225](/SIN/issues/SIN-62225) §3.4-§3.8 and enforced by the
  `aicache` analyzer and the renderer in `internal/ai/render/`)
  prevents operator-supplied content from being interpreted as
  system instructions.
- Redaction-aware logging ([ADR 0004](./0004-logging-and-audit.md))
  keeps PII out of our own logs.
- Rate limiting (`internal/ai/port/ratelimiter.go`) caps how much
  any one tenant can send to the model.

Anonymisation adds a fourth layer: **defense in depth** says we do
not rely on any one of these holding. A bug in any single layer must
not be the difference between safe and unsafe.

## Decision

**An anonymizer port `internal/aiassist.Anonymizer` runs immediately
before the LLM port. The first adapter,
`internal/adapter/llm/openrouter/anonymizer.go`, applies regex-based
masking for three PII kinds (phone, email, CPF). Anonymisation is
default-on per board decision #8 on
[SIN-62203](/SIN/issues/SIN-62203); each call emits an audit record
with masked counts and kinds, never the masked payload itself.**

### D1 — Port surface

`internal/aiassist/anonymizer.go`:

```go
type Anonymizer interface {
    Mask(ctx context.Context, scope Scope, in MaskInput) (MaskOutput, error)
}

type MaskInput struct {
    TenantID uuid.UUID
    Text     string
}

type MaskOutput struct {
    Text          string                  // same length OR shorter than input
    MaskedCount   int                     // total tokens replaced
    MaskedKinds   map[MaskKind]int        // per-kind counts
}

type MaskKind string

const (
    MaskPhone MaskKind = "phone"
    MaskEmail MaskKind = "email"
    MaskCPF   MaskKind = "cpf"
)
```

The use-case (`internal/aiassist/usecase.go`) calls `Mask` between
`policy.Resolve` and `llm.Generate`:

```go
if pol.Anonymize {
    out, err := anon.Mask(ctx, scope, MaskInput{TenantID: req.TenantID, Text: req.Prompt.UserContent()})
    if err != nil { return resp, fmt.Errorf("anonymize: %w", err) }
    req.Prompt = req.Prompt.WithUserContent(out.Text)
    audit.Emit(ctx, "ai.anonymize", attrs(req.TenantID, scope, out.MaskedCount, out.MaskedKinds))
}
```

The use-case never inspects the masked payload. The audit emitter
reads counts and kinds; the **text itself is dropped on the floor**
the moment `req.Prompt` is reassigned. There is no log line, span
attribute, or metric label that carries the unmasked or masked text.

### D2 — Masking rules

Three masks ship in Fase 3. Each is a deterministic regex applied to
the prompt text before dispatch.

**Phone** — Brazilian E.164 with country code. Pattern:
`\+55\s?\(?\d{2}\)?\s?9?\d{4}-?\d{4}`. Replacement: `<phone>`.
Examples:

- `+5511987654321` → `<phone>`
- `+55 (11) 98765-4321` → `<phone>`
- `+55 11 987654321` → `<phone>`

Numbers without the `+55` prefix are deliberately **not** masked at
this phase. Operators frequently paste local numbers (`(11) 9XXXX-
XXXX`) that look identical to product codes, ticket numbers, and
random 11-digit strings; a broader regex causes false positives
that break the model's ability to reason about the conversation
("the customer mentioned order #1234567890" must not become "the
customer mentioned order `<phone>`"). The board accepted this trade-
off in decision #8 of [SIN-62203](/SIN/issues/SIN-62203). The
defense-in-depth layers above this (policy opt-in, redacted logs,
rate limit) cover the residual risk.

**Email** — RFC-5322 conformant subset. Pattern:
`[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}`. Replacement:
`<email>`. The regex is intentionally narrow — international
domains with IDN and exotic local-parts are rare in our corpus and
broadening the regex pulls in too many false positives (e.g.
file paths with `@` in them).

**CPF** — Standard formatted CPF only. Pattern:
`\d{3}\.\d{3}\.\d{3}-\d{2}`. Replacement: `<cpf>`. Unformatted CPFs
(an 11-digit run with no separators) are not masked at this phase
for the same false-positive reason as bare phone numbers (any
11-digit string would match). Operators in this CRM consistently
paste formatted CPFs because the source UIs format them; the
unformatted case is rare enough to be acceptable residual risk for
Fase 3.

Mask kinds are applied in a deterministic order: **CPF → email →
phone**. The order matters when a phone-shaped substring overlaps
with a CPF-shaped one (it can't, by length, but the order is fixed
to keep replacement stable across runs and easier to test).

All three regexes are anchored with word boundaries
(`\b...\b` where applicable) so they do not cut inside arbitrary
strings. The implementation compiles them once at adapter init and
uses `regexp.Regexp.ReplaceAllString`.

### D3 — Default-on, opt-out is policy-driven

`policy.Anonymize = true` is the default for every scope. A tenant
who explicitly opts out via the policy cascade (see [ADR
0042](./0042-policy-cascade.md)) skips the `Mask` call entirely.
The opt-out path:

1. Is configured per scope (channel / team / tenant), not globally.
2. Is audited on every prompt: an opt-out call emits
   `ai.anonymize.skipped` with the resolved policy id and the scope
   that disabled it, so a forensic review can answer "why was this
   tenant's email sent to OpenRouter on day X."
3. Cannot be set in code; only the operator's policy admin (or a
   master with `master.policy.write`) can flip it. The flip itself
   is an audited event.

The lens **defense in depth** drove the decision to keep the
default-on at the use-case layer rather than only in the policy
loader. Even if a misconfigured policy returned an "unknown" answer
for `anonymize`, the use-case must treat unknown as `true`. The
typed `Scope` and policy resolver enforce this with an explicit
`Default()` value of `Anonymize: true`.

### D4 — Audit emission

Every `Mask` call emits one structured audit record. The schema:

```json
{
  "event": "ai.anonymize",
  "ts": "2026-05-16T19:21:55.123Z",
  "tenant_id": "01HXYZ...",
  "scope": "summarise",
  "masked_count": 3,
  "masked_kinds": { "phone": 1, "email": 2 },
  "policy_id": "01HXYZ..."
}
```

The audit pipeline (`internal/adapter/audit/`) writes to the
`audit_log` table per [ADR 0004](./0004-logging-and-audit.md). The
**masked payload is never stored, logged, traced, or metric-labelled
anywhere in the system**. The audit record carries only counts and
kinds; that is enough for compliance reporting ("how many
emails / month does tenant X expose to the LLM after masking")
without storing the data.

A separate audit event, `ai.anonymize.skipped`, fires when
`policy.Anonymize = false`. It carries the same shape minus
`masked_kinds` (which would be misleading) and adds
`skipped_reason: "policy"` to make forensics fast.

### D5 — Hexagonal boundary

`internal/aiassist/anonymizer.go` defines the port and the typed
`MaskKind` enum. It has no transport, SDK, or storage imports.

`internal/adapter/llm/openrouter/anonymizer.go` is the first
adapter. It implements `Anonymizer` using compiled regexes. The
file lives under `internal/adapter/llm/openrouter/` because the
masks today are tuned to the OpenRouter pipeline (the `<phone>`
sentinel format is one OpenRouter's tokenizer handles cleanly);
when we add a second LLM adapter, we will either (a) move the
anonymizer to `internal/adapter/anonymizer/` for sharing, or (b)
write a per-adapter version if the sentinel format differs.
Option (a) is the expected outcome; option (b) is an escape hatch.

The use-case wires both ports together. It does **not** assume the
anonymizer is co-located with any specific LLM adapter; the wiring
in `internal/bootstrap/aiassist.go` is the only place that knows
which anonymizer pairs with which LLM client.

### D6 — Testing

Unit tests live in `internal/adapter/llm/openrouter/anonymizer_test.go`
and cover, at minimum:

- Each kind in isolation (positive examples + negative non-matches).
- Mixed input with two kinds in one prompt.
- Overlapping near-matches that should **not** be masked (e.g. a
  random 11-digit order number must not be masked as a phone; an
  arbitrary `text@here` without a TLD must not be masked as an
  email).
- Idempotence: `Mask(Mask(x)) == Mask(x)`. Masking a prompt that
  already contains `<email>` must not replace anything further.
- Audit counts: a prompt with 2 emails and 1 phone produces
  `MaskedCount = 3`, `MaskedKinds = {email: 2, phone: 1}`.

A property-based test generates random prompts seeded with planted
PII and asserts the output contains no raw PII matching the input
regexes. The property test runs in CI but with a short seed budget
(≤ 500 cases per run) to keep CI fast.

The audit-pipeline test asserts that **the masked payload never
appears** in any of: spans, structured-log records, metric labels,
or `audit_log` rows produced by an end-to-end run that includes a
`Mask` call.

### D7 — What the anonymizer is not

The anonymizer is **not** a DLP system. It does not:

- Detect free-form PII (a person's name written inline, an address
  without a postcode pattern, a banking IBAN).
- Re-identify masked tokens server-side. Once masked, the original
  cannot be recovered from the response — there is no hidden
  pseudonym map. (Future work may add this for "answer with the
  customer's name" use cases; out of scope for Fase 3.)
- Apply to the LLM's *response*. Provider hallucinations or
  echoes-of-prompt of un-masked PII are out of scope here; mitigated
  by sending pre-masked prompts in the first place.

The defense-in-depth layers (policy opt-in, redacted logs, rate
limit) cover what the anonymizer cannot. The anonymizer's job is
narrow and well-defined: three regexes, applied deterministically,
audited per call, default-on.

## Consequences

Positive:

- **Defense in depth** holds: a misconfigured policy that wrongly
  says `anonymize = false` is one of four layers; the redaction-
  aware logger still keeps PII out of our own logs, and the
  ratelimiter still caps blast radius.
- **Hexagonal** boundary preserved: the use-case sees one
  `Anonymizer` interface; the adapter is one file with three
  compiled regexes. Replaceable when we have a better detector
  (e.g. a small classifier in Fase 5+).
- Audit trail is rich enough for compliance reporting and forensics
  without ever storing the data itself.
- Default-on is enforced at the use-case layer, not only at the
  policy loader. A buggy policy resolver cannot accidentally
  disable the anonymizer.

Negative / costs:

- The regex set is intentionally narrow. Operators who paste an
  unformatted CPF or a bare local phone number will see those
  forwarded to the LLM. This is documented in the operator handbook
  with a recommendation to format inputs before pasting; it is also
  a known limitation we plan to address with a smarter detector in
  Fase 5.
- Every prompt pays a small CPU cost for regex matching. The three
  regexes combined cost ~10µs on typical prompt sizes (≤ 4 KB); the
  overall LLM call latency budget is 8s, so the relative cost is
  negligible.
- The audit emitter must be wired before the anonymizer is
  released. Shipping the anonymizer without `ai.anonymize` events
  would defeat the compliance reporting argument; CI fails the
  build if the audit-pipeline integration test cannot observe the
  event.

Risk residual:

- A bug in one of the three regexes (e.g. a phone regex that
  matches `999.999.999-99` and turns a CPF into `<phone>`) would
  produce surprising audit counts but would still mask the data.
  Mitigation: the test for each kind is paired with an integration
  test that runs the full mask pipeline over a fixed corpus.
- The provider's response may echo a portion of the prompt. If the
  prompt did not contain raw PII (post-masking), the echo cannot
  either. If the prompt was opted out, the echo can — and the
  redacted logger is the next layer. The `ai.anonymize.skipped`
  audit event makes this visible during a forensic review.

## Alternatives considered

### Option A — No anonymisation, rely on provider DPA

Trust the provider's Data Processing Agreement to handle our PII
correctly.

Rejected because:

- DPAs do not protect against provider-side bugs or breaches; they
  only allocate liability. Liability is not the same as protection.
- LGPD's data-minimisation principle is independent of DPAs: even
  if the provider would do nothing wrong, we should send less data
  rather than more.
- Removes a layer from the **defense in depth** stack; we would be
  one bug away from a full PII leak.

### Option B — Server-side classifier (ML-based) instead of regex

Train or fine-tune a small NER model that detects PII more broadly
(free-form names, addresses, banking codes).

Rejected for Fase 3 because:

- Cost: shipping a classifier (even small) means inference
  infrastructure or a synchronous classifier call before every LLM
  call. Latency budget is tight (8s p99); adding a 100-300ms
  classifier eats into retries.
- Maintenance: a classifier requires training data, drift
  monitoring, and retraining. Three regexes do not.
- Boring-tech budget: regex is in the stdlib, deterministic, and
  testable. The classifier is a Fase 5+ upgrade when we have
  the operational headroom.

The port surface is designed so a classifier-backed adapter can
replace the regex adapter without touching the use-case. When the
classifier ships, this ADR will be superseded by a new one that
documents the migration plan and the operational requirements
(latency budget, drift monitoring, on-call ownership).

### Option C — Anonymise as a domain operation, regexes in `internal/aiassist`

Put the regex compilation and masking logic inside the domain
package instead of an adapter.

Rejected because:

- The mask format (sentinel strings, regex tuning per provider) is
  an adapter concern. The domain only needs to know "there is a
  masker; it accepts text and a tenant id; it returns text and
  counts."
- Keeps the domain free of regex churn when we tune masks in
  response to false-positive reports.

## Lenses cited

- **Defense in depth.** The anonymizer is one of four PII safety
  layers (policy, anonymizer, redacted logs, rate limit). Each
  failure mode is contained.
- **Hexagonal / ports & adapters.** Single port, swappable adapter,
  domain free of regex and SDK concerns.
- **Least privilege.** Anonymizer holds no privileges beyond
  "read text, return text + counts." It cannot read the tenant's
  policy, cannot mutate state, cannot reach the network.
- **Observability before optimisation.** Audit emission ships with
  the anonymizer; reporting is possible from day one.
- **Boring technology budget.** Regex. Stdlib. Three patterns.

## Security review checklist

The SecurityEngineer reviewing this ADR (in-thread on the PR) should
confirm:

- [ ] Masked payloads are never logged, traced, metric-labelled, or
      stored.
- [ ] The opt-out path is audited (`ai.anonymize.skipped` event).
- [ ] Default policy returns `Anonymize: true` even when the
      cascade finds no matching scope.
- [ ] The phone, email, and CPF regexes are bounded (no
      catastrophic backtracking).
- [ ] Tests assert idempotence: `Mask(Mask(x)) == Mask(x)`.
- [ ] The audit-pipeline integration test asserts the masked
      payload does not appear in any observability artifact.
- [ ] The trade-off on bare local phone numbers and unformatted CPF
      is acceptable given the defense-in-depth layers above this
      one. Flag if not.

## Out of scope

- Response-side masking (re-anonymising echoes from the LLM
  output). The mitigation is to send pre-masked prompts.
- Free-form PII detection (names, addresses, IBANs). Future
  classifier-backed adapter.
- Reversible pseudonymisation. There is no hidden pseudonym map;
  masked tokens cannot be reversed.
- Provider-side data-residency configuration. Out of scope for this
  ADR; covered by the operational checklist in [SIN-62196](/SIN/issues/SIN-62196).
