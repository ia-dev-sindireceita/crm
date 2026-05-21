# Runbook — Quarterly restore drill

Owning task: [SIN-63187](/SIN/issues/SIN-63187) (Fase 6 PR5).
Script: [`scripts/restore-drill.sh`](../../scripts/restore-drill.sh).
Compose: [`deploy/compose/docker-compose.restore-drill.yml`](../../deploy/compose/docker-compose.restore-drill.yml).
Cadence + SLOs: [`docs/ops/slo-rpo-rto.md`](./slo-rpo-rto.md).
Past reports: [`docs/ops/restore-drills/`](./restore-drills/).

This runbook tells the on-call engineer how to run the drill manually
when the script breaks, and how to escalate when the drill reveals a
real gap in our restore posture.

## Success criteria

The drill **passes** when both of the following hold:

| SLO | Budget | Source of truth |
| --- | ------ | --------------- |
| RTO (recovery time)            | ≤ **4h**  | wall-clock from script start to verified app `/health` 200 |
| RPO (recovery point objective) | ≤ **24h** | now − `LastModified` of the newest Postgres dump in the vault |

A drill that completes end-to-end but **breaches RTO or RPO** exits with
code 2. Treat it as a SEV-3 finding: file a follow-up ticket under
Fase 6 within 24h and link it to the drill report.

## Prerequisites

### Tools (operator workstation)

| Tool             | Min version | Install hint |
| ---------------- | ----------- | ------------ |
| `bash`           | 4.0         | preinstalled on Linux/macOS |
| `docker` + `compose` plugin | 24.x | <https://docs.docker.com/engine/install/> |
| `aws` CLI v2     | 2.13        | `apt install awscli` / `brew install awscli` |
| `jq`             | 1.6         | `apt install jq` / `brew install jq` |
| `curl`           | 7.79        | preinstalled |
| GNU `date`       | 8.32        | preinstalled on Linux; macOS users `brew install coreutils` and alias `date=gdate` |

### Credentials

Pull these from the operator vault (1Password → "CRM Backup vault — read
only") into a shell-local `.env.restore-drill` file. **Do not commit.**

```sh
export BACKUP_S3_ENDPOINT="https://s3.us-east-005.backblazeb2.com"
export BACKUP_S3_BUCKET="crm-backups-prod"
export BACKUP_PG_PREFIX="pg/"
export BACKUP_MINIO_PREFIX="minio/"
export AWS_ACCESS_KEY_ID="000XXXXXXXXXXXX"          # read-only role
export AWS_SECRET_ACCESS_KEY="…"
export AWS_DEFAULT_REGION="us-east-005"             # b2 region
```

The IAM principal MUST have `s3:GetObject` and `s3:ListBucket` ONLY.
Any write capability on the vault from a drill workstation is a
finding — escalate immediately.

### Host capacity

The drill restores the entire production Postgres database into a local
container. Plan for:

- Free disk ≥ 2× the largest pg_dump size.
- 4 GiB RAM free during the drill (Postgres + MinIO + app container).
- Outbound bandwidth to the S3 vault (full-size download).

## Run the drill (happy path)

```sh
. ./.env.restore-drill
scripts/restore-drill.sh
```

The script will:

1. List the vault and pick the newest pg_dump + MinIO snapshot.
2. Download both into a temp work dir.
3. Boot the isolated stack (`docker-compose.restore-drill.yml`,
   project `crm-restore-drill`).
4. `psql -f` the dump into the drill Postgres.
5. `docker cp` the MinIO snapshot under `/data` and restart MinIO.
6. Bring the app up against the restored data and wait for `/health` 200.
7. Run an authenticated DB query (`SELECT count(*) FROM tenants;`).
8. Write `docs/ops/restore-drills/restore-drill-report-<YYYY-MM-DD>.md`.
9. `docker compose down --volumes` everything.

Expected wall-clock against today's prod (≈800 MiB pg_dump, ≈1.2 GiB
MinIO snapshot, residential bandwidth): **15–30 minutes**.

Commit the generated report in the same PR that closes the drill
ticket — see "Filing the report" below.

## Manual fallback (script broken)

If `restore-drill.sh` itself fails, run the equivalent steps by hand.
This is also the procedure the on-call uses during a real disaster.

### 1. Discover the latest backups

```sh
aws --endpoint-url "$BACKUP_S3_ENDPOINT" s3api list-objects-v2 \
  --bucket "$BACKUP_S3_BUCKET" --prefix "$BACKUP_PG_PREFIX" \
  --query 'sort_by(Contents, &LastModified)[-1].{Key:Key,T:LastModified}'

aws --endpoint-url "$BACKUP_S3_ENDPOINT" s3api list-objects-v2 \
  --bucket "$BACKUP_S3_BUCKET" --prefix "$BACKUP_MINIO_PREFIX" \
  --query 'sort_by(Contents, &LastModified)[-1].{Key:Key,T:LastModified}'
```

Record the two `LastModified` values — they're your RPO inputs.

### 2. Download

```sh
work=$(mktemp -d)
aws --endpoint-url "$BACKUP_S3_ENDPOINT" s3api get-object \
  --bucket "$BACKUP_S3_BUCKET" --key "<pg-key>" "$work/pg.dump"
aws --endpoint-url "$BACKUP_S3_ENDPOINT" s3api get-object \
  --bucket "$BACKUP_S3_BUCKET" --key "<minio-key>" "$work/minio.tar.gz"
mkdir "$work/minio" && tar -xzf "$work/minio.tar.gz" -C "$work/minio"
```

### 3. Boot the drill stack

```sh
docker compose -p crm-restore-drill \
  -f deploy/compose/docker-compose.restore-drill.yml \
  up -d --wait postgres minio
```

### 4. Restore Postgres

```sh
docker cp "$work/pg.dump" crm-restore-drill-postgres-1:/tmp/restore.sql
docker exec -e PGPASSWORD="$POSTGRES_PASSWORD" \
  crm-restore-drill-postgres-1 \
  psql -U "$POSTGRES_USER" -d "$POSTGRES_DB" \
  -v ON_ERROR_STOP=1 -f /tmp/restore.sql
```

If the dump is a custom-format `pg_restore` archive instead of plain
SQL, swap `psql -f` for `pg_restore -d "$POSTGRES_DB" --clean --if-exists`.

### 5. Restore MinIO

```sh
docker cp "$work/minio/." crm-restore-drill-minio-1:/data/
docker restart crm-restore-drill-minio-1
```

### 6. Boot app + probe

```sh
docker compose -p crm-restore-drill \
  -f deploy/compose/docker-compose.restore-drill.yml \
  up -d --wait app
curl -fsS http://127.0.0.1:18080/health
docker exec -e PGPASSWORD="$POSTGRES_PASSWORD" \
  crm-restore-drill-postgres-1 \
  psql -U "$POSTGRES_USER" -d "$POSTGRES_DB" -tAc \
  'SELECT count(*) FROM tenants;'
```

### 7. Write a report by hand

Copy `docs/ops/restore-drills/2026-Q2-drill-report.md` as a template
and replace the values. Commit it in the PR that closes the drill
ticket.

### 8. Tear down

```sh
docker compose -p crm-restore-drill \
  -f deploy/compose/docker-compose.restore-drill.yml \
  down --volumes --remove-orphans
```

## Filing the report

Every drill — passing or failing — produces a Markdown file under
`docs/ops/restore-drills/restore-drill-report-<YYYY-MM-DD>.md` and is
committed to `main` via a PR titled
`[SIN-XXXXX] docs(ops): <YYYY>-Q<n> restore drill report`.

If the drill failed (exit 2 or any earlier abort), open a Fase 6
follow-up ticket linked from the report. Assign to the CTO for
triage. The report itself stays — do not edit historical reports to
make them look retroactively green.

## Escalation

| Failure type                                                | Page              | Who acts                  |
| ----------------------------------------------------------- | ----------------- | ------------------------- |
| Vault unreachable / 403                                     | ticket            | DPO + CTO                 |
| Backup older than RPO (≥ 24h) at drill time                 | ticket            | CTO; CEO if recurring     |
| Postgres restore aborts mid-dump                            | **page** the CTO  | CTO + Coder on-call       |
| App fails to come up against restored data                  | **page** the CTO  | CTO + Coder on-call       |
| Authenticated query returns wrong row count / wrong schema  | **page** the CTO  | CTO + Coder on-call       |
| Drill PASSes but takes > 4h end-to-end                      | ticket            | CTO triages capacity      |

Page = WhatsApp + Slack `#sev` channel + phone call after 15 min of
silence. Tickets file under the Fase 6 milestone for the next planning
window.

## CI gate (synthetic mode)

`.github/workflows/restore-drill.yml` runs the script on
`workflow_dispatch` with `--synthetic`. It does NOT need vault
credentials; it fabricates a tiny SQL dump + MinIO snapshot locally and
exercises the rest of the pipeline (docker compose up, restore, probe,
teardown, report). The gate exists to catch breakage of the drill
plumbing itself in between quarterly runs — without it, the operator
would only discover that the script is broken at the moment they need
it most.

The gate is `workflow_dispatch`-only by design: scheduled runs would
spin up disposable Docker stacks on every PR and add ~5 minutes to CI
for no marginal signal beyond "the script still parses".

## Related

- ADR — boring tech budget: docker + bash for low-frequency ops.
- Backup pipeline (separate repo): [SIN-62261](/SIN/issues/SIN-62261),
  [SIN-62267](/SIN/issues/SIN-62267).
- Backup sandbox smoke test: [SIN-62260](/SIN/issues/SIN-62260).
- Restore playbook for agnu-api: [SIN-62626](/SIN/issues/SIN-62626).
