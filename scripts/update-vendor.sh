#!/usr/bin/env bash
# update-vendor.sh — SIN-62284
#
# Idempotent vendor refresh for HTMX. Downloads the official
# npm tarball published by the maintainers, validates the package-level
# SHA-512 integrity against the value in the npm registry (which is
# signed with the maintainer's npm key), extracts the minified asset,
# rewrites web/static/vendor/<lib>/<version>/<lib>.min.js, and refreshes
# the corresponding entry in CHECKSUMS.txt with a fresh SHA-384 SRI hash.
#
# Defence in depth — two hash chains protect the supply chain:
#   1. npm sha-512 integrity from the registry metadata (maintainer-signed)
#      gates ingestion before the file ever lands on disk.
#   2. The repo CHECKSUMS.txt sha-384 (SRI) gates every subsequent build
#      and template render once the file is committed.
#
# Boring-tech budget — relies only on bash, curl, tar, openssl, jq, find.
# No node/npm runtime is required.
#
# Usage:
#   scripts/update-vendor.sh <lib> <version>
#
# Examples:
#   scripts/update-vendor.sh htmx 2.0.9

set -euo pipefail

usage() {
  cat <<'USAGE' >&2
usage: scripts/update-vendor.sh <lib> <version>

Supported libraries:
  htmx       — pulls htmx.org from npm, extracts dist/htmx.min.js
USAGE
  exit 2
}

if [[ $# -ne 2 ]]; then
  usage
fi

lib="$1"
version="$2"

case "${lib}" in
  htmx)
    npm_pkg="htmx.org"
    asset_in_tarball="package/dist/htmx.min.js"
    output_filename="htmx.min.js"
    ;;
  *)
    echo "update-vendor: unknown lib '${lib}'" >&2
    usage
    ;;
esac

# Cheap input validation — keeps the value safe to interpolate into URLs
# and shell paths even though we always quote it.
if [[ ! "${version}" =~ ^[0-9]+\.[0-9]+\.[0-9]+([.-][A-Za-z0-9.]+)?$ ]]; then
  echo "update-vendor: version '${version}' is not a valid semver-ish string" >&2
  exit 2
fi

for tool in curl tar openssl jq find; do
  if ! command -v "${tool}" >/dev/null 2>&1; then
    echo "update-vendor: required tool not found in PATH: ${tool}" >&2
    exit 1
  fi
done

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
vendor_root="${repo_root}/web/static/vendor"
checksums_file="${vendor_root}/CHECKSUMS.txt"
lib_root="${vendor_root}/${lib}"
target_dir="${lib_root}/${version}"
target_file="${target_dir}/${output_filename}"

tmp_dir="$(mktemp -d)"
trap 'rm -rf "${tmp_dir}"' EXIT

# 1. Resolve the maintainer-published tarball + integrity hash from npm.
metadata_url="https://registry.npmjs.org/${npm_pkg}/${version}"
echo "update-vendor: fetching metadata ${metadata_url}"
metadata_file="${tmp_dir}/metadata.json"
if ! curl -fsSL --retry 3 -o "${metadata_file}" "${metadata_url}"; then
  echo "update-vendor: npm registry returned an error for ${npm_pkg}@${version}" >&2
  echo "update-vendor: manual fallback — confirm the version exists, then bump the URL above." >&2
  exit 1
fi

tarball_url="$(jq -r '.dist.tarball' "${metadata_file}")"
integrity="$(jq -r '.dist.integrity' "${metadata_file}")"
if [[ -z "${tarball_url}" || "${tarball_url}" == "null" ]]; then
  echo "update-vendor: missing dist.tarball in npm metadata" >&2
  exit 1
fi
if [[ -z "${integrity}" || "${integrity}" == "null" ]]; then
  echo "update-vendor: missing dist.integrity in npm metadata" >&2
  exit 1
fi
if [[ "${integrity}" != sha512-* ]]; then
  echo "update-vendor: only sha512-<base64> npm integrity values are supported (got '${integrity}')" >&2
  exit 1
fi
expected_b64="${integrity#sha512-}"

# 2. Download the tarball and verify its sha-512 against the registry value.
tarball_file="${tmp_dir}/${npm_pkg}-${version}.tgz"
echo "update-vendor: downloading ${tarball_url}"
curl -fsSL --retry 3 -o "${tarball_file}" "${tarball_url}"

actual_b64="$(openssl dgst -sha512 -binary "${tarball_file}" | openssl base64 -A)"
if [[ "${actual_b64}" != "${expected_b64}" ]]; then
  echo "update-vendor: TARBALL INTEGRITY MISMATCH for ${npm_pkg}@${version}" >&2
  echo "  expected: sha512-${expected_b64}" >&2
  echo "  actual:   sha512-${actual_b64}" >&2
  exit 1
fi
echo "update-vendor: tarball sha-512 matches registry integrity"

# 3. Extract just the asset we care about.
if ! tar -tzf "${tarball_file}" "${asset_in_tarball}" >/dev/null 2>&1; then
  echo "update-vendor: tarball does not contain ${asset_in_tarball}" >&2
  exit 1
fi
tar -xzf "${tarball_file}" -C "${tmp_dir}" "${asset_in_tarball}"

# 4. Replace the on-disk vendor file. Wipe any previous version directory
#    for this lib so we never ship two copies — rollback is via git history.
rm -rf "${lib_root}"
mkdir -p "${target_dir}"
cp "${tmp_dir}/${asset_in_tarball}" "${target_file}"
chmod 0644 "${target_file}"

# 5. Compute the SRI sha-384 hash and refresh CHECKSUMS.txt in place.
sri_b64="$(openssl dgst -sha384 -binary "${target_file}" | openssl base64 -A)"
new_entry="sha384-${sri_b64}  ${lib}/${version}/${output_filename}"

mkdir -p "${vendor_root}"
touch "${checksums_file}"

new_checksums="${tmp_dir}/CHECKSUMS.txt.new"
# Strip any pre-existing entry for this lib (matches "<hash>  <lib>/...").
awk -v lib="${lib}" '
  /^[[:space:]]*$/ { print; next }
  /^#/ { print; next }
  {
    n = split($0, parts, /[[:space:]]+/)
    if (n >= 2 && index(parts[2], lib "/") == 1) next
    print
  }
' "${checksums_file}" > "${new_checksums}"

# Drop trailing blank lines so the appended entry stays adjacent.
sed -i -e :a -e '/^$/{$d;N;ba' -e '}' "${new_checksums}" || true
printf '%s\n' "${new_entry}" >> "${new_checksums}"

# Sort by path for stable, reviewable diffs.
sorted="${tmp_dir}/CHECKSUMS.txt.sorted"
awk '!/^$/ && !/^#/' "${new_checksums}" | sort -k2,2 > "${sorted}"
mv "${sorted}" "${checksums_file}"

echo "update-vendor: wrote ${target_file}"
echo "update-vendor: CHECKSUMS.txt updated"
echo
echo "  ${new_entry}"
