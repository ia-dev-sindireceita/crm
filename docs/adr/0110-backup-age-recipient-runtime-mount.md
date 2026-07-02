# ADR 0110 — Backup age recipient injected at runtime, not baked into the signed image

- Status: Accepted
- Date: 2026-07-02
- Deciders: CTO
- Drives: [SIN-66537](/SIN/issues/SIN-66537) (this ADR — code half of the deferred first-time backup install)
- Resolves: the recipient→image design gap raised on [SIN-66536](/SIN/issues/SIN-66536)
- Builds on: [ADR 0104](./0104-backup-restore-rpo-rto-drill.md) (backup/restore pipeline), the SIN-63195 encrypted-backup sidecar, and the hardening invariants tracked against ADR 0102 (least privilege / defense in depth for the sidecar)
- Lenses: **Boring technology / reversibility**, **Least privilege / defense in depth**, **Hexagonal**

## Context

The encrypted Postgres backup pipeline (SIN-63195) encrypts each dump with
`age -R <recipients>` to a **public** recipient before uploading to object
storage. The private key never touches the scheduled sidecar; only the
out-of-band restore drill mounts it.

The real production recipient must reach the running sidecar, but the obvious
"bake it into the image" path is blocked by two independent constraints:

1. **CI pins the committed recipient to a placeholder.**
   `internal/backup.TestPublicRecipientParses` asserts that
   `infra/age-backup.pub` in git is exactly the non-functional bootstrap
   placeholder. This is the rotation gate (SIN-62220 must-fix #2): a real
   recipient in git implies its private half may linger in `/tmp` on whichever
   host generated it. So the real recipient cannot be committed.

2. **Only upstream `pericles-luz/crm` may push the cosign-signed image to
   GHCR.** The fork's CI/build/signing pipeline cannot produce a
   recipient-bearing signed image, so there is no clean path to bake the real
   recipient into the artifact that the staging/prod host actually pulls.

The code already supports mounting the recipient: `scripts/backup.sh:131`
reads `recipients=${BACKUP_AGE_RECIPIENTS:-"$repo_root/infra/age-backup.pub"}`,
and the compose service already sets `BACKUP_AGE_RECIPIENTS`.

## Options

1. **Bake the real recipient into the signed image.** Requires either
   weakening the CI placeholder guard or standing up an out-of-band manual
   `docker build` + `cosign` sign on a trusted host outside the reproducible
   pipeline. Both fork the build/sign path and add a trusted-host signing key
   — high blast radius, low benefit.

2. **Inject the recipient at runtime via a read-only host mount** of
   `/etc/lmhost/age-backup.pub`, repointing `BACKUP_AGE_RECIPIENTS` at the
   mounted path. Zero changes to CI/build/signing; the committed placeholder
   stays put as the baked fallback default.

## Decision

**Option 2 — runtime host mount.** The `backup` service in both
`compose.stg.yml` and `compose.yml` bind-mounts the operator-provisioned
recipient read-only at `/etc/lmhost/age-backup.pub` and sets
`BACKUP_AGE_RECIPIENTS=/etc/lmhost/age-backup.pub`. The image keeps the
`COPY infra/age-backup.pub` placeholder as a non-functional fallback default;
the mount overrides it at run time. The first-install and rotation runbooks
(`docs/operations/backup-restore.md`) become a host file swap, not an image
rebuild.

## Consequences

- **Reversibility / boring tech.** Recipient rotation is an operator file swap
  plus a sidecar restart — fully reversible, no image rebuild, no out-of-band
  cosign signing key on a trusted host, no fork of the reproducible build.
- **Least privilege / defense in depth.** The recipient file is
  `0640 root:lmhost-backup` inside the same `/etc/lmhost` trust boundary as the
  private key (SIN-66525), but it is only **public** key material. The
  scheduled sidecar still cannot read the private key — enforced by
  `TestComposeBackupSidecarDeniesPrivateKey` and
  `TestComposeBackupSidecarKeyIsolationFromVolumes` (the latter now permits the
  single-file read-only public-recipient mount and nothing else under
  `/etc/lmhost`). Host write-access to swap the recipient already implies
  access to the PII being dumped, so baking would add no real confidentiality
  control.
- **Hexagonal.** The recipient is config injected at the boundary (env + mount),
  not compiled/baked into the artifact.
- **Fail-loud.** If the recipient file is absent at run time, the sidecar fails
  to start on the missing bind source rather than silently encrypting to the
  non-functional placeholder. `docker compose config` still renders without the
  file present, so review boxes are unaffected.
- **Restore unchanged.** The private key path/mount for the restore drill
  (`--user 0:0`, `-v /etc/lmhost/age-backup.key:...:ro`) is untouched.
