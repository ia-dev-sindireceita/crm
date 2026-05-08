package mfa

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
)

// fakeSeedRepoEnrolled returns a fixed-ciphertext seed for any
// userID; LoadSeed is the only method the verify path uses heavily.
type fakeSeedRepoEnrolled struct {
	loadCalls         int
	loadErr           error
	ciphertext        []byte
	markVerifiedCalls int
	markVerifiedErr   error
	markReenrollCalls int
	markReenrollErr   error
}

func (f *fakeSeedRepoEnrolled) StoreSeed(_ context.Context, _ uuid.UUID, _ []byte) error {
	return nil
}
func (f *fakeSeedRepoEnrolled) LoadSeed(_ context.Context, _ uuid.UUID) ([]byte, error) {
	f.loadCalls++
	if f.loadErr != nil {
		return nil, f.loadErr
	}
	return f.ciphertext, nil
}
func (f *fakeSeedRepoEnrolled) MarkVerified(_ context.Context, _ uuid.UUID) error {
	f.markVerifiedCalls++
	return f.markVerifiedErr
}
func (f *fakeSeedRepoEnrolled) MarkReenrollRequired(_ context.Context, _ uuid.UUID) error {
	f.markReenrollCalls++
	return f.markReenrollErr
}

// realisticCipher reuses the TOTP seed bytes literally — Encrypt
// returns the seed prefixed with a marker, Decrypt strips it. The
// service's Verify path calls Decrypt(LoadSeed result), so we can
// drive it with a real RFC 6238 vector.
type realisticCipher struct{ decryptErr error }

func (c realisticCipher) Encrypt(p []byte) ([]byte, error) { return append([]byte("ENC:"), p...), nil }
func (c realisticCipher) Decrypt(in []byte) ([]byte, error) {
	if c.decryptErr != nil {
		return nil, c.decryptErr
	}
	if !bytes.HasPrefix(in, []byte("ENC:")) {
		return nil, errors.New("realistic cipher: bad prefix")
	}
	return in[4:], nil
}

// recordingAudit lets tests assert which audit method fired.
type recordingAudit struct {
	enrolled, verified, recoveryUsed, recoveryRegen, mfaRequired int
	verifiedErr                                                  error
}

func (a *recordingAudit) LogEnrolled(context.Context, uuid.UUID) error { a.enrolled++; return nil }
func (a *recordingAudit) LogVerified(context.Context, uuid.UUID) error {
	a.verified++
	return a.verifiedErr
}
func (a *recordingAudit) LogRecoveryUsed(context.Context, uuid.UUID) error {
	a.recoveryUsed++
	return nil
}
func (a *recordingAudit) LogRecoveryRegenerated(context.Context, uuid.UUID) error {
	a.recoveryRegen++
	return nil
}
func (a *recordingAudit) LogMFARequired(_ context.Context, _ uuid.UUID, _, _ string) error {
	a.mfaRequired++
	return nil
}

type recordingAlerter struct {
	calls      int
	err        error
	regenCalls int
	regenErr   error
}

func (a *recordingAlerter) AlertRecoveryUsed(context.Context, uuid.UUID) error {
	a.calls++
	return a.err
}
func (a *recordingAlerter) AlertRecoveryRegenerated(context.Context, uuid.UUID) error {
	a.regenCalls++
	return a.regenErr
}

func newVerifyService(t *testing.T, seeds *fakeSeedRepoEnrolled, codes *fakeRecoveryStore, audit *recordingAudit, alerter *recordingAlerter, clock func() time.Time) *Service {
	t.Helper()
	svc, err := NewService(Config{
		SeedRepository: seeds,
		SeedCipher:     realisticCipher{},
		RecoveryStore:  codes,
		CodeHasher:     fakeHasher{},
		Audit:          audit,
		Alerter:        alerter,
		Issuer:         "Sindireceita",
		Clock:          clock,
	})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	return svc
}

// ---------------------------------------------------------------------------
// Service.Verify — TOTP submission
// ---------------------------------------------------------------------------

func TestVerify_RejectsNilUserID(t *testing.T) {
	svc := newVerifyService(t, &fakeSeedRepoEnrolled{}, &fakeRecoveryStore{}, &recordingAudit{}, &recordingAlerter{}, nil)
	if err := svc.Verify(context.Background(), uuid.Nil, "287082"); err == nil {
		t.Fatal("Verify(uuid.Nil) returned nil error")
	}
}

func TestVerify_NotEnrolledMapsToInvalidCode(t *testing.T) {
	seeds := &fakeSeedRepoEnrolled{loadErr: ErrNotEnrolled}
	svc := newVerifyService(t, seeds, &fakeRecoveryStore{}, &recordingAudit{}, &recordingAlerter{}, nil)
	err := svc.Verify(context.Background(), uuid.New(), "287082")
	if !errors.Is(err, ErrInvalidCode) {
		t.Fatalf("err: got %v want ErrInvalidCode (anti-enumeration)", err)
	}
}

func TestVerify_LoadSeedErrorPropagates(t *testing.T) {
	seeds := &fakeSeedRepoEnrolled{loadErr: errors.New("db down")}
	svc := newVerifyService(t, seeds, &fakeRecoveryStore{}, &recordingAudit{}, &recordingAlerter{}, nil)
	err := svc.Verify(context.Background(), uuid.New(), "287082")
	if err == nil || errors.Is(err, ErrInvalidCode) {
		t.Fatalf("expected wrapped non-ErrInvalidCode error, got %v", err)
	}
}

func TestVerify_DecryptErrorPropagates(t *testing.T) {
	seeds := &fakeSeedRepoEnrolled{ciphertext: []byte("ENC:xxxxxxxxxxxxxxxxxxxx")}
	cfg := Config{
		SeedRepository: seeds,
		SeedCipher:     realisticCipher{decryptErr: errors.New("bad key")},
		RecoveryStore:  &fakeRecoveryStore{},
		CodeHasher:     fakeHasher{},
		Audit:          &recordingAudit{},
		Alerter:        &recordingAlerter{},
		Issuer:         "Sindireceita",
	}
	svc, err := NewService(cfg)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	if err := svc.Verify(context.Background(), uuid.New(), "287082"); err == nil {
		t.Fatal("Verify with broken cipher returned nil error")
	}
}

func TestVerify_HappyPath_RFC6238Vector(t *testing.T) {
	// RFC 6238 §Appendix B: secret "12345678901234567890" at unix=59
	// produces 6-digit code 287082.
	seed := []byte("12345678901234567890")
	encrypted := append([]byte("ENC:"), seed...) // realisticCipher round-trip
	seeds := &fakeSeedRepoEnrolled{ciphertext: encrypted}
	codes := &fakeRecoveryStore{}
	audit := &recordingAudit{}
	clock := func() time.Time { return time.Unix(59, 0) }
	svc := newVerifyService(t, seeds, codes, audit, &recordingAlerter{}, clock)

	uid := uuid.New()
	if err := svc.Verify(context.Background(), uid, "287082"); err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if seeds.loadCalls != 1 {
		t.Errorf("LoadSeed calls: got %d want 1", seeds.loadCalls)
	}
	if seeds.markVerifiedCalls != 1 {
		t.Errorf("MarkVerified calls: got %d want 1", seeds.markVerifiedCalls)
	}
	if audit.verified != 1 {
		t.Errorf("LogVerified calls: got %d want 1", audit.verified)
	}
}

func TestVerify_WrongCodeReturnsInvalidCode(t *testing.T) {
	seed := []byte("12345678901234567890")
	encrypted := append([]byte("ENC:"), seed...)
	seeds := &fakeSeedRepoEnrolled{ciphertext: encrypted}
	clock := func() time.Time { return time.Unix(59, 0) }
	svc := newVerifyService(t, seeds, &fakeRecoveryStore{}, &recordingAudit{}, &recordingAlerter{}, clock)
	if err := svc.Verify(context.Background(), uuid.New(), "000000"); !errors.Is(err, ErrInvalidCode) {
		t.Fatalf("err: got %v want ErrInvalidCode", err)
	}
}

func TestVerify_ShortSeedCollapsesToInvalidCode(t *testing.T) {
	// A truncated stored seed should not leak ErrSeedTooShort to the
	// caller — both modes look like "wrong code".
	seeds := &fakeSeedRepoEnrolled{ciphertext: append([]byte("ENC:"), []byte("short")...)}
	svc := newVerifyService(t, seeds, &fakeRecoveryStore{}, &recordingAudit{}, &recordingAlerter{}, nil)
	if err := svc.Verify(context.Background(), uuid.New(), "287082"); !errors.Is(err, ErrInvalidCode) {
		t.Fatalf("err: got %v want ErrInvalidCode", err)
	}
}

func TestVerify_MarkVerifiedFailureIsFatal(t *testing.T) {
	seed := []byte("12345678901234567890")
	encrypted := append([]byte("ENC:"), seed...)
	seeds := &fakeSeedRepoEnrolled{ciphertext: encrypted, markVerifiedErr: errors.New("db blip")}
	clock := func() time.Time { return time.Unix(59, 0) }
	svc := newVerifyService(t, seeds, &fakeRecoveryStore{}, &recordingAudit{}, &recordingAlerter{}, clock)
	err := svc.Verify(context.Background(), uuid.New(), "287082")
	if err == nil || errors.Is(err, ErrInvalidCode) {
		t.Fatalf("err: got %v want non-nil non-ErrInvalidCode (mark verified failure should propagate)", err)
	}
}

func TestVerify_AuditFailureIsFatal(t *testing.T) {
	seed := []byte("12345678901234567890")
	encrypted := append([]byte("ENC:"), seed...)
	seeds := &fakeSeedRepoEnrolled{ciphertext: encrypted}
	audit := &recordingAudit{verifiedErr: errors.New("audit boom")}
	clock := func() time.Time { return time.Unix(59, 0) }
	svc := newVerifyService(t, seeds, &fakeRecoveryStore{}, audit, &recordingAlerter{}, clock)
	err := svc.Verify(context.Background(), uuid.New(), "287082")
	if err == nil {
		t.Fatal("Verify with audit error returned nil")
	}
}

// ---------------------------------------------------------------------------
// Service.ConsumeRecovery — single-use plaintext code submission
// ---------------------------------------------------------------------------

// recoveryStoreScripted is a tighter fake than fakeRecoveryStore that
// returns a scripted ListActive result and tracks MarkConsumed.
type recoveryStoreScripted struct {
	listResult       []RecoveryCodeRecord
	listErr          error
	markConsumed     []uuid.UUID
	markConsumedErr  error
	invalidates      int
	invalidatedUsers []uuid.UUID
	invalidateErr    error
}

func (r *recoveryStoreScripted) InsertHashes(context.Context, uuid.UUID, []string) error {
	return nil
}
func (r *recoveryStoreScripted) ListActive(context.Context, uuid.UUID) ([]RecoveryCodeRecord, error) {
	if r.listErr != nil {
		return nil, r.listErr
	}
	out := make([]RecoveryCodeRecord, len(r.listResult))
	copy(out, r.listResult)
	return out, nil
}
func (r *recoveryStoreScripted) MarkConsumed(_ context.Context, id uuid.UUID) error {
	r.markConsumed = append(r.markConsumed, id)
	return r.markConsumedErr
}
func (r *recoveryStoreScripted) InvalidateAll(_ context.Context, uid uuid.UUID) (int, error) {
	r.invalidates++
	r.invalidatedUsers = append(r.invalidatedUsers, uid)
	return 0, r.invalidateErr
}

// hasherForCode is a hasher whose Verify(stored, plain) returns true
// iff stored == "MATCH:" + plain. Tests pre-build storage rows with
// the matching prefix.
type hasherForCode struct{ verifyErr error }

func (h hasherForCode) Hash(plain string) (string, error) { return "MATCH:" + plain, nil }
func (h hasherForCode) Verify(stored, plain string) (bool, error) {
	if h.verifyErr != nil {
		return false, h.verifyErr
	}
	return stored == "MATCH:"+plain, nil
}

func newConsumeService(t *testing.T, codes *recoveryStoreScripted, audit *recordingAudit, alerter *recordingAlerter, hasher CodeHasher, seeds SeedRepository) *Service {
	t.Helper()
	cfg := Config{
		SeedRepository: seeds,
		SeedCipher:     fakeCipher{},
		RecoveryStore:  codes,
		CodeHasher:     hasher,
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

func TestConsumeRecovery_RejectsNilUserID(t *testing.T) {
	svc := newConsumeService(t,
		&recoveryStoreScripted{},
		&recordingAudit{},
		&recordingAlerter{},
		hasherForCode{},
		&fakeSeedRepository{},
	)
	if err := svc.ConsumeRecovery(context.Background(), uuid.Nil, "ABCDE23456"); err == nil {
		t.Fatal("expected non-nil error")
	}
}

func TestConsumeRecovery_MalformedCodeMapsToInvalidCode(t *testing.T) {
	svc := newConsumeService(t,
		&recoveryStoreScripted{},
		&recordingAudit{},
		&recordingAlerter{},
		hasherForCode{},
		&fakeSeedRepository{},
	)
	cases := []string{"", "ABC", "tooooolongggg", "ABCDE12345" /* 1 not in base32 alphabet */}
	for _, in := range cases {
		t.Run(fmt.Sprintf("input=%q", in), func(t *testing.T) {
			err := svc.ConsumeRecovery(context.Background(), uuid.New(), in)
			if !errors.Is(err, ErrInvalidCode) {
				t.Fatalf("err: got %v want ErrInvalidCode", err)
			}
		})
	}
}

func TestConsumeRecovery_HappyPath(t *testing.T) {
	uid := uuid.New()
	matchID := uuid.New()
	codes := &recoveryStoreScripted{
		listResult: []RecoveryCodeRecord{
			{ID: uuid.New(), Hash: "MATCH:OTHER0CODE"},
			{ID: matchID, Hash: "MATCH:ABCDE23456"},
			{ID: uuid.New(), Hash: "MATCH:THIRD0CODE"},
		},
	}
	audit := &recordingAudit{}
	alerter := &recordingAlerter{}
	seeds := &fakeSeedRepository{}
	svc := newConsumeService(t, codes, audit, alerter, hasherForCode{}, seeds)

	err := svc.ConsumeRecovery(context.Background(), uid, "ABCDE-23456") // dashed input
	if err != nil {
		t.Fatalf("ConsumeRecovery: %v", err)
	}

	t.Run("matched row marked consumed", func(t *testing.T) {
		if len(codes.markConsumed) != 1 {
			t.Fatalf("MarkConsumed calls: got %d want 1", len(codes.markConsumed))
		}
		if codes.markConsumed[0] != matchID {
			t.Errorf("MarkConsumed id: got %v want %v", codes.markConsumed[0], matchID)
		}
	})

	t.Run("master flagged for re-enroll", func(t *testing.T) {
		if seeds.markReenrollCalls != 1 {
			t.Errorf("MarkReenrollRequired calls: got %d want 1", seeds.markReenrollCalls)
		}
	})

	t.Run("audit + alert both fired", func(t *testing.T) {
		if audit.recoveryUsed != 1 {
			t.Errorf("LogRecoveryUsed calls: got %d want 1", audit.recoveryUsed)
		}
		if alerter.calls != 1 {
			t.Errorf("AlertRecoveryUsed calls: got %d want 1", alerter.calls)
		}
	})
}

func TestConsumeRecovery_WalkExhaustsListEvenAfterMatch(t *testing.T) {
	// Anti-timing-oracle: the loop must not short-circuit after the
	// first match. We assert this by counting hasher.Verify calls —
	// with 5 rows, all 5 must be called even if the second matches.
	uid := uuid.New()
	rows := []RecoveryCodeRecord{
		{ID: uuid.New(), Hash: "MATCH:CODE1AAAAA"},
		{ID: uuid.New(), Hash: "MATCH:ABCDE23456"}, // match
		{ID: uuid.New(), Hash: "MATCH:CODE3AAAAA"},
		{ID: uuid.New(), Hash: "MATCH:CODE4AAAAA"},
		{ID: uuid.New(), Hash: "MATCH:CODE5AAAAA"},
	}
	counter := &countingHasher{}
	codes := &recoveryStoreScripted{listResult: rows}
	svc := newConsumeService(t, codes, &recordingAudit{}, &recordingAlerter{}, counter, &fakeSeedRepository{})
	if err := svc.ConsumeRecovery(context.Background(), uid, "ABCDE23456"); err != nil {
		t.Fatalf("ConsumeRecovery: %v", err)
	}
	if counter.calls != len(rows) {
		t.Fatalf("Verify calls: got %d want %d (early-exit timing oracle)", counter.calls, len(rows))
	}
}

type countingHasher struct{ calls int }

func (h *countingHasher) Hash(plain string) (string, error) { return "MATCH:" + plain, nil }
func (h *countingHasher) Verify(stored, plain string) (bool, error) {
	h.calls++
	return stored == "MATCH:"+plain, nil
}

func TestConsumeRecovery_NoMatchReturnsInvalidCode(t *testing.T) {
	rows := []RecoveryCodeRecord{
		{ID: uuid.New(), Hash: "MATCH:ZZZZZ77777"},
		{ID: uuid.New(), Hash: "MATCH:WWWWW77777"},
	}
	svc := newConsumeService(t,
		&recoveryStoreScripted{listResult: rows},
		&recordingAudit{},
		&recordingAlerter{},
		hasherForCode{},
		&fakeSeedRepository{},
	)
	err := svc.ConsumeRecovery(context.Background(), uuid.New(), "ABCDE23456")
	if !errors.Is(err, ErrInvalidCode) {
		t.Fatalf("err: got %v want ErrInvalidCode", err)
	}
}

func TestConsumeRecovery_EmptyActiveSetReturnsInvalidCode(t *testing.T) {
	svc := newConsumeService(t,
		&recoveryStoreScripted{listResult: nil},
		&recordingAudit{},
		&recordingAlerter{},
		hasherForCode{},
		&fakeSeedRepository{},
	)
	err := svc.ConsumeRecovery(context.Background(), uuid.New(), "ABCDE23456")
	if !errors.Is(err, ErrInvalidCode) {
		t.Fatalf("err: got %v want ErrInvalidCode", err)
	}
}

func TestConsumeRecovery_HasherErrorPropagates(t *testing.T) {
	rows := []RecoveryCodeRecord{{ID: uuid.New(), Hash: "MATCH:ABCDE23456"}}
	svc := newConsumeService(t,
		&recoveryStoreScripted{listResult: rows},
		&recordingAudit{},
		&recordingAlerter{},
		hasherForCode{verifyErr: errors.New("argon decode boom")},
		&fakeSeedRepository{},
	)
	err := svc.ConsumeRecovery(context.Background(), uuid.New(), "ABCDE23456")
	if err == nil || errors.Is(err, ErrInvalidCode) {
		t.Fatalf("err: got %v want wrapped non-ErrInvalidCode (system-side hasher failure)", err)
	}
}

func TestConsumeRecovery_ListActiveErrorPropagates(t *testing.T) {
	svc := newConsumeService(t,
		&recoveryStoreScripted{listErr: errors.New("db down")},
		&recordingAudit{},
		&recordingAlerter{},
		hasherForCode{},
		&fakeSeedRepository{},
	)
	err := svc.ConsumeRecovery(context.Background(), uuid.New(), "ABCDE23456")
	if err == nil || errors.Is(err, ErrInvalidCode) {
		t.Fatalf("err: got %v want wrapped non-ErrInvalidCode", err)
	}
}

func TestConsumeRecovery_AlertFailureIsNonFatal(t *testing.T) {
	// The code is already consumed when the alert fires. A Slack
	// outage MUST NOT cause us to tell the master "wrong code" — that
	// would burn a recovery code AND lock them out.
	rows := []RecoveryCodeRecord{{ID: uuid.New(), Hash: "MATCH:ABCDE23456"}}
	codes := &recoveryStoreScripted{listResult: rows}
	audit := &recordingAudit{}
	alerter := &recordingAlerter{err: errors.New("slack 503")}
	svc := newConsumeService(t, codes, audit, alerter, hasherForCode{}, &fakeSeedRepository{})

	if err := svc.ConsumeRecovery(context.Background(), uuid.New(), "ABCDE23456"); err != nil {
		t.Fatalf("expected nil error despite alert failure, got %v", err)
	}
	// And the soft-fail audit hook fired (LogMFARequired with reason
	// prefix "alerter_failed:") so ops can spot the degraded path.
	if audit.mfaRequired != 1 {
		t.Errorf("LogMFARequired calls: got %d want 1 (soft-fail trail)", audit.mfaRequired)
	}
}

func TestConsumeRecovery_MarkConsumedFailureIsFatal(t *testing.T) {
	rows := []RecoveryCodeRecord{{ID: uuid.New(), Hash: "MATCH:ABCDE23456"}}
	codes := &recoveryStoreScripted{listResult: rows, markConsumedErr: errors.New("db blip")}
	svc := newConsumeService(t, codes, &recordingAudit{}, &recordingAlerter{}, hasherForCode{}, &fakeSeedRepository{})
	if err := svc.ConsumeRecovery(context.Background(), uuid.New(), "ABCDE23456"); err == nil {
		t.Fatal("MarkConsumed failure must surface, got nil")
	}
}

func TestConsumeRecovery_MarkReenrollFailureIsFatal(t *testing.T) {
	rows := []RecoveryCodeRecord{{ID: uuid.New(), Hash: "MATCH:ABCDE23456"}}
	codes := &recoveryStoreScripted{listResult: rows}
	seeds := &fakeSeedRepository{}
	// Override MarkReenrollRequired error via a wrapper.
	failingSeeds := &reenrollFailingSeeds{base: seeds}
	svc := newConsumeService(t, codes, &recordingAudit{}, &recordingAlerter{}, hasherForCode{}, failingSeeds)
	if err := svc.ConsumeRecovery(context.Background(), uuid.New(), "ABCDE23456"); err == nil {
		t.Fatal("MarkReenrollRequired failure must surface, got nil")
	}
}

type reenrollFailingSeeds struct{ base SeedRepository }

func (r *reenrollFailingSeeds) StoreSeed(ctx context.Context, u uuid.UUID, b []byte) error {
	return r.base.StoreSeed(ctx, u, b)
}
func (r *reenrollFailingSeeds) LoadSeed(ctx context.Context, u uuid.UUID) ([]byte, error) {
	return r.base.LoadSeed(ctx, u)
}
func (r *reenrollFailingSeeds) MarkVerified(ctx context.Context, u uuid.UUID) error {
	return r.base.MarkVerified(ctx, u)
}
func (r *reenrollFailingSeeds) MarkReenrollRequired(_ context.Context, _ uuid.UUID) error {
	return errors.New("reenroll boom")
}
