#!/usr/bin/env bash
# scripts/rotate-secret.sh — driver for the secrets-rotation runbook
# (SIN-63189). One process per cycle; one cycle per rotation; two
# audit rows ('started' + 'completed' or 'failed') per cycle.
#
# Companion docs:
#   docs/ops/secrets-rotation.md           — per-secret procedure
#   docs/ops/secret-rotation-schedule.md   — cadence calendar
#
# Usage:
#   scripts/rotate-secret.sh <name> [--dry-run] [--rollback] [--audit-only]
#
# Names handled here (fully scripted cycle):
#   db:app_runtime          — dual-role swap, zero downtime
#   db:app_admin            — single-role swap, offline use
#   db:app_master_ops       — dual-role swap, master-ops console
#   marker:campaigns        — HMAC dual-key 72h window
#
# Names handled by scripts/update-config-and-redeploy.sh (semi-manual):
#   openrouter, pagarme, slack-alerts
#
# Manual-only (script refuses):
#   backup-encryption       — see docs/ops/secrets-rotation.md §8.
#
# Required environment:
#   CRM_OPS_ACTOR_USER_ID   — UUID of the operator (matches users.id).
#   CRM_AUDIT_DSN           — DSN for the app_audit role (INSERT-only).
#   PGPASSWORD              — read by psql for the audit DSN.
#   CRM_DEPLOY_ENV_FILE     — path to the .env file to edit (default:
#                             deploy/compose/.env).
#
# Optional environment:
#   CRM_REDEPLOY_CMD        — override the default redeploy command.
#   CRM_SUPERUSER_DSN       — superuser DSN, required only for db:app_admin.
#   CRM_DB_ADMIN_DSN        — app_admin DSN, required for db:app_runtime /
#                             db:app_master_ops (used for CREATE/DROP/RENAME
#                             ROLE in their dual-role swap).
#   CRM_DRY_RUN             — '1' = same as --dry-run.
#
# Exit codes:
#   0  cycle completed (or dry-run printed cleanly)
#   1  generic failure (validation, redeploy, audit insert)
#   2  rollback completed (rollback path always exits 2 to signal
#      "rotation did not land" without conflating with success)
#   64 usage error
#   65 manual-only secret requested (backup-encryption)
#   69 missing prerequisite tool or env var
#
# Boring tech: pure bash + psql + openssl + docker. The script never
# echoes a secret value. Any helper that touches plaintext key
# material is wrapped to scrub the value before logging.
#
# Library mode: when ROTATE_SECRET_LIB_ONLY=1 the script sources without
# executing main, so scripts/rotate-secret.test.sh can call the pure
# helpers directly.

set -euo pipefail

# ----------------------------------------------------------------------
# Logging helpers — never log secret values.
# ----------------------------------------------------------------------

log()  { printf '[rotate-secret] %s\n' "$*" >&2; }
warn() { printf '[rotate-secret] WARN: %s\n' "$*" >&2; }
die()  { printf '[rotate-secret] FATAL: %s\n' "$*" >&2; exit "${2:-1}"; }

# redact <maybe-secret> — returns '***REDACTED***' if the string is
# non-empty, otherwise the empty string. Used in every log line that
# could include a secret value by accident.
redact() {
	if [[ -n "${1:-}" ]]; then
		printf '%s' '***REDACTED***'
	fi
}

# ----------------------------------------------------------------------
# Config / globals
# ----------------------------------------------------------------------

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
DEFAULT_ENV_FILE="${REPO_ROOT}/deploy/compose/.env"
CRM_DEPLOY_ENV_FILE="${CRM_DEPLOY_ENV_FILE:-$DEFAULT_ENV_FILE}"
CRM_REDEPLOY_CMD="${CRM_REDEPLOY_CMD:-docker compose -f ${REPO_ROOT}/deploy/compose/compose.yml up -d --no-deps app}"

MODE_DRY_RUN=0
MODE_ROLLBACK=0
MODE_AUDIT_ONLY=0
SECRET_NAME=""

if [[ "${CRM_DRY_RUN:-0}" == "1" ]]; then
	MODE_DRY_RUN=1
fi

# Names of secrets this script is willing to drive end-to-end.
SCRIPTED_NAMES=(
	"db:app_runtime"
	"db:app_admin"
	"db:app_master_ops"
	"marker:campaigns"
)

# Names that belong to update-config-and-redeploy.sh — we redirect.
DELEGATED_NAMES=(
	"openrouter"
	"pagarme"
	"slack-alerts"
)

# Names that MUST be done by hand (script refuses).
MANUAL_ONLY_NAMES=(
	"backup-encryption"
)

# ----------------------------------------------------------------------
# Pure helpers (library-safe — no I/O)
# ----------------------------------------------------------------------

# name_is_in <name> <space-separated-list> — exit 0 if name is a member,
# 1 otherwise.
name_is_in() {
	local needle="$1"
	shift
	local item
	for item in "$@"; do
		if [[ "$item" == "$needle" ]]; then
			return 0
		fi
	done
	return 1
}

# classify_name <name> — echoes one of:
#   scripted | delegated | manual | unknown
# and exits 0. Pure function, safe to call from tests.
classify_name() {
	local name="$1"
	if name_is_in "$name" "${SCRIPTED_NAMES[@]}"; then
		echo "scripted"; return 0
	fi
	if name_is_in "$name" "${DELEGATED_NAMES[@]}"; then
		echo "delegated"; return 0
	fi
	if name_is_in "$name" "${MANUAL_ONLY_NAMES[@]}"; then
		echo "manual"; return 0
	fi
	echo "unknown"
}

# audit_payload <name> <phase> [extra-json-fragment] — composes the JSON
# payload for an audit row. Never includes the secret value. The optional
# third arg is a comma-prefixed JSON fragment (e.g. ',"dual_window_expires_at":"..."').
# stdout: JSON object as a single line.
audit_payload() {
	local name="$1" phase="$2" extra="${3:-}"
	printf '{"secret":"%s","phase":"%s"%s}' "$name" "$phase" "$extra"
}

# validate_actor_uuid <uuid> — exit 0 if the string matches a v4-ish UUID
# shape, 1 otherwise. The check is shape-only; the DB enforces existence
# in users.id via the FK at INSERT time.
validate_actor_uuid() {
	local u="$1"
	[[ "$u" =~ ^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$ ]]
}

# require_env <var> — die with exit 69 if the named env var is empty.
# Logs the var name only — never the value.
require_env() {
	local var="$1"
	if [[ -z "${!var:-}" ]]; then
		die "missing required env var: $var" 69
	fi
}

# require_cmd <bin> — die with exit 69 if the named binary is not on PATH.
require_cmd() {
	if ! command -v "$1" >/dev/null 2>&1; then
		die "missing required tool: $1" 69
	fi
}

# gen_password — emits a 32-byte CSPRNG base64 string on stdout.
# Caller MUST consume the value into a `mode=0600` tempfile or env var
# WITHOUT echoing. The function itself does not log.
gen_password() {
	openssl rand -base64 32 | tr -d '\n'
}

# write_secret_tempfile — receives a secret on stdin, writes it to a
# fresh `mode=0600` tempfile under /dev/shm (or $TMPDIR) and echoes the
# tempfile path. The temp file is the caller's to delete on exit.
write_secret_tempfile() {
	local base="${TMPDIR:-/dev/shm}"
	if [[ ! -d "$base" ]]; then
		base="${TMPDIR:-/tmp}"
	fi
	local f
	f=$(mktemp "${base}/rotate-secret.XXXXXXXX")
	chmod 600 "$f"
	cat >"$f"
	echo "$f"
}

# ----------------------------------------------------------------------
# I/O helpers (the side-effecting half — guarded by --dry-run)
# ----------------------------------------------------------------------

# emit_audit <name> <phase> [extra] — inserts one audit_log_security row.
# Dry-run prints the SQL and stays silent on the wire. Never echoes the
# secret value; payload is built by audit_payload().
emit_audit() {
	local name="$1" phase="$2" extra="${3:-}"
	local payload
	payload=$(audit_payload "$name" "$phase" "$extra")
	local sql
	sql=$(printf '%s' \
		"INSERT INTO audit_log_security (actor_user_id, event_type, target) " \
		"VALUES ('%s', 'key_rotation', %s::jsonb);")
	# shellcheck disable=SC2059
	sql=$(printf "$sql" "$CRM_OPS_ACTOR_USER_ID" "$(printf "%q" "$payload")")
	if (( MODE_DRY_RUN == 1 )); then
		log "[dry-run] audit emit: $payload"
		return 0
	fi
	require_env CRM_AUDIT_DSN
	require_env PGPASSWORD
	echo "$sql" | psql "$CRM_AUDIT_DSN" --quiet --no-psqlrc --set ON_ERROR_STOP=1 >/dev/null
	log "audit emit OK: $payload"
}

# update_env_file <key> <value> — replaces or appends KEY=VALUE in the
# deploy env file. The previous file is preserved at "${file}.prev"
# (mode=0600). Never logs the value.
update_env_file() {
	local key="$1" value="$2"
	local f="$CRM_DEPLOY_ENV_FILE"
	if [[ ! -f "$f" ]]; then
		die "deploy env file not found: $f" 1
	fi
	if (( MODE_DRY_RUN == 1 )); then
		log "[dry-run] would update $f: ${key}=$(redact "$value")"
		return 0
	fi
	cp -p "$f" "${f}.prev"
	chmod 600 "${f}.prev"
	local tmp
	tmp=$(mktemp "${f}.XXXXXX")
	chmod 600 "$tmp"
	# Replace existing key or append.
	if grep -qE "^${key}=" "$f"; then
		awk -v k="$key" -v v="$value" '
			BEGIN { FS=OFS="=" }
			$1==k { print k"="v; replaced=1; next }
			{ print }
			END { if (!replaced) print k"="v }' "$f" >"$tmp"
	else
		cat "$f" >"$tmp"
		printf '%s=%s\n' "$key" "$value" >>"$tmp"
	fi
	mv "$tmp" "$f"
	chmod 600 "$f"
	log "updated $f: ${key}=$(redact "$value")"
}

# restore_env_file — restore "${CRM_DEPLOY_ENV_FILE}.prev" over the
# current file. Used on --rollback. Idempotent.
restore_env_file() {
	local f="$CRM_DEPLOY_ENV_FILE"
	if [[ ! -f "${f}.prev" ]]; then
		warn "no previous env file at ${f}.prev — nothing to restore"
		return 0
	fi
	if (( MODE_DRY_RUN == 1 )); then
		log "[dry-run] would restore ${f}.prev → $f"
		return 0
	fi
	cp -p "${f}.prev" "$f"
	chmod 600 "$f"
	log "restored ${f}.prev → $f"
}

# run_redeploy — runs ${CRM_REDEPLOY_CMD}. Dry-run prints the command.
run_redeploy() {
	if (( MODE_DRY_RUN == 1 )); then
		log "[dry-run] would run: $CRM_REDEPLOY_CMD"
		return 0
	fi
	log "running redeploy: $CRM_REDEPLOY_CMD"
	# shellcheck disable=SC2086
	bash -c "$CRM_REDEPLOY_CMD"
}

# ----------------------------------------------------------------------
# Argument parsing
# ----------------------------------------------------------------------

usage() {
	sed -n '2,45p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'
}

parse_args() {
	while [[ $# -gt 0 ]]; do
		case "$1" in
			--dry-run)    MODE_DRY_RUN=1 ;;
			--rollback)   MODE_ROLLBACK=1 ;;
			--audit-only) MODE_AUDIT_ONLY=1 ;;
			-h|--help)    usage; exit 0 ;;
			--*)          die "unknown flag: $1" 64 ;;
			*)
				if [[ -z "$SECRET_NAME" ]]; then
					SECRET_NAME="$1"
				else
					die "unexpected extra argument: $1" 64
				fi
				;;
		esac
		shift
	done
	if [[ -z "$SECRET_NAME" ]]; then
		die "missing required argument: <name>" 64
	fi
}

# ----------------------------------------------------------------------
# Per-secret drivers (one function per scripted name)
# ----------------------------------------------------------------------

# drive_db_role_swap <role-name> [--bypassrls] — dual-role swap for
# app_runtime / app_master_ops. Used by drive_db_app_runtime and
# drive_db_app_master_ops to keep the orchestration in one place.
drive_db_role_swap() {
	local role="$1"
	local with_bypassrls="${2:-}"
	local next_role="${role}_next"
	require_env CRM_DB_ADMIN_DSN
	require_cmd psql
	require_cmd openssl

	local pw_file
	pw_file=$(gen_password | write_secret_tempfile)
	trap 'rm -f "'"$pw_file"'"' RETURN

	local bypassrls_attr=""
	if [[ "$with_bypassrls" == "--bypassrls" ]]; then
		bypassrls_attr="BYPASSRLS"
	else
		bypassrls_attr="NOBYPASSRLS"
	fi

	if (( MODE_DRY_RUN == 1 )); then
		log "[dry-run] CREATE ROLE $next_role LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE $bypassrls_attr PASSWORD '***REDACTED***'"
		log "[dry-run] update env: POSTGRES_USER=$next_role, POSTGRES_PASSWORD=***REDACTED***"
		log "[dry-run] redeploy + validate, then DROP ROLE $role; ALTER ROLE $next_role RENAME TO $role"
		return 0
	fi

	local pw
	pw=$(cat "$pw_file")
	# Compose the CREATE ROLE statement. The password is interpolated
	# via psql's :'var' binding so it is never echoed into the SQL log
	# even on ON_ERROR_STOP=1 failure paths.
	PGPASSWORD="${CRM_DB_ADMIN_PASSWORD:-$PGPASSWORD}" psql "$CRM_DB_ADMIN_DSN" \
		--quiet --no-psqlrc --set ON_ERROR_STOP=1 \
		-v new_role="$next_role" -v new_pw="$pw" <<'SQL' >/dev/null
DO $$
BEGIN
  EXECUTE format(
    'CREATE ROLE %I LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOBYPASSRLS PASSWORD %L',
    :'new_role', :'new_pw');
END $$;
SQL
	# Caller is responsible for grant duplication: emit the canonical
	# grant set via the helper below.
	clone_grants "$role" "$next_role"

	update_env_file "POSTGRES_USER" "$next_role"
	update_env_file "POSTGRES_PASSWORD" "$pw"

	run_redeploy
	validate_role_health "$next_role"

	# Validation succeeded — drop old, rename next.
	PGPASSWORD="${CRM_DB_ADMIN_PASSWORD:-$PGPASSWORD}" psql "$CRM_DB_ADMIN_DSN" \
		--quiet --no-psqlrc --set ON_ERROR_STOP=1 \
		-v old_role="$role" -v new_role="$next_role" <<'SQL' >/dev/null
BEGIN;
DO $$
BEGIN
  EXECUTE format('DROP ROLE IF EXISTS %I', :'old_role');
  EXECUTE format('ALTER ROLE %I RENAME TO %I', :'new_role', :'old_role');
END $$;
COMMIT;
SQL
	update_env_file "POSTGRES_USER" "$role"
}

# clone_grants <from-role> <to-role> — duplicate role memberships and
# default privileges. Real implementations cover the grant set documented
# in ADR 0071; the stub here is a placeholder that the dual-role swap
# calls into. Side-effects via psql.
clone_grants() {
	local from_role="$1" to_role="$2"
	if (( MODE_DRY_RUN == 1 )); then
		log "[dry-run] clone grants: $from_role -> $to_role"
		return 0
	fi
	# The actual grant set is environment-specific; ops keeps it in the
	# secret vault under crm/db/role-grants/<role>.sql and the script
	# applies it here. Refuse silently if the file is absent so the
	# operator notices and supplies it.
	local grants_file="${CRM_ROLE_GRANTS_DIR:-/etc/crm/role-grants}/${from_role}.sql"
	if [[ ! -f "$grants_file" ]]; then
		die "grant template missing: $grants_file (see docs/ops/secrets-rotation.md §1)" 1
	fi
	PGPASSWORD="${CRM_DB_ADMIN_PASSWORD:-$PGPASSWORD}" psql "$CRM_DB_ADMIN_DSN" \
		--quiet --no-psqlrc --set ON_ERROR_STOP=1 \
		-v target_role="$to_role" -f "$grants_file" >/dev/null
}

# validate_role_health <role> — confirms the redeployed app can reach
# the DB and serve /health. Fails with exit 1 if either check breaks.
validate_role_health() {
	local role="$1"
	if (( MODE_DRY_RUN == 1 )); then
		log "[dry-run] validate /health and DB select as $role"
		return 0
	fi
	local url="${CRM_HEALTH_URL:-http://127.0.0.1:8080/health}"
	local timeout="${CRM_VALIDATE_TIMEOUT:-60}"
	local elapsed=0
	while (( elapsed < timeout )); do
		if curl -fsS -o /dev/null "$url"; then
			log "validation OK: GET $url"
			return 0
		fi
		sleep 2
		elapsed=$((elapsed + 2))
	done
	die "validation failed: $url did not return 200 within ${timeout}s" 1
}

# drive_db_app_admin — single-role rotation for the migration runner.
# No dual-role swap because no live pod holds the credential.
drive_db_app_admin() {
	require_env CRM_SUPERUSER_DSN
	require_cmd psql
	require_cmd openssl
	local pw_file
	pw_file=$(gen_password | write_secret_tempfile)
	trap 'rm -f "'"$pw_file"'"' RETURN
	if (( MODE_DRY_RUN == 1 )); then
		log "[dry-run] ALTER ROLE app_admin PASSWORD '***REDACTED***'"
		log "[dry-run] write new value into secret manager (crm/db/app_admin/password)"
		return 0
	fi
	local pw
	pw=$(cat "$pw_file")
	PGPASSWORD="${CRM_SUPERUSER_PASSWORD:-$PGPASSWORD}" psql "$CRM_SUPERUSER_DSN" \
		--quiet --no-psqlrc --set ON_ERROR_STOP=1 \
		-v new_pw="$pw" <<'SQL' >/dev/null
DO $$ BEGIN EXECUTE format('ALTER ROLE app_admin PASSWORD %L', :'new_pw'); END $$;
SQL
	log "ALTER ROLE app_admin PASSWORD '***REDACTED***' OK"
	log "remember to write the new value into the secret manager (crm/db/app_admin/password)"
}

# drive_marker_campaigns — HMAC dual-key 72h window. Writes the new key
# as the primary, demotes the previous primary to _PREVIOUS, schedules
# the cleanup ticket.
drive_marker_campaigns() {
	require_cmd openssl
	local new_key_file
	new_key_file=$(gen_password | write_secret_tempfile)
	trap 'rm -f "'"$new_key_file"'"' RETURN
	if (( MODE_DRY_RUN == 1 )); then
		log "[dry-run] demote CAMPAIGNS_MARKER_SIGNING_KEY -> CAMPAIGNS_MARKER_SIGNING_KEY_PREVIOUS"
		log "[dry-run] write CAMPAIGNS_MARKER_SIGNING_KEY=***REDACTED***"
		log "[dry-run] redeploy app; schedule 72h cleanup ticket"
		return 0
	fi
	local prev_value=""
	if grep -qE '^CAMPAIGNS_MARKER_SIGNING_KEY=' "$CRM_DEPLOY_ENV_FILE"; then
		prev_value=$(grep -E '^CAMPAIGNS_MARKER_SIGNING_KEY=' "$CRM_DEPLOY_ENV_FILE" | head -n1 | cut -d= -f2-)
	fi
	if [[ -n "$prev_value" ]]; then
		update_env_file "CAMPAIGNS_MARKER_SIGNING_KEY_PREVIOUS" "$prev_value"
	fi
	local new_key
	new_key=$(cat "$new_key_file")
	update_env_file "CAMPAIGNS_MARKER_SIGNING_KEY" "$new_key"
	run_redeploy
	log "schedule cleanup ticket 72h from now:"
	log "  gh issue create --title '[ops] drop CAMPAIGNS_MARKER_SIGNING_KEY_PREVIOUS (rotation $(date -u +%Y-%m-%d))' --body 'Drop the dual-key window value 72h after rotation. See docs/ops/secrets-rotation.md §7.'"
}

# ----------------------------------------------------------------------
# main
# ----------------------------------------------------------------------

main() {
	parse_args "$@"

	local class
	class=$(classify_name "$SECRET_NAME")
	case "$class" in
		manual)
			cat >&2 <<EOF
[rotate-secret] '$SECRET_NAME' is a manual-only rotation.
See docs/ops/secrets-rotation.md §8 for the CEO ceremony.
EOF
			exit 65
			;;
		delegated)
			cat >&2 <<EOF
[rotate-secret] '$SECRET_NAME' belongs to scripts/update-config-and-redeploy.sh.
Run: scripts/update-config-and-redeploy.sh $SECRET_NAME
EOF
			exit 64
			;;
		unknown)
			die "unknown secret name: $SECRET_NAME (see docs/ops/secrets-rotation.md for the matrix)" 64
			;;
		scripted)
			:
			;;
		*)
			die "internal: unexpected classification '$class'" 1
			;;
	esac

	require_env CRM_OPS_ACTOR_USER_ID
	if ! validate_actor_uuid "$CRM_OPS_ACTOR_USER_ID"; then
		die "CRM_OPS_ACTOR_USER_ID does not look like a UUID" 64
	fi

	if (( MODE_ROLLBACK == 1 )); then
		emit_audit "$SECRET_NAME" "rollback_started"
		restore_env_file
		run_redeploy
		emit_audit "$SECRET_NAME" "failed" ',"reason":"rollback_invoked"'
		exit 2
	fi

	if (( MODE_AUDIT_ONLY == 1 )); then
		emit_audit "$SECRET_NAME" "completed" ',"audit_only":true'
		exit 0
	fi

	# Standard cycle.
	emit_audit "$SECRET_NAME" "started"
	local rc=0
	case "$SECRET_NAME" in
		db:app_runtime)     drive_db_role_swap "app_runtime"           || rc=$? ;;
		db:app_master_ops)  drive_db_role_swap "app_master_ops" --bypassrls || rc=$? ;;
		db:app_admin)       drive_db_app_admin                          || rc=$? ;;
		marker:campaigns)   drive_marker_campaigns                      || rc=$? ;;
		*)                  die "internal: scripted name without driver: $SECRET_NAME" 1 ;;
	esac

	if (( rc != 0 )); then
		emit_audit "$SECRET_NAME" "failed" ",\"driver_rc\":${rc}"
		exit "$rc"
	fi
	emit_audit "$SECRET_NAME" "completed"
}

if [[ "${ROTATE_SECRET_LIB_ONLY:-0}" != "1" ]]; then
	main "$@"
fi
