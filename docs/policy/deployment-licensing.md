# Deployment licensing policy — GPL-3.0 in the WhatsApp transport

- Status: Active
- Date: 2026-06-29
- Owner: SecurityEngineer (review) / CTO (enforcement at merge)
- Refs: [SIN-66265](/SIN/issues/SIN-66265) (CEO decision — Option 1),
  [SIN-66266](/SIN/issues/SIN-66266), [SIN-66267](/SIN/issues/SIN-66267),
  ADR [0107](../adr/0107-whatsapp-session-whatsmeow.md) addendum.

## The rule

**Any component that links `go.mau.fi/libsignal` (i.e. the WhatsApp /
`whatsmeow` transport) MUST be delivered SaaS-only. It MUST NOT be:**

- distributed as a binary,
- shipped or installed on-prem,
- made downloadable, or
- shipped as a tenant / customer / partner container or image,

**until the isolation work (Option 2, below) is completed.**

This is a hard constraint, not a guideline. A violation puts the entire
combined binary under GPLv3 obligations on the party that distributes it.

## Why

`go.mau.fi/whatsmeow` (MPL-2.0) pulls in `go.mau.fi/libsignal` **v0.2.2**
transitively, and libsignal is **GPL-3.0** — full GPLv3, **no LGPL / linking
exception**.

The legal distinction that makes the SaaS shape safe:

- **GPLv3 ≠ AGPLv3.** Under GPLv3, providing **network access** to software
  (SaaS) is **not** "conveying" (distribution), so it does **not** trigger the
  copyleft obligation. (AGPLv3 *would* extend copyleft to network use —
  libsignal is GPLv3, not AGPLv3.)
- **Static linking + distribution** of a binary that includes libsignal-linked
  code **would** obligate the whole combined binary under GPLv3. That is the
  line this policy keeps us behind.

So: SaaS = safe; distribution / on-prem / downloadable artifact = **not
permitted** while the transport is part of the binary.

The CEO risk-accepted the GPL-3.0 dependency for the SaaS shape in
[SIN-66265](/SIN/issues/SIN-66265) (Option 1: risk-accept + machine-enforced
guardrail).

## Enforcement (defense in depth)

1. **CI license gate** — `.github/workflows/license-scan.yml` +
   `scripts/ci/check-licenses.py`. Fails the build on any GPL/AGPL-family
   module except the single allowlisted, risk-accepted `go.mau.fi/libsignal`.
   Any *new* GPL/AGPL transitive fails loudly. (Layer 2 next to
   `govulncheck.yml`, which covers CVEs only.)
2. **This policy doc** — the human-readable constraint the gate encodes; the
   reference for release/deployment decisions.
3. **Code review** — reviewers reject any change that packages the
   whatsmeow/libsignal-linked code into a distributable artifact, or that adds
   a `replace` directive turning whatsmeow into a private fork (see ADR 0107
   D2 for the MPL-2.0 side of that rule).

## Option 2 — the path to a distributed / on-prem model

If a distributed or on-prem deployment model is ever seriously proposed,
**isolate the whatsmeow / libsignal-linked code into a separate,
network-isolated, arms-length service** that the CRM talks to over an API. The
CRM's distributable binary then does not link GPLv3 code, and only the isolated
transport service carries the obligation (and stays SaaS-side).

This work is **not** done today. It is tracked as DT-WA-04 in ADR 0107 and is a
**hard prerequisite** for any non-SaaS deployment. Until it lands, the rule at
the top of this document stands without exception.
