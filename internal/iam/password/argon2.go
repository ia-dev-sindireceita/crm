package password

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"golang.org/x/crypto/argon2"
)

// ADR 0070 §1 — Argon2id production parameters. Calibrated to ~250 ms on
// the staging runner (see ADR §7 benchmark gate). Parameter changes are
// an ADR amendment, not a code-level decision.
const (
	defaultMemoryKiB   uint32 = 64 * 1024 // 64 MiB
	defaultIterations  uint32 = 3
	defaultParallelism uint8  = 1
	defaultSaltLen     int    = 16
	defaultKeyLen      uint32 = 32
)

// argonAlg is the algorithm identifier required by ADR 0070 §2 (lower
// case, exact). Verify also tolerates the legacy SIN-62213 leading-'$'
// shape so stored values written under the old helper continue to verify
// — needsRehash flags those rows for §3 quiet upgrade on next login.
const argonAlg = "argon2id"

// Argon2idHasher implements Hasher and Verifier.
//
// The zero value is NOT useful — callers MUST construct via Default() or
// pass parameters explicitly. This is deliberate: silently hashing under
// p=0/m=0/t=0 would compute a constant digest and look "successful".
type Argon2idHasher struct {
	MemoryKiB   uint32
	Iterations  uint32
	Parallelism uint8
	SaltLen     int
	KeyLen      uint32

	// randRead is injected only for deterministic vector tests. Production
	// callers leave it nil; Hash falls back to crypto/rand.Read.
	randRead func([]byte) (int, error)
}

// Default returns a hasher pinned to the ADR 0070 §1 production params.
func Default() *Argon2idHasher {
	return &Argon2idHasher{
		MemoryKiB:   defaultMemoryKiB,
		Iterations:  defaultIterations,
		Parallelism: defaultParallelism,
		SaltLen:     defaultSaltLen,
		KeyLen:      defaultKeyLen,
	}
}

// Hash implements Hasher. The encoded form follows ADR 0070 §2:
//
//	argon2id$v=19$m=65536,t=3,p=1$<salt-b64>$<hash-b64>
//
// Salt is read fresh from crypto/rand on every call. The plaintext, the
// derived bytes, and the encoded string MUST NOT be logged at any
// verbosity; see docs/security/passwords.md.
func (h *Argon2idHasher) Hash(plain string) (string, error) {
	if plain == "" {
		return "", ErrEmptyPlaintext
	}
	if h.SaltLen <= 0 || h.KeyLen == 0 || h.MemoryKiB == 0 || h.Iterations == 0 || h.Parallelism == 0 {
		return "", ErrInvalidParams
	}
	salt := make([]byte, h.SaltLen)
	read := h.randRead
	if read == nil {
		read = rand.Read
	}
	if _, err := read(salt); err != nil {
		return "", fmt.Errorf("password: read salt: %w", err)
	}
	derived := argon2.IDKey([]byte(plain), salt, h.Iterations, h.MemoryKiB, h.Parallelism, h.KeyLen)
	return encode(h.MemoryKiB, h.Iterations, h.Parallelism, salt, derived), nil
}

// Verify implements Verifier. It tolerates two encodings:
//
//   - Canonical (ADR 0070 §2): "argon2id$v=19$m=…,t=…,p=…$<salt>$<hash>"
//   - Legacy (SIN-62213, leading '$'): "$argon2id$v=19$…$<salt>$<hash>"
//
// Both are parsed; the parsed (m, t, p) drive the re-derivation so a row
// written under old params still verifies. needsRehash is set to true
// whenever (m, t, p) differ from this Hasher's current values OR the
// stored encoding used the legacy leading-'$' shape — both conditions
// trigger the §3 quiet upgrade path.
func (h *Argon2idHasher) Verify(stored, plain string) (bool, bool, error) {
	parsed, err := decode(stored)
	if err != nil {
		return false, false, err
	}
	derived := argon2.IDKey([]byte(plain), parsed.salt, parsed.iterations, parsed.memoryKiB, parsed.parallelism, uint32(len(parsed.hash)))
	ok := subtle.ConstantTimeCompare(derived, parsed.hash) == 1
	needsRehash := parsed.legacyLead ||
		parsed.memoryKiB != h.MemoryKiB ||
		parsed.iterations != h.Iterations ||
		parsed.parallelism != h.Parallelism ||
		uint32(len(parsed.hash)) != h.KeyLen
	return ok, needsRehash, nil
}

type encoded struct {
	memoryKiB   uint32
	iterations  uint32
	parallelism uint8
	salt        []byte
	hash        []byte
	legacyLead  bool // true if input started with '$' (SIN-62213 shape)
}

func encode(m uint32, t uint32, p uint8, salt, hash []byte) string {
	var b strings.Builder
	b.Grow(96)
	b.WriteString(argonAlg)
	b.WriteString("$v=")
	b.WriteString(strconv.Itoa(argon2.Version))
	b.WriteString("$m=")
	b.WriteString(strconv.FormatUint(uint64(m), 10))
	b.WriteString(",t=")
	b.WriteString(strconv.FormatUint(uint64(t), 10))
	b.WriteString(",p=")
	b.WriteString(strconv.FormatUint(uint64(p), 10))
	b.WriteByte('$')
	b.WriteString(base64.RawStdEncoding.EncodeToString(salt))
	b.WriteByte('$')
	b.WriteString(base64.RawStdEncoding.EncodeToString(hash))
	return b.String()
}

func decode(s string) (encoded, error) {
	if s == "" {
		return encoded{}, ErrInvalidEncoding
	}
	legacy := false
	if strings.HasPrefix(s, "$") {
		legacy = true
		s = s[1:]
	}
	parts := strings.Split(s, "$")
	if len(parts) != 5 {
		return encoded{}, ErrInvalidEncoding
	}
	if parts[0] != argonAlg {
		return encoded{}, ErrInvalidEncoding
	}
	if parts[1] != "v="+strconv.Itoa(argon2.Version) {
		return encoded{}, ErrInvalidEncoding
	}
	m, t, p, err := parseParams(parts[2])
	if err != nil {
		return encoded{}, err
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[3])
	if err != nil || len(salt) == 0 {
		return encoded{}, ErrInvalidEncoding
	}
	hash, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil || len(hash) == 0 {
		return encoded{}, ErrInvalidEncoding
	}
	return encoded{
		memoryKiB:   m,
		iterations:  t,
		parallelism: p,
		salt:        salt,
		hash:        hash,
		legacyLead:  legacy,
	}, nil
}

func parseParams(s string) (uint32, uint32, uint8, error) {
	fields := strings.Split(s, ",")
	if len(fields) != 3 {
		return 0, 0, 0, ErrInvalidEncoding
	}
	m, err := parsePrefixedUint32(fields[0], "m=")
	if err != nil {
		return 0, 0, 0, err
	}
	t, err := parsePrefixedUint32(fields[1], "t=")
	if err != nil {
		return 0, 0, 0, err
	}
	pRaw, err := parsePrefixedUint32(fields[2], "p=")
	if err != nil {
		return 0, 0, 0, err
	}
	if pRaw == 0 || pRaw > 255 {
		return 0, 0, 0, ErrInvalidEncoding
	}
	if m == 0 || t == 0 {
		return 0, 0, 0, ErrInvalidEncoding
	}
	return m, t, uint8(pRaw), nil
}

func parsePrefixedUint32(field, prefix string) (uint32, error) {
	if !strings.HasPrefix(field, prefix) {
		return 0, ErrInvalidEncoding
	}
	v, err := strconv.ParseUint(field[len(prefix):], 10, 32)
	if err != nil {
		return 0, ErrInvalidEncoding
	}
	return uint32(v), nil
}

// Sentinel errors. Verify uses errors.Is from callers; never panics on
// adversarial input.
var (
	ErrEmptyPlaintext  = errors.New("password: empty plaintext")
	ErrInvalidParams   = errors.New("password: hasher params invalid")
	ErrInvalidEncoding = errors.New("password: invalid encoded hash")
)
