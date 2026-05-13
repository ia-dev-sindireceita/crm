package password

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"
)

// TestHash_RoundTrip is the core acceptance criterion #1 lite — hash and
// verify round-trip with the production parameters.
func TestHash_RoundTrip(t *testing.T) {
	t.Parallel()
	h := Default()
	enc, err := h.Hash("correct-horse-battery-staple")
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}
	if !strings.HasPrefix(enc, "argon2id$v=19$m=65536,t=3,p=1$") {
		t.Fatalf("encoded prefix wrong: %q", enc)
	}
	ok, needsRehash, err := h.Verify(enc, "correct-horse-battery-staple")
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if !ok {
		t.Fatalf("Verify: ok=false on round-trip")
	}
	if needsRehash {
		t.Fatalf("Verify: needsRehash=true on round-trip with current params")
	}
	bad, _, err := h.Verify(enc, "wrong-password")
	if err != nil {
		t.Fatalf("Verify(wrong): err=%v want nil", err)
	}
	if bad {
		t.Fatalf("Verify(wrong): ok=true (mismatch must be ok=false, err=nil)")
	}
}

// TestHash_FixedSalt_Deterministic — acceptance criterion #1: vetor fixo.
// With a deterministic randRead, the encoded output is byte-stable across
// runs, which is what makes the algorithm parameters auditable from a
// stored hash.
func TestHash_FixedSalt_Deterministic(t *testing.T) {
	t.Parallel()
	h := Default()
	// Use a small parameter to keep the test fast — the determinism
	// property is parameter-independent.
	h.MemoryKiB = 8 * 1024 // 8 MiB
	h.Iterations = 1
	h.Parallelism = 1
	h.randRead = func(b []byte) (int, error) {
		for i := range b {
			b[i] = byte(i)
		}
		return len(b), nil
	}
	a, err := h.Hash("super-secret-12")
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}
	b, err := h.Hash("super-secret-12")
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}
	if a != b {
		t.Fatalf("Hash with deterministic salt produced different outputs:\n  a=%q\n  b=%q", a, b)
	}
	// Round-trip the deterministic vector under the canonical Verify path
	// (with prod parameters) — this asserts decode parses params correctly
	// and Verify falls through to argon2.IDKey with the parsed values.
	ver := Default()
	ok, needsRehash, err := ver.Verify(a, "super-secret-12")
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if !ok {
		t.Fatalf("Verify: ok=false on deterministic vector")
	}
	if !needsRehash {
		t.Fatalf("Verify: needsRehash=false — params (m=8192,t=1) differ from prod, must flag for upgrade")
	}
}

// TestHash_SaltUnique — every Hash call must read fresh entropy from the
// salt source so two identical plaintexts produce two distinct encodings.
func TestHash_SaltUnique(t *testing.T) {
	t.Parallel()
	h := Default()
	h.MemoryKiB = 8 * 1024
	h.Iterations = 1
	a, err := h.Hash("same-input")
	if err != nil {
		t.Fatalf("Hash a: %v", err)
	}
	b, err := h.Hash("same-input")
	if err != nil {
		t.Fatalf("Hash b: %v", err)
	}
	if a == b {
		t.Fatalf("two Hash calls produced identical output (salt not random)")
	}
}

// TestHash_RejectsEmpty — boundary: empty plaintext returns the typed
// sentinel; never silently hashes "".
func TestHash_RejectsEmpty(t *testing.T) {
	t.Parallel()
	if _, err := Default().Hash(""); !errors.Is(err, ErrEmptyPlaintext) {
		t.Fatalf("err=%v want ErrEmptyPlaintext", err)
	}
}

// TestHash_RejectsZeroParams — a misconfigured hasher with zero memory /
// iterations / parallelism / saltLen / keyLen MUST refuse rather than
// silently produce a low-cost or zero-byte digest.
func TestHash_RejectsZeroParams(t *testing.T) {
	t.Parallel()
	cases := map[string]Argon2idHasher{
		"zero-memory":      {MemoryKiB: 0, Iterations: 1, Parallelism: 1, SaltLen: 16, KeyLen: 32},
		"zero-iterations":  {MemoryKiB: 1024, Iterations: 0, Parallelism: 1, SaltLen: 16, KeyLen: 32},
		"zero-parallelism": {MemoryKiB: 1024, Iterations: 1, Parallelism: 0, SaltLen: 16, KeyLen: 32},
		"zero-saltlen":     {MemoryKiB: 1024, Iterations: 1, Parallelism: 1, SaltLen: 0, KeyLen: 32},
		"zero-keylen":      {MemoryKiB: 1024, Iterations: 1, Parallelism: 1, SaltLen: 16, KeyLen: 0},
	}
	for name, h := range cases {
		h := h
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, err := h.Hash("anything"); !errors.Is(err, ErrInvalidParams) {
				t.Fatalf("err=%v want ErrInvalidParams", err)
			}
		})
	}
}

// TestVerify_NeedsRehash_OldParams — acceptance criterion #3: a stored
// hash with non-current parameters returns needsRehash=true; one with
// current parameters returns needsRehash=false.
func TestVerify_NeedsRehash_OldParams(t *testing.T) {
	t.Parallel()
	old := &Argon2idHasher{
		MemoryKiB: 8 * 1024, Iterations: 1, Parallelism: 1,
		SaltLen: 16, KeyLen: 32,
	}
	enc, err := old.Hash("hello-world-12")
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}
	ver := Default()
	ok, needsRehash, err := ver.Verify(enc, "hello-world-12")
	if err != nil || !ok {
		t.Fatalf("Verify: ok=%v err=%v", ok, err)
	}
	if !needsRehash {
		t.Fatalf("needsRehash=false — old params should flag for §3 upgrade")
	}

	// And the round-trip with current params: needsRehash=false.
	enc2, err := ver.Hash("hello-world-12")
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}
	ok, needsRehash, err = ver.Verify(enc2, "hello-world-12")
	if err != nil || !ok {
		t.Fatalf("Verify: ok=%v err=%v", ok, err)
	}
	if needsRehash {
		t.Fatalf("needsRehash=true on identical params — should be false")
	}
}

// TestVerify_NeedsRehash_LegacyShape — the SIN-62213 helper emitted
// "$argon2id$v=19$m=65536,t=3,p=4$..." with a leading '$' and p=4. Verify
// MUST decode it (so existing rows in user.password_hash still log in)
// and flag needsRehash=true so they migrate to the canonical shape.
func TestVerify_NeedsRehash_LegacyShape(t *testing.T) {
	t.Parallel()
	// Synthesize a legacy-shape encoded value by hashing under p=4 and
	// prepending '$' — same bytes a SIN-62213 row would carry.
	legacyParams := &Argon2idHasher{
		MemoryKiB: 8 * 1024, Iterations: 1, Parallelism: 4,
		SaltLen: 16, KeyLen: 32,
	}
	body, err := legacyParams.Hash("legacy-pwd-12")
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}
	stored := "$" + body
	ver := Default()
	ok, needsRehash, err := ver.Verify(stored, "legacy-pwd-12")
	if err != nil || !ok {
		t.Fatalf("Verify legacy: ok=%v err=%v", ok, err)
	}
	if !needsRehash {
		t.Fatalf("legacy-shape stored value did not flag needsRehash")
	}
}

// TestVerify_RejectsMalformed covers acceptance criterion #5 (`go vet` /
// `staticcheck` clean) and #1 (typed errors) — every malformed input
// returns ErrInvalidEncoding, no panic, no false positive.
func TestVerify_RejectsMalformed(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"empty":             "",
		"just-dollar":       "$",
		"missing-version":   "argon2id$m=65536,t=3,p=1$YWJjZGFiY2RhYmNkYWJjZA$YWJjZGFiY2RhYmNkYWJjZGFiY2RhYmNkYWJjZGFiY2RhYmNkYWJjZA",
		"wrong-algo":        "argon2i$v=19$m=65536,t=3,p=1$YWJjZGFiY2RhYmNkYWJjZA$YWJjZGFiY2RhYmNkYWJjZGFiY2RhYmNkYWJjZGFiY2RhYmNkYWJjZA",
		"wrong-version":     "argon2id$v=18$m=65536,t=3,p=1$YWJjZGFiY2RhYmNkYWJjZA$YWJjZGFiY2RhYmNkYWJjZGFiY2RhYmNkYWJjZGFiY2RhYmNkYWJjZA",
		"non-numeric-mem":   "argon2id$v=19$m=abc,t=3,p=1$YWJjZGFiY2RhYmNkYWJjZA$YWJjZGFiY2RhYmNkYWJjZGFiY2RhYmNkYWJjZGFiY2RhYmNkYWJjZA",
		"missing-params":    "argon2id$v=19$$YWJjZGFiY2RhYmNkYWJjZA$YWJjZGFiY2RhYmNkYWJjZGFiY2RhYmNkYWJjZGFiY2RhYmNkYWJjZA",
		"only-two-params":   "argon2id$v=19$m=65536,t=3$YWJjZGFiY2RhYmNkYWJjZA$YWJjZGFiY2RhYmNkYWJjZGFiY2RhYmNkYWJjZGFiY2RhYmNkYWJjZA",
		"zero-iterations":   "argon2id$v=19$m=65536,t=0,p=1$YWJjZGFiY2RhYmNkYWJjZA$YWJjZGFiY2RhYmNkYWJjZGFiY2RhYmNkYWJjZGFiY2RhYmNkYWJjZA",
		"zero-parallelism":  "argon2id$v=19$m=65536,t=3,p=0$YWJjZGFiY2RhYmNkYWJjZA$YWJjZGFiY2RhYmNkYWJjZGFiY2RhYmNkYWJjZGFiY2RhYmNkYWJjZA",
		"too-many-segments": "argon2id$v=19$m=65536,t=3,p=1$YWJjZGFiY2RhYmNkYWJjZA$YWJjZGFiY2RhYmNkYWJjZGFiY2RhYmNkYWJjZGFiY2RhYmNkYWJjZA$extra",
		"bad-base64-salt":   "argon2id$v=19$m=65536,t=3,p=1$!!!!!!$YWJjZGFiY2RhYmNkYWJjZGFiY2RhYmNkYWJjZGFiY2RhYmNkYWJjZA",
		"empty-salt":        "argon2id$v=19$m=65536,t=3,p=1$$YWJjZGFiY2RhYmNkYWJjZGFiY2RhYmNkYWJjZGFiY2RhYmNkYWJjZA",
		"empty-hash":        "argon2id$v=19$m=65536,t=3,p=1$YWJjZGFiY2RhYmNkYWJjZA$",
	}
	ver := Default()
	for name, enc := range cases {
		enc := enc
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ok, _, err := ver.Verify(enc, "anything")
			if ok {
				t.Fatalf("ok=true for malformed input")
			}
			if !errors.Is(err, ErrInvalidEncoding) {
				t.Fatalf("err=%v want ErrInvalidEncoding", err)
			}
		})
	}
}

// TestVerify_NoPanicOnAdversarial — property-style coverage for callers
// that may pass attacker-controlled bytes into Verify.
func TestVerify_NoPanicOnAdversarial(t *testing.T) {
	t.Parallel()
	cases := []string{"", "$", "$$$$$", "argon2id$", "argon2id$v=19$"}
	ver := Default()
	for _, enc := range cases {
		enc := enc
		t.Run(enc, func(t *testing.T) {
			t.Parallel()
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("panic on adversarial input %q: %v", enc, r)
				}
			}()
			_, _, _ = ver.Verify(enc, "x")
		})
	}
}

// TestHash_ReadSaltError surfaces the salt-source failure path so callers
// see a wrapped error rather than a constant-salt hash.
func TestHash_ReadSaltError(t *testing.T) {
	t.Parallel()
	h := Default()
	h.randRead = func(b []byte) (int, error) {
		return 0, errors.New("entropy: drained")
	}
	_, err := h.Hash("anything")
	if err == nil {
		t.Fatalf("expected error from failed salt read, got nil")
	}
	if !strings.Contains(err.Error(), "entropy") {
		t.Fatalf("error did not wrap underlying cause: %v", err)
	}
}

// TestEncode_Stable byte-checks the on-disk format so a future refactor
// of the encoder cannot silently drift away from ADR §2.
func TestEncode_Stable(t *testing.T) {
	t.Parallel()
	salt := bytes.Repeat([]byte{0xAA}, 16)
	hash := bytes.Repeat([]byte{0xBB}, 32)
	got := encode(65536, 3, 1, salt, hash)
	const want = "argon2id$v=19$m=65536,t=3,p=1$qqqqqqqqqqqqqqqqqqqqqg$u7u7u7u7u7u7u7u7u7u7u7u7u7u7u7u7u7u7u7u7u7s"
	if got != want {
		t.Fatalf("encode shape drifted:\n  got:  %q\n  want: %q", got, want)
	}
}

// BenchmarkHashProductionParams enforces the ADR §7 latency band on the
// CI runner: 150 ms ≤ median Hash time ≤ 400 ms.
//
// The benchmark is wrapped in a Test so `go test -short` is honoured —
// the wall-clock band only makes sense on the standard CI runner; on
// lightweight local laptops or under -race instrumentation it is
// systematically slow, so we skip there. CI (which does not pass -short)
// runs the band assertion.
func TestBenchmarkHashProductionParams_Band(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping latency band assertion under -short; CI runs without -short")
	}
	if raceEnabled {
		// -race instrumentation pushes argon2 ~3-5× slower; the
		// production-shape band cannot apply. See race_on_test.go.
		t.Skip("skipping latency band assertion under -race; production binaries do not use -race")
	}
	h := Default()
	const samples = 5
	durations := make([]time.Duration, 0, samples)
	for i := 0; i < samples; i++ {
		start := time.Now()
		if _, err := h.Hash("benchmark-plaintext-12"); err != nil {
			t.Fatalf("Hash: %v", err)
		}
		durations = append(durations, time.Since(start))
	}
	median := medianDuration(durations)
	const lo = 150 * time.Millisecond
	const hi = 400 * time.Millisecond
	if median < lo || median > hi {
		t.Fatalf("Hash median latency %v outside ADR §7 band [%v, %v] — re-tune ADR 0070 §1, do NOT relax this assertion", median, lo, hi)
	}
}

func medianDuration(ds []time.Duration) time.Duration {
	cp := make([]time.Duration, len(ds))
	copy(cp, ds)
	// Insertion sort — small N, predictable, no test dep on slices/sort.
	for i := 1; i < len(cp); i++ {
		for j := i; j > 0 && cp[j-1] > cp[j]; j-- {
			cp[j-1], cp[j] = cp[j], cp[j-1]
		}
	}
	return cp[len(cp)/2]
}
