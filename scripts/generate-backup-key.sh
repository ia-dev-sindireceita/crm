#!/usr/bin/env bash
# Bootstrap and rotate the Sindireceita backup encryption key.
#
# Run on the BACKUP HOST only. Writes the private key to
# /etc/sindireceita/age-backup.key with mode 0440 and ownership
# root:sindireceita-backup, so the dedicated backup-service group can read
# the key during the restore drill but no other local user can. The operator
# copies the public key into infra/age-backup.pub (commit it) and
# SOPS-encrypts the private key into infra/sops/age-backup.key.enc.
#
# Off-site copy: also stash the private key in the 2nd-tier secret store
# (cofre fisico, password manager, hardware HSM) — losing the key means
# losing every backup it protects.
#
# SIN-62250.
set -Eeuo pipefail
shopt -s inherit_errexit

log() { printf '[generate-backup-key.sh] %s %s\n' "$(date -u +%FT%TZ)" "$*" >&2; }
fail() { log "ERROR: $*"; exit 1; }

key_path=${BACKUP_AGE_KEY:-/etc/sindireceita/age-backup.key}
key_dir=$(dirname -- "$key_path")
key_group=${BACKUP_AGE_GROUP:-sindireceita-backup}

if [[ -e "$key_path" ]]; then
  fail "$key_path already exists; refusing to overwrite. Rotate via the runbook (docs/operations/backup-restore.md)."
fi

if ! getent group "$key_group" >/dev/null 2>&1; then
  fail "group '$key_group' does not exist on this host. Create the user/group first (see docs/operations/backup-restore.md § Primeira instalação)."
fi

[[ -d "$key_dir" ]] || install -d -m 0700 "$key_dir"

umask 0077
age-keygen -o "$key_path"
# root owns the key; the backup-service group can read it. backup.sh does
# not need the private key (it encrypts via the public recipient), so the
# systemd unit additionally hides the file via InaccessiblePaths.
chown "root:$key_group" "$key_path"
chmod 0440 "$key_path"

log "wrote private key to $key_path (mode 0440, owner root:$key_group)"
log "public key:"
grep -E '^# public key:' "$key_path" | sed 's/^# public key: //'

cat <<'NEXT' >&2

NEXT STEPS
1. REPLACE the marker line at the bottom of infra/age-backup.pub with the
   public key printed above. THIS EDIT IS HOST-LOCAL: do NOT git add /
   git commit / git push it. The committed placeholder must stay the
   placeholder forever — CI (TestPublicRecipientParses) rejects any commit
   that swaps it for a real recipient. See docs/operations/backup-restore.md
   § "Primeira instalação" step 5.
2. SOPS-encrypt the private key (the ciphertext IS commit-safe):
     sops --encrypt --age "<recipient>" "$BACKUP_AGE_KEY" > infra/sops/age-backup.key.enc
3. Stash a second copy of the private key in the offline secret store
   (cofre fisico). Without it, every backup encrypted to this key is
   unrecoverable.
4. Wipe any temporary copies (shred -u or equivalent).
NEXT
