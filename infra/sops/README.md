# SOPS-encrypted secrets

This directory holds SOPS-encrypted secrets that ride alongside the repo. The
ciphertext is safe to commit; only hosts with the matching SOPS recipient
key can decrypt.

## age-backup.key.enc — backup encryption private key

The age private key used by `scripts/restore-drill.sh` to decrypt Postgres
dumps pulled from S3/B2.

```bash
# Encrypt (run on the backup host once after generate-backup-key.sh):
sops --encrypt --age "$SOPS_AGE_RECIPIENT" \
     /etc/sindireceita/age-backup.key \
     > infra/sops/age-backup.key.enc

# Decrypt (CI / disaster recovery only — backup host already has the cleartext):
sops --decrypt infra/sops/age-backup.key.enc > /etc/sindireceita/age-backup.key
chmod 0400 /etc/sindireceita/age-backup.key
```

`SOPS_AGE_RECIPIENT` is the public key of a *separate* SOPS keypair (NOT the
backup recipient itself — those serve different threat models). Manage that
SOPS keypair via the secret-store runbook in `docs/operations/secrets.md` (Fase 6).

> **REPLACE `infra/age-backup.pub` with the bootstrap recipient emitted by
> `scripts/generate-backup-key.sh`. The committed placeholder is
> non-functional by design — `age -R` against it fails hard, which is the
> rotation gate.** That edit is host-local and must NOT be committed. The
> ciphertext file in this directory (`age-backup.key.enc`) is the only
> recipient-related artefact that lives in git after rotation; the public
> recipient stays as the placeholder so CI catches any accidental commit
> that introduces a real X25519 recipient. See
> `docs/operations/backup-restore.md` § "Primeira instalação" step 5.

## Why two layers?

- **Backup recipient key** (`infra/age-backup.pub`) encrypts the dumps. Lives
  on the backup host as `/etc/sindireceita/age-backup.key`. The committed
  copy is a non-functional placeholder; the real recipient is host-local.
- **SOPS recipient key** encrypts the backup recipient *private key* in this
  file so it can be redistributed to a fresh backup host without out-of-band
  copying. Lives in the platform secret store. Recipients MUST be distinct
  from the backup recipient — encrypting the backup private key to itself
  collapses both threat models.

If both keys are lost the dumps are unrecoverable — see
`docs/operations/backup-restore.md` for the catastrophic-loss procedure.
