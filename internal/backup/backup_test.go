package backup

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"filippo.io/age"
)

// ageMagic is the first eight bytes of every age v1 ciphertext header.
// Reference: https://age-encryption.org/v1 (header line literal).
const ageMagic = "age-encryption.org/v1"

// committedPlaceholderRecipient is the exact non-functional bootstrap value
// that infra/age-backup.pub must hold in git. Anything else means a real
// recipient slipped into the repo (forbidden by SIN-62220 must-fix #2: the
// matching private half could linger in /tmp on whichever host generated it).
// Operators replace this with the production recipient on the backup host
// per docs/operations/backup-restore.md § "Primeira instalação"; the file in git
// stays the placeholder forever.
const committedPlaceholderRecipient = "age1placeholder0000000000000000000000000000000000000000000000000"

// repoRoot resolves the repo root from this test file's location. Using
// runtime path resolution keeps the tests stable even if the test binary is
// invoked from a different working directory.
func repoRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	// internal/backup -> repo root.
	return filepath.Clean(filepath.Join(wd, "..", ".."))
}

// readPublicRecipientLine returns the single non-comment, non-empty line from
// the recipients file, mirroring `age -R`'s parser.
func readPublicRecipientLine(t *testing.T, path string) string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	var lines []string
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		lines = append(lines, line)
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("scan %s: %v", path, err)
	}
	if len(lines) != 1 {
		t.Fatalf("expected exactly one recipient in %s, got %d: %v", path, len(lines), lines)
	}
	return lines[0]
}

// TestPublicRecipientParses has two jobs, both required for the placeholder
// regime authorized by the CTO on SIN-62250:
//
//  1. The parser invariant — `age.ParseX25519Recipient` accepts a real X25519
//     recipient. A fresh ephemeral identity is generated inside the test so
//     this assertion is fully self-contained and never touches the committed
//     file's bytes.
//  2. The "real recipient must not slip back in" guard — the single non-
//     comment line in the *committed* infra/age-backup.pub must be exactly
//     the bootstrap placeholder. Anything else (including a syntactically
//     valid age recipient) fails CI, which is the rotation gate.
//
// Operators do replace the placeholder with the production recipient on the
// backup host before the first prod backup. That host-local edit is never
// supposed to land in git — see docs/operations/backup-restore.md § "Primeira
// instalação" and infra/sops/README.md.
func TestPublicRecipientParses(t *testing.T) {
	t.Parallel()

	// (1) Parser invariant — exercised against a fresh ephemeral identity so
	// the assertion does not depend on what is committed.
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatalf("generate ephemeral identity: %v", err)
	}
	ephemeralRecipient := id.Recipient().String()
	if !strings.HasPrefix(ephemeralRecipient, "age1") {
		t.Fatalf("ephemeral recipient %q does not look like an age X25519 public key", ephemeralRecipient)
	}
	if _, err := age.ParseX25519Recipient(ephemeralRecipient); err != nil {
		t.Fatalf("age.ParseX25519Recipient(ephemeral %q): %v", ephemeralRecipient, err)
	}

	// (2) Committed-file guard — the file in git is the non-functional
	// placeholder, full stop. Encrypting to it via `age -R` is intentionally
	// supposed to fail; that is what forces the operator to rotate before
	// real PII reaches the bucket.
	root := repoRoot(t)
	pubPath := filepath.Join(root, "infra", "age-backup.pub")
	committed := readPublicRecipientLine(t, pubPath)
	if committed != committedPlaceholderRecipient {
		t.Fatalf("committed %s recipient line is not the bootstrap placeholder.\n"+
			"  got:  %q\n  want: %q\n"+
			"A real recipient must NEVER be committed — its private half could "+
			"linger in /tmp on whichever host generated it. Replace the file "+
			"with the bootstrap placeholder and rotate the production key on "+
			"the backup host instead (see docs/operations/backup-restore.md).",
			pubPath, committed, committedPlaceholderRecipient)
	}
}

func TestEncryptDecryptRoundTrip(t *testing.T) {
	t.Parallel()
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatalf("generate identity: %v", err)
	}
	plaintext := bytes.Repeat([]byte("PII payload — do not leak\n"), 4096)

	var ct bytes.Buffer
	w, err := age.Encrypt(&ct, id.Recipient())
	if err != nil {
		t.Fatalf("age.Encrypt: %v", err)
	}
	if _, err := w.Write(plaintext); err != nil {
		t.Fatalf("write plaintext: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close encryptor: %v", err)
	}

	if !bytes.HasPrefix(ct.Bytes(), []byte(ageMagic)) {
		t.Fatalf("ciphertext does not start with age magic %q; got %q",
			ageMagic, ct.Bytes()[:min(ct.Len(), 32)])
	}

	r, err := age.Decrypt(bytes.NewReader(ct.Bytes()), id)
	if err != nil {
		t.Fatalf("age.Decrypt: %v", err)
	}
	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read plaintext: %v", err)
	}
	if !bytes.Equal(got, plaintext) {
		t.Fatalf("round-trip mismatch (len got=%d want=%d)", len(got), len(plaintext))
	}
}

func TestTamperedCiphertextFailsDecrypt(t *testing.T) {
	// age v1 includes an HMAC over the ciphertext; flipping a payload byte
	// must surface as a decrypt error rather than silently returning bytes.
	t.Parallel()
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatalf("generate identity: %v", err)
	}
	var ct bytes.Buffer
	w, err := age.Encrypt(&ct, id.Recipient())
	if err != nil {
		t.Fatalf("age.Encrypt: %v", err)
	}
	if _, err := w.Write([]byte("a small dump that fits in one chunk")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	tampered := append([]byte(nil), ct.Bytes()...)
	// Flip a byte well past the header (header ends with --- + base64 mac line).
	if len(tampered) < 200 {
		t.Fatalf("ciphertext too short to test tamper resistance: %d", len(tampered))
	}
	tampered[len(tampered)-10] ^= 0x01

	r, err := age.Decrypt(bytes.NewReader(tampered), id)
	if err != nil {
		// Some tamper points fail at header parse — that's also acceptable.
		return
	}
	if _, err := io.ReadAll(r); err == nil {
		t.Fatal("decrypt of tampered ciphertext returned no error; age MAC is not protecting payload")
	}
}

func TestShellScriptsParse(t *testing.T) {
	t.Parallel()
	root := repoRoot(t)
	scripts := []string{
		"scripts/backup.sh",
		"scripts/backup-restore.sh",
		"scripts/generate-backup-key.sh",
	}
	for _, rel := range scripts {
		rel := rel
		t.Run(rel, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(root, rel)
			cmd := exec.Command("bash", "-n", path)
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("bash -n %s failed: %v\n%s", path, err, out)
			}
			info, err := os.Stat(path)
			if err != nil {
				t.Fatalf("stat %s: %v", path, err)
			}
			if info.Mode()&0o111 == 0 {
				t.Fatalf("%s is not executable (mode=%v)", path, info.Mode())
			}
		})
	}
}

// TestBackupScriptPipelineProducesAgeCiphertext exercises scripts/backup.sh
// end-to-end with mocked `pg_dump` and `aws` binaries injected via PATH. The
// "uploaded" object is captured as a local file and asserted to be valid age
// ciphertext that decrypts back to the synthetic dump bytes.
//
// This is the smallest test that proves the encryption stage cannot be
// silently dropped from the pipeline — the existing approach would still
// upload, but the assertion on age magic bytes would fail.
func TestBackupScriptPipelineProducesAgeCiphertext(t *testing.T) {
	if _, err := exec.LookPath("age"); err != nil {
		t.Skipf("age binary not in PATH: %v", err)
	}

	root := repoRoot(t)
	tmp := t.TempDir()

	// Generate an ephemeral keypair so the test has a private key to decrypt
	// with. The committed infra/age-backup.pub is overridden via the
	// BACKUP_AGE_RECIPIENTS env knob.
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatalf("generate identity: %v", err)
	}
	recipientsPath := filepath.Join(tmp, "recipients.pub")
	if err := os.WriteFile(recipientsPath, []byte("# test\n"+id.Recipient().String()+"\n"), 0o600); err != nil {
		t.Fatalf("write recipients: %v", err)
	}

	// Synthetic dump payload. The exact bytes don't matter to age; we just
	// need to prove encryption and end-to-end pass-through.
	dump := bytes.Repeat([]byte("synthetic pg_dump payload — secret\n"), 256)
	dumpPath := filepath.Join(tmp, "dump.bin")
	if err := os.WriteFile(dumpPath, dump, 0o600); err != nil {
		t.Fatalf("write dump: %v", err)
	}

	// Mock pg_dump: ignore args, stream the synthetic dump to stdout.
	mockBin := filepath.Join(tmp, "bin")
	if err := os.MkdirAll(mockBin, 0o700); err != nil {
		t.Fatalf("mkdir bin: %v", err)
	}
	pgDumpPath := filepath.Join(mockBin, "pg_dump")
	pgDumpScript := fmt.Sprintf("#!/usr/bin/env bash\nexec cat %q\n", dumpPath)
	if err := os.WriteFile(pgDumpPath, []byte(pgDumpScript), 0o700); err != nil {
		t.Fatalf("write mock pg_dump: %v", err)
	}

	// Mock aws: capture the s3 cp source from a file path argument to a local
	// file, derived from the destination URL. backup.sh passes the path (not
	// a stream) to aws s3 cp; backup.sh post-SIN-62267 also runs s3api
	// head-object to verify the upload landed.
	uploadedPath := filepath.Join(tmp, "uploaded.bin")
	awsPath := filepath.Join(mockBin, "aws")
	awsScript := fmt.Sprintf(`#!/usr/bin/env bash
# minimal mock — handles `+"`aws s3 cp ... SRC DEST`"+` and `+"`aws s3api head-object ...`"+`
set -euo pipefail
service=${1:-}
op=${2:-}
shift 2 || true
# strip --endpoint-url <url> if present
args=()
while (( $# )); do
  case "$1" in
    --endpoint-url) shift; shift; continue ;;
    *) args+=("$1"); shift ;;
  esac
done
set -- "${args[@]}"
case "$service/$op" in
  s3/cp)
    pos=()
    while (( $# )); do
      case "$1" in
        --no-progress) shift ;;
        --expected-size) shift; shift ;;
        --*) shift ;;
        *) pos+=("$1"); shift ;;
      esac
    done
    src=${pos[0]:-}
    cp -- "$src" %q
    exit 0
    ;;
  s3api/head-object)
    # Print ContentLength of the previously-uploaded file so backup.sh's
    # verify stage can compare against the local ciphertext size.
    stat -c %%s -- %q
    exit 0
    ;;
  *)
    echo "mock aws: unexpected args: $service $op" >&2
    exit 2
    ;;
esac
`, uploadedPath, uploadedPath)
	if err := os.WriteFile(awsPath, []byte(awsScript), 0o700); err != nil {
		t.Fatalf("write mock aws: %v", err)
	}

	cmd := exec.Command("bash", filepath.Join(root, "scripts", "backup.sh"))
	stateDir := filepath.Join(tmp, "state")
	tmpStage := filepath.Join(tmp, "stage")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatalf("mkdir state: %v", err)
	}
	if err := os.MkdirAll(tmpStage, 0o700); err != nil {
		t.Fatalf("mkdir stage: %v", err)
	}
	cmd.Env = append(
		os.Environ(),
		"PATH="+mockBin+string(os.PathListSeparator)+os.Getenv("PATH"),
		"DATABASE_URL=postgres://ignored",
		"BACKUP_BUCKET=test-bucket",
		"BACKUP_AGE_RECIPIENTS="+recipientsPath,
		"BACKUP_NODE_ID=test-node",
		"BACKUP_STATE_DIR="+stateDir,
		"BACKUP_TMPDIR="+tmpStage,
	)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("backup.sh failed: %v\nstderr:\n%s", err, stderr.String())
	}

	got, err := os.ReadFile(uploadedPath)
	if err != nil {
		t.Fatalf("read uploaded file: %v", err)
	}
	if !bytes.HasPrefix(got, []byte(ageMagic)) {
		hexLen := len(got)
		if hexLen > 64 {
			hexLen = 64
		}
		t.Fatalf("uploaded object does not start with age magic %q; first %d bytes: %q",
			ageMagic, hexLen, got[:hexLen])
	}

	// The mock aws must have received non-trivial bytes — guards against a
	// regression where stdin gets disconnected mid-pipeline.
	if len(got) <= len(dump) {
		t.Fatalf("uploaded object suspiciously small: got=%d, plaintext=%d (age adds header overhead)", len(got), len(dump))
	}

	// Finally, decrypt and confirm we recover the original dump bytes — the
	// pipeline really did encrypt with a recipient we control.
	r, err := age.Decrypt(bytes.NewReader(got), id)
	if err != nil {
		t.Fatalf("age.Decrypt uploaded object: %v", err)
	}
	plain, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read decrypted: %v", err)
	}
	if !bytes.Equal(plain, dump) {
		t.Fatalf("decrypted payload != original dump (got=%d bytes, want=%d)", len(plain), len(dump))
	}
}

// agePrivateKeyNeedle is the regex (POSIX ERE / Go RE2 compatible) that
// matches an age v1 private key: the version-tagged bech32 HRP followed by
// at least eight bech32 payload characters. The bech32 alphabet excludes B,
// I, O, and 1.
//
// The pattern is assembled from three string fragments so this source file
// never contains the contiguous HRP literal — otherwise git log -p of the
// diff that added the scan would match itself.
const agePrivateKeyNeedle = "AGE" + "-SECRET-KEY-" + `1[ACDEFGHJKLMNPQRSTUVWXYZ234567]{8,}`

// scanGitForAgeSecret runs the git-history age-private-key scan against the
// repo at root. It returns the offending patch line if a leak is detected,
// or "" otherwise. The match is anchored on patch +/- lines so commit
// message bodies that legitimately reference the bech32 HRP byte for
// explanatory purposes do not trip the scan.
func scanGitForAgeSecret(t *testing.T, root string) string {
	t.Helper()
	cmd := exec.Command("git", "-C", root, "log", "--all", "-p", "-G", agePrivateKeyNeedle)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git log failed: %v\n%s", err, out)
	}
	re := regexp.MustCompile(`(?m)^[+-].*` + agePrivateKeyNeedle)
	if loc := re.FindIndex(out); loc != nil {
		return string(out[loc[0]:loc[1]])
	}
	return ""
}

// TestNoAgeSecretInGitHistory greps the entire git history for the bech32
// HRP that age-keygen emits in front of every private key, followed by
// enough payload bytes to distinguish a real key from prose. A hit means a
// private key was committed at some point and the repo is compromised
// forever.
func TestNoAgeSecretInGitHistory(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("git not in PATH: %v", err)
	}
	root := repoRoot(t)
	if _, err := os.Stat(filepath.Join(root, ".git")); err != nil {
		t.Skipf(".git not present (likely not a git checkout): %v", err)
	}
	if hit := scanGitForAgeSecret(t, root); hit != "" {
		t.Fatalf("git history added/removed an age private key; rotate immediately:\n%s", hit)
	}
}

// TestSecretScanFiresOnLeakedKey is the regression test for the regression
// test: synthesize a leaked-shape secret in a temp git repo and confirm
// scanGitForAgeSecret detects it. Without this, "scan always returns clean"
// regressions would stay invisible.
func TestSecretScanFiresOnLeakedKey(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("git not in PATH: %v", err)
	}

	tmp := t.TempDir()
	gitInit := func(args ...string) {
		t.Helper()
		full := append([]string{"-C", tmp}, args...)
		cmd := exec.Command("git", full...)
		// Hermetic env so the test does not pick up developer config.
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@example.com",
			"GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@example.com",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	gitInit("init", "-q")
	gitInit("config", "commit.gpgsign", "false")

	// Synthesize a fake-but-pattern-valid key. Q is in the bech32 alphabet.
	// The constant is built via concatenation so this source line itself
	// does not match the needle when the test repo (the real CRM repo) is
	// scanned by TestNoAgeSecretInGitHistory.
	fakeKey := "AGE" + "-SECRET-KEY-" + "1QQQQQQQQQQQQQQQQQQQQQQQQQ"
	leakPath := filepath.Join(tmp, "leaked.txt")
	if err := os.WriteFile(leakPath, []byte(fakeKey+"\n"), 0o600); err != nil {
		t.Fatalf("write leak fixture: %v", err)
	}
	gitInit("add", "leaked.txt")
	gitInit("commit", "-q", "-m", "fixture: synthetic leaked key for scan regression test")

	hit := scanGitForAgeSecret(t, tmp)
	if hit == "" {
		t.Fatalf("scan did NOT detect synthetic leaked key in temp repo at %s — secret-scan regression", tmp)
	}
	// The matched line must include the synthesized HRP, otherwise we are
	// matching something else and the assertion is meaningless.
	if !regexp.MustCompile(agePrivateKeyNeedle).MatchString(hit) {
		t.Fatalf("scan hit does not contain the expected needle:\n%s", hit)
	}
}

// TestPgRestoreRejectsRawAge proves that a raw .age blob cannot be fed into
// pg_restore. This is the deployment safety net: if a future operator forgets
// the `age -d` step, pg_restore must fail loudly.
func TestPgRestoreRejectsRawAge(t *testing.T) {
	if _, err := exec.LookPath("pg_restore"); err != nil {
		t.Skipf("pg_restore not in PATH: %v", err)
	}

	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatalf("generate identity: %v", err)
	}
	tmp := t.TempDir()
	encPath := filepath.Join(tmp, "dump.pgc.age")

	f, err := os.Create(encPath)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	w, err := age.Encrypt(f, id.Recipient())
	if err != nil {
		f.Close()
		t.Fatalf("age.Encrypt: %v", err)
	}
	if _, err := w.Write([]byte("not a real pg dump — magic bytes must still mismatch")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close file: %v", err)
	}

	// `pg_restore --list` only reads the file header; it must reject the
	// age magic with a non-zero exit. We use --list because it doesn't need
	// a database connection.
	cmd := exec.Command("pg_restore", "--list", encPath)
	out, err := cmd.CombinedOutput()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("pg_restore --list on raw age file unexpectedly succeeded; output:\n%s", out)
	}
	if exitErr.ExitCode() == 0 {
		t.Fatalf("pg_restore exited 0 on raw age file; output:\n%s", out)
	}
}

// TestBackupScriptRejectsOldAge stubs `age --version` to claim v0.9.3 and
// asserts backup.sh aborts BEFORE invoking pg_dump. v0.x age binaries lack
// the HMAC over the ciphertext, so silently using one would defeat the
// tamper-detection invariant proved by TestTamperedCiphertextFailsDecrypt.
func TestBackupScriptRejectsOldAge(t *testing.T) {
	root := repoRoot(t)
	tmp := t.TempDir()

	mockBin := filepath.Join(tmp, "bin")
	if err := os.MkdirAll(mockBin, 0o700); err != nil {
		t.Fatalf("mkdir bin: %v", err)
	}

	// Stub age binary that lies about its version.
	ageStub := filepath.Join(mockBin, "age")
	ageScript := `#!/usr/bin/env bash
case "${1:-}" in
  --version) echo "v0.9.3"; exit 0 ;;
  *) echo "stub age: unexpected invocation: $*" >&2; exit 99 ;;
esac
`
	if err := os.WriteFile(ageStub, []byte(ageScript), 0o700); err != nil {
		t.Fatalf("write stub age: %v", err)
	}

	// Sentinel: pg_dump touches a marker file. If the version guard fires
	// BEFORE the pipeline starts, this file must not exist after the run.
	sentinel := filepath.Join(tmp, "pg_dump.invoked")
	pgDumpStub := filepath.Join(mockBin, "pg_dump")
	pgDumpScript := fmt.Sprintf("#!/usr/bin/env bash\ntouch %q\n", sentinel)
	if err := os.WriteFile(pgDumpStub, []byte(pgDumpScript), 0o700); err != nil {
		t.Fatalf("write stub pg_dump: %v", err)
	}

	awsStub := filepath.Join(mockBin, "aws")
	if err := os.WriteFile(awsStub, []byte("#!/usr/bin/env bash\nexit 0\n"), 0o700); err != nil {
		t.Fatalf("write stub aws: %v", err)
	}

	cmd := exec.Command("bash", filepath.Join(root, "scripts", "backup.sh"))
	cmd.Env = append(
		os.Environ(),
		"PATH="+mockBin+string(os.PathListSeparator)+os.Getenv("PATH"),
		"DATABASE_URL=postgres://ignored",
		"BACKUP_BUCKET=test-bucket",
		"BACKUP_AGE_RECIPIENTS="+filepath.Join(root, "infra", "age-backup.pub"),
	)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	err := cmd.Run()
	if err == nil {
		t.Fatalf("backup.sh exited 0 with stub age v0.9.3; expected hard fail.\nstderr:\n%s", stderr.String())
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("expected ExitError, got %T: %v", err, err)
	}
	if !strings.Contains(stderr.String(), "age >= 1.0 required") {
		t.Fatalf("stderr does not mention the version guard message:\n%s", stderr.String())
	}
	if _, err := os.Stat(sentinel); err == nil {
		t.Fatalf("backup.sh invoked pg_dump despite stub age v0.9.3; version preflight must run BEFORE the pipeline.\nstderr:\n%s", stderr.String())
	}
}

// readBackupServiceBlock returns the YAML body of the `backup:` service from
// a compose file (the lines from `  backup:` through the next sibling
// `<key>:` at the same indentation, or EOF). This is a flat byte slice — we
// do NOT want to depend on a YAML parser dependency just to do invariant
// grepping over a single service block.
func readBackupServiceBlock(t *testing.T, composePath string) []byte {
	t.Helper()
	body, err := os.ReadFile(composePath)
	if err != nil {
		t.Fatalf("read %s: %v", composePath, err)
	}
	// Match indented `  backup:` (2-space indent inside `services:`) up to
	// the next two-space-indented `<word>:` or EOF. The (?m)-flag anchors
	// `^` to a line, and `(?s)` lets `.*?` cross newlines.
	re := regexp.MustCompile(`(?ms)^  backup:\s*\n(.*?)(?:^  [A-Za-z][A-Za-z0-9_-]*:\s*$|\z)`)
	loc := re.FindSubmatchIndex(body)
	if loc == nil {
		t.Fatalf("could not locate `  backup:` service block in %s", composePath)
	}
	return body[loc[2]:loc[3]]
}

// TestComposeBackupSidecarDeniesPrivateKey is the regression test for the
// container-equivalent of legacy SIN-62250 MEDIUM #2 (formerly
// `InaccessiblePaths=/etc/sindireceita/age-backup.key` on the systemd unit).
//
// The backup sidecar does NOT need /etc/sindireceita/age-backup.key — the
// scheduled service only encrypts via the public recipient. Therefore the
// service definition MUST NOT bind-mount, env-mount, or otherwise reference
// the age-backup.key path anywhere in its block. Only `backup-restore.sh`
// (invoked manually via `docker compose run --rm --user 0:0
// -v ...key:...:ro backup /usr/local/bin/backup-restore.sh`) ever sees
// the private key. (PR #226 introduced a separate `restore-drill.sh`
// orchestrator for the Fase 6 quarterly drill — different scope; that
// script lives at the same path on `fork/main` but does not bind-mount
// the age private key.)
//
// This catches:
//   - someone adding a `volumes: ["/etc/sindireceita/age-backup.key:..."]`
//     line to the backup service to "help with the restore drill"
//   - someone adding `BACKUP_AGE_KEY=...` as an env var to the backup
//     service block
//   - someone adding a `secrets:` reference that exposes the private key
//
// See ADR 0102 § "Least privilege" and the hardening-invariant mapping
// table for why this matters.
func TestComposeBackupSidecarDeniesPrivateKey(t *testing.T) {
	t.Parallel()
	root := repoRoot(t)
	for _, composeRel := range []string{
		"deploy/compose/compose.stg.yml",
		"deploy/compose/compose.yml",
	} {
		composeRel := composeRel
		t.Run(composeRel, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(root, composeRel)
			block := readBackupServiceBlock(t, path)
			// `age-backup.key` is the only forbidden token (the public
			// `age-backup.pub` may be referenced, e.g. baked into the image
			// or mounted read-only).
			if bytes.Contains(block, []byte("age-backup.key")) {
				t.Fatalf("compose service `backup` in %s references age-backup.key — the private key must be unreachable from the scheduled sidecar. Block:\n%s",
					path, block)
			}
		})
	}
}

// TestComposeBackupSidecarHardeningInvariants asserts the security-bar
// compose primitives are present on the backup service in both staging and
// dev compose files. This is the structural equivalent of the legacy
// systemd-unit hardening grep (User=, NoNewPrivileges=true, ProtectSystem,
// etc.) — see the hardening-invariant mapping table in ADR 0102 for the
// 1:1 correspondence.
//
// Each invariant absence has a concrete failure mode:
//   - `user: "65534:65534"` missing → container runs as root (legacy:
//     `User=sindireceita-backup` on the systemd unit)
//   - `read_only: true` missing → image filesystem writable (legacy:
//     `ProtectSystem=strict`)
//   - `cap_drop:` not dropping ALL → setuid/setgid attack surface preserved
//     (legacy: `RestrictSUIDSGID=true`)
//   - `security_opt: no-new-privileges:true` missing → setuid binaries can
//     still elevate (legacy: `NoNewPrivileges=true`)
//   - `tmpfs: /tmp` missing → dump/ciphertext staging lands on a writable
//     persistent FS (legacy: `PrivateTmp=true`)
func TestComposeBackupSidecarHardeningInvariants(t *testing.T) {
	t.Parallel()
	root := repoRoot(t)
	for _, composeRel := range []string{
		"deploy/compose/compose.stg.yml",
		"deploy/compose/compose.yml",
	} {
		composeRel := composeRel
		t.Run(composeRel, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(root, composeRel)
			block := readBackupServiceBlock(t, path)
			required := []struct {
				needle string
				why    string
			}{
				{`user: "65534:65534"`, "container must run as nobody, not root"},
				{`read_only: true`, "root filesystem must be read-only (ProtectSystem=strict equivalent)"},
				{`no-new-privileges:true`, "must block setuid privilege escalation (NoNewPrivileges=true equivalent)"},
				{`ALL`, "cap_drop must drop ALL caps (RestrictSUIDSGID + ProtectKernelModules equivalent)"},
				{`/tmp`, "tmpfs /tmp must be present for scratch staging (PrivateTmp equivalent)"},
			}
			for _, req := range required {
				if !bytes.Contains(block, []byte(req.needle)) {
					t.Errorf("compose backup service in %s missing required hardening token %q (%s).\nblock:\n%s",
						path, req.needle, req.why, block)
				}
			}
		})
	}
}

// TestRunbookRestorePipelineInvocation is the regression test for the
// SIN-63195 SE-review BLOCKER #2 fix. The documented restore-pipeline
// invocation in docs/operations/backup-restore.md MUST satisfy two
// invariants, or the quarterly Fase 6 drill ([SIN-62199]) literally
// cannot execute:
//
//  1. The container runs with `--user 0:0` so the bind-mounted host
//     private key (host perms `0440 root:sindireceita-backup`) is
//     readable. The scheduled backup service runs as `user: 65534`
//     (nobody) by compose default; nobody is not in the
//     sindireceita-backup group, so without the explicit `--user 0:0`
//     the read fails EACCES and pg_restore never sees the cleartext
//     dump. The container retains `read_only`, `cap_drop: ALL`,
//     `no-new-privileges`, and `tmpfs /tmp` from the service
//     definition; `--user 0:0` only changes the UID inside the
//     container namespace.
//
//  2. The restore target is passed via `PG*` env vars (NOT a
//     `RESTORE_URL=postgres://user:pw@host/db` form). pg_restore /
//     psql read PGHOST/PGPORT/PGDATABASE/PGUSER/PGPASSWORD from the
//     environment, which is invisible to `ps aux`. A URL-on-argv form
//     leaks the password to anything that can read `/proc/<pid>/cmdline`
//     on the host (SE-review MEDIUM #1).
//
// If a future doc edit removes either invariant, this test fails. The
// "Restore drill (Fase 6)" section is the canonical operator-facing
// invocation and the one the quarterly drill orchestrator
// (`scripts/restore-drill.sh`, SIN-63187) calls for the real-decrypt
// path; the rotation section's invocations are also asserted via grep
// to keep the patterns consistent across the doc.
func TestRunbookRestorePipelineInvocation(t *testing.T) {
	t.Parallel()
	root := repoRoot(t)
	path := filepath.Join(root, "docs", "operations", "backup-restore.md")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}

	// Extract the "Restore drill (Fase 6)" section, which is the
	// canonical operator-facing invocation. Bounded by `^## Restore drill`
	// header through the next `^## ` (next H2) — `(?ms)` lets `.*?`
	// cross newlines and anchors `^` to a line.
	re := regexp.MustCompile(`(?ms)^## Restore drill \(Fase 6\)\s*\n(.*?)(?:^## |\z)`)
	loc := re.FindSubmatchIndex(body)
	if loc == nil {
		t.Fatalf("could not locate `## Restore drill (Fase 6)` section in %s", path)
	}
	section := body[loc[2]:loc[3]]

	required := []struct {
		needle string
		why    string
	}{
		{"--user 0:0",
			"restore-pipeline container MUST run as root so bind-mounted host key (0440 root:sindireceita-backup) is readable — SIN-63195 SE BLOCKER #2"},
		{"-v /etc/sindireceita/age-backup.key:/etc/sindireceita/age-backup.key:ro",
			"private age key MUST be bind-mounted read-only with the absolute host path"},
		{"backup /usr/local/bin/backup-restore.sh",
			"invocation MUST target the inner restore pipeline at /usr/local/bin/backup-restore.sh (NOT restore-drill.sh — the latter is the SIN-63187 outer drill orchestrator)"},
		{"PGHOST=",
			"restore target MUST use PG* env vars (libpq env interface), NOT --dbname=$URL on argv — SIN-63195 SE MEDIUM #1"},
		{"PGPASSWORD=",
			"restore target MUST pass password via PGPASSWORD env (NOT a libpq URL on argv) — SIN-63195 SE MEDIUM #1"},
	}
	for _, req := range required {
		if !bytes.Contains(section, []byte(req.needle)) {
			t.Errorf("Restore drill (Fase 6) section in %s missing required token %q (%s).\nsection start:\n%s",
				path, req.needle, req.why, section[:min(len(section), 800)])
		}
	}

	// Negative: the older `RESTORE_URL='postgres://...@host/db'` form
	// MUST NOT appear in the doc — every documented invocation
	// (including the rotation section) was migrated to the PG* split.
	// Old runbook snippets that survive a future copy-paste are exactly
	// what this test catches.
	if bytes.Contains(body, []byte("RESTORE_URL='postgres://")) {
		t.Errorf("%s still contains a legacy `RESTORE_URL='postgres://...` invocation. "+
			"Replace with PG* env vars (libpq env interface) — SIN-63195 SE MEDIUM #1 forbids URL-on-argv.", path)
	}
}

// TestComposeBackupSidecarKeyIsolationFromVolumes is a tighter SE-review
// safety net for BLOCKER #2: even though TestComposeBackupSidecarDeniesPrivateKey
// already greps for the literal `age-backup.key` token, this test
// additionally asserts that the scheduled `backup` service in compose
// declares NO host bind mounts under `/etc/sindireceita/` whatsoever.
// The named docker volume `sindireceita-backup-state` for state is fine
// (no host path); a host bind like `- /etc/sindireceita:...` would
// expose the entire secrets directory — including the private key — to
// the scheduled service even if the literal key path is not named.
//
// If you legitimately need to mount /etc/sindireceita into the
// scheduled service for some future reason (you almost certainly do not
// — restore is out-of-band by design), update this test in the SAME
// commit and explain why on the PR.
func TestComposeBackupSidecarKeyIsolationFromVolumes(t *testing.T) {
	t.Parallel()
	root := repoRoot(t)
	for _, composeRel := range []string{
		"deploy/compose/compose.stg.yml",
		"deploy/compose/compose.yml",
	} {
		composeRel := composeRel
		t.Run(composeRel, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(root, composeRel)
			block := readBackupServiceBlock(t, path)
			if bytes.Contains(block, []byte("/etc/sindireceita")) {
				t.Fatalf("compose service `backup` in %s mounts /etc/sindireceita (any path under it) — the scheduled service must not see host secrets. Block:\n%s",
					path, block)
			}
		})
	}
}
