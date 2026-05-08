package mfa

import (
	"context"

	"github.com/google/uuid"
)

// CodeHasher is the port the MFA flow uses to hash and verify recovery
// codes. The production wiring binds this to the Argon2id helper from
// internal/iam/password (ADR 0070): the same parameters and stored
// format that hash user passwords also hash recovery codes (ADR 0074
// §2).
//
// Hash returns the encoded representation that goes into
// master_recovery_code.code_hash; Verify checks a plaintext against
// such an encoding. A simple mismatch is ok=false, err=nil — Verify
// only returns an error for malformed input.
type CodeHasher interface {
	Hash(plain string) (string, error)
	Verify(stored, plain string) (ok bool, err error)
}

// SeedRepository is the port for the persisted master TOTP seed. The
// production adapter writes ciphertext (see SeedCipher) into
// master_mfa.totp_seed_encrypted; the domain layer treats the seed as
// opaque bytes.
type SeedRepository interface {
	StoreSeed(ctx context.Context, userID uuid.UUID, seedCiphertext []byte) error
	LoadSeed(ctx context.Context, userID uuid.UUID) ([]byte, error)
	MarkVerified(ctx context.Context, userID uuid.UUID) error
	MarkReenrollRequired(ctx context.Context, userID uuid.UUID) error
}

// SeedCipher encrypts the TOTP seed before it ever reaches Postgres.
// The production adapter binds this to the application's symmetric key
// (env var, ADR 0074 §1). Plaintext seeds MUST NOT travel through any
// other code path — Hash domain functions take []byte so the caller
// can feed the decrypted seed directly without an intermediate string
// copy.
type SeedCipher interface {
	Encrypt(plaintext []byte) ([]byte, error)
	Decrypt(ciphertext []byte) ([]byte, error)
}

// RecoveryStore is the port for the master_recovery_code table. The
// methods are split so the domain layer never holds a transaction —
// callers compose the operations or wrap them in a UnitOfWork as
// appropriate.
type RecoveryStore interface {
	// InsertHashes adds N freshly-generated, Argon2id-hashed codes for
	// the given master. Implementations MUST set generated_at to the
	// server clock and consumed_at to NULL.
	InsertHashes(ctx context.Context, userID uuid.UUID, hashes []string) error

	// ListActive returns the not-yet-consumed codes. Order is
	// implementation-defined; the verifier walks all of them.
	ListActive(ctx context.Context, userID uuid.UUID) ([]RecoveryCodeRecord, error)

	// MarkConsumed records that the named row was used. Must be safe
	// against double-marking (idempotent — second call must NOT clobber
	// the original timestamp).
	MarkConsumed(ctx context.Context, codeID uuid.UUID) error

	// InvalidateAll is the regenerate path: bulk-mark every active row
	// for the user as consumed_at = now(). Returns the count of rows
	// touched so callers can audit the regen size.
	InvalidateAll(ctx context.Context, userID uuid.UUID) (int, error)
}

// RecoveryCodeRecord is the on-the-wire row shape returned by
// RecoveryStore.ListActive. The plaintext code is NEVER stored; the
// hash field carries the Argon2id encoding written at insert time.
type RecoveryCodeRecord struct {
	ID   uuid.UUID
	Hash string
}

// AuditLogger emits the master MFA events listed in ADR 0074 §1, §2,
// §3, §5. Each method is its own narrowly-scoped port so adapters can
// log structurally without inspecting an opaque event type.
type AuditLogger interface {
	LogEnrolled(ctx context.Context, userID uuid.UUID) error
	LogVerified(ctx context.Context, userID uuid.UUID) error
	LogRecoveryUsed(ctx context.Context, userID uuid.UUID) error
	LogRecoveryRegenerated(ctx context.Context, userID uuid.UUID) error
	LogMFARequired(ctx context.Context, userID uuid.UUID, route, reason string) error
}

// Alerter posts the immediate Slack #alerts notification when a
// master recovery code is consumed (ADR 0074 §5). The domain layer
// names the event; the adapter renders the message and routes it.
type Alerter interface {
	AlertRecoveryUsed(ctx context.Context, userID uuid.UUID) error
}
