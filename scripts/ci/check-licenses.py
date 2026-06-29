#!/usr/bin/env python3
"""License gate — fail the build on strong-copyleft (GPL/AGPL family) modules.

SIN-66267 (parent SIN-66266; decision SIN-66265). Layer 2 of the
defense-in-depth answer next to govulncheck.yml: govulncheck gates *CVEs*,
this gates *licenses*. They cover disjoint risks; neither subsumes the other.

Why this exists
---------------
`go.mau.fi/whatsmeow` (MPL-2.0, fine) pulls in `go.mau.fi/libsignal` v0.2.2
transitively, and that module is **GPL-3.0** (full GPLv3, no LGPL/linking
exception). For the SaaS deployment shape that is acceptable — network access
is not "conveying" under GPLv3 — and the CEO risk-accepted it in SIN-66265
(Option 1: risk-accept + machine-enforced guardrail). This script *is* that
machine-enforced guardrail: it lets the one accepted module through an
explicit, inline-justified allowlist and fails loudly on any *new* GPL/AGPL
transitive so the acceptance can never silently widen.

Input
-----
Reads the CSV produced by `go-licenses report ./...` on stdin. go-licenses
emits one line per dependency module:

    <module/import path>,<license URL>,<license name>

e.g. `go.mau.fi/libsignal,https://.../LICENSE,GPL-3.0`. The third column is the
classifier's detected license name (SPDX-ish: `Apache-2.0`, `MPL-2.0`,
`GPL-3.0`, `AGPL-3.0`, `GPL-3.0-or-later`, ...).

Policy (fail-closed)
--------------------
* Block the GPL/AGPL *family* by name, not exact strings: any license whose
  name matches `^A?GPL(-|$|\b)` — this catches GPL-2.0, GPL-3.0,
  GPL-3.0-only, GPL-3.0-or-later, AGPL-3.0, bare GPL/AGPL, etc., while NOT
  matching LGPL (different obligations; out of scope for this gate) or MPL.
* Allow exactly the modules in ALLOWLIST below (currently one), each with an
  inline rationale. An allowlisted module is reported but does not fail.
* If go-licenses produced no usable rows, or this script cannot parse its
  output, exit non-zero. The secure default is to fail the build, never to
  pass silently.

Exit codes: 0 = clean, 1 = blocked license found, 2 = gate could not run.
"""

from __future__ import annotations

import csv
import re
import sys

# --- Allowlist: machine-enforced record of an explicit risk-acceptance. ------
#
# Each key is a Go module path. Adding an entry here is the ONLY sanctioned way
# to let a GPL/AGPL-family module past this gate, and every entry MUST carry a
# rationale linking the approval. Keep this list as small as possible.
ALLOWLIST: dict[str, str] = {
    # go.mau.fi/libsignal v0.2.x is GPL-3.0 (full GPLv3, no linking exception),
    # pulled in transitively by go.mau.fi/whatsmeow (MPL-2.0). Risk-accepted by
    # the CEO in SIN-66265 (Option 1) for the SaaS-only deployment shape:
    # network access is not "conveying" under GPLv3, so SaaS does not trigger
    # the copyleft obligation. This acceptance is CONDITIONAL: the
    # whatsmeow/libsignal-linked code MUST NOT be distributed, shipped on-prem,
    # or made downloadable — see docs/policy/deployment-licensing.md. If a
    # distributed model is ever pursued, this entry must be removed and the
    # isolation work (SIN-66265 Option 2) completed first.
    "go.mau.fi/libsignal": "SIN-66265 risk-accept, SaaS-only (GPL-3.0)",
}

# GPL/AGPL family, by detected license name. Deliberately excludes LGPL.
COPYLEFT_RE = re.compile(r"^A?GPL(-|$|\b)", re.IGNORECASE)


def main() -> int:
    rows = list(csv.reader(sys.stdin))
    # Fail closed: an empty report means go-licenses did not run / produced
    # nothing, which we must not treat as "no copyleft found".
    usable = [r for r in rows if len(r) >= 3 and r[0].strip()]
    if not usable:
        print(
            "license-gate: ERROR — no usable rows from go-licenses report; "
            "refusing to pass (fail-closed).",
            file=sys.stderr,
        )
        return 2

    violations: list[tuple[str, str]] = []
    allowed_hits: list[tuple[str, str]] = []
    unknown: list[str] = []

    for row in usable:
        module = row[0].strip()
        license_name = row[2].strip()

        if not license_name or license_name.lower() in ("unknown", "n/a"):
            unknown.append(module)
            continue

        if COPYLEFT_RE.match(license_name):
            allow_key = _allowlist_key(module)
            if allow_key is not None:
                allowed_hits.append((module, license_name))
            else:
                violations.append((module, license_name))

    # Report allowlisted copyleft (visible, auditable — not a silent pass).
    for module, lic in allowed_hits:
        print(
            f"license-gate: ALLOWED {module} ({lic}) — {ALLOWLIST[_allowlist_key(module)]}"
        )

    # Unknown licenses are surfaced loudly but do not fail the gate: this gate's
    # mandate (SIN-66267) is the GPL/AGPL family. go-licenses routinely reports
    # Unknown for benign modules, and failing on those would make the gate flaky
    # and get it disabled — the opposite of secure. CVE/other gaps are covered
    # by separate gates. Revisit if Unknowns ever mask a real copyleft module.
    if unknown:
        print(
            "license-gate: NOTE — license could not be classified for "
            f"{len(unknown)} module(s) (not failing on these): "
            + ", ".join(sorted(unknown)[:20])
            + (" ..." if len(unknown) > 20 else ""),
            file=sys.stderr,
        )

    if violations:
        print("\nlicense-gate: BLOCKED — strong-copyleft (GPL/AGPL) module(s) found:", file=sys.stderr)
        for module, lic in violations:
            print(f"  - {module}: {lic}", file=sys.stderr)
        print(
            "\nThese licenses obligate the whole combined binary under "
            "(A)GPL on distribution. If this is intentional and SaaS-only, add "
            "the module to ALLOWLIST in scripts/ci/check-licenses.py WITH a "
            "rationale and an approval link, and update "
            "docs/policy/deployment-licensing.md. Do not weaken the matcher.",
            file=sys.stderr,
        )
        return 1

    print(f"license-gate: OK — scanned {len(usable)} module(s); no un-allowlisted GPL/AGPL.")
    return 0


def _allowlist_key(module: str) -> str | None:
    """Return the matching allowlist key for a module path, or None.

    Matches the exact module path or any sub-path of an allowlisted module, so
    a single entry covers `go.mau.fi/libsignal` and any of its sub-packages
    that go-licenses might report independently.
    """
    for key in ALLOWLIST:
        if module == key or module.startswith(key + "/"):
            return key
    return None


if __name__ == "__main__":
    sys.exit(main())
