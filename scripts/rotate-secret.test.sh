#!/usr/bin/env bash
# shellcheck disable=SC2030,SC2031
# SC2030/SC2031: every test runs in a subshell on purpose so the lib
# script's globals don't leak across cases. The "modification is local
# to subshell" / "modification might be lost" warnings are noise for
# this pattern — we WANT subshell isolation.
#
# scripts/rotate-secret.test.sh — unit tests for the pure-bash helpers
# inside scripts/rotate-secret.sh (SIN-63189).
#
# The DB / docker side-effects are gated behind --dry-run in the
# library; this test file covers the parts that can run without
# Postgres or Docker — argument parsing, name classification, audit
# payload composition, env-file update, redact, password generation,
# and rollback restoration.
#
# Usage: scripts/rotate-secret.test.sh

set -uo pipefail

cd "$(dirname "$0")/.." || exit 1

SCRIPT="scripts/rotate-secret.sh"

# shellcheck source=scripts/rotate-secret.sh
ROTATE_SECRET_LIB_ONLY=1 source "$SCRIPT"

failures=0
pass() { echo "PASS  $1"; }
fail() { echo "FAIL  $1"; failures=$((failures+1)); }

# ----------------------------------------------------------------------
# classify_name
# ----------------------------------------------------------------------

t_classify_scripted() {
	local n
	for n in db:app_runtime db:app_admin db:app_master_ops marker:campaigns; do
		local got
		got=$(classify_name "$n")
		if [[ "$got" != "scripted" ]]; then
			fail "classify_name($n) = $got, want scripted"
			return
		fi
	done
	pass "classify_name scripted set"
}

t_classify_delegated() {
	local n
	for n in openrouter pagarme slack-alerts; do
		local got
		got=$(classify_name "$n")
		if [[ "$got" != "delegated" ]]; then
			fail "classify_name($n) = $got, want delegated"
			return
		fi
	done
	pass "classify_name delegated set"
}

t_classify_manual_and_unknown() {
	local got
	got=$(classify_name "backup-encryption")
	[[ "$got" == "manual" ]] || { fail "classify_name(backup-encryption) = $got, want manual"; return; }
	got=$(classify_name "totally-not-a-secret")
	[[ "$got" == "unknown" ]] || { fail "classify_name(unknown) = $got, want unknown"; return; }
	pass "classify_name manual + unknown"
}

# ----------------------------------------------------------------------
# audit_payload — must include name + phase, NEVER the value
# ----------------------------------------------------------------------

t_audit_payload_shape() {
	local got
	got=$(audit_payload "db:app_runtime" "started")
	[[ "$got" == '{"secret":"db:app_runtime","phase":"started"}' ]] \
		|| { fail "audit_payload basic = $got"; return; }
	got=$(audit_payload "marker:campaigns" "completed" ',"dual_window_expires_at":"2026-08-21T00:00:00Z"')
	[[ "$got" == '{"secret":"marker:campaigns","phase":"completed","dual_window_expires_at":"2026-08-21T00:00:00Z"}' ]] \
		|| { fail "audit_payload with extra = $got"; return; }
	pass "audit_payload shape"
}

t_audit_payload_never_includes_value() {
	# The function takes only (name, phase, extra) — there is no value
	# parameter to leak. Test that calling it with a "fake value extra"
	# only echoes what we explicitly passed (no environmental leakage).
	local got
	got=$(audit_payload "openrouter" "started")
	if [[ "$got" == *"REDACTED"* || "$got" == *"password"* || "$got" == *"secret_value"* ]]; then
		fail "audit_payload leaks token-shaped substring: $got"
		return
	fi
	pass "audit_payload never includes value"
}

# ----------------------------------------------------------------------
# redact — empty input emits nothing; non-empty becomes REDACTED.
# ----------------------------------------------------------------------

t_redact() {
	local got
	got=$(redact "")
	[[ -z "$got" ]] || { fail "redact empty = '$got'"; return; }
	got=$(redact "hunter2")
	[[ "$got" == "***REDACTED***" ]] || { fail "redact nonempty = $got"; return; }
	# Even a single space counts as non-empty -> must redact, otherwise
	# a key that happens to start with whitespace would leak.
	got=$(redact " ")
	[[ "$got" == "***REDACTED***" ]] || { fail "redact whitespace = $got"; return; }
	pass "redact"
}

# ----------------------------------------------------------------------
# validate_actor_uuid
# ----------------------------------------------------------------------

t_validate_actor_uuid() {
	validate_actor_uuid "11111111-2222-3333-4444-555555555555" \
		|| { fail "validate_actor_uuid rejected a valid uuid"; return; }
	! validate_actor_uuid "not-a-uuid" \
		|| { fail "validate_actor_uuid accepted garbage"; return; }
	! validate_actor_uuid "" \
		|| { fail "validate_actor_uuid accepted empty"; return; }
	! validate_actor_uuid "11111111-2222-3333-4444" \
		|| { fail "validate_actor_uuid accepted short uuid"; return; }
	pass "validate_actor_uuid"
}

# ----------------------------------------------------------------------
# gen_password — CSPRNG output is non-empty, base64-ish, deterministic
# only in length (32 bytes -> 44 chars base64).
# ----------------------------------------------------------------------

t_gen_password() {
	local p
	p=$(gen_password)
	# base64 of 32 random bytes = 44 chars (with '=' padding).
	if (( ${#p} < 40 )); then
		fail "gen_password too short: ${#p}"; return
	fi
	# Two consecutive calls MUST differ — CSPRNG, not deterministic.
	local p2
	p2=$(gen_password)
	if [[ "$p" == "$p2" ]]; then
		fail "gen_password returned same value twice — not CSPRNG"; return
	fi
	pass "gen_password"
}

# ----------------------------------------------------------------------
# update_env_file (dry-run) — must not write anything, must redact in log.
# ----------------------------------------------------------------------

t_update_env_file_dry_run_does_not_write() {
	local tmp; tmp=$(mktemp)
	printf 'EXISTING=keep-me\nFOO=bar\n' >"$tmp"
	local before; before=$(cat "$tmp")
	(
		CRM_DEPLOY_ENV_FILE="$tmp"
		MODE_DRY_RUN=1
		update_env_file "FOO" "new-value-secret" 2>/dev/null
	)
	local after; after=$(cat "$tmp")
	if [[ "$before" != "$after" ]]; then
		fail "dry-run wrote to env file: '$before' -> '$after'"
		rm -f "$tmp"
		return
	fi
	rm -f "$tmp"
	pass "update_env_file dry-run is no-op"
}

# ----------------------------------------------------------------------
# update_env_file (real) — replaces existing key, preserves others,
# appends if missing, writes a .prev backup.
# ----------------------------------------------------------------------

t_update_env_file_replaces_existing_key() {
	local tmp; tmp=$(mktemp)
	printf 'EXISTING=keep-me\nFOO=bar\nLAST=tail\n' >"$tmp"
	(
		CRM_DEPLOY_ENV_FILE="$tmp"
		MODE_DRY_RUN=0
		update_env_file "FOO" "new-value" >/dev/null 2>&1
	)
	# FOO replaced, other lines kept, file present.
	if ! grep -qE '^FOO=new-value$' "$tmp"; then
		fail "FOO not replaced: $(cat "$tmp")"
		rm -f "$tmp" "${tmp}.prev"
		return
	fi
	if ! grep -qE '^EXISTING=keep-me$' "$tmp"; then
		fail "EXISTING was lost: $(cat "$tmp")"
		rm -f "$tmp" "${tmp}.prev"
		return
	fi
	if ! grep -qE '^LAST=tail$' "$tmp"; then
		fail "LAST was lost: $(cat "$tmp")"
		rm -f "$tmp" "${tmp}.prev"
		return
	fi
	# .prev created with old contents.
	if ! grep -qE '^FOO=bar$' "${tmp}.prev"; then
		fail ".prev missing old FOO=bar: $(cat "${tmp}.prev" 2>/dev/null)"
		rm -f "$tmp" "${tmp}.prev"
		return
	fi
	rm -f "$tmp" "${tmp}.prev"
	pass "update_env_file replaces existing + writes .prev"
}

t_update_env_file_appends_missing_key() {
	local tmp; tmp=$(mktemp)
	printf 'EXISTING=keep-me\n' >"$tmp"
	(
		CRM_DEPLOY_ENV_FILE="$tmp"
		MODE_DRY_RUN=0
		update_env_file "NEW_KEY" "new-value" >/dev/null 2>&1
	)
	if ! grep -qE '^NEW_KEY=new-value$' "$tmp"; then
		fail "NEW_KEY not appended: $(cat "$tmp")"
		rm -f "$tmp" "${tmp}.prev"
		return
	fi
	rm -f "$tmp" "${tmp}.prev"
	pass "update_env_file appends missing key"
}

# ----------------------------------------------------------------------
# restore_env_file — .prev → file, idempotent if no .prev exists.
# ----------------------------------------------------------------------

t_restore_env_file_round_trip() {
	local tmp; tmp=$(mktemp)
	printf 'FOO=old\n' >"$tmp"
	(
		CRM_DEPLOY_ENV_FILE="$tmp"
		MODE_DRY_RUN=0
		update_env_file "FOO" "new" >/dev/null 2>&1
		restore_env_file >/dev/null 2>&1
	)
	if ! grep -qE '^FOO=old$' "$tmp"; then
		fail "restore did not bring FOO back to 'old': $(cat "$tmp")"
		rm -f "$tmp" "${tmp}.prev"
		return
	fi
	rm -f "$tmp" "${tmp}.prev"
	pass "restore_env_file round-trip"
}

t_restore_env_file_missing_prev_is_warn() {
	local tmp; tmp=$(mktemp)
	printf 'FOO=intact\n' >"$tmp"
	(
		CRM_DEPLOY_ENV_FILE="$tmp"
		MODE_DRY_RUN=0
		restore_env_file 2>/dev/null
	) >/dev/null
	if ! grep -qE '^FOO=intact$' "$tmp"; then
		fail "restore with no .prev should be no-op; file changed: $(cat "$tmp")"
		rm -f "$tmp"
		return
	fi
	rm -f "$tmp"
	pass "restore_env_file no-prev is no-op"
}

# ----------------------------------------------------------------------
# name_is_in
# ----------------------------------------------------------------------

t_name_is_in() {
	name_is_in "foo" "foo" "bar" || { fail "name_is_in foo in [foo bar] should pass"; return; }
	! name_is_in "baz" "foo" "bar" || { fail "name_is_in baz in [foo bar] should fail"; return; }
	pass "name_is_in"
}

# ----------------------------------------------------------------------
# write_secret_tempfile — mode 0600, contents preserved exactly.
# ----------------------------------------------------------------------

t_write_secret_tempfile() {
	local f
	f=$(printf 'hunter2-secret' | write_secret_tempfile)
	if [[ ! -f "$f" ]]; then
		fail "write_secret_tempfile returned non-existent path: $f"; return
	fi
	local perms
	perms=$(stat -c '%a' "$f" 2>/dev/null || stat -f '%Lp' "$f")
	if [[ "$perms" != "600" ]]; then
		fail "write_secret_tempfile perms = $perms, want 600"
		rm -f "$f"; return
	fi
	local got; got=$(cat "$f")
	if [[ "$got" != "hunter2-secret" ]]; then
		fail "write_secret_tempfile contents wrong: $got"
		rm -f "$f"; return
	fi
	rm -f "$f"
	pass "write_secret_tempfile"
}

# ----------------------------------------------------------------------
# Argument parsing — usage errors return 64.
# ----------------------------------------------------------------------

t_parse_args_missing_name() {
	local rc=0
	# Run in a fresh subshell so the lib script's globals don't leak across cases.
	(
		# shellcheck source=scripts/rotate-secret.sh
		ROTATE_SECRET_LIB_ONLY=1 source "$SCRIPT"
		parse_args
	) >/dev/null 2>&1 || rc=$?
	if [[ "$rc" -ne 64 ]]; then
		fail "parse_args missing name = $rc, want 64"; return
	fi
	pass "parse_args missing name returns 64"
}

t_parse_args_unknown_flag() {
	local rc=0
	(
		# shellcheck source=scripts/rotate-secret.sh
		ROTATE_SECRET_LIB_ONLY=1 source "$SCRIPT"
		parse_args --bogus foo
	) >/dev/null 2>&1 || rc=$?
	if [[ "$rc" -ne 64 ]]; then
		fail "parse_args unknown flag = $rc, want 64"; return
	fi
	pass "parse_args unknown flag returns 64"
}

t_parse_args_happy() {
	if ! (
		# shellcheck source=scripts/rotate-secret.sh
		ROTATE_SECRET_LIB_ONLY=1 source "$SCRIPT"
		parse_args db:app_runtime --dry-run
		# SC2031: SECRET_NAME / MODE_DRY_RUN are set in this subshell by parse_args
		# and only read in the same subshell — the warning does not apply.
		# shellcheck disable=SC2031
		if [[ "$SECRET_NAME" != "db:app_runtime" || "$MODE_DRY_RUN" -ne 1 ]]; then
			exit 1
		fi
	); then
		fail "parse_args happy path did not set globals"
		return
	fi
	pass "parse_args happy path"
}

# ----------------------------------------------------------------------
# Driver entrypoints — manual / delegated / unknown classifications.
# Run the real main() in a sandbox so we can assert on exit codes.
# ----------------------------------------------------------------------

run_main_in_sandbox() {
	local rc=0
	# shellcheck disable=SC2031
	(
		# Strip the lib-only guard so main() runs.
		unset ROTATE_SECRET_LIB_ONLY
		set +u
		bash "$SCRIPT" "$@"
	) >/dev/null 2>&1 || rc=$?
	echo "$rc"
}

t_main_manual_only_secret_exits_65() {
	local rc
	rc=$(CRM_OPS_ACTOR_USER_ID="" run_main_in_sandbox "backup-encryption")
	if [[ "$rc" -ne 65 ]]; then
		fail "main backup-encryption = $rc, want 65"; return
	fi
	pass "main backup-encryption exits 65"
}

t_main_delegated_secret_exits_64() {
	local rc
	rc=$(CRM_OPS_ACTOR_USER_ID="" run_main_in_sandbox "openrouter")
	if [[ "$rc" -ne 64 ]]; then
		fail "main openrouter = $rc, want 64"; return
	fi
	pass "main openrouter exits 64"
}

t_main_unknown_secret_exits_64() {
	local rc
	rc=$(CRM_OPS_ACTOR_USER_ID="" run_main_in_sandbox "not-a-thing")
	if [[ "$rc" -ne 64 ]]; then
		fail "main not-a-thing = $rc, want 64"; return
	fi
	pass "main unknown secret exits 64"
}

t_main_missing_actor_exits_69() {
	# Pick a scripted name so we get past the classification check.
	local rc=0
	(
		unset ROTATE_SECRET_LIB_ONLY CRM_OPS_ACTOR_USER_ID
		bash "$SCRIPT" "db:app_runtime"
	) >/dev/null 2>&1 || rc=$?
	if [[ "$rc" -ne 69 ]]; then
		fail "main missing actor = $rc, want 69"
		return
	fi
	pass "main missing CRM_OPS_ACTOR_USER_ID exits 69"
}

t_main_bad_actor_uuid_exits_64() {
	local rc=0
	(
		unset ROTATE_SECRET_LIB_ONLY
		CRM_OPS_ACTOR_USER_ID="not-a-uuid" bash "$SCRIPT" "db:app_runtime"
	) >/dev/null 2>&1 || rc=$?
	if [[ "$rc" -ne 64 ]]; then
		fail "main bad uuid = $rc, want 64"
		return
	fi
	pass "main bad actor uuid exits 64"
}

# ----------------------------------------------------------------------
# Drive every test in source order and exit non-zero if any failed.
# ----------------------------------------------------------------------

t_classify_scripted
t_classify_delegated
t_classify_manual_and_unknown
t_audit_payload_shape
t_audit_payload_never_includes_value
t_redact
t_validate_actor_uuid
t_gen_password
t_update_env_file_dry_run_does_not_write
t_update_env_file_replaces_existing_key
t_update_env_file_appends_missing_key
t_restore_env_file_round_trip
t_restore_env_file_missing_prev_is_warn
t_name_is_in
t_write_secret_tempfile
t_parse_args_missing_name
t_parse_args_unknown_flag
t_parse_args_happy
t_main_manual_only_secret_exits_65
t_main_delegated_secret_exits_64
t_main_unknown_secret_exits_64
t_main_missing_actor_exits_69
t_main_bad_actor_uuid_exits_64

if (( failures > 0 )); then
	printf '\n%d test(s) FAILED\n' "$failures" >&2
	exit 1
fi
printf '\nALL TESTS PASSED\n'
