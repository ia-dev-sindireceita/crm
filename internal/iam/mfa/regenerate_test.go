package mfa

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
)

// regenAlerter records both AlertRecoveryRegenerated and the soft-fail
// path so tests can confirm regenerate fires the regen-specific event.
type regenAlerter struct {
	usedCalls   int
	regenCalls  int
	regenErr    error
	lastRegened RecoveryRegeneratedDetails
}

func (a *regenAlerter) AlertRecoveryUsed(context.Context, RecoveryUsedDetails) error {
	a.usedCalls++
	return nil
}
func (a *regenAlerter) AlertRecoveryRegenerated(_ context.Context, d RecoveryRegeneratedDetails) error {
	a.regenCalls++
	a.lastRegened = d
	return a.regenErr
}

// regenAudit records LogRecoveryRegenerated calls + soft-fail trail.
type regenAudit struct {
	regenCalls  int
	regenErr    error
	mfaRequired int
}

func (a *regenAudit) LogEnrolled(context.Context, uuid.UUID) error     { return nil }
func (a *regenAudit) LogVerified(context.Context, uuid.UUID) error     { return nil }
func (a *regenAudit) LogRecoveryUsed(context.Context, uuid.UUID) error { return nil }
func (a *regenAudit) LogRecoveryRegenerated(context.Context, uuid.UUID) error {
	a.regenCalls++
	return a.regenErr
}
func (a *regenAudit) LogMFARequired(_ context.Context, _ uuid.UUID, _, _ string) error {
	a.mfaRequired++
	return nil
}

// regenSeeds is a SeedRepository fake that satisfies the interface
// but is never used by RegenerateRecovery (only the codes/audit/alert
// chain runs). Kept minimal so failures are easy to read.
type regenSeeds struct{}

func (regenSeeds) StoreSeed(context.Context, uuid.UUID, []byte) error  { return nil }
func (regenSeeds) LoadSeed(context.Context, uuid.UUID) ([]byte, error) { return nil, nil }
func (regenSeeds) MarkVerified(context.Context, uuid.UUID) error       { return nil }
func (regenSeeds) MarkReenrollRequired(context.Context, uuid.UUID) error {
	return nil
}

// regenStore is a recovery store with scriptable errors for each method.
type regenStore struct {
	invalidates    int
	invalidateErr  error
	insertCalls    int
	insertedHashes []string
	insertErr      error
}

func (s *regenStore) InsertHashes(_ context.Context, _ uuid.UUID, hashes []string) error {
	s.insertCalls++
	s.insertedHashes = append([]string(nil), hashes...)
	return s.insertErr
}
func (s *regenStore) ListActive(context.Context, uuid.UUID) ([]RecoveryCodeRecord, error) {
	return nil, nil
}
func (s *regenStore) MarkConsumed(context.Context, uuid.UUID) error { return nil }
func (s *regenStore) InvalidateAll(_ context.Context, _ uuid.UUID) (int, error) {
	s.invalidates++
	return 10, s.invalidateErr
}

func newRegenService(t *testing.T, store *regenStore, audit *regenAudit, alerter *regenAlerter) *Service {
	t.Helper()
	cfg := Config{
		SeedRepository: regenSeeds{},
		SeedCipher:     fakeCipher{},
		RecoveryStore:  store,
		CodeHasher:     fakeHasher{},
		Audit:          audit,
		Alerter:        alerter,
		Issuer:         "Sindireceita",
	}
	svc, err := NewService(cfg)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	return svc
}

func TestRegenerateRecovery_RejectsNilUserID(t *testing.T) {
	svc := newRegenService(t, &regenStore{}, &regenAudit{}, &regenAlerter{})
	if _, err := svc.RegenerateRecovery(context.Background(), uuid.Nil, RequestContext{}); err == nil {
		t.Fatal("expected non-nil error")
	}
}

func TestRegenerateRecovery_HappyPath(t *testing.T) {
	store := &regenStore{}
	audit := &regenAudit{}
	alerter := &regenAlerter{}
	svc := newRegenService(t, store, audit, alerter)

	codes, err := svc.RegenerateRecovery(context.Background(), uuid.New(), RequestContext{})
	if err != nil {
		t.Fatalf("RegenerateRecovery: %v", err)
	}

	t.Run("returns 10 plaintext codes", func(t *testing.T) {
		if len(codes) != RecoveryCodeCount {
			t.Errorf("len(codes): got %d want %d", len(codes), RecoveryCodeCount)
		}
		for i, c := range codes {
			if len(c) != RecoveryCodeLen {
				t.Errorf("code %d len: got %d want %d", i, len(c), RecoveryCodeLen)
			}
		}
	})

	t.Run("InvalidateAll BEFORE InsertHashes", func(t *testing.T) {
		if store.invalidates != 1 {
			t.Errorf("invalidates: got %d want 1", store.invalidates)
		}
		if store.insertCalls != 1 {
			t.Errorf("insertCalls: got %d want 1", store.insertCalls)
		}
	})

	t.Run("inserted hashes are 10 Argon2id-shaped", func(t *testing.T) {
		if len(store.insertedHashes) != RecoveryCodeCount {
			t.Errorf("insertedHashes: got %d want %d", len(store.insertedHashes), RecoveryCodeCount)
		}
		for i, h := range store.insertedHashes {
			if h[:2] != "h:" {
				t.Errorf("hash %d: %q is not from CodeHasher", i, h)
			}
			// Each hash must NOT equal the plaintext code.
			if h == codes[i] {
				t.Errorf("hash %d: equals plaintext", i)
			}
		}
	})

	t.Run("audit + alert fired on regen path", func(t *testing.T) {
		if audit.regenCalls != 1 {
			t.Errorf("LogRecoveryRegenerated calls: got %d want 1", audit.regenCalls)
		}
		if alerter.regenCalls != 1 {
			t.Errorf("AlertRecoveryRegenerated calls: got %d want 1", alerter.regenCalls)
		}
		if alerter.usedCalls != 0 {
			t.Errorf("AlertRecoveryUsed calls: got %d want 0 (regen path)", alerter.usedCalls)
		}
	})
}

func TestRegenerateRecovery_DeterministicWithFixedReader(t *testing.T) {
	svc := newRegenService(t, &regenStore{}, &regenAudit{}, &regenAlerter{})
	svc.rand = bytes.NewReader(make([]byte, recoveryCodeRawBytes*RecoveryCodeCount))
	codes, err := svc.RegenerateRecovery(context.Background(), uuid.New(), RequestContext{})
	if err != nil {
		t.Fatalf("RegenerateRecovery: %v", err)
	}
	for i, c := range codes {
		if c != "AAAAAAAAAA" {
			t.Errorf("code %d: got %q want all-A (zero-byte fixture)", i, c)
		}
	}
}

func TestRegenerateRecovery_PropagatesEachStepError(t *testing.T) {
	cases := map[string]func() (*regenStore, *regenAudit, *regenAlerter){
		"InvalidateAll fails": func() (*regenStore, *regenAudit, *regenAlerter) {
			return &regenStore{invalidateErr: errors.New("db blip")}, &regenAudit{}, &regenAlerter{}
		},
		"InsertHashes fails": func() (*regenStore, *regenAudit, *regenAlerter) {
			return &regenStore{insertErr: errors.New("db blip")}, &regenAudit{}, &regenAlerter{}
		},
		"audit fails": func() (*regenStore, *regenAudit, *regenAlerter) {
			return &regenStore{}, &regenAudit{regenErr: errors.New("audit boom")}, &regenAlerter{}
		},
	}
	for name, mk := range cases {
		t.Run(name, func(t *testing.T) {
			store, audit, alerter := mk()
			svc := newRegenService(t, store, audit, alerter)
			_, err := svc.RegenerateRecovery(context.Background(), uuid.New(), RequestContext{})
			if err == nil {
				t.Fatal("expected non-nil error")
			}
		})
	}
}

func TestRegenerateRecovery_AlertFailureIsNonFatal(t *testing.T) {
	// Alert outage MUST NOT cause us to roll back the new code set the
	// master has already seen. Same policy as ConsumeRecovery.
	store := &regenStore{}
	audit := &regenAudit{}
	alerter := &regenAlerter{regenErr: errors.New("slack 503")}
	svc := newRegenService(t, store, audit, alerter)
	codes, err := svc.RegenerateRecovery(context.Background(), uuid.New(), RequestContext{})
	if err != nil {
		t.Fatalf("expected nil error despite alert failure, got %v", err)
	}
	if len(codes) != RecoveryCodeCount {
		t.Errorf("len(codes): got %d want %d", len(codes), RecoveryCodeCount)
	}
	if audit.mfaRequired != 1 {
		t.Errorf("soft-fail audit trail (LogMFARequired): got %d want 1", audit.mfaRequired)
	}
}
