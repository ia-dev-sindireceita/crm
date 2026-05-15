# MinIO bucket policy — runtime + quarantine isolation

- Issue: [SIN-62805](/SIN/issues/SIN-62805) (F2-05d)
- ADR: 0080 (uploads), 0072 (RLS), depends on [SIN-62804](/SIN/issues/SIN-62804) (worker)

The mediascan pipeline runs across two buckets. The application (`app_runtime`)
talks only to `media`; the mediascan worker (`worker_quarantine`) is the only
identity that can write into `media-quarantine`. Neither identity can read
each other's prefix. That isolation is enforced by MinIO IAM policies + STS
short-lived credentials, NOT by application code.

## Buckets

| Bucket            | Purpose                                                                  | Read by                 | Write by               |
|-------------------|--------------------------------------------------------------------------|-------------------------|------------------------|
| `media`           | Live media served to tenants via the static origin                       | `app_runtime`           | `app_runtime` (upload) |
| `media-quarantine`| Infected blobs moved off the live path; never served                     | (audit-only, manual)    | `worker_quarantine`    |

`media-quarantine` is intentionally NOT readable by `app_runtime`. If a future
sweep needs to re-scan or audit, an operator uses an admin identity (MinIO
console / `mc admin user`) to inspect — there is no programmatic read path.

## Identities + policies

Two IAM users (or service accounts on MinIO STS), each with a single policy
attached.

### `app_runtime` — restricted to `media/*`

The runtime application reads + writes only inside the `media` bucket. The
policy is prefix-scoped so even a compromised app credential cannot enumerate
or fetch from `media-quarantine`.

```json
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Effect": "Allow",
      "Action": [
        "s3:GetObject",
        "s3:PutObject",
        "s3:HeadObject"
      ],
      "Resource": [
        "arn:aws:s3:::media/*"
      ]
    },
    {
      "Effect": "Allow",
      "Action": [
        "s3:ListBucket"
      ],
      "Resource": [
        "arn:aws:s3:::media"
      ]
    }
  ]
}
```

Apply with `mc`:

```bash
mc admin policy create REPLACE_MINIO_ALIAS app_runtime ./policies/app_runtime.json
mc admin user add REPLACE_MINIO_ALIAS REPLACE_APP_ACCESS_KEY REPLACE_APP_SECRET_KEY
mc admin policy attach REPLACE_MINIO_ALIAS app_runtime --user=REPLACE_APP_ACCESS_KEY
```

### `worker_quarantine` — quarantine bucket access only

The mediascan worker uses an STS assume-role flow (see "STS rotation" below)
to receive 1-hour credentials scoped to:

- Read the object it just scanned from `media/` (to perform `CopyObject`).
- Write into `media-quarantine/`.
- Delete the source from `media/` (`CopyObject` + `DeleteObject` = move).

```json
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Sid": "ReadFromMediaForMove",
      "Effect": "Allow",
      "Action": [
        "s3:GetObject"
      ],
      "Resource": [
        "arn:aws:s3:::media/*"
      ]
    },
    {
      "Sid": "DeleteFromMediaForMove",
      "Effect": "Allow",
      "Action": [
        "s3:DeleteObject"
      ],
      "Resource": [
        "arn:aws:s3:::media/*"
      ]
    },
    {
      "Sid": "WriteToQuarantine",
      "Effect": "Allow",
      "Action": [
        "s3:PutObject",
        "s3:HeadObject"
      ],
      "Resource": [
        "arn:aws:s3:::media-quarantine/*"
      ]
    }
  ]
}
```

`worker_quarantine` is intentionally not granted `s3:GetObject` on
`media-quarantine/*` — the worker writes infected blobs and never reads them
back. Audit reads use a separate admin identity.

## STS rotation (1-hour credentials)

`mediascan-worker` runs without long-lived MinIO credentials. The deployment
provides a single "STS bootstrap" identity whose only permission is to call
`AssumeRole` against the `worker_quarantine` role; the worker performs an
`AssumeRole` at startup and refreshes the resulting `(AccessKeyID,
SecretAccessKey, SessionToken)` triple ~10 minutes before its 1-hour TTL.

The MinIO `mc` flow:

```bash
# Bootstrap identity (deploy-time, one-time per environment)
mc admin user svcacct add \
  --access-key REPLACE_STS_BOOTSTRAP_AK \
  --secret-key REPLACE_STS_BOOTSTRAP_SK \
  REPLACE_MINIO_ALIAS \
  worker_quarantine

# Inside the worker container, on boot
mc admin sts assume-role \
  --duration 1h \
  --policy /etc/mediascan/quarantine.policy.json \
  REPLACE_MINIO_ALIAS \
  --output json
# → returns {AccessKey, SecretKey, SessionToken, Expiration}
```

`cmd/mediascan-worker` consumes those three strings via the
`internal/adapter/media/minio` adapter (see `Config.{AccessKeyID,
SecretAccessKey, SessionToken}`). Rotation is the deployment harness's
responsibility — the adapter is stateless across renews.

## Bucket creation

Run `scripts/minio/init-quarantine.sh` once per environment (called from the
provisioning step in `deploy/scripts/stg-deploy.sh` for staging). The script
is idempotent:

```bash
scripts/minio/init-quarantine.sh REPLACE_MINIO_ALIAS
```

It creates both buckets (no-op if they exist), uploads the two policies, and
prints the user/role wire-up commands that the operator must run with the
real key material (the script intentionally does not generate keys — those
flow through the secrets manager).

## Verification

```bash
# media → app_runtime should see only its bucket
mc --json ls REPLACE_MINIO_ALIAS/media          # → OK
mc --json ls REPLACE_MINIO_ALIAS/media-quarantine # → AccessDenied

# worker_quarantine should write to quarantine + delete from media
mc cp /tmp/eicar.txt REPLACE_MINIO_ALIAS/media-quarantine/test-key  # → OK
mc rm REPLACE_MINIO_ALIAS/media/test-key                             # → OK
mc cat REPLACE_MINIO_ALIAS/media-quarantine/test-key                 # → AccessDenied (no GetObject)
```

The `AccessDenied` on read-back is the desired result: the worker writes and
walks away.

## Rollback

To revert (e.g. during incident triage), an operator with admin credentials
can re-enable read access on `media-quarantine` for `app_runtime`:

```bash
mc admin policy attach REPLACE_MINIO_ALIAS app_runtime_quarantine_read --user=REPLACE_APP_ACCESS_KEY
```

The `app_runtime_quarantine_read` policy is NOT created by
`init-quarantine.sh` — it must be created on demand and removed when the
incident closes. This keeps the default state strict.
