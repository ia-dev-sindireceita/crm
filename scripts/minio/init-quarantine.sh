#!/usr/bin/env bash
# scripts/minio/init-quarantine.sh — idempotent provisioning of the
# media + media-quarantine buckets and their IAM policies, per
# [SIN-62805] F2-05d. See docs/runbooks/minio-bucket-policy.md for the
# end-to-end wire-up (this script is just the bucket + policy slice).
#
# Usage:
#   scripts/minio/init-quarantine.sh <mc-alias>
#
# <mc-alias> is the local `mc` alias pointing at the target MinIO server
# (e.g. "stg" for staging). The caller is responsible for having set the
# alias up via `mc alias set` with an admin credential before invoking
# this script.

set -euo pipefail

if [[ $# -lt 1 ]]; then
  echo "usage: $0 <mc-alias>" >&2
  exit 64
fi

ALIAS="$1"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
POLICY_DIR="${SCRIPT_DIR}/policies"

mkdir -p "${POLICY_DIR}"

write_policy_app_runtime() {
  cat >"${POLICY_DIR}/app_runtime.json" <<'JSON'
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Effect": "Allow",
      "Action": ["s3:GetObject", "s3:PutObject", "s3:HeadObject"],
      "Resource": ["arn:aws:s3:::media/*"]
    },
    {
      "Effect": "Allow",
      "Action": ["s3:ListBucket"],
      "Resource": ["arn:aws:s3:::media"]
    }
  ]
}
JSON
}

write_policy_worker_quarantine() {
  cat >"${POLICY_DIR}/worker_quarantine.json" <<'JSON'
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Sid": "ReadFromMediaForMove",
      "Effect": "Allow",
      "Action": ["s3:GetObject"],
      "Resource": ["arn:aws:s3:::media/*"]
    },
    {
      "Sid": "DeleteFromMediaForMove",
      "Effect": "Allow",
      "Action": ["s3:DeleteObject"],
      "Resource": ["arn:aws:s3:::media/*"]
    },
    {
      "Sid": "WriteToQuarantine",
      "Effect": "Allow",
      "Action": ["s3:PutObject", "s3:HeadObject"],
      "Resource": ["arn:aws:s3:::media-quarantine/*"]
    }
  ]
}
JSON
}

ensure_bucket() {
  local bucket="$1"
  if mc --json stat "${ALIAS}/${bucket}" >/dev/null 2>&1; then
    echo "bucket ${bucket}: present"
  else
    mc mb "${ALIAS}/${bucket}"
    echo "bucket ${bucket}: created"
  fi
}

ensure_policy() {
  local name="$1" file="$2"
  # `mc admin policy create` is idempotent on existing names with an
  # identical body; for changed bodies it errors out — caller must
  # explicitly `policy remove` first.
  if mc admin policy info "${ALIAS}" "${name}" >/dev/null 2>&1; then
    echo "policy ${name}: present (no change applied — run 'mc admin policy remove' to refresh)"
  else
    mc admin policy create "${ALIAS}" "${name}" "${file}"
    echo "policy ${name}: created"
  fi
}

write_policy_app_runtime
write_policy_worker_quarantine

ensure_bucket "media"
ensure_bucket "media-quarantine"
ensure_policy "app_runtime" "${POLICY_DIR}/app_runtime.json"
ensure_policy "worker_quarantine" "${POLICY_DIR}/worker_quarantine.json"

cat <<NEXT

Next steps (run with REAL key material from your secrets manager):

  # Create app_runtime user + attach policy
  mc admin user add ${ALIAS} REPLACE_APP_ACCESS_KEY REPLACE_APP_SECRET_KEY
  mc admin policy attach ${ALIAS} app_runtime --user=REPLACE_APP_ACCESS_KEY

  # Create worker_quarantine service account + attach policy
  mc admin user svcacct add \\
    --access-key REPLACE_STS_BOOTSTRAP_AK \\
    --secret-key REPLACE_STS_BOOTSTRAP_SK \\
    ${ALIAS} worker_quarantine
  mc admin policy attach ${ALIAS} worker_quarantine --user=worker_quarantine

See docs/runbooks/minio-bucket-policy.md for STS rotation + verification.
NEXT
