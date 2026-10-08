#!/usr/bin/env bash
# govulncheck-sweep.sh — out-of-PR weekly vulnerability scan (SIN-62251).
#
# Scope:
#   1. Run `govulncheck -json ./...` against the current tree.
#   2. Extract OSV ids (CVE/GHSA) of *called* findings only.
#   3. Diff against the baseline file `.govulncheck-known.txt` to compute
#      "new" and "resolved" sets.
#   4. Emit a single JSON report on stdout describing the diff plus a
#      per-library grouping that the calling agent can turn into Paperclip
#      child issues.
#
# This script does NOT call the Paperclip API and does NOT open a GitHub PR.
# Those are the responsibility of the agent handling the routine execution
# issue (SIN-62251) — that keeps secrets and idempotency-key logic out of
# bash and in the agent heartbeat where they belong.
#
# Subcommands (all consume govulncheck JSON on $1):
#   sweep               run govulncheck and emit the report (default)
#   extract-called      print sorted unique OSV ids from called findings
#   diff <cur> <base>   print "<status>\t<id>" with status in {new,resolved}
#   report <json>       emit the report from a pre-saved govulncheck JSON
#   validate <json> <rc> fail (non-zero) if <json>+<rc> indicate the scan did
#                       not complete (false-clean guard; SIN-67201)
#   help                print usage
#
# Why these subcommands? The routine itself only ever calls `sweep`. The
# others are factored out so `tools/supply-chain/test_govulncheck_sweep.sh`
# can drive them with fixture JSON instead of needing the real Go tool.

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
BASELINE_FILE="${REPO_ROOT}/.govulncheck-known.txt"

usage() {
  sed -n '2,29p' "$0" >&2
}

# ---- extract_called -----------------------------------------------------------
# `govulncheck -json` emits a stream of NDJSON records with one of these top-level
# keys: config, progress, osv, finding. A "called" finding has a `trace` array
# whose first frame includes a non-empty `function` field — that means
# govulncheck saw a call edge to vulnerable code. Findings without a function in
# the first trace frame are import-only / module-only and are intentionally
# skipped: they would otherwise page humans about vulns we never reach.
extract_called() {
  local json="$1"
  if [[ ! -f "${json}" ]]; then
    echo "extract-called: input not found: ${json}" >&2
    return 2
  fi
  jq -r '
    select(.finding != null)
    | .finding
    | select(.trace != null and (.trace | length) > 0)
    | select(.trace[0].function != null and .trace[0].function != "")
    | .osv
  ' "${json}" | sort -u
}

# ---- diff_baseline ------------------------------------------------------------
# `comm` requires both inputs to be sorted; the callers already sort.
# -23 keeps lines unique to file 1 (= new ids).
# -13 keeps lines unique to file 2 (= resolved ids).
diff_baseline() {
  local current="$1" baseline="$2"
  if [[ ! -f "${current}" ]]; then
    echo "diff: current ids file not found: ${current}" >&2
    return 2
  fi
  local base_input="${baseline}"
  if [[ ! -f "${baseline}" ]]; then
    base_input="/dev/null"  # treat missing baseline as empty
  fi
  # Defensive: pre-sort inputs so callers don't have to.
  local cur_sorted base_sorted
  cur_sorted="$(mktemp)"
  base_sorted="$(mktemp)"
  sort -u "${current}"     > "${cur_sorted}"
  sort -u "${base_input}"  > "${base_sorted}"
  comm -23 "${cur_sorted}" "${base_sorted}" | awk 'NF{print "new\t"$0}'
  comm -13 "${cur_sorted}" "${base_sorted}" | awk 'NF{print "resolved\t"$0}'
  rm -f "${cur_sorted}" "${base_sorted}"
}

# ---- group_by_library ---------------------------------------------------------
# For each new OSV id, look up its affected packages in the govulncheck JSON
# and emit "<library>\t<id>\t<severity>". The agent then groups by library
# (one issue per library, highest-severity id wins) and applies the per-run cap.
#
# Severity preference: the OSV record's `database_specific.severity` (a string
# like "CRITICAL") takes precedence; falls back to the `severity[0].score`
# CVSS string. Missing values become "UNKNOWN".
group_by_library() {
  local json="$1" ids_file="$2"
  if [[ ! -f "${json}" || ! -f "${ids_file}" ]]; then
    echo "group-by-library: missing input(s): ${json} or ${ids_file}" >&2
    return 2
  fi
  # Build a small jq script that joins osv records with the requested ids.
  jq -r --slurpfile wanted <(jq -R '.' "${ids_file}" | jq -s '.') '
    select(.osv != null)
    | .osv as $o
    | select(($wanted[0] // []) | index($o.id))
    | (
        $o.database_specific.severity //
        ($o.severity[0].score // "UNKNOWN")
      ) as $sev
    | ($o.affected // []) | .[]?
    | [(.package.name // "unknown"), $o.id, $sev] | @tsv
  ' "${json}" | sort -u
}

# ---- emit_report --------------------------------------------------------------
# Emits a single JSON document collecting:
#   - new ids (with library + severity rows)
#   - resolved ids (just the id list)
#   - generated_at ISO timestamp
# The shape is stable so the agent can consume it deterministically.
emit_report() {
  local json="$1"
  local current_ids new_ids resolved_ids grouping
  current_ids="$(mktemp)"
  new_ids="$(mktemp)"
  resolved_ids="$(mktemp)"
  grouping="$(mktemp)"

  extract_called "${json}" > "${current_ids}"
  diff_baseline "${current_ids}" "${BASELINE_FILE}" \
    | awk -F'\t' '$1=="new"{print $2}' > "${new_ids}"
  diff_baseline "${current_ids}" "${BASELINE_FILE}" \
    | awk -F'\t' '$1=="resolved"{print $2}' > "${resolved_ids}"

  if [[ -s "${new_ids}" ]]; then
    group_by_library "${json}" "${new_ids}" > "${grouping}"
  else
    : > "${grouping}"
  fi

  jq -n \
    --arg generated_at "$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
    --rawfile current "${current_ids}" \
    --rawfile new "${new_ids}" \
    --rawfile resolved "${resolved_ids}" \
    --rawfile grouping "${grouping}" \
    '
    def lines(s): s | split("\n") | map(select(length > 0));
    def grouped(g):
      lines(g) | map(split("\t") | {library: .[0], osv: .[1], severity: .[2]});
    {
      generated_at: $generated_at,
      current_ids: lines($current),
      new_ids: lines($new),
      resolved_ids: lines($resolved),
      new_findings: grouped($grouping)
    }
    '

  rm -f "${current_ids}" "${new_ids}" "${resolved_ids}" "${grouping}"
}

# ---- validate_scan ------------------------------------------------------------
# Guardrail against a FALSE-CLEAN report (SIN-67201). govulncheck exits
# non-zero for TWO very different reasons:
#   (a) it found *called* vulnerabilities — expected; the stream contains
#       `finding` records and we still want the report; OR
#   (b) it could not load the packages at all (e.g. the govulncheck binary was
#       built with an older Go toolchain than the module's go/toolchain
#       directive, "package load error", "requires newer Go"). In this case it
#       writes an error to stderr and emits ZERO `finding` records — so
#       emit_report would happily produce a perfectly clean `{new_ids: []}` and
#       the unattended weekly routine would close "0 new" while real reachable
#       CVEs hid behind the load failure. Hit live during the SIN-67198 sweep
#       on 2026-07-13.
#
# We distinguish the two using the structure of the stream, not the exit code
# alone:
#   - A genuine run ALWAYS emits a `config` record first. No `config` record
#     means the scan never produced output → hard error.
#   - A non-zero exit is legitimate ONLY when it is accompanied by at least one
#     `finding` record. Non-zero + zero findings === case (b) → hard error.
# Fail loud (return 3) rather than let a clean-looking report through.
#
# Args: <govulncheck-json-stream> <govulncheck-exit-code>
validate_scan() {
  local json="$1" rc="${2:-0}"
  if [[ ! -f "${json}" ]]; then
    echo "validate: scan output not found: ${json}" >&2
    return 2
  fi

  # Structural completeness: a real scan emits a `config` record at the start.
  if ! jq -e 'select(.config != null) | .config' "${json}" >/dev/null 2>&1; then
    echo "validate: govulncheck emitted no 'config' record — scan did not complete (exit=${rc}); refusing to emit a clean report" >&2
    return 3
  fi

  # Non-zero exit is expected ONLY when called vulns were found (→ findings).
  # Non-zero with zero findings means a package-load / toolchain failure.
  if [[ "${rc}" -ne 0 ]]; then
    if ! jq -e 'select(.finding != null) | .finding' "${json}" >/dev/null 2>&1; then
      echo "validate: govulncheck exited ${rc} with zero 'finding' records — treating as a scan failure (likely package-load/toolchain error), not a clean result" >&2
      return 3
    fi
  fi
  return 0
}

# ---- run_sweep ----------------------------------------------------------------
# The default mode: install/locate govulncheck, run it, emit the report.
# Designed to be safe on a CI-like environment with Go installed — does not
# mutate anything in the working tree.
run_sweep() {
  local out err rc
  out="$(mktemp)"
  err="$(mktemp)"

  if ! command -v govulncheck >/dev/null 2>&1; then
    if ! command -v go >/dev/null 2>&1; then
      echo "sweep: neither govulncheck nor go are on PATH" >&2
      rm -f "${out}" "${err}"
      return 2
    fi
    GOFLAGS=-mod=readonly go install golang.org/x/vuln/cmd/govulncheck@latest >&2
    PATH="$(go env GOPATH)/bin:${PATH}"
  fi

  # `-mode source` matches the PR-time CI gate (.github/workflows/govulncheck.yml).
  # Capture the exit code separately — we must NOT blanket-swallow it with
  # `|| true` the way the original did, because that hid package-load failures
  # (see validate_scan / SIN-67201). govulncheck exits non-zero when it finds
  # called CVEs, so a non-zero exit is not by itself an error.
  set +e
  govulncheck -json -mode source ./... > "${out}" 2>"${err}"
  rc=$?
  set -e

  # Surface govulncheck's own diagnostics regardless of outcome.
  if [[ -s "${err}" ]]; then
    cat "${err}" >&2
  fi

  # Fail loud instead of emitting a false-clean report when the scan did not
  # actually complete.
  if ! validate_scan "${out}" "${rc}"; then
    rm -f "${out}" "${err}"
    return 3
  fi

  emit_report "${out}"
  rm -f "${out}" "${err}"
}

main() {
  local cmd="${1:-sweep}"
  case "${cmd}" in
    sweep)              run_sweep ;;
    extract-called)     shift; extract_called "$@" ;;
    diff)               shift; diff_baseline "$@" ;;
    report)             shift; emit_report "$@" ;;
    validate)           shift; validate_scan "$@" ;;
    group-by-library)   shift; group_by_library "$@" ;;
    -h|--help|help)     usage ;;
    *)                  echo "unknown subcommand: ${cmd}" >&2; usage; exit 2 ;;
  esac
}

main "$@"
