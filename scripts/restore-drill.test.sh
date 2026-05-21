#!/usr/bin/env bash
# scripts/restore-drill.test.sh — unit tests for the pure-bash helpers
# inside scripts/restore-drill.sh (SIN-63187).
#
# The Docker-driven parts of the drill are exercised end-to-end by the
# `.github/workflows/restore-drill.yml` job (synthetic mode on a hot
# GHA runner). This file covers the parts that can be tested without
# Docker — argument parsing, time math, verdict logic, synthetic-backup
# generation, and report rendering.
#
# Usage: scripts/restore-drill.test.sh

set -uo pipefail

cd "$(dirname "$0")/.."

SCRIPT="scripts/restore-drill.sh"

# Source the library helpers without executing main.
# shellcheck disable=SC1090
DRILL_LIB_ONLY=1 source "$SCRIPT"

failures=0
pass() { echo "PASS  $1"; }
fail() { echo "FAIL  $1"; failures=$((failures+1)); }

# ----------------------------------------------------------------------
# format_duration
# ----------------------------------------------------------------------

t_format_duration() {
	local got
	got=$(format_duration 0);     [[ "$got" == "0h 00m 00s" ]] || { fail "format_duration 0=$got"; return; }
	got=$(format_duration 59);    [[ "$got" == "0h 00m 59s" ]] || { fail "format_duration 59=$got"; return; }
	got=$(format_duration 60);    [[ "$got" == "0h 01m 00s" ]] || { fail "format_duration 60=$got"; return; }
	got=$(format_duration 3661);  [[ "$got" == "1h 01m 01s" ]] || { fail "format_duration 3661=$got"; return; }
	got=$(format_duration 14400); [[ "$got" == "4h 00m 00s" ]] || { fail "format_duration 14400=$got"; return; }
	# Negative should clamp to zero, not print a negative-looking string.
	got=$(format_duration -5);    [[ "$got" == "0h 00m 00s" ]] || { fail "format_duration -5=$got"; return; }
	pass "format_duration"
}

# ----------------------------------------------------------------------
# verdict_for_budget
# ----------------------------------------------------------------------

t_verdict_for_budget() {
	local got
	got=$(verdict_for_budget 10 100);    [[ "$got" == "PASS" ]] || { fail "verdict 10/100=$got"; return; }
	got=$(verdict_for_budget 100 100);   [[ "$got" == "PASS" ]] || { fail "verdict 100/100=$got (edge equality must pass)"; return; }
	got=$(verdict_for_budget 101 100);   [[ "$got" == "FAIL" ]] || { fail "verdict 101/100=$got"; return; }
	got=$(verdict_for_budget 0 0);       [[ "$got" == "PASS" ]] || { fail "verdict 0/0=$got"; return; }
	pass "verdict_for_budget"
}

# ----------------------------------------------------------------------
# iso_to_epoch round-trip with GNU date
# ----------------------------------------------------------------------

t_iso_to_epoch() {
	local got
	got=$(iso_to_epoch "1970-01-01T00:00:00Z")
	[[ "$got" == "0" ]] || { fail "iso_to_epoch epoch=$got (want 0)"; return; }
	got=$(iso_to_epoch "2026-05-21T00:00:00Z")
	[[ "$got" =~ ^[0-9]+$ ]] || { fail "iso_to_epoch 2026=$got (want digits)"; return; }
	(( got > 1700000000 )) || { fail "iso_to_epoch 2026=$got (want > 2023)"; return; }
	pass "iso_to_epoch"
}

# ----------------------------------------------------------------------
# synthesize_pg_dump produces valid SQL with the canary table
# ----------------------------------------------------------------------

t_synthesize_pg_dump() {
	local tmp; tmp=$(mktemp)
	synthesize_pg_dump "$tmp"
	if grep -q "drill_canary" "$tmp" && grep -q "CREATE TABLE" "$tmp"; then
		pass "synthesize_pg_dump"
	else
		fail "synthesize_pg_dump missing canary/CREATE TABLE"
	fi
	rm -f "$tmp"
}

# ----------------------------------------------------------------------
# synthesize_minio_snapshot creates the media/ tree
# ----------------------------------------------------------------------

t_synthesize_minio_snapshot() {
	local tmp; tmp=$(mktemp -d)
	synthesize_minio_snapshot "$tmp"
	if [[ -f "$tmp/media/canary.txt" ]]; then
		pass "synthesize_minio_snapshot"
	else
		fail "synthesize_minio_snapshot missing media/canary.txt"
	fi
	rm -rf "$tmp"
}

# ----------------------------------------------------------------------
# write_report renders all required sections + the verdict matches
# ----------------------------------------------------------------------

t_write_report_passing() {
	local tmpdir; tmpdir=$(mktemp -d)
	REPORT_DIR="$tmpdir" REPORT_DATE="2026-05-21" RTO_BUDGET_SECONDS=14400 RPO_BUDGET_SECONDS=86400 \
		write_report \
			"2026-05-21T00:00:00Z" "2026-05-21T00:04:30Z" \
			270 0 \
			"pg/2026-05-21.sql" "2026-05-21T00:00:00Z" \
			"minio/2026-05-21.tar.gz" "2026-05-21T00:00:00Z" \
			"42" "synthetic (CI)" >/dev/null
	local path="$tmpdir/restore-drill-report-2026-05-21.md"
	if [[ ! -f "$path" ]]; then
		fail "write_report did not create $path"
		rm -rf "$tmpdir"
		return
	fi
	local must_contain=(
		"# Restore drill report — 2026-05-21"
		"synthetic (CI)"
		"Overall verdict: **PASS**"
		"0h 04m 30s (270s)"
		"4h 00m 00s (14400s)"
		"\`pg/2026-05-21.sql\`"
		"\`minio/2026-05-21.tar.gz\`"
		"returned **42** row(s)"
		"SIN-63187"
	)
	local missing=""
	for s in "${must_contain[@]}"; do
		grep -qF "$s" "$path" || missing+="\n  missing: $s"
	done
	if [[ -z "$missing" ]]; then
		pass "write_report (passing)"
	else
		fail "write_report (passing) -$missing"
	fi
	rm -rf "$tmpdir"
}

t_write_report_breach() {
	# Force RTO breach: measured > budget.
	local tmpdir; tmpdir=$(mktemp -d)
	REPORT_DIR="$tmpdir" REPORT_DATE="2026-05-21" RTO_BUDGET_SECONDS=100 RPO_BUDGET_SECONDS=100 \
		write_report \
			"2026-05-21T00:00:00Z" "2026-05-21T03:00:00Z" \
			500 200 \
			"pg/old.sql" "2026-05-20T00:00:00Z" \
			"minio/old.tar.gz" "2026-05-20T00:00:00Z" \
			"0" "real (S3 vault)" >/dev/null
	local path="$tmpdir/restore-drill-report-2026-05-21.md"
	if grep -q "Overall verdict: \*\*FAIL\*\*" "$path" && \
	   grep -q "Verdict          | \*\*FAIL\*\*" "$path"; then
		pass "write_report (breach)"
	else
		fail "write_report (breach) — expected FAIL verdict in report"
		echo "--- report ---"
		cat "$path"
		echo "--------------"
	fi
	rm -rf "$tmpdir"
}

# ----------------------------------------------------------------------
# argument parsing — flags toggle the globals
# ----------------------------------------------------------------------

t_parse_args_synthetic() {
	MODE_SYNTHETIC=0 MODE_DRY_RUN=0 KEEP_STACK=0
	parse_args --synthetic
	if [[ "$MODE_SYNTHETIC" == "1" && "$MODE_DRY_RUN" == "0" && "$KEEP_STACK" == "0" ]]; then
		pass "parse_args --synthetic"
	else
		fail "parse_args --synthetic produced synth=$MODE_SYNTHETIC dry=$MODE_DRY_RUN keep=$KEEP_STACK"
	fi
}

t_parse_args_combined() {
	MODE_SYNTHETIC=0 MODE_DRY_RUN=0 KEEP_STACK=0
	parse_args --synthetic --dry-run --keep --report-date 2099-12-31
	if [[ "$MODE_SYNTHETIC" == "1" && "$MODE_DRY_RUN" == "1" && "$KEEP_STACK" == "1" && "$REPORT_DATE" == "2099-12-31" ]]; then
		pass "parse_args (combined)"
	else
		fail "parse_args (combined) — REPORT_DATE=$REPORT_DATE synth=$MODE_SYNTHETIC dry=$MODE_DRY_RUN keep=$KEEP_STACK"
	fi
}

t_parse_args_rejects_unknown() {
	MODE_SYNTHETIC=0 MODE_DRY_RUN=0 KEEP_STACK=0
	local rc=0
	(parse_args --no-such-flag) 2>/dev/null || rc=$?
	if [[ "$rc" == "1" ]]; then
		pass "parse_args rejects unknown flag"
	else
		fail "parse_args rejects unknown flag — got rc=$rc"
	fi
}

# ----------------------------------------------------------------------
# end-to-end: invoking the script with --dry-run never touches docker
# ----------------------------------------------------------------------

t_dry_run_no_docker() {
	# Stub `docker` by prepending a PATH dir that fails if docker is called.
	local stubdir; stubdir=$(mktemp -d)
	cat >"$stubdir/docker" <<'EOF'
#!/usr/bin/env bash
echo "docker stub: must not be invoked during --dry-run (got: $*)" >&2
exit 99
EOF
	chmod +x "$stubdir/docker"
	if PATH="$stubdir:$PATH" bash "$SCRIPT" --dry-run >/dev/null 2>&1; then
		pass "dry-run does not invoke docker"
	else
		fail "dry-run invoked docker (or otherwise failed)"
	fi
	rm -rf "$stubdir"
}

# ----------------------------------------------------------------------
# Run all
# ----------------------------------------------------------------------

t_format_duration
t_verdict_for_budget
t_iso_to_epoch
t_synthesize_pg_dump
t_synthesize_minio_snapshot
t_write_report_passing
t_write_report_breach
t_parse_args_synthetic
t_parse_args_combined
t_parse_args_rejects_unknown
t_dry_run_no_docker

if (( failures > 0 )); then
	echo
	echo "${failures} test(s) failed" >&2
	exit 1
fi
echo
echo "all tests passed"
