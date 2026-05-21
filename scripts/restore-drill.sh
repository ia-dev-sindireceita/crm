#!/usr/bin/env bash
# scripts/restore-drill.sh — quarterly backup restore drill (SIN-63187).
#
# Pulls the most recent Postgres + MinIO backups from the S3 vault, restores
# them into an isolated Docker stack, boots the app against the restored
# data, validates /health + one authenticated DB query, then tears the
# whole stack down — leaving a dated report in
# docs/ops/restore-drills/restore-drill-report-<YYYY-MM-DD>.md.
#
# Companion docs: docs/ops/restore-drill-runbook.md (operator playbook),
# docs/ops/slo-rpo-rto.md (SLOs + quarterly cadence).
#
# Usage:
#   scripts/restore-drill.sh [--synthetic] [--dry-run] [--keep] \
#                            [--report-dir DIR] [--report-date YYYY-MM-DD]
#
# Modes:
#   default     — real drill. Requires AWS_* env vars and a docker daemon.
#   --synthetic — skip S3; fabricate a tiny pg_dump + minio snapshot
#                 locally. Used by the CI gate to prove the drill plumbing
#                 itself works without needing real backup credentials.
#   --dry-run   — print every action, run nothing destructive. No docker
#                 calls. Useful for runbook walkthrough.
#   --keep      — leave the drill stack running after the report is
#                 written (skip teardown). For human debugging only.
#
# Required env vars in default mode:
#   BACKUP_S3_ENDPOINT      — S3-compatible endpoint URL (no trailing slash).
#   BACKUP_S3_BUCKET        — bucket holding daily backups.
#   BACKUP_PG_PREFIX        — key prefix for pg_dump artefacts (e.g. "pg/").
#   BACKUP_MINIO_PREFIX     — key prefix for MinIO snapshot artefacts.
#   AWS_ACCESS_KEY_ID       — read-only IAM key with s3:Get* + s3:List*.
#   AWS_SECRET_ACCESS_KEY
#   AWS_DEFAULT_REGION      — defaults to us-east-1.
#
# Exit codes:
#   0  drill passed (RTO + RPO satisfied)
#   1  generic failure (download, restore, or app boot)
#   2  drill ran end-to-end but breached RTO or RPO budgets
#   64 usage error
#   69 prerequisite tool missing (aws, mc, docker, jq)
#
# Boring tech: bash + docker compose + aws-cli + mc. No Terraform, no
# k8s, no Ansible. The drill runs four times a year; a 400-line bash
# script is the correct cost ceiling for that cadence (ADR — Boring
# tech budget).

set -euo pipefail

# ----------------------------------------------------------------------
# Config (overridable via env)
# ----------------------------------------------------------------------

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
COMPOSE_FILE="${COMPOSE_FILE:-${REPO_ROOT}/deploy/compose/docker-compose.restore-drill.yml}"
PROJECT_NAME="${PROJECT_NAME:-crm-restore-drill}"
APP_HEALTH_URL="${APP_HEALTH_URL:-http://127.0.0.1:18080/health}"
APP_BOOT_TIMEOUT="${APP_BOOT_TIMEOUT:-180}"
RTO_BUDGET_SECONDS="${RTO_BUDGET_SECONDS:-14400}"   # 4h
RPO_BUDGET_SECONDS="${RPO_BUDGET_SECONDS:-86400}"   # 24h
AWS_DEFAULT_REGION="${AWS_DEFAULT_REGION:-us-east-1}"
export AWS_DEFAULT_REGION

POSTGRES_DB="${POSTGRES_DB:-crm}"
POSTGRES_USER="${POSTGRES_USER:-crm}"
POSTGRES_PASSWORD="${POSTGRES_PASSWORD:-drill-postgres-pw}"
MINIO_ROOT_USER="${MINIO_ROOT_USER:-drillroot}"
MINIO_ROOT_PASSWORD="${MINIO_ROOT_PASSWORD:-drillrootpw}"
export POSTGRES_DB POSTGRES_USER POSTGRES_PASSWORD MINIO_ROOT_USER MINIO_ROOT_PASSWORD

# ----------------------------------------------------------------------
# Logging helpers
# ----------------------------------------------------------------------

log()  { printf '[restore-drill] %s\n' "$*" >&2; }
warn() { printf '[restore-drill] WARN: %s\n' "$*" >&2; }
die()  { printf '[restore-drill] FATAL: %s\n' "$*" >&2; exit 1; }

# ----------------------------------------------------------------------
# Mode + argument parsing
# ----------------------------------------------------------------------

MODE_SYNTHETIC=0
MODE_DRY_RUN=0
KEEP_STACK=0
REPORT_DIR="${REPORT_DIR:-${REPO_ROOT}/docs/ops/restore-drills}"
REPORT_DATE="$(date -u +%Y-%m-%d)"

parse_args() {
	while [[ $# -gt 0 ]]; do
		case "$1" in
			--synthetic)  MODE_SYNTHETIC=1 ;;
			--dry-run)    MODE_DRY_RUN=1 ;;
			--keep)       KEEP_STACK=1 ;;
			--report-dir) shift; REPORT_DIR="$1" ;;
			--report-date) shift; REPORT_DATE="$1" ;;
			-h|--help)    usage; exit 0 ;;
			*) die "unknown argument: $1" ;;
		esac
		shift
	done
}

usage() {
	sed -n '2,45p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'
}

# ----------------------------------------------------------------------
# Prereq checks
# ----------------------------------------------------------------------

require_cmd() {
	command -v "$1" >/dev/null 2>&1 || { warn "missing tool: $1"; return 1; }
}

check_prereqs() {
	local missing=0
	require_cmd docker || missing=$((missing+1))
	require_cmd jq     || missing=$((missing+1))
	if [[ $MODE_SYNTHETIC -eq 0 ]]; then
		require_cmd aws || missing=$((missing+1))
	fi
	if (( missing > 0 )); then
		exit 69
	fi
}

# ----------------------------------------------------------------------
# Time + math helpers (pure bash, easy to unit-test)
# ----------------------------------------------------------------------

# now_epoch — current UTC epoch seconds. Wrapped so tests can stub it.
now_epoch() { date -u +%s; }

# iso_to_epoch <iso8601> — convert "2026-05-21T12:34:56Z" -> epoch.
# Uses GNU date; the script's CI workflow runs on ubuntu-latest so this
# is fine. macOS users without coreutils will get a clearer failure.
iso_to_epoch() {
	local iso="$1"
	date -u -d "$iso" +%s 2>/dev/null \
		|| die "iso_to_epoch: cannot parse '$iso' (need GNU date)"
}

# format_duration <seconds> — human-friendly "1h 23m 04s".
format_duration() {
	local s="$1"
	(( s < 0 )) && s=0
	printf '%dh %02dm %02ds' "$((s/3600))" "$(((s%3600)/60))" "$((s%60))"
}

# verdict_for_budget <actual> <budget> -> "PASS" or "FAIL".
verdict_for_budget() {
	local actual="$1" budget="$2"
	if (( actual <= budget )); then echo "PASS"; else echo "FAIL"; fi
}

# ----------------------------------------------------------------------
# Backup discovery + download
# ----------------------------------------------------------------------

# pick_latest_key <prefix> — echo the newest object key under prefix.
# Uses aws s3api list-objects-v2 + jq to sort by LastModified desc.
# stdout: "<key>\t<iso8601>".
pick_latest_key() {
	local prefix="$1"
	local out
	out=$(aws --endpoint-url "$BACKUP_S3_ENDPOINT" \
		s3api list-objects-v2 \
		--bucket "$BACKUP_S3_BUCKET" --prefix "$prefix" \
		--output json) || die "s3 list-objects-v2 failed for prefix $prefix"
	echo "$out" | jq -r '
		.Contents
		| if (. // []) | length == 0
			then "EMPTY\t1970-01-01T00:00:00Z"
			else (sort_by(.LastModified) | reverse | .[0]
			      | "\(.Key)\t\(.LastModified)")
		end'
}

# download_object <key> <dest> — pulls a single S3 object via aws-cli.
download_object() {
	local key="$1" dest="$2"
	aws --endpoint-url "$BACKUP_S3_ENDPOINT" \
		s3api get-object \
		--bucket "$BACKUP_S3_BUCKET" --key "$key" "$dest" \
		>/dev/null \
		|| die "s3 get-object failed for $key"
}

# ----------------------------------------------------------------------
# Synthetic backup (CI mode)
# ----------------------------------------------------------------------

synthesize_pg_dump() {
	local dest="$1"
	# Minimal but valid SQL — exercises the restore + connect path
	# without dragging the full schema in. The drill in CI is about the
	# plumbing, not the schema. Quarterly real drills exercise the
	# schema for real.
	cat >"$dest" <<'SQL'
-- synthetic drill snapshot
CREATE TABLE IF NOT EXISTS drill_canary (
  id          int PRIMARY KEY,
  restored_at timestamptz NOT NULL DEFAULT now()
);
INSERT INTO drill_canary (id) VALUES (1) ON CONFLICT DO NOTHING;
SQL
}

synthesize_minio_snapshot() {
	local dest="$1"
	mkdir -p "$dest/media"
	echo "synthetic drill object" >"$dest/media/canary.txt"
}

# ----------------------------------------------------------------------
# Stack lifecycle
# ----------------------------------------------------------------------

compose_cmd() {
	docker compose -p "$PROJECT_NAME" -f "$COMPOSE_FILE" "$@"
}

boot_stack() {
	log "booting isolated stack: project=$PROJECT_NAME"
	compose_cmd up -d --wait postgres minio
}

teardown_stack() {
	if [[ $KEEP_STACK -eq 1 ]]; then
		warn "--keep set; leaving stack up. Tear down with:"
		warn "  docker compose -p $PROJECT_NAME -f $COMPOSE_FILE down --volumes"
		return
	fi
	log "tearing down stack (volumes included)"
	compose_cmd down --volumes --remove-orphans || warn "teardown reported errors"
}

restore_postgres() {
	local dump_file="$1"
	log "restoring Postgres from $(basename "$dump_file")"
	# Copy the dump into the container first; piping over `docker exec`
	# is brittle for >100MB dumps (TTY-mode buffering can stall).
	docker cp "$dump_file" "${PROJECT_NAME}-postgres-1:/tmp/restore.sql"
	docker exec -e PGPASSWORD="$POSTGRES_PASSWORD" \
		"${PROJECT_NAME}-postgres-1" \
		psql -U "$POSTGRES_USER" -d "$POSTGRES_DB" \
		-v ON_ERROR_STOP=1 -f /tmp/restore.sql >/dev/null \
		|| die "psql restore failed"
}

restore_minio() {
	local snapshot_dir="$1"
	log "restoring MinIO objects from $snapshot_dir"
	# Copy the snapshot tree under MinIO's data dir. MinIO will pick the
	# bucket(s) up on next list. No mc gymnastics required for a
	# bucket-level snapshot.
	docker cp "$snapshot_dir/." "${PROJECT_NAME}-minio-1:/data/"
	# Force MinIO to rescan; restarting is the simplest portable way
	# (`mc admin heal` requires an alias + creds).
	docker restart "${PROJECT_NAME}-minio-1" >/dev/null
}

boot_app() {
	log "booting app against restored data"
	compose_cmd up -d --wait app || die "app failed to come up"
}

# wait_health <url> <timeout-seconds> — poll until 200 or timeout.
wait_health() {
	local url="$1" timeout="$2"
	local deadline=$(( $(now_epoch) + timeout ))
	while (( $(now_epoch) < deadline )); do
		if curl -fsS "$url" >/dev/null 2>&1; then
			log "health probe OK: $url"
			return 0
		fi
		sleep 2
	done
	die "health probe timed out after ${timeout}s: $url"
}

# run_authenticated_query — runs the canonical sanity query against the
# restored DB using the app's role. "Authenticated" here means the DB
# credential path is exercised end-to-end; combined with the /health
# probe above this satisfies AC #1.iv (basic + authenticated checks).
run_authenticated_query() {
	local table_query="$1"
	log "running authenticated DB query: $table_query"
	docker exec -e PGPASSWORD="$POSTGRES_PASSWORD" \
		"${PROJECT_NAME}-postgres-1" \
		psql -U "$POSTGRES_USER" -d "$POSTGRES_DB" \
		-tAc "$table_query" \
		|| die "authenticated query failed"
}

# ----------------------------------------------------------------------
# Report
# ----------------------------------------------------------------------

write_report() {
	local started_iso="$1" finished_iso="$2"
	local rto_seconds="$3" rpo_seconds="$4"
	local backup_pg_key="$5" backup_pg_ts="$6"
	local backup_minio_key="$7" backup_minio_ts="$8"
	local sanity_row_count="$9"
	local mode_label="${10}"

	local rto_verdict rpo_verdict overall_verdict
	rto_verdict=$(verdict_for_budget "$rto_seconds" "$RTO_BUDGET_SECONDS")
	rpo_verdict=$(verdict_for_budget "$rpo_seconds" "$RPO_BUDGET_SECONDS")
	if [[ "$rto_verdict" == "PASS" && "$rpo_verdict" == "PASS" ]]; then
		overall_verdict="PASS"
	else
		overall_verdict="FAIL"
	fi

	mkdir -p "$REPORT_DIR"
	local report_path="${REPORT_DIR}/restore-drill-report-${REPORT_DATE}.md"

	cat >"$report_path" <<EOF
# Restore drill report — ${REPORT_DATE}

Mode: **${mode_label}**
Overall verdict: **${overall_verdict}**

## Window

| Field            | Value |
| ---------------- | ----- |
| Started (UTC)    | ${started_iso} |
| Finished (UTC)   | ${finished_iso} |

## RTO

| Field            | Value |
| ---------------- | ----- |
| Measured         | $(format_duration "$rto_seconds") (${rto_seconds}s) |
| Budget           | $(format_duration "$RTO_BUDGET_SECONDS") (${RTO_BUDGET_SECONDS}s) |
| Verdict          | **${rto_verdict}** |

## RPO

| Field            | Value |
| ---------------- | ----- |
| Measured         | $(format_duration "$rpo_seconds") (${rpo_seconds}s) |
| Budget           | $(format_duration "$RPO_BUDGET_SECONDS") (${RPO_BUDGET_SECONDS}s) |
| Verdict          | **${rpo_verdict}** |

## Backup objects exercised

| Source           | Key                          | Age (UTC stamp)       |
| ---------------- | ---------------------------- | --------------------- |
| Postgres dump    | \`${backup_pg_key}\`         | ${backup_pg_ts}       |
| MinIO snapshot   | \`${backup_minio_key}\`      | ${backup_minio_ts}    |

## Validation

- \`GET ${APP_HEALTH_URL}\` returned 200.
- Authenticated DB query (\`SELECT count(*) FROM drill_canary\` or
  schema-equivalent) returned **${sanity_row_count}** row(s).

## Next drill

The quarterly cadence and cron suggestion live in
[docs/ops/slo-rpo-rto.md](../slo-rpo-rto.md). Operator runbook:
[docs/ops/restore-drill-runbook.md](../restore-drill-runbook.md).

## Provenance

Generated by \`scripts/restore-drill.sh\` for [SIN-63187](/SIN/issues/SIN-63187).
EOF

	log "report written: $report_path"
	echo "$report_path"
}

# ----------------------------------------------------------------------
# Real-mode pipeline (S3-driven)
# ----------------------------------------------------------------------

real_pipeline() {
	: "${BACKUP_S3_ENDPOINT:?BACKUP_S3_ENDPOINT not set}"
	: "${BACKUP_S3_BUCKET:?BACKUP_S3_BUCKET not set}"
	: "${BACKUP_PG_PREFIX:?BACKUP_PG_PREFIX not set}"
	: "${BACKUP_MINIO_PREFIX:?BACKUP_MINIO_PREFIX not set}"
	: "${AWS_ACCESS_KEY_ID:?AWS_ACCESS_KEY_ID not set}"
	: "${AWS_SECRET_ACCESS_KEY:?AWS_SECRET_ACCESS_KEY not set}"

	local workdir; workdir=$(mktemp -d -t restore-drill.XXXXXX)
	trap 'rm -rf "$workdir"' EXIT

	log "picking latest pg backup under ${BACKUP_PG_PREFIX}"
	local pg_line pg_key pg_ts
	pg_line=$(pick_latest_key "$BACKUP_PG_PREFIX")
	pg_key="${pg_line%%$'\t'*}"
	pg_ts="${pg_line##*$'\t'}"
	[[ "$pg_key" == "EMPTY" ]] && die "no pg backup found under ${BACKUP_PG_PREFIX}"

	log "picking latest minio backup under ${BACKUP_MINIO_PREFIX}"
	local minio_line minio_key minio_ts
	minio_line=$(pick_latest_key "$BACKUP_MINIO_PREFIX")
	minio_key="${minio_line%%$'\t'*}"
	minio_ts="${minio_line##*$'\t'}"
	[[ "$minio_key" == "EMPTY" ]] && die "no minio backup found under ${BACKUP_MINIO_PREFIX}"

	local pg_dump="$workdir/pg.dump"
	local minio_tar="$workdir/minio.tar.gz"
	download_object "$pg_key" "$pg_dump"
	download_object "$minio_key" "$minio_tar"

	local minio_dir="$workdir/minio"
	mkdir -p "$minio_dir"
	tar -xzf "$minio_tar" -C "$minio_dir"

	echo "$pg_key|$pg_ts|$pg_dump|$minio_key|$minio_ts|$minio_dir"
}

synthetic_pipeline() {
	local workdir; workdir=$(mktemp -d -t restore-drill.XXXXXX)
	trap 'rm -rf "$workdir"' EXIT

	local pg_dump="$workdir/pg.sql"
	local minio_dir="$workdir/minio"
	synthesize_pg_dump "$pg_dump"
	synthesize_minio_snapshot "$minio_dir"

	local now; now="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
	echo "synthetic/pg.sql|$now|$pg_dump|synthetic/minio.tar.gz|$now|$minio_dir"
}

# ----------------------------------------------------------------------
# Main
# ----------------------------------------------------------------------

main() {
	parse_args "$@"

	if [[ $MODE_DRY_RUN -eq 1 ]]; then
		log "DRY RUN — printing intent only"
		log "  mode:           $( ((MODE_SYNTHETIC)) && echo synthetic || echo real )"
		log "  compose file:   $COMPOSE_FILE"
		log "  project:        $PROJECT_NAME"
		log "  report dir:     $REPORT_DIR"
		log "  report date:    $REPORT_DATE"
		log "  RTO budget (s): $RTO_BUDGET_SECONDS"
		log "  RPO budget (s): $RPO_BUDGET_SECONDS"
		log "  app health URL: $APP_HEALTH_URL"
		return 0
	fi

	check_prereqs

	local started_epoch; started_epoch="$(now_epoch)"
	local started_iso;   started_iso="$(date -u -d "@$started_epoch" +%Y-%m-%dT%H:%M:%SZ)"

	local pipeline
	if [[ $MODE_SYNTHETIC -eq 1 ]]; then
		pipeline="$(synthetic_pipeline)"
	else
		pipeline="$(real_pipeline)"
	fi

	local pg_key pg_ts pg_dump minio_key minio_ts minio_dir
	IFS='|' read -r pg_key pg_ts pg_dump minio_key minio_ts minio_dir <<<"$pipeline"

	local backup_epoch; backup_epoch="$(iso_to_epoch "$pg_ts")"
	local rpo_seconds=$(( started_epoch - backup_epoch ))

	boot_stack
	restore_postgres "$pg_dump"
	restore_minio "$minio_dir"
	boot_app
	wait_health "$APP_HEALTH_URL" "$APP_BOOT_TIMEOUT"

	# Sanity query target depends on the schema present in the dump.
	# Synthetic dumps create drill_canary; real dumps carry the full
	# schema, where `tenants` is the canonical non-tenant-scoped table
	# (migrations/0004_create_tenant.up.sql). We try the real table
	# first; fall back to drill_canary for synthetic runs.
	local rows
	if rows=$(run_authenticated_query 'SELECT count(*) FROM tenants;' 2>/dev/null); then
		: # rows already set
	else
		rows=$(run_authenticated_query 'SELECT count(*) FROM drill_canary;')
	fi

	local finished_epoch; finished_epoch="$(now_epoch)"
	local finished_iso;   finished_iso="$(date -u -d "@$finished_epoch" +%Y-%m-%dT%H:%M:%SZ)"
	local rto_seconds=$(( finished_epoch - started_epoch ))

	local mode_label
	mode_label=$( ((MODE_SYNTHETIC)) && echo "synthetic (CI)" || echo "real (S3 vault)" )

	local report_path
	report_path=$(write_report \
		"$started_iso" "$finished_iso" \
		"$rto_seconds" "$rpo_seconds" \
		"$pg_key" "$pg_ts" \
		"$minio_key" "$minio_ts" \
		"$rows" "$mode_label")

	teardown_stack

	local rto_v rpo_v
	rto_v="$(verdict_for_budget "$rto_seconds" "$RTO_BUDGET_SECONDS")"
	rpo_v="$(verdict_for_budget "$rpo_seconds" "$RPO_BUDGET_SECONDS")"
	if [[ "$rto_v" == "PASS" && "$rpo_v" == "PASS" ]]; then
		log "drill PASSED — report: $report_path"
		return 0
	fi
	warn "drill BREACHED budget — RTO=$rto_v RPO=$rpo_v — report: $report_path"
	return 2
}

# Library guard: tests source this file with DRILL_LIB_ONLY=1 to load
# helper functions without executing main.
if [[ "${DRILL_LIB_ONLY:-0}" -eq 0 ]]; then
	main "$@"
fi
