# ADR 0106 — Wireup-lint: fail CI when any `internal/adapter/httpapi/<sub>` package is unreachable from `cmd/server`

- Status: Accepted
- Date: 2026-05-27
- Deciders: CTO, Coder
- Drives: [SIN-63339](/SIN/issues/SIN-63339) (this ADR — structural follow-up to F11)
- Follows: [SIN-63337](/SIN/issues/SIN-63337) (F11 — `/admin/2fa/*` orphaned), [SIN-63338](/SIN/issues/SIN-63338) (urgent wireup fix), [SIN-63303](/SIN/issues/SIN-63303) (prior incident — `/static/` FileServer unmounted)
- Lenses: **Defense in depth**, **Observability before optimization**

## Context

We have shipped at least two production gaps in the past two weeks where an
`internal/adapter/httpapi/<X>` package was fully implemented, fully unit-tested
via `httptest`, and yet **completely unreachable** from the running binary:

- **SIN-63303** (2026-05-19) — the `/static/` FileServer mount was removed
  from the production router. Handler code intact; routes 404 in staging.
- **SIN-63337 / F11** (2026-05-26) — the tenant 2FA admin handler
  (`/admin/2fa/*`) and the `iam/login.go` mfa-pending branch were never
  imported from `cmd/server`. The `internal/adapter/httpapi/usermfa` package
  passed every unit test in CI, but the production binary did not serve a
  single one of its routes. F11 was found by SecurityEngineer doing a manual
  re-sweep of Fase 6 PR1 — not by automation.

Both incidents share the same structural shape: a sub-package can compile,
pass `go vet`, pass `staticcheck`, pass `httptest`-based handler tests, and
still be unreachable from `main()` because no production `.go` file imports
it. CI is silent.

This class is invisible to every gate we have today. Unit tests build
`http.Handler` instances directly. The notenant / forbidimport / aicache
analyzers all operate on packages that ARE compiled — they have nothing
to say about a package that no one imports. Even `go build ./cmd/server`
is happy: the binary builds fine, it just lacks routes.

## Decision

### D1 — Add a small bash + `go list` lint that fails when any `internal/adapter/httpapi/<sub>` package is unreachable from the `cmd/server` production import closure.

The script lives at `scripts/check-adapter-wireup.sh`. Mechanism:

1. Enumerate every sub-package under `./internal/adapter/httpapi/...`,
   excluding the parent package itself (`internal/adapter/httpapi`, which
   contains `router.go` and is always wired by definition).
2. Compute the production closure with `go list -deps ./cmd/server/...`.
   This closure is the set of packages transitively reachable from the
   `cmd/server` `main` package; `-deps` deliberately excludes test-only
   imports, which is the property that exposes the F11 shape.
3. Fail with exit 1 and a clear pointer-to-offender message if any
   sub-package is not in the closure and not in the allowlist.
4. Print an `allowlisted (orphan permitted)` line for every entry the
   allowlist suppressed, so the lint is auditable.

The script is wired into:

- `make lint-adapter-wireup` (new target).
- `make lint` (so local `make lint` catches the failure before push).
- `.github/workflows/ci.yml` job `lint` (so PRs and pushes to `main` fail
  before tests run).

### D2 — Allowlist via `scripts/adapter-wireup-allowlist.txt`, one full import path per line, with a one-line `#` comment explaining why each entry is acceptable.

A package may be legitimately unreachable from `cmd/server` at a point in
time (parked behind a feature flag, exposed for composition only, etc.).
The allowlist is the documented escape hatch:

```
# Future-dated handler — flag SIN-XXXXX scheduled for 2026-06-15, owner @user.
github.com/pericles-luz/crm/internal/adapter/httpapi/future_handler
```

Each entry must name a tracking issue or owner. The file is intentionally
empty today (every existing sub-package is reachable); we keep the file
in-repo so the lint and the allowlist mechanism ship together.

### D3 — Out of scope: other adapter trees, E2E router tests.

The first pass is narrow on purpose. Other `internal/adapter/*` sub-trees
(db, channel, message broker) have different wireup contracts — a db adapter
is reached from a use-case package via a port, not from `cmd/server`
directly — and would need different rules. We will revisit if this class
of bug recurs there.

E2E router tests (boot the server, hit each declared route) are a
complementary defense, not a replacement: they catch a different failure
mode (handler mounted but with wrong path, wrong middleware stack) and
they run AFTER the lint. The lint is cheap (~1s) and runs before tests
even start, so it pays even when the route tests would have caught the
same bug.

### D4 — Boring-tech choice: bash + `go list`, not a custom `go/analysis` analyzer.

The three options considered in [SIN-63339](/SIN/issues/SIN-63339):

1. Custom `go/analysis` linter integrated into `make lint`. Idiomatic but
   medium implementation cost.
2. Bash + `go list` script with an allowlist file.
3. `go vet -tags=wireuplint` with a tiny custom checker.

Option 2 is the boring-tech choice. The check is exactly "is this package
in `go list -deps ./cmd/server/...`?" which is one sort + grep against
`go list` output. A `go/analysis` analyzer would re-implement that scan
inside the Go analyzer framework with more LOC and no behavioural gain.
The script's failure message is auditable from the terminal; the allowlist
is a plain text file an operator can read. If we ever generalize to other
adapter trees (D3), we re-evaluate.

## Consequences

### Positive

- Two-incident-pattern (SIN-63303, SIN-63337) is now structurally
  impossible without a positive entry in the allowlist.
- Catches the gap **before** tests start, so the failure mode is
  diagnosed in seconds rather than in a SecurityEngineer manual re-sweep.
- Adds a documented allowlist contract — engineers know exactly how to
  ship a deliberately-orphan package (write the lint, write the
  allowlist entry, name the issue).

### Negative / risk

- One more lint to maintain. Mitigated by the script being ~80 lines
  of `bash` with no external deps beyond `go list`.
- False positives are possible if a future sub-package is deliberately
  unimported (e.g. a `tools/`-style helper). The allowlist absorbs that
  cost: add the entry, document why, move on.
- The lint runs `go list -deps ./cmd/server/...`, which is the same
  command the build already runs; cost is amortized on CI runners.

### Reversibility

- The lint is one bash script + one allowlist file + one Make target +
  one CI step. Revert is two `git revert` commits at most. The
  allowlist file format is plain text, so partial relaxation (skip a
  single package while we debug a flake) is a one-line PR.

## Verification

The acceptance criterion from [SIN-63339](/SIN/issues/SIN-63339) is
reproduced in the script's test plan:

1. On `main` at HEAD (every sub-package reachable, allowlist empty):

   ```
   $ ./scripts/check-adapter-wireup.sh
   check-adapter-wireup: ok — 9 sub-packages under internal/adapter/httpapi all reachable from cmd/server
   ```

2. After temporarily neutralising the SIN-63338 wireup (e.g. moving
   `cmd/server/usermfa_wire.go` out of the tree):

   ```
   $ ./scripts/check-adapter-wireup.sh
   check-adapter-wireup: FAIL — the following internal/adapter/httpapi/* packages are NOT reachable
   from cmd/server (production import graph). Their handlers will compile and unit-test cleanly, but
   the running binary will not serve any of their routes (F11 / SIN-63337 failure shape):

     - github.com/pericles-luz/crm/internal/adapter/httpapi/usermfa
   ; exit 1
   ```

3. With `usermfa` added to `scripts/adapter-wireup-allowlist.txt`, the
   same neutralised state produces an `allowlisted (orphan permitted)`
   line and exit 0. This is the documented escape hatch.

Both paths were exercised on this branch during implementation.
