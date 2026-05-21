#!/usr/bin/env bash
# shellcheck disable=SC2030,SC2031
# SC2030/SC2031: every test runs in a subshell on purpose so the lib
# script's globals don't leak across cases. See the note in
# scripts/rotate-secret.test.sh.
#
# scripts/update-config-and-redeploy.test.sh — unit tests for the
# pure-bash helpers inside scripts/update-config-and-redeploy.sh
# (SIN-63189).
#
# Provider HTTP calls and docker redeploys are excluded — those are
# covered manually by the rotation runbook. This file covers
# argument parsing, supported-name table, env-key mapping, payload
# composition, env-file update, and redact.
#
# Usage: scripts/update-config-and-redeploy.test.sh

set -uo pipefail

cd "$(dirname "$0")/.." || exit 1

SCRIPT="scripts/update-config-and-redeploy.sh"

# shellcheck source=scripts/update-config-and-redeploy.sh
UPDATE_CONFIG_LIB_ONLY=1 source "$SCRIPT"

failures=0
pass() { echo "PASS  $1"; }
fail() { echo "FAIL  $1"; failures=$((failures+1)); }

# ----------------------------------------------------------------------
# is_supported_name + env_key_for
# ----------------------------------------------------------------------

t_env_key_for() {
	[[ "$(env_key_for openrouter)" == "OPENROUTER_API_KEY" ]] \
		|| { fail "env_key_for openrouter"; return; }
	[[ "$(env_key_for pagarme)" == "PAGARME_API_KEY" ]] \
		|| { fail "env_key_for pagarme"; return; }
	[[ "$(env_key_for slack-alerts)" == "SLACK_ALERTS_WEBHOOK_URL" ]] \
		|| { fail "env_key_for slack-alerts"; return; }
	if env_key_for "totally-bogus" 2>/dev/null; then
		fail "env_key_for bogus should fail"
		return
	fi
	pass "env_key_for"
}

t_is_supported_name() {
	is_supported_name openrouter   || { fail "openrouter rejected"; return; }
	is_supported_name pagarme      || { fail "pagarme rejected"; return; }
	is_supported_name slack-alerts || { fail "slack-alerts rejected"; return; }
	! is_supported_name backup-encryption || { fail "backup-encryption accepted"; return; }
	! is_supported_name db:app_runtime    || { fail "db:app_runtime accepted by wrong driver"; return; }
	pass "is_supported_name"
}

t_default_redeploy_for() {
	local got
	got=$(default_redeploy_for openrouter)
	[[ "$got" == *"--no-deps app"* ]] \
		|| { fail "openrouter redeploy = $got"; return; }
	got=$(default_redeploy_for slack-alerts)
	[[ "$got" == *"wallet-alerter-worker"* ]] \
		|| { fail "slack-alerts redeploy = $got"; return; }
	if default_redeploy_for "bogus" 2>/dev/null; then
		fail "default_redeploy_for bogus should fail"
		return
	fi
	pass "default_redeploy_for"
}

# ----------------------------------------------------------------------
# audit_payload — never includes value
# ----------------------------------------------------------------------

t_audit_payload_shape() {
	local got
	got=$(audit_payload "openrouter" "started")
	[[ "$got" == '{"secret":"openrouter","phase":"started"}' ]] \
		|| { fail "audit_payload basic = $got"; return; }
	got=$(audit_payload "slack-alerts" "failed" ',"reason":"validation_failed"')
	[[ "$got" == '{"secret":"slack-alerts","phase":"failed","reason":"validation_failed"}' ]] \
		|| { fail "audit_payload with extra = $got"; return; }
	pass "audit_payload shape"
}

# ----------------------------------------------------------------------
# redact + validate_actor_uuid (re-tested to lock the contract for
# this script too — they live in two scripts and MUST behave identically).
# ----------------------------------------------------------------------

t_redact() {
	[[ -z "$(redact "")" ]] || { fail "redact empty leaked"; return; }
	[[ "$(redact "sk-live-secret-key")" == "***REDACTED***" ]] \
		|| { fail "redact nonempty wrong"; return; }
	pass "redact"
}

t_validate_actor_uuid() {
	validate_actor_uuid "11111111-2222-3333-4444-555555555555" \
		|| { fail "valid uuid rejected"; return; }
	! validate_actor_uuid "openrouter-key-not-uuid" \
		|| { fail "non-uuid accepted"; return; }
	pass "validate_actor_uuid"
}

# ----------------------------------------------------------------------
# update_env_file (dry-run + real) — same contract as rotate-secret;
# duplicated here because both scripts ship their own copy of the
# helper (kept independent so the runbook works even when one script
# is missing).
# ----------------------------------------------------------------------

t_update_env_file_dry_run_does_not_write() {
	local tmp; tmp=$(mktemp)
	printf 'A=keep\nB=bar\n' >"$tmp"
	local before; before=$(cat "$tmp")
	(
		CRM_DEPLOY_ENV_FILE="$tmp"
		MODE_DRY_RUN=1
		update_env_file "B" "new" 2>/dev/null
	)
	local after; after=$(cat "$tmp")
	[[ "$before" == "$after" ]] \
		|| { fail "dry-run wrote: '$before' -> '$after'"; rm -f "$tmp"; return; }
	rm -f "$tmp"
	pass "update_env_file dry-run is no-op"
}

t_update_env_file_replaces_and_backs_up() {
	local tmp; tmp=$(mktemp)
	printf 'A=keep\nB=bar\n' >"$tmp"
	(
		CRM_DEPLOY_ENV_FILE="$tmp"
		MODE_DRY_RUN=0
		update_env_file "B" "new-val" >/dev/null 2>&1
	)
	grep -qE '^B=new-val$' "$tmp" \
		|| { fail "B not replaced: $(cat "$tmp")"; rm -f "$tmp" "${tmp}.prev"; return; }
	grep -qE '^A=keep$' "$tmp" \
		|| { fail "A lost: $(cat "$tmp")"; rm -f "$tmp" "${tmp}.prev"; return; }
	grep -qE '^B=bar$' "${tmp}.prev" \
		|| { fail ".prev missing old B"; rm -f "$tmp" "${tmp}.prev"; return; }
	rm -f "$tmp" "${tmp}.prev"
	pass "update_env_file replaces + writes .prev"
}

# ----------------------------------------------------------------------
# restore_env_file
# ----------------------------------------------------------------------

t_restore_env_file_round_trip() {
	local tmp; tmp=$(mktemp)
	printf 'K=old\n' >"$tmp"
	(
		CRM_DEPLOY_ENV_FILE="$tmp"
		MODE_DRY_RUN=0
		update_env_file "K" "new" >/dev/null 2>&1
		restore_env_file >/dev/null 2>&1
	)
	grep -qE '^K=old$' "$tmp" \
		|| { fail "restore did not bring K back: $(cat "$tmp")"; rm -f "$tmp" "${tmp}.prev"; return; }
	rm -f "$tmp" "${tmp}.prev"
	pass "restore_env_file round-trip"
}

# ----------------------------------------------------------------------
# Argument parsing
# ----------------------------------------------------------------------

t_parse_args_missing_name_exits_64() {
	local rc=0
	(
		# shellcheck source=scripts/update-config-and-redeploy.sh
		UPDATE_CONFIG_LIB_ONLY=1 source "$SCRIPT"
		parse_args
	) >/dev/null 2>&1 || rc=$?
	[[ "$rc" -eq 64 ]] || { fail "parse_args missing name = $rc"; return; }
	pass "parse_args missing name returns 64"
}

t_parse_args_unknown_flag_exits_64() {
	local rc=0
	(
		# shellcheck source=scripts/update-config-and-redeploy.sh
		UPDATE_CONFIG_LIB_ONLY=1 source "$SCRIPT"
		parse_args --nope openrouter
	) >/dev/null 2>&1 || rc=$?
	[[ "$rc" -eq 64 ]] || { fail "parse_args unknown flag = $rc"; return; }
	pass "parse_args unknown flag returns 64"
}

t_parse_args_happy() {
	if ! (
		# shellcheck source=scripts/update-config-and-redeploy.sh
		UPDATE_CONFIG_LIB_ONLY=1 source "$SCRIPT"
		parse_args openrouter --dry-run
		if [[ "$SECRET_NAME" != "openrouter" || "$MODE_DRY_RUN" -ne 1 ]]; then
			exit 1
		fi
	); then
		fail "happy path globals not set"
		return
	fi
	pass "parse_args happy"
}

# ----------------------------------------------------------------------
# main — unknown name exits 64; missing actor exits 69; bad uuid 64.
# ----------------------------------------------------------------------

t_main_unknown_secret_exits_64() {
	local rc=0
	(
		unset UPDATE_CONFIG_LIB_ONLY
		CRM_OPS_ACTOR_USER_ID="" bash "$SCRIPT" "totally-unknown"
	) >/dev/null 2>&1 || rc=$?
	[[ "$rc" -eq 64 ]] || { fail "unknown name = $rc"; return; }
	pass "main unknown secret exits 64"
}

t_main_missing_actor_exits_69() {
	local rc=0
	(
		unset UPDATE_CONFIG_LIB_ONLY CRM_OPS_ACTOR_USER_ID
		bash "$SCRIPT" "openrouter"
	) >/dev/null 2>&1 || rc=$?
	[[ "$rc" -eq 69 ]] || { fail "missing actor = $rc"; return; }
	pass "main missing actor exits 69"
}

t_main_bad_actor_uuid_exits_64() {
	local rc=0
	(
		unset UPDATE_CONFIG_LIB_ONLY
		CRM_OPS_ACTOR_USER_ID="x" bash "$SCRIPT" "openrouter"
	) >/dev/null 2>&1 || rc=$?
	[[ "$rc" -eq 64 ]] || { fail "bad uuid = $rc"; return; }
	pass "main bad uuid exits 64"
}

t_env_key_for
t_is_supported_name
t_default_redeploy_for
t_audit_payload_shape
t_redact
t_validate_actor_uuid
t_update_env_file_dry_run_does_not_write
t_update_env_file_replaces_and_backs_up
t_restore_env_file_round_trip
t_parse_args_missing_name_exits_64
t_parse_args_unknown_flag_exits_64
t_parse_args_happy
t_main_unknown_secret_exits_64
t_main_missing_actor_exits_69
t_main_bad_actor_uuid_exits_64

if (( failures > 0 )); then
	printf '\n%d test(s) FAILED\n' "$failures" >&2
	exit 1
fi
printf '\nALL TESTS PASSED\n'
