package mfa

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/google/uuid"
)

// Service is the orchestration layer for the master MFA flow. It is
// constructed once at startup with the full set of collaborators and
// shared across HTTP handlers — it carries no per-request state and
// is safe for concurrent use.
//
// Each public method maps to one of the actions defined in ADR 0074:
// Enroll (§1 + §2), and — landing in subsequent PRs — Verify (§4),
// ConsumeRecovery (§5), RegenerateRecovery (§2 regenerate path).
type Service struct {
	seeds   SeedRepository
	cipher  SeedCipher
	codes   RecoveryStore
	hasher  CodeHasher
	audit   AuditLogger
	alerter Alerter
	issuer  string
	rand    io.Reader
	clock   func() time.Time
}

// Config is the constructor input. Required fields are checked by
// NewService; nil collaborators produce an error rather than a runtime
// nil-deref.
type Config struct {
	SeedRepository SeedRepository
	SeedCipher     SeedCipher
	RecoveryStore  RecoveryStore
	CodeHasher     CodeHasher
	Audit          AuditLogger
	Alerter        Alerter
	// Issuer names the product in the otpauth:// URI (e.g.
	// "Sindireceita"). Authenticators show this string above the code.
	Issuer string
	// Rand is the random source for seed and recovery code generation.
	// Production wiring leaves this nil and crypto/rand is used; tests
	// pass a deterministic reader to pin output.
	Rand io.Reader
	// Clock is the time source (currently unused by Enroll but threaded
	// through for Verify/Consume in subsequent PRs). Defaults to
	// time.Now.
	Clock func() time.Time
}

// ErrMissingCollaborator is returned by NewService when a required
// port is nil. The wrapped string names which one so a misconfigured
// deploy fails with a precise bootstrap error.
var ErrMissingCollaborator = errors.New("mfa: missing collaborator")

// ErrEmptyIssuer is returned by NewService when Config.Issuer is
// empty. Authenticators that strip an empty issuer end up showing
// only the bare label, which is a degraded UX worth refusing.
var ErrEmptyIssuer = errors.New("mfa: issuer must not be empty")

// NewService validates and returns a Service ready for use.
func NewService(cfg Config) (*Service, error) {
	switch {
	case cfg.SeedRepository == nil:
		return nil, fmt.Errorf("%w: SeedRepository", ErrMissingCollaborator)
	case cfg.SeedCipher == nil:
		return nil, fmt.Errorf("%w: SeedCipher", ErrMissingCollaborator)
	case cfg.RecoveryStore == nil:
		return nil, fmt.Errorf("%w: RecoveryStore", ErrMissingCollaborator)
	case cfg.CodeHasher == nil:
		return nil, fmt.Errorf("%w: CodeHasher", ErrMissingCollaborator)
	case cfg.Audit == nil:
		return nil, fmt.Errorf("%w: Audit", ErrMissingCollaborator)
	case cfg.Alerter == nil:
		return nil, fmt.Errorf("%w: Alerter", ErrMissingCollaborator)
	case cfg.Issuer == "":
		return nil, ErrEmptyIssuer
	}
	clock := cfg.Clock
	if clock == nil {
		clock = time.Now
	}
	return &Service{
		seeds:   cfg.SeedRepository,
		cipher:  cfg.SeedCipher,
		codes:   cfg.RecoveryStore,
		hasher:  cfg.CodeHasher,
		audit:   cfg.Audit,
		alerter: cfg.Alerter,
		issuer:  cfg.Issuer,
		rand:    cfg.Rand,
		clock:   clock,
	}, nil
}

// EnrollResult is what Enroll returns to the caller. The HTTP layer
// renders this exactly once: the OTPAuthURI becomes the QR code, the
// SecretEncoded is shown for manual entry, and the plaintext
// RecoveryCodes are displayed inline with a copy-all button.
//
// The plaintext codes MUST NOT be persisted by any caller — the Service
// has already stored their Argon2id hashes via RecoveryStore. Once the
// HTTP handler flushes the response, the values exist nowhere except
// the user's transcribed copy.
type EnrollResult struct {
	OTPAuthURI    string
	SecretEncoded string
	RecoveryCodes []string
}

// Enroll runs the full first-time-or-regenerate enrolment flow for the
// master named by userID. label is what the authenticator shows under
// the issuer (typically the master's email).
//
// Sequence:
//  1. Mint a fresh 32-byte TOTP seed.
//  2. Encrypt the seed via SeedCipher.
//  3. StoreSeed — upsert. The adapter's ON CONFLICT clears
//     reenroll_required and last_verified_at (regenerate semantics
//     from ADR 0074 §5).
//  4. InvalidateAll — bulk-mark every still-active recovery code as
//     consumed. No-op on a first-time enrol; load-bearing on a
//     regenerate so the old set cannot be used after the re-enrol.
//  5. Generate 10 plaintext recovery codes.
//  6. Argon2id-hash each via CodeHasher.
//  7. InsertHashes — persist.
//  8. Render the otpauth:// URI for the QR code.
//  9. Audit "master_mfa_enrolled".
//
// On any error the function returns immediately with a descriptive
// wrap; the HTTP layer maps it to a 500 / "please retry" page. A
// partial failure between steps 3 and 7 leaves the master with a seed
// stored but with no recovery codes — the next attempt overwrites the
// seed and retries the codes path.
func (s *Service) Enroll(ctx context.Context, userID uuid.UUID, label string) (EnrollResult, error) {
	if userID == uuid.Nil {
		return EnrollResult{}, fmt.Errorf("mfa: Enroll: userID is nil")
	}
	if label == "" {
		return EnrollResult{}, fmt.Errorf("mfa: Enroll: label is empty")
	}

	seed, err := NewSecret(s.rand)
	if err != nil {
		return EnrollResult{}, fmt.Errorf("mfa: Enroll: new secret: %w", err)
	}
	ciphertext, err := s.cipher.Encrypt(seed)
	if err != nil {
		return EnrollResult{}, fmt.Errorf("mfa: Enroll: encrypt seed: %w", err)
	}
	if err := s.seeds.StoreSeed(ctx, userID, ciphertext); err != nil {
		return EnrollResult{}, fmt.Errorf("mfa: Enroll: store seed: %w", err)
	}
	if _, err := s.codes.InvalidateAll(ctx, userID); err != nil {
		return EnrollResult{}, fmt.Errorf("mfa: Enroll: invalidate prior codes: %w", err)
	}

	plain, err := GenerateRecoveryCodes(s.rand)
	if err != nil {
		return EnrollResult{}, fmt.Errorf("mfa: Enroll: generate recovery codes: %w", err)
	}
	hashes := make([]string, len(plain))
	for i, code := range plain {
		h, err := s.hasher.Hash(code)
		if err != nil {
			return EnrollResult{}, fmt.Errorf("mfa: Enroll: hash code %d: %w", i, err)
		}
		hashes[i] = h
	}
	if err := s.codes.InsertHashes(ctx, userID, hashes); err != nil {
		return EnrollResult{}, fmt.Errorf("mfa: Enroll: insert hashes: %w", err)
	}

	uri, err := OTPAuthURI(s.issuer, label, seed)
	if err != nil {
		return EnrollResult{}, fmt.Errorf("mfa: Enroll: otpauth uri: %w", err)
	}
	encoded, err := EncodeSecret(seed)
	if err != nil {
		return EnrollResult{}, fmt.Errorf("mfa: Enroll: encode secret: %w", err)
	}

	if err := s.audit.LogEnrolled(ctx, userID); err != nil {
		// Audit failure is fatal — every successful enrol MUST be
		// logged for the master_ops compliance story (ADR 0074 §1).
		// The user is told to retry; the prior writes upsert cleanly.
		return EnrollResult{}, fmt.Errorf("mfa: Enroll: audit: %w", err)
	}

	return EnrollResult{
		OTPAuthURI:    uri,
		SecretEncoded: encoded,
		RecoveryCodes: plain,
	}, nil
}

// totpVerifyWindow is the ±step tolerance (ADR 0074 §1: ±1 step).
// Pulled into a constant so a future ADR amendment relaxing the
// window has exactly one place to land.
const totpVerifyWindow = 1

// Verify checks a six-digit TOTP code against the seed stored for
// userID. Returns nil on success, ErrInvalidCode on a mismatch, or a
// wrapped error for storage / decryption failures.
//
// The seed is loaded fresh from the SeedRepository on every call —
// callers MUST NOT cache it client-side. On success the
// last_verified_at column is bumped via MarkVerified and an audit
// record is emitted.
//
// A missing master_mfa row is reported as ErrInvalidCode (not a
// distinct "not enrolled" error) so a hostile prober cannot
// distinguish "no enrolment yet" from "wrong code". The
// RequireMasterMFA middleware (PR6) gates the verify handler on the
// "is enrolled" check upstream — by the time we reach Verify the
// user is expected to be enrolled.
func (s *Service) Verify(ctx context.Context, userID uuid.UUID, code string) error {
	if userID == uuid.Nil {
		return fmt.Errorf("mfa: Verify: userID is nil")
	}
	ciphertext, err := s.seeds.LoadSeed(ctx, userID)
	if err != nil {
		// ErrNotEnrolled specifically maps to ErrInvalidCode (anti-
		// enumeration: a hostile prober cannot distinguish "no enrol
		// yet" from "wrong code"). Other errors propagate wrapped.
		if errors.Is(err, ErrNotEnrolled) {
			return ErrInvalidCode
		}
		return fmt.Errorf("mfa: Verify: load seed: %w", err)
	}
	seed, err := s.cipher.Decrypt(ciphertext)
	if err != nil {
		return fmt.Errorf("mfa: Verify: decrypt seed: %w", err)
	}
	// Calls the package-level TOTP function (totp.go), not Service.Verify.
	if err := totpVerify(seed, code, s.clock(), totpVerifyWindow); err != nil {
		// Don't leak the exact reason (ErrSeedTooShort vs
		// ErrInvalidCode) at this layer — both collapse to "wrong code"
		// from the caller's view.
		return ErrInvalidCode
	}
	if err := s.seeds.MarkVerified(ctx, userID); err != nil {
		return fmt.Errorf("mfa: Verify: mark verified: %w", err)
	}
	if err := s.audit.LogVerified(ctx, userID); err != nil {
		return fmt.Errorf("mfa: Verify: audit: %w", err)
	}
	return nil
}

// ConsumeRecovery validates a single recovery code against the
// per-user active set, marks it consumed, flips the master into
// reenroll-required state, and fires audit + Slack alert. Returns
// nil on success, ErrInvalidCode on no match.
//
// Sequence (ADR 0074 §5):
//  1. Normalise the submitted plaintext (strip dashes/spaces, upper-
//     case, refuse non-base32).
//  2. List the master's active codes.
//  3. Walk the list calling CodeHasher.Verify against each row's
//     stored Argon2id hash. The walk is exhaustive — short-circuit
//     would leak which row matched via timing; instead we record the
//     first match and continue past the remaining rows. (10 rows
//     max, ~250ms each, 2.5s worst case is acceptable on the cold
//     recovery path.)
//  4. If no match: return ErrInvalidCode.
//  5. MarkConsumed on the matched row.
//  6. MarkReenrollRequired on the master so the next session forces
//     a fresh TOTP enrol.
//  7. LogRecoveryUsed (audit — fatal).
//  8. AlertRecoveryUsed (Slack — non-fatal, logged on failure since
//     the code is already consumed and rolling back would be
//     complex).
func (s *Service) ConsumeRecovery(ctx context.Context, userID uuid.UUID, submitted string) error {
	if userID == uuid.Nil {
		return fmt.Errorf("mfa: ConsumeRecovery: userID is nil")
	}
	canonical, err := NormalizeRecoveryCode(submitted)
	if err != nil {
		// Anti-enumeration: surface the same generic error a wrong
		// (well-formed) code would produce.
		return ErrInvalidCode
	}
	rows, err := s.codes.ListActive(ctx, userID)
	if err != nil {
		return fmt.Errorf("mfa: ConsumeRecovery: list active: %w", err)
	}
	var matched *RecoveryCodeRecord
	for i := range rows {
		ok, vErr := s.hasher.Verify(rows[i].Hash, canonical)
		if vErr != nil {
			// Malformed stored hash is a system-side problem; surface
			// it as a wrap rather than collapsing to ErrInvalidCode so
			// ops can spot it.
			return fmt.Errorf("mfa: ConsumeRecovery: verify row: %w", vErr)
		}
		if ok && matched == nil {
			matched = &rows[i]
			// We deliberately keep iterating to avoid a timing oracle.
		}
	}
	if matched == nil {
		return ErrInvalidCode
	}
	if err := s.codes.MarkConsumed(ctx, matched.ID); err != nil {
		return fmt.Errorf("mfa: ConsumeRecovery: mark consumed: %w", err)
	}
	if err := s.seeds.MarkReenrollRequired(ctx, userID); err != nil {
		return fmt.Errorf("mfa: ConsumeRecovery: mark reenroll: %w", err)
	}
	if err := s.audit.LogRecoveryUsed(ctx, userID); err != nil {
		return fmt.Errorf("mfa: ConsumeRecovery: audit: %w", err)
	}
	if err := s.alerter.AlertRecoveryUsed(ctx, userID); err != nil {
		// Slack outage MUST NOT cause the master to be told their code
		// was wrong (the consume already happened). The alert is best-
		// effort; the audit_log entry is the durable record.
		s.alertFailure(ctx, userID, err)
	}
	return nil
}

// alertFailure is the soft-fail logging hook for AlertRecoveryUsed
// outages. Pulled into a method so a future caller (Regenerate in
// PR7) can reuse it.
func (s *Service) alertFailure(ctx context.Context, userID uuid.UUID, cause error) {
	// LogMFARequired carries route + reason fields we can repurpose
	// to surface "alerter degraded" without adding a new audit method.
	// Any error here is double-trouble (audit AND alert failed); we
	// drop it on the floor — the master_ops_audit DB trail still
	// captures the underlying mutations.
	_ = s.audit.LogMFARequired(ctx, userID, "/m/2fa/recover", "alerter_failed:"+cause.Error())
}
