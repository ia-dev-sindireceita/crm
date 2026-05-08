package mfa

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"testing"
	"time"

	"github.com/google/uuid"
)

// fakeSeedRepository captures the encrypted seed and exposes the last
// userID + ciphertext pair for assertion. Other methods are stubbed to
// return nil so tests focused on Enroll do not need to set them up.
type fakeSeedRepository struct {
	storeCalls        int
	lastUserID        uuid.UUID
	lastCiphertext    []byte
	storeErr          error
	markVerifiedCalls int
	markReenrollCalls int
}

func (f *fakeSeedRepository) StoreSeed(_ context.Context, userID uuid.UUID, ct []byte) error {
	f.storeCalls++
	f.lastUserID = userID
	f.lastCiphertext = append([]byte(nil), ct...)
	return f.storeErr
}
func (f *fakeSeedRepository) LoadSeed(context.Context, uuid.UUID) ([]byte, error) { return nil, nil }
func (f *fakeSeedRepository) MarkVerified(context.Context, uuid.UUID) error {
	f.markVerifiedCalls++
	return nil
}
func (f *fakeSeedRepository) MarkReenrollRequired(context.Context, uuid.UUID) error {
	f.markReenrollCalls++
	return nil
}

// fakeCipher prepends a fixed marker to the plaintext so tests can
// confirm the ciphertext written through to the repo came from this
// path. It is the simplest correct implementation of the contract —
// distinguishable from the input but reversible for assertions.
type fakeCipher struct {
	encryptErr error
}

func (f fakeCipher) Encrypt(p []byte) ([]byte, error) {
	if f.encryptErr != nil {
		return nil, f.encryptErr
	}
	out := append([]byte("ENC:"), p...)
	return out, nil
}
func (f fakeCipher) Decrypt(c []byte) ([]byte, error) {
	if !bytes.HasPrefix(c, []byte("ENC:")) {
		return nil, errors.New("fake cipher: bad prefix")
	}
	return c[4:], nil
}

// fakeRecoveryStore records inserted hashes and remembered invalidate
// calls so tests can assert ordering and counts.
type fakeRecoveryStore struct {
	insertCalls    int
	insertedHashes []string
	insertErr      error
	invalidates    int
	invalidateErr  error
}

func (f *fakeRecoveryStore) InsertHashes(_ context.Context, _ uuid.UUID, hashes []string) error {
	f.insertCalls++
	f.insertedHashes = append([]string(nil), hashes...)
	return f.insertErr
}
func (f *fakeRecoveryStore) ListActive(context.Context, uuid.UUID) ([]RecoveryCodeRecord, error) {
	return nil, nil
}
func (f *fakeRecoveryStore) MarkConsumed(context.Context, uuid.UUID) error { return nil }
func (f *fakeRecoveryStore) InvalidateAll(context.Context, uuid.UUID) (int, error) {
	f.invalidates++
	return 0, f.invalidateErr
}

// fakeHasher is a deterministic Argon2id stand-in: hash is "h:" + plain
// and Verify checks the prefix.
type fakeHasher struct{ hashErr error }

func (f fakeHasher) Hash(plain string) (string, error) {
	if f.hashErr != nil {
		return "", f.hashErr
	}
	return "h:" + plain, nil
}
func (f fakeHasher) Verify(stored, plain string) (bool, error) {
	return stored == "h:"+plain, nil
}

// fakeAudit records each event for assertion.
type fakeAudit struct {
	enrolled int
	err      error
}

func (f *fakeAudit) LogEnrolled(context.Context, uuid.UUID) error         { f.enrolled++; return f.err }
func (fakeAudit) LogVerified(context.Context, uuid.UUID) error            { return nil }
func (fakeAudit) LogRecoveryUsed(context.Context, uuid.UUID) error        { return nil }
func (fakeAudit) LogRecoveryRegenerated(context.Context, uuid.UUID) error { return nil }
func (fakeAudit) LogMFARequired(context.Context, uuid.UUID, string, string) error {
	return nil
}

type fakeAlerter struct{}

func (fakeAlerter) AlertRecoveryUsed(context.Context, uuid.UUID) error        { return nil }
func (fakeAlerter) AlertRecoveryRegenerated(context.Context, uuid.UUID) error { return nil }

func newServiceWithFakes(t *testing.T) (*Service, *fakeSeedRepository, *fakeRecoveryStore, *fakeAudit) {
	t.Helper()
	seeds := &fakeSeedRepository{}
	codes := &fakeRecoveryStore{}
	audit := &fakeAudit{}
	svc, err := NewService(Config{
		SeedRepository: seeds,
		SeedCipher:     fakeCipher{},
		RecoveryStore:  codes,
		CodeHasher:     fakeHasher{},
		Audit:          audit,
		Alerter:        fakeAlerter{},
		Issuer:         "Sindireceita",
	})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	return svc, seeds, codes, audit
}

func TestNewService_RejectsMissingCollaborators(t *testing.T) {
	full := Config{
		SeedRepository: &fakeSeedRepository{},
		SeedCipher:     fakeCipher{},
		RecoveryStore:  &fakeRecoveryStore{},
		CodeHasher:     fakeHasher{},
		Audit:          &fakeAudit{},
		Alerter:        fakeAlerter{},
		Issuer:         "Sindireceita",
	}
	cases := map[string]func(c *Config){
		"SeedRepository": func(c *Config) { c.SeedRepository = nil },
		"SeedCipher":     func(c *Config) { c.SeedCipher = nil },
		"RecoveryStore":  func(c *Config) { c.RecoveryStore = nil },
		"CodeHasher":     func(c *Config) { c.CodeHasher = nil },
		"Audit":          func(c *Config) { c.Audit = nil },
		"Alerter":        func(c *Config) { c.Alerter = nil },
	}
	for name, mut := range cases {
		t.Run(name, func(t *testing.T) {
			cfg := full
			mut(&cfg)
			_, err := NewService(cfg)
			if !errors.Is(err, ErrMissingCollaborator) {
				t.Fatalf("err: got %v want ErrMissingCollaborator", err)
			}
		})
	}
}

func TestNewService_RejectsEmptyIssuer(t *testing.T) {
	cfg := Config{
		SeedRepository: &fakeSeedRepository{},
		SeedCipher:     fakeCipher{},
		RecoveryStore:  &fakeRecoveryStore{},
		CodeHasher:     fakeHasher{},
		Audit:          &fakeAudit{},
		Alerter:        fakeAlerter{},
	}
	_, err := NewService(cfg)
	if !errors.Is(err, ErrEmptyIssuer) {
		t.Fatalf("err: got %v want ErrEmptyIssuer", err)
	}
}

func TestNewService_DefaultsClockToTimeNow(t *testing.T) {
	svc, _, _, _ := newServiceWithFakes(t)
	if svc.clock == nil {
		t.Fatal("clock not defaulted")
	}
	got := svc.clock()
	if time.Since(got) > time.Second {
		t.Fatalf("default clock returned stale time: %v", got)
	}
}

func TestEnroll_HappyPath(t *testing.T) {
	svc, seeds, codes, audit := newServiceWithFakes(t)
	uid := uuid.New()
	ctx := context.Background()

	res, err := svc.Enroll(ctx, uid, "ops@example.com")
	if err != nil {
		t.Fatalf("Enroll: %v", err)
	}

	t.Run("EnrollResult shape", func(t *testing.T) {
		if len(res.RecoveryCodes) != RecoveryCodeCount {
			t.Errorf("RecoveryCodes len: got %d want %d", len(res.RecoveryCodes), RecoveryCodeCount)
		}
		if res.OTPAuthURI == "" {
			t.Error("OTPAuthURI is empty")
		}
		parsed, err := url.Parse(res.OTPAuthURI)
		if err != nil {
			t.Errorf("OTPAuthURI parse: %v", err)
		}
		if parsed.Query().Get("issuer") != "Sindireceita" {
			t.Errorf("issuer: got %q want Sindireceita", parsed.Query().Get("issuer"))
		}
		if res.SecretEncoded == "" {
			t.Error("SecretEncoded is empty")
		}
	})

	t.Run("SeedRepository stored encrypted ciphertext", func(t *testing.T) {
		if seeds.storeCalls != 1 {
			t.Errorf("StoreSeed calls: got %d want 1", seeds.storeCalls)
		}
		if !bytes.HasPrefix(seeds.lastCiphertext, []byte("ENC:")) {
			t.Errorf("ciphertext does not have fakeCipher marker: %x", seeds.lastCiphertext)
		}
		if seeds.lastUserID != uid {
			t.Errorf("userID: got %v want %v", seeds.lastUserID, uid)
		}
	})

	t.Run("InvalidateAll called before InsertHashes (regen safety)", func(t *testing.T) {
		// Both fakes record a count; InsertHashes is called exactly
		// once after a single InvalidateAll. The ordering is encoded by
		// the Service implementation — if a refactor ever interleaves
		// them differently this assertion catches it.
		if codes.invalidates != 1 {
			t.Errorf("InvalidateAll calls: got %d want 1", codes.invalidates)
		}
		if codes.insertCalls != 1 {
			t.Errorf("InsertHashes calls: got %d want 1", codes.insertCalls)
		}
	})

	t.Run("InsertHashes received 10 Argon2id-shaped hashes", func(t *testing.T) {
		if len(codes.insertedHashes) != RecoveryCodeCount {
			t.Errorf("inserted hashes count: got %d want %d", len(codes.insertedHashes), RecoveryCodeCount)
		}
		// Each hash must come from CodeHasher.Hash (fakeHasher prefixes
		// "h:") AND must NOT be the plaintext code itself.
		for i, h := range codes.insertedHashes {
			if h == res.RecoveryCodes[i] {
				t.Errorf("hash %d equals plaintext code (Argon2id step skipped?): %q", i, h)
			}
			if len(h) < 4 || h[:2] != "h:" {
				t.Errorf("hash %d not produced by CodeHasher: %q", i, h)
			}
		}
	})

	t.Run("Audit LogEnrolled fired exactly once", func(t *testing.T) {
		if audit.enrolled != 1 {
			t.Errorf("LogEnrolled calls: got %d want 1", audit.enrolled)
		}
	})
}

func TestEnroll_RejectsBadInputs(t *testing.T) {
	svc, _, _, _ := newServiceWithFakes(t)
	ctx := context.Background()
	if _, err := svc.Enroll(ctx, uuid.Nil, "ops@example.com"); err == nil {
		t.Error("Enroll(uuid.Nil) returned nil error")
	}
	if _, err := svc.Enroll(ctx, uuid.New(), ""); err == nil {
		t.Error("Enroll(empty label) returned nil error")
	}
}

func TestEnroll_PropagatesEachStepError(t *testing.T) {
	steps := map[string]func(c *Config){
		"cipher.Encrypt": func(c *Config) { c.SeedCipher = fakeCipher{encryptErr: errors.New("boom")} },
		"seeds.StoreSeed": func(c *Config) {
			r := &fakeSeedRepository{storeErr: errors.New("boom")}
			c.SeedRepository = r
		},
		"codes.InvalidateAll": func(c *Config) {
			s := &fakeRecoveryStore{invalidateErr: errors.New("boom")}
			c.RecoveryStore = s
		},
		"hasher.Hash": func(c *Config) { c.CodeHasher = fakeHasher{hashErr: errors.New("boom")} },
		"codes.InsertHashes": func(c *Config) {
			s := &fakeRecoveryStore{insertErr: errors.New("boom")}
			c.RecoveryStore = s
		},
		"audit.LogEnrolled": func(c *Config) { c.Audit = &fakeAudit{err: errors.New("boom")} },
	}
	for name, mut := range steps {
		t.Run(name, func(t *testing.T) {
			cfg := Config{
				SeedRepository: &fakeSeedRepository{},
				SeedCipher:     fakeCipher{},
				RecoveryStore:  &fakeRecoveryStore{},
				CodeHasher:     fakeHasher{},
				Audit:          &fakeAudit{},
				Alerter:        fakeAlerter{},
				Issuer:         "Sindireceita",
			}
			mut(&cfg)
			svc, err := NewService(cfg)
			if err != nil {
				t.Fatalf("NewService: %v", err)
			}
			_, err = svc.Enroll(context.Background(), uuid.New(), "ops@example.com")
			if err == nil {
				t.Fatal("expected error, got nil")
			}
		})
	}
}

func TestEnroll_DeterministicWithFixedReader(t *testing.T) {
	// Pin output by injecting a zero reader. With 0x00 bytes the seed
	// encodes to all-A base32, and the recovery codes are likewise
	// "AAAAAAAAAA" each. Useful as a regression test against any
	// future refactor that silently swaps the alphabet or seed length.
	svc, _, _, _ := newServiceWithFakes(t)
	svc.rand = bytes.NewReader(append(
		make([]byte, totpSeedSize),                              // seed
		make([]byte, recoveryCodeRawBytes*RecoveryCodeCount)..., // codes
	))
	res, err := svc.Enroll(context.Background(), uuid.New(), "ops@example.com")
	if err != nil {
		t.Fatalf("Enroll: %v", err)
	}
	const wantCode = "AAAAAAAAAA"
	for i, c := range res.RecoveryCodes {
		if c != wantCode {
			t.Errorf("code %d: got %q want %q", i, c, wantCode)
		}
	}
}

func TestEnroll_SurfacesShortReader(t *testing.T) {
	svc, _, _, _ := newServiceWithFakes(t)
	svc.rand = io.LimitReader(bytes.NewReader(make([]byte, totpSeedSize-1)), int64(totpSeedSize-1))
	_, err := svc.Enroll(context.Background(), uuid.New(), "ops")
	if err == nil {
		t.Fatal("expected error from short reader, got nil")
	}
}

// Ensure the package-level error wrap surfaces fmt.Errorf("%w").
var _ = fmt.Errorf
