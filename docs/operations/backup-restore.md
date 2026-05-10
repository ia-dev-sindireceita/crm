# Backup & restore (encrypted Postgres dumps)

Sindireceita Postgres backups are encrypted client-side with [age](https://age-encryption.org)
before they leave the backup host. Even if Backblaze B2 / S3 credentials leak,
an attacker who downloads every snapshot still sees nothing but ciphertext.

This runbook covers daily operation, key rotation, and the catastrophic loss
scenarios. Lands in Fase 6 ([SIN-62199](/SIN/issues/SIN-62199)) as part of
[SIN-62250](/SIN/issues/SIN-62250).

## Files

| Path | Role |
|------|------|
| `infra/age-backup.pub` | Public recipient. Committed. Used by `backup.sh`. |
| `infra/sops/age-backup.key.enc` | SOPS-encrypted private key. Committed (ciphertext). |
| `/etc/sindireceita/age-backup.key` | Decrypted private key on the backup host. Mode `0440`, owner `root:sindireceita-backup`. NEVER committed. |
| `/etc/sindireceita/backup.env` | Service environment file. Mode `0640`, owner `root:sindireceita-backup`. NEVER committed. |
| `scripts/backup.sh` | `pg_dump | age -R | aws s3 cp -`. |
| `scripts/restore-drill.sh` | `aws s3 cp - | age -d -i KEY | pg_restore`. |
| `scripts/generate-backup-key.sh` | Bootstraps the keypair on a fresh backup host. |
| `infra/systemd/sindireceita-backup.{service,timer}` | Daily cron. |

## Primeira instalação (first-time setup)

Run on a fresh backup host before enabling the timer. Each step assumes a
shell with `sudo` on the host.

1. **Create the service user/group.** The dedicated user owns nothing in
   the repo; it exists only to run the backup unit and the restore drill
   under least privilege.
   ```bash
   sudo groupadd --system sindireceita-backup
   sudo useradd  --system --gid sindireceita-backup \
                 --home-dir /opt/sindireceita --shell /usr/sbin/nologin \
                 sindireceita-backup
   ```
2. **Lay down `/etc/sindireceita`** as the only place the host stores
   secrets:
   ```bash
   sudo install -d -m 0750 -o root -g sindireceita-backup /etc/sindireceita
   ```
3. **Provision `backup.env`.** This file holds DB credentials and S3
   credentials; it is NEVER committed and NEVER echoed to logs.
   ```bash
   sudo install -m 0640 -o root -g sindireceita-backup /dev/null /etc/sindireceita/backup.env
   sudoedit /etc/sindireceita/backup.env
   ```
   Required variables:
   ```ini
   DATABASE_URL=postgres://backup:<pw>@127.0.0.1:5432/sindireceita?sslmode=verify-full
   BACKUP_BUCKET=sindireceita-backups
   AWS_ACCESS_KEY_ID=...
   AWS_SECRET_ACCESS_KEY=...
   # optional:
   AWS_ENDPOINT_URL=https://s3.us-west-002.backblazeb2.com
   BACKUP_PREFIX=prod
   BACKUP_NODE_ID=          # defaults to `hostname -s`; set if hostname may change
   ```
4. **Generate the recipient keypair** (also used during rotation; see below):
   ```bash
   sudo ./scripts/generate-backup-key.sh
   ```
   The script refuses to overwrite an existing key. It chowns the result to
   `root:sindireceita-backup` mode `0440` so the service group can read it
   for the restore drill while the systemd unit itself hides it via
   `InaccessiblePaths=`. The script also prints the matching public
   recipient on stdout — capture it for the next step.
5. **REPLACE `infra/age-backup.pub` with the bootstrap recipient emitted by
   `scripts/generate-backup-key.sh`. The committed placeholder is
   non-functional by design — `age -R` against it fails hard, which is the
   rotation gate.** This edit is host-local: never push the real recipient
   back to git. CI asserts the committed file is exactly the placeholder
   (`TestPublicRecipientParses` in `internal/backup`); a PR that contains a
   real recipient is automatically rejected. Keep the comment header in
   `infra/age-backup.pub` intact and replace only the marker line at the
   bottom:
   ```bash
   # On the backup host (still as sudo). The private key file's first comment
   # block records the matching public recipient — that is the line we copy.
   pub=$(grep -E '^# public key:' /etc/sindireceita/age-backup.key | sed 's/^# public key: //')
   sudo sed -i "s|^age1placeholder.*|$pub|" infra/age-backup.pub
   sudo -u sindireceita-backup age -R infra/age-backup.pub /dev/null < /dev/null  # must now succeed
   ```
   Until this swap happens, `backup.sh` aborts at the `age -R` stage — that
   is the intended safety net.
6. **SOPS-encrypt the private key** so a fresh backup host can be re-bootstrapped
   from git without out-of-band copying:
   ```bash
   sudo sops --encrypt --age "$SOPS_AGE_RECIPIENT" \
        /etc/sindireceita/age-backup.key \
        > infra/sops/age-backup.key.enc
   ```
   See `infra/sops/README.md` for `SOPS_AGE_RECIPIENT` and the
   recipient-distinctness rule.
7. **Stash a second copy of the cleartext private key in the offline cofre**
   — see § "Cofre offline" for the storage policy and the verification cadence.
8. **Verify the install** with a dry-run:
   ```bash
   sudo systemctl start sindireceita-backup.service
   sudo journalctl -u sindireceita-backup --since '5 min ago'
   ```
   A clean run prints `backup complete -> s3://...` and exits 0.

> Group hygiene: only humans/services that need to decrypt restored dumps
> should be members of `sindireceita-backup`. Audit `getent group sindireceita-backup`
> at every off-boarding.

## Daily operation

The systemd timer fires at 03:17 UTC every day; logs go to `journalctl -u sindireceita-backup`.

```bash
# Inspect last run
sudo journalctl -u sindireceita-backup --since '24h ago'

# Trigger an out-of-cycle backup
sudo systemctl start sindireceita-backup.service
```

Smoke-check that an object actually landed. Objects are nested under the
node id (`hostname -s` by default, or `BACKUP_NODE_ID` if set), so listings
must include the per-host directory:

```bash
NODE_ID=${BACKUP_NODE_ID:-$(hostname -s)}
aws s3 ls "s3://$BACKUP_BUCKET/$(date -u +%F)/$NODE_ID/"
# Expect: dump.pgc.age <some-mb>
```

Sanity-check that the object is real age ciphertext (not, e.g., a captured
error page that just happens to be 200 OK):

```bash
aws s3 cp "s3://$BACKUP_BUCKET/$(date -u +%F)/$NODE_ID/dump.pgc.age" - \
  | head -c 32 | xxd
# Expect first line to start with: 6167652d 656e6372 79707469 6f6e2e6f
# (= "age-encryption.o" — age v1 magic).
```

## Restore drill (Fase 6)

The restore drill must run end-to-end at least once per quarter. It re-hydrates
the latest dump into an ephemeral Postgres and runs a smoke query.

```bash
export RESTORE_URL="postgres://drill:drill@127.0.0.1:5440/sindireceita_drill"
export BACKUP_BUCKET=sindireceita-backups
export RESTORE_VERIFY_SQL='select count(*) from users'
export RESTORE_VERIFY_MIN=1   # tighten once we know prod row count
sudo -u sindireceita-backup -E ./scripts/restore-drill.sh
```

Pass criteria:

- `pg_restore --exit-on-error` finishes 0.
- Smoke query returns >= `RESTORE_VERIFY_MIN`.
- Cleartext dump never lands on disk (`lsof | grep dump.pgc` during the run
  shows only `age` and `pg_restore` reading from anonymous pipes).

If the drill fails, raise an issue against this runbook and *do not* mark
the quarter's drill as passed.

## Negative test (must fail)

Trying to `pg_restore` an encrypted dump directly must fail loudly. Add this
to the drill's CI to catch a regression where someone accidentally turns
encryption off.

```bash
aws s3 cp "s3://$BACKUP_BUCKET/$(date -u +%F)/dump.pgc.age" /tmp/raw.age
! pg_restore --list /tmp/raw.age 2>/dev/null
# pg_restore must exit non-zero with "input file appears to be a text format
# dump. Please use psql." or "did not find magic number".
shred -u /tmp/raw.age
```

## Rotacao de chave (planned rotation)

Rotate the recipient key on a regular cadence (default: yearly) and any time
a backup host is decommissioned or compromised. Rotation does NOT re-encrypt
old dumps — they stay encrypted to the old key. Keep the old private key in
the offline secret store until the retention window for those dumps elapses.

The recommended path is a **dual-recipient transition**: list both old and
new public keys in `infra/age-backup.pub` for one rotation cycle so either
private key decrypts new dumps. This avoids a sharp cutover where one
mistimed change blocks restores. `age -R` reads every non-comment line of
the recipients file and encrypts to all of them.

1. **On the new backup host**, generate the keypair (the script writes it
   `0440 root:sindireceita-backup`; see § Primeira instalação):
   ```bash
   sudo BACKUP_AGE_KEY=/etc/sindireceita/age-backup.key.new \
     ./scripts/generate-backup-key.sh
   ```
2. **Append** the new public key as a second line in `infra/age-backup.pub`
   — do not delete the old line yet. Keep the comment header. Commit.
   ```text
   # Sindireceita backup recipient(s). Encrypt to ALL non-comment lines.
   age1<old-public-key>
   age1<new-public-key>   # added during rotation YYYY-MM-DD
   ```
3. SOPS-encrypt the new private key:
   ```bash
   sops --encrypt --age "$SOPS_AGE_RECIPIENT" \
        /etc/sindireceita/age-backup.key.new \
        > infra/sops/age-backup.key.enc.new
   git mv infra/sops/age-backup.key.enc.new infra/sops/age-backup.key.enc
   ```
4. **Stash a second copy of the cleartext private key in the offline cofre.**
   The cofre configuration is fixed (see § Cofre offline for the full
   policy):
   - Encrypt the private key into a **KeePassXC** `.kdbx` database
     (AES-256, Argon2id KDF) **inside** a **LUKS** (Linux) or **VeraCrypt**
     (cross-platform) volume on a **dedicated USB stick**.
   - **Único conteúdo do USB:** the encrypted volume, the KeePassXC DB,
     and a plain-text copy of `age-backup.pub` for verification — nothing
     else. No other files, no spare backups, no day-to-day documents.
   - **Localização física:** the USB MUST be stored **outside the
     custódio's primary residence** (family member, trusted relative,
     lawyer's office, or postal box are all acceptable). The USB MUST NOT
     live on the dev laptop — co-locating the cofre with the dev laptop
     nullifies the defense against the joint scenario "AWS credential
     compromised AND dev laptop compromised".
   - Verify the custódio primário (Pericles Luz) acknowledges receipt;
     the next quarterly audit (see § Auditoria trimestral) confirms the
     rotated key still decrypts.
5. Smoke a backup with both recipients — confirm `restore-drill.sh` works
   with **either** private key:
   ```bash
   sudo systemctl start sindireceita-backup.service
   sudo BACKUP_AGE_KEY=/etc/sindireceita/age-backup.key       \
     RESTORE_URL=postgres://drill:drill@127.0.0.1:5440/scratch \
     -u sindireceita-backup -E ./scripts/restore-drill.sh    # old key
   sudo BACKUP_AGE_KEY=/etc/sindireceita/age-backup.key.new   \
     RESTORE_URL=postgres://drill:drill@127.0.0.1:5440/scratch \
     -u sindireceita-backup -E ./scripts/restore-drill.sh    # new key
   ```
6. **One retention window later**, when every dump in the bucket can be
   decrypted by the new key alone, swap atomically and drop the old line:
   ```bash
   sudo install -m 0440 -o root -g sindireceita-backup \
        /etc/sindireceita/age-backup.key.new /etc/sindireceita/age-backup.key
   sudo rm /etc/sindireceita/age-backup.key.new
   ```
   Edit `infra/age-backup.pub` to remove the old recipient line; commit.
7. Continue to retain the old private key in the cofre until the longest
   retention window for any dump still encrypted to it has elapsed.

If you must do a hard cutover (incident: the old key is compromised), skip
the dual-recipient phase: replace the line in `infra/age-backup.pub`,
deploy, run the new backup once, and treat any old dumps as forensic
evidence to be decrypted only under controlled conditions.

## Cofre offline (2nd-tier secret store)

The offline copy of the private key is the single thing that turns the
catastrophic-loss scenario into a recoverable one. The configuration is
**fixed** (decision in [SIN-62261](/SIN/issues/SIN-62261)) — not a menu of
options — and is the same for every host:

- **Storage stack:** [KeePassXC](https://keepassxc.org) `.kdbx` database
  (AES-256 cipher, Argon2id KDF) **inside** an encrypted volume:
  - **LUKS** (Linux) — preferred when the custódio operates from a Linux
    host; OR
  - **VeraCrypt** — cross-platform fallback when the custódio operates
    from a non-Linux host.
- **Container:** a **dedicated USB stick** that holds the encrypted
  volume, the KeePassXC DB, and a plain-text copy of the matching public
  recipient (`age-backup.pub`) for verification — and **nothing else**.
  No other documents, no spare backups, no day-to-day files.
- **Localização física:** the USB MUST be stored **outside the custódio's
  primary residence** — family member, trusted relative, lawyer's
  office, or postal box are all acceptable. The USB MUST NOT live on the
  dev laptop. Co-locating the cofre with the dev laptop nullifies the
  defense against the joint scenario "AWS credential compromised AND
  dev laptop compromised".
- **Custódia primária:** Pericles Luz.
- **Plano B (catastrophic):** sealed envelope handed to a trusted party
  located outside the primary residence. See § Chave perdida for the
  retrieval procedure. Trusted-party identity is captured operationally
  with the placeholder `<TBD: Pericles preenche antes de Fase 4 prod cutoff>`.
- **4-eyes:** not satisfied by design — Sindireceita is a single-founder
  org during Fase 0–4. The formal multi-person procedure activates when
  a 2ª pessoa-chave is hired (see § Offboarding).

Record the operational state in this runbook (review during the
quarterly audit; see § Auditoria trimestral):

| Field | Value |
|-------|-------|
| Storage stack | KeePassXC `.kdbx` inside LUKS (or VeraCrypt) on dedicated USB |
| USB serial | _e.g. `Kingston DT 50 A1B2C3...`_ |
| Custódio primário | Pericles Luz |
| Localização física | `<TBD: Pericles preenche antes de Fase 4 prod cutoff>` |
| Trusted party (Plano B) | `<TBD: Pericles preenche antes de Fase 4 prod cutoff>` |
| Last verified | _YYYY-MM-DD_ (atualizado pela auditoria trimestral) |

> **Operator action — must be filled before Fase 4 prod cutoff.** If the
> table above still contains placeholders at cutoff, the encrypted-backup
> pipeline is not ready for production traffic. Block the Fase 4 sign-off.

Verification protocol (quarterly): see § Auditoria trimestral. The audit
exercises the cofre by mounting the volume, exporting the key, decrypting
a synthetic ciphertext, and re-sealing — then updates *Last verified*
above.

## Chave perdida (catastrophic loss)

The recovery path depends on which copies survive. Try the procedures in
order; only when **every** path below fails is the loss truly catastrophic.

### Plano A — primary cofre intact

The custódio primário (Pericles Luz) has the dedicated USB. Mount the
LUKS/VeraCrypt volume, open the KeePassXC `.kdbx`, export
`age-backup.key` to a temp file, and run the restore per § Restore drill.
This is the normal recovery flow and is exercised end-to-end during the
quarterly audit (see § Auditoria trimestral).

### Plano B — primary cofre destroyed, sealed envelope retrieval (B1)

The sealed envelope handed to a trusted party located outside the primary
residence is the second-tier offline copy. Trusted-party identity is
captured operationally; until the placeholder `<TBD: Pericles preenche
antes de Fase 4 prod cutoff>` in § Cofre offline is filled in, this
procedure is **non-functional** and Fase 4 prod cutoff is blocked.

Retrieval procedure (B1):

1. Pericles contacts the trusted party (identity per § Cofre offline) and
   coordinates an in-person hand-off.
2. Pericles retrieves the sealed envelope. Tamper evidence on the seal is
   inspected and recorded; a broken or modified seal escalates to a
   critical incident regardless of whether retrieval succeeds.
3. Open the envelope **in the presence of a witness** when feasible. The
   single-founder configuration may waive the witness requirement when no
   second person-key exists; record the waiver in the post-mortem.
4. Use the recorded passphrase to unlock the LUKS/VeraCrypt volume and
   the KeePassXC DB.
5. Export `age-backup.key` to a temp file (RAM-backed `tmpfs` preferred).
6. Run the restore per § Restore drill (Fase 6) against the target dump.
7. `shred -u` the temp file, prepare a fresh sealed envelope, and rotate
   to a new trusted-party hand-off slot before closing the incident.

> **4-eyes is not satisfied by design** in Fase 0–4 — Sindireceita is a
> single-founder org. The formal multi-person retrieval procedure
> activates when a 2ª pessoa-chave is hired; see § Offboarding for the
> activation trigger.

### Plano C — both cofre copies destroyed, key truly lost

If the recipient private key is lost AND **neither** offline copy exists
(USB destroyed AND sealed envelope unavailable):

- **Every dump encrypted to that key is unrecoverable.** Do not pretend
  otherwise.
- Open a critical incident.
- Pivot to the most recent recoverable source: streaming replica, WAL
  archive, or app-level export. Restore drills should already have
  validated those.
- Generate a new keypair (see § Rotação de chave) and start producing
  fresh encrypted backups immediately — every additional hour without an
  off-site backup compounds exposure.
- Run a full post-mortem: how did **all** copies disappear? The offline
  cofre + Plano B envelope exist specifically to make this scenario
  unreachable; understand why neither saved us.

## Auditoria trimestral

Cadence: **first Monday of January, April, July, and October**. The audit
is single-person by design (single-founder org during Fase 0–4) and
validates that the cofre configuration still matches policy AND that the
offline private key still decrypts a fresh ciphertext.

Procedure (6 steps):

1. **CTO routine** opens the quarterly audit issue with this checklist
   pre-filled and assigns it to Pericles.
2. **Pericles** mounts the LUKS/VeraCrypt volume, opens the KeePassXC
   `.kdbx`, and exports `age-backup.key` to a temp file (`tmpfs`
   preferred, e.g. `/dev/shm/age-backup.key`).
3. **Coder** generates a synthetic dump (~1 000 fake rows in a throwaway
   schema) and encrypts it with `infra/age-backup.pub`.
4. **Pericles** runs the round-trip:

   ```bash
   age -d -i /dev/shm/age-backup.key < dump.pgc.age \
     | pg_restore --clean -d "$DB_EPHEMERAL"
   ```

   and confirms the row count matches the synthetic dump.
5. **Pericles** `shred -u`s the temp key file and posts evidence on the
   audit issue: SHA-256 of the encrypted dump, restored row count,
   timestamp, and any anomalies.
6. **Failure at any step ⇒ P0 incident.** The cofre and/or recipient key
   are considered at risk; rotate per § Rotação de chave and treat the
   quarter's audit as failed.

Update the *Last verified* row in § Cofre offline at the end of every
successful audit.

## Offboarding

> **Status:** activates when a 2ª pessoa-chave is hired. Until then: **N/A.**
> The cofre is currently a single-custódio configuration (Pericles Luz);
> there is no role to off-board.

When activated (i.e. once the org has at least two key custodians):

- **≤ 4h after departure:** revoke the departing custodian's access by
  rotating the KeePassXC master password AND removing their keyfile (or
  hardware token) from the `.kdbx` configuration.
- **≤ 24h after departure:** rotate `infra/age-backup.pub` per § Rotação
  de chave (full dual-recipient cycle when the retention window permits;
  hard-cutover if the departure was acrimonious or the key may be
  compromised).
- **Same PR as the rotation:** update the nominal custodian list in this
  runbook (§ Cofre offline) so the audit log reflects the new state.
- **Out-of-cadence audit ≤ 7d after departure** to confirm the new cofre
  configuration still decrypts production ciphertexts and that the
  departed custodian's copies are demonstrably destroyed (or accounted
  for as forensic evidence under controlled conditions).

## Threat model recap

| Vector | Mitigated? |
|--------|------------|
| AWS/B2 credential leak | yes — attacker downloads ciphertext only |
| Backup host compromised | partial — attacker has the live key; rotate + revoke immediately |
| Repo credentials leaked (commit access) | yes — only ciphertext + public recipient in git |
| SOPS recipient key compromised | partial — attacker can decrypt the SOPS file *if* they also have repo read access; rotate both |
| Tampering with stored dump | yes — age v1+ MACs the payload (HMAC-SHA-256). `pg_restore` fails if even one byte is flipped. |
| Confused deputy on the backup host | partial — systemd unit runs as `sindireceita-backup`, the unit hides the private key via `InaccessiblePaths=`, and the key file is `0440 root:sindireceita-backup` |
| Tampered `age` binary on the backup host | not mitigated in this layer — pin via OS package manager and verify upstream signatures during host provisioning. Tracked alongside [SIN-62199](/SIN/issues/SIN-62199) hardening tasks. |

`age` v1.0+ is required (HMAC of the ciphertext). Earlier `age` releases
lack the MAC; both `backup.sh` and `restore-drill.sh` enforce this at
runtime via an `age --version` preflight, and `TestBackupScriptRejectsOldAge`
guards against accidental removal of that check.

### Defense-in-depth additions (recommended, not required)

- **Bucket-level SSE / Object Lock.** Client-side `age` is the actual
  confidentiality boundary, but enabling SSE-S3 (or B2 server-side
  encryption) plus an Object Lock retention policy adds one extra step
  for an attacker pivoting across compromised storage credentials. Apply
  during bucket provisioning; orthogonal to this runbook.
- **Pre-commit hooks.** `TestNoAgeSecretInGitHistory` is a passive
  scan that catches a leak after it lands in git. Pair it with
  pre-commit hooks (`gitleaks`, `trufflehog`) that block the commit
  before the leak ever reaches `git push`. The Go test is a guardrail,
  not a substitute for the pre-commit layer.
