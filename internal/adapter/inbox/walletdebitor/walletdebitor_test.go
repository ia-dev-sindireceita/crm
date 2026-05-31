package walletdebitor_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/adapter/inbox/walletdebitor"
	"github.com/pericles-luz/crm/internal/wallet"
)

// fakeWalletService is a hand-written fake of the wallet.Service slice
// the adapter consumes. Each method records every call so tests can
// assert the exact reserve/charge/commit/release ordering. The fake is
// goroutine-safe; the race tests exercise it under -race.
type fakeWalletService struct {
	mu sync.Mutex

	reserveResult *wallet.Reservation
	reserveErr    error
	commitErr     error
	releaseErr    error

	reserveCalls []reserveCall
	commitCalls  []commitCall
	releaseCalls []releaseCall
}

type reserveCall struct {
	tenantID uuid.UUID
	amount   int64
	key      string
}

type commitCall struct {
	reservation *wallet.Reservation
	amount      int64
	key         string
}

type releaseCall struct {
	reservation *wallet.Reservation
	key         string
}

func (f *fakeWalletService) Reserve(_ context.Context, tenantID uuid.UUID, amount int64, idempotencyKey string) (*wallet.Reservation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reserveCalls = append(f.reserveCalls, reserveCall{tenantID: tenantID, amount: amount, key: idempotencyKey})
	if f.reserveErr != nil {
		return nil, f.reserveErr
	}
	if f.reserveResult != nil {
		return f.reserveResult, nil
	}
	r := &wallet.Reservation{
		ID:             uuid.New(),
		WalletID:       uuid.New(),
		TenantID:       tenantID,
		Amount:         amount,
		IdempotencyKey: idempotencyKey,
	}
	f.reserveResult = r
	return r, nil
}

func (f *fakeWalletService) Commit(_ context.Context, r *wallet.Reservation, actualAmount int64, idempotencyKey string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.commitCalls = append(f.commitCalls, commitCall{reservation: r, amount: actualAmount, key: idempotencyKey})
	return f.commitErr
}

func (f *fakeWalletService) Release(_ context.Context, r *wallet.Reservation, idempotencyKey string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.releaseCalls = append(f.releaseCalls, releaseCall{reservation: r, key: idempotencyKey})
	return f.releaseErr
}

func (f *fakeWalletService) snapshot() (reserves []reserveCall, commits []commitCall, releases []releaseCall) {
	f.mu.Lock()
	defer f.mu.Unlock()
	reserves = append(reserves, f.reserveCalls...)
	commits = append(commits, f.commitCalls...)
	releases = append(releases, f.releaseCalls...)
	return
}

func newAdapter(t *testing.T, svc walletdebitor.WalletService, opts ...walletdebitor.Option) *walletdebitor.Adapter {
	t.Helper()
	a, err := walletdebitor.New(svc, opts...)
	if err != nil {
		t.Fatalf("New: unexpected error: %v", err)
	}
	return a
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func bufferedLogger() (*slog.Logger, *bytes.Buffer) {
	buf := &bytes.Buffer{}
	return slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})), buf
}

// (a) reserve + charge-ok + commit
func TestDebit_ReserveChargeCommit_HappyPath(t *testing.T) {
	t.Parallel()
	svc := &fakeWalletService{}
	a := newAdapter(t, svc, walletdebitor.WithLogger(discardLogger()))

	var chargeCalled int
	tenant := uuid.New()
	err := a.Debit(context.Background(), tenant, 7, func(_ context.Context) error {
		chargeCalled++
		return nil
	})
	if err != nil {
		t.Fatalf("Debit: unexpected error: %v", err)
	}
	if chargeCalled != 1 {
		t.Fatalf("charge calls = %d, want 1", chargeCalled)
	}

	reserves, commits, releases := svc.snapshot()
	if len(reserves) != 1 {
		t.Fatalf("reserve calls = %d, want 1", len(reserves))
	}
	if reserves[0].tenantID != tenant {
		t.Errorf("reserve tenant = %s, want %s", reserves[0].tenantID, tenant)
	}
	if reserves[0].amount != 7 {
		t.Errorf("reserve amount = %d, want 7", reserves[0].amount)
	}
	if len(commits) != 1 {
		t.Fatalf("commit calls = %d, want 1", len(commits))
	}
	if commits[0].amount != 7 {
		t.Errorf("commit amount = %d, want 7", commits[0].amount)
	}
	if len(releases) != 0 {
		t.Errorf("release calls = %d, want 0", len(releases))
	}
	if commits[0].key == reserves[0].key {
		t.Errorf("commit key must differ from reserve key; both = %q", commits[0].key)
	}
}

// (b) reserve + charge-fail + release
func TestDebit_ChargeError_ReleasesReservation(t *testing.T) {
	t.Parallel()
	svc := &fakeWalletService{}
	a := newAdapter(t, svc, walletdebitor.WithLogger(discardLogger()))

	wantErr := errors.New("carrier rejected")
	tenant := uuid.New()
	err := a.Debit(context.Background(), tenant, 3, func(_ context.Context) error {
		return wantErr
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("Debit error = %v, want wantErr", err)
	}

	reserves, commits, releases := svc.snapshot()
	if len(reserves) != 1 {
		t.Fatalf("reserve calls = %d, want 1", len(reserves))
	}
	if len(commits) != 0 {
		t.Errorf("commit calls = %d, want 0", len(commits))
	}
	if len(releases) != 1 {
		t.Fatalf("release calls = %d, want 1", len(releases))
	}
	if releases[0].reservation != svc.lastReservation() {
		t.Errorf("release reservation pointer mismatch")
	}
	if releases[0].key == reserves[0].key {
		t.Errorf("release key must differ from reserve key")
	}
}

func (f *fakeWalletService) lastReservation() *wallet.Reservation {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.reserveResult
}

// (c) reserve-fail (no charge, no release)
func TestDebit_ReserveError_DoesNotCharge(t *testing.T) {
	t.Parallel()
	svc := &fakeWalletService{reserveErr: wallet.ErrInsufficientFunds}
	a := newAdapter(t, svc, walletdebitor.WithLogger(discardLogger()))

	var chargeCalled bool
	err := a.Debit(context.Background(), uuid.New(), 5, func(_ context.Context) error {
		chargeCalled = true
		return nil
	})
	if !errors.Is(err, wallet.ErrInsufficientFunds) {
		t.Fatalf("Debit error = %v, want ErrInsufficientFunds", err)
	}
	if chargeCalled {
		t.Fatalf("charge invoked despite reserve failure")
	}

	_, commits, releases := svc.snapshot()
	if len(commits) != 0 || len(releases) != 0 {
		t.Fatalf("commits=%d releases=%d, want 0/0", len(commits), len(releases))
	}
}

// (d) commit-fail (log + reraise)
func TestDebit_CommitError_LogsAndReturnsError(t *testing.T) {
	t.Parallel()
	commitErr := errors.New("ledger unavailable")
	svc := &fakeWalletService{commitErr: commitErr}
	logger, buf := bufferedLogger()
	a := newAdapter(t, svc, walletdebitor.WithLogger(logger))

	err := a.Debit(context.Background(), uuid.New(), 1, func(_ context.Context) error { return nil })
	if !errors.Is(err, commitErr) {
		t.Fatalf("Debit error = %v, want commitErr", err)
	}
	if !strings.Contains(err.Error(), "commit after successful charge") {
		t.Errorf("error message = %q, want wrapper prefix", err.Error())
	}
	if !strings.Contains(buf.String(), "commit after successful charge failed") {
		t.Errorf("log = %q, want commit-after-charge warning", buf.String())
	}

	// Release MUST NOT run after a charge-success-commit-fail; the
	// reservation already represents a debited spend.
	_, _, releases := svc.snapshot()
	if len(releases) != 0 {
		t.Errorf("release calls = %d, want 0 after commit failure", len(releases))
	}
}

// AC #5: cost == 0 still invokes charge, never touches the wallet.
func TestDebit_ZeroCost_SkipsWalletButRunsCharge(t *testing.T) {
	t.Parallel()
	svc := &fakeWalletService{}
	a := newAdapter(t, svc, walletdebitor.WithLogger(discardLogger()))

	var chargeCalled int
	err := a.Debit(context.Background(), uuid.New(), 0, func(_ context.Context) error {
		chargeCalled++
		return nil
	})
	if err != nil {
		t.Fatalf("Debit: unexpected error: %v", err)
	}
	if chargeCalled != 1 {
		t.Fatalf("charge calls = %d, want 1", chargeCalled)
	}

	reserves, commits, releases := svc.snapshot()
	if len(reserves)+len(commits)+len(releases) != 0 {
		t.Fatalf("wallet was touched on zero cost: r=%d c=%d rel=%d", len(reserves), len(commits), len(releases))
	}
}

// AC #5 (cost == 0) with a failing charge: error must propagate but
// the wallet still stays untouched.
func TestDebit_ZeroCost_ChargeError_Propagates(t *testing.T) {
	t.Parallel()
	svc := &fakeWalletService{}
	a := newAdapter(t, svc, walletdebitor.WithLogger(discardLogger()))

	wantErr := errors.New("send dropped")
	err := a.Debit(context.Background(), uuid.New(), 0, func(_ context.Context) error { return wantErr })
	if !errors.Is(err, wantErr) {
		t.Fatalf("Debit error = %v, want wantErr", err)
	}
	reserves, commits, releases := svc.snapshot()
	if len(reserves)+len(commits)+len(releases) != 0 {
		t.Fatalf("wallet was touched on zero-cost failure: r=%d c=%d rel=%d", len(reserves), len(commits), len(releases))
	}
}

// Release failure after a charge failure is logged at warn and the
// caller still sees the original charge error.
func TestDebit_ChargeError_ReleaseError_LogsAndReturnsChargeErr(t *testing.T) {
	t.Parallel()
	releaseErr := errors.New("release boom")
	svc := &fakeWalletService{releaseErr: releaseErr}
	logger, buf := bufferedLogger()
	a := newAdapter(t, svc, walletdebitor.WithLogger(logger))

	wantErr := errors.New("carrier rejected")
	err := a.Debit(context.Background(), uuid.New(), 9, func(_ context.Context) error { return wantErr })
	if !errors.Is(err, wantErr) {
		t.Fatalf("Debit error = %v, want wantErr (carrier rejection)", err)
	}
	if errors.Is(err, releaseErr) {
		t.Fatalf("Debit error must not mask carrier rejection with release error")
	}
	if !strings.Contains(buf.String(), "release after failed charge failed") {
		t.Errorf("log = %q, want release-after-charge warning", buf.String())
	}
}

// Nil charge callback is rejected at the boundary.
func TestDebit_NilCharge_Rejected(t *testing.T) {
	t.Parallel()
	a := newAdapter(t, &fakeWalletService{})
	if err := a.Debit(context.Background(), uuid.New(), 1, nil); err == nil {
		t.Fatal("Debit with nil charge: want error, got nil")
	}
}

// Negative cost is rejected at the boundary.
func TestDebit_NegativeCost_Rejected(t *testing.T) {
	t.Parallel()
	a := newAdapter(t, &fakeWalletService{})
	err := a.Debit(context.Background(), uuid.New(), -1, func(_ context.Context) error { return nil })
	if !errors.Is(err, wallet.ErrInvalidAmount) {
		t.Fatalf("Debit error = %v, want ErrInvalidAmount", err)
	}
}

// New rejects a nil service.
func TestNew_NilService_Rejected(t *testing.T) {
	t.Parallel()
	if _, err := walletdebitor.New(nil); err == nil {
		t.Fatal("New(nil): want error, got nil")
	}
}

// MustNew panics when New would error.
func TestMustNew_PanicsOnNilService(t *testing.T) {
	t.Parallel()
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("MustNew(nil): want panic, got none")
		}
	}()
	_ = walletdebitor.MustNew(nil)
}

// MustNew returns a working adapter on the happy path.
func TestMustNew_HappyPath(t *testing.T) {
	t.Parallel()
	a := walletdebitor.MustNew(&fakeWalletService{}, walletdebitor.WithLogger(discardLogger()))
	if a == nil {
		t.Fatal("MustNew returned nil adapter")
	}
	if err := a.Debit(context.Background(), uuid.New(), 1, func(_ context.Context) error { return nil }); err != nil {
		t.Fatalf("Debit: %v", err)
	}
}

// Idempotency hints carried on context produce deterministic, distinct
// keys for reserve/commit/release and identical keys across calls with
// the same triple.
func TestDebit_IdempotencyHints_ProduceDeterministicKeys(t *testing.T) {
	t.Parallel()
	svc := &fakeWalletService{}
	a := newAdapter(t, svc, walletdebitor.WithLogger(discardLogger()))

	tenant := uuid.New()
	conv := uuid.New()
	msg := uuid.New()

	ctx := walletdebitor.WithIdempotencyHints(context.Background(), conv, msg)
	if err := a.Debit(ctx, tenant, 4, func(_ context.Context) error { return nil }); err != nil {
		t.Fatalf("Debit: %v", err)
	}

	reserves, commits, _ := svc.snapshot()
	if len(reserves) != 1 || len(commits) != 1 {
		t.Fatalf("calls: r=%d c=%d", len(reserves), len(commits))
	}
	if !strings.HasPrefix(reserves[0].key, "inbox-send:reserve:") {
		t.Errorf("reserve key = %q, want inbox-send:reserve: prefix", reserves[0].key)
	}
	if !strings.Contains(reserves[0].key, conv.String()) || !strings.Contains(reserves[0].key, msg.String()) {
		t.Errorf("reserve key = %q, want conv/msg embedded", reserves[0].key)
	}
	if reserves[0].key == commits[0].key {
		t.Errorf("reserve and commit keys must differ: both = %q", reserves[0].key)
	}

	// A second Debit with the same hints reuses the same keys — that is
	// the property that lets the wallet collapse retries to no-op rows.
	if err := a.Debit(ctx, tenant, 4, func(_ context.Context) error { return nil }); err != nil {
		t.Fatalf("Debit (retry): %v", err)
	}
	reserves2, commits2, _ := svc.snapshot()
	if len(reserves2) != 2 || len(commits2) != 2 {
		t.Fatalf("retry calls: r=%d c=%d", len(reserves2), len(commits2))
	}
	if reserves2[0].key != reserves2[1].key {
		t.Errorf("reserve keys diverge across retries: %q vs %q", reserves2[0].key, reserves2[1].key)
	}
	if commits2[0].key != commits2[1].key {
		t.Errorf("commit keys diverge across retries: %q vs %q", commits2[0].key, commits2[1].key)
	}
}

// Hints with nil ids fall through to per-call UUID keys (caller opts
// out of idempotency).
func TestDebit_WithIdempotencyHints_NilIDs_FallsBackToUUID(t *testing.T) {
	t.Parallel()
	svc := &fakeWalletService{}
	a := newAdapter(t, svc, walletdebitor.WithLogger(discardLogger()))

	ctx := walletdebitor.WithIdempotencyHints(context.Background(), uuid.Nil, uuid.New())
	if err := a.Debit(ctx, uuid.New(), 1, func(_ context.Context) error { return nil }); err != nil {
		t.Fatalf("Debit: %v", err)
	}
	reserves, _, _ := svc.snapshot()
	if strings.Contains(reserves[0].key, uuid.Nil.String()) {
		t.Errorf("reserve key embedded nil uuid: %q", reserves[0].key)
	}
}

// Without hints the default function still produces a non-empty key
// every call. The keys differ across calls (per-call UUID).
func TestDebit_DefaultKeys_AreNonEmptyAndUnique(t *testing.T) {
	t.Parallel()
	svc := &fakeWalletService{}
	a := newAdapter(t, svc, walletdebitor.WithLogger(discardLogger()))

	for range 3 {
		if err := a.Debit(context.Background(), uuid.New(), 1, func(_ context.Context) error { return nil }); err != nil {
			t.Fatalf("Debit: %v", err)
		}
	}
	reserves, _, _ := svc.snapshot()
	seen := map[string]struct{}{}
	for _, c := range reserves {
		if c.key == "" {
			t.Fatal("default reserve key was empty")
		}
		seen[c.key] = struct{}{}
	}
	if len(seen) != len(reserves) {
		t.Errorf("default reserve keys collided across calls: %v", reserves)
	}
}

// Custom IdempotencyKeyFn is honoured (and exercised across all three
// operations).
func TestDebit_WithIdempotencyKeyFn_Override(t *testing.T) {
	t.Parallel()
	svc := &fakeWalletService{}
	gen := func(_ context.Context, _ uuid.UUID, op string) string { return "custom-" + op }
	a := newAdapter(t, svc,
		walletdebitor.WithLogger(discardLogger()),
		walletdebitor.WithIdempotencyKeyFn(gen),
	)

	// charge-fail to exercise release as well.
	_ = a.Debit(context.Background(), uuid.New(), 2, func(_ context.Context) error { return errors.New("x") })
	// charge-ok to exercise commit.
	if err := a.Debit(context.Background(), uuid.New(), 2, func(_ context.Context) error { return nil }); err != nil {
		t.Fatalf("Debit: %v", err)
	}

	reserves, commits, releases := svc.snapshot()
	if len(reserves) != 2 || len(commits) != 1 || len(releases) != 1 {
		t.Fatalf("calls: r=%d c=%d rel=%d", len(reserves), len(commits), len(releases))
	}
	if reserves[0].key != "custom-reserve" || reserves[1].key != "custom-reserve" {
		t.Errorf("reserve keys = %q,%q, want custom-reserve", reserves[0].key, reserves[1].key)
	}
	if commits[0].key != "custom-commit" {
		t.Errorf("commit key = %q, want custom-commit", commits[0].key)
	}
	if releases[0].key != "custom-release" {
		t.Errorf("release key = %q, want custom-release", releases[0].key)
	}
}

// Nil options must be tolerated (variadic noise).
func TestNew_NilOption_Tolerated(t *testing.T) {
	t.Parallel()
	a, err := walletdebitor.New(&fakeWalletService{}, nil, walletdebitor.WithLogger(discardLogger()), nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := a.Debit(context.Background(), uuid.New(), 1, func(_ context.Context) error { return nil }); err != nil {
		t.Fatalf("Debit: %v", err)
	}
}

// Nil logger / nil idempotency-fn options leave the defaults in place.
func TestOptions_NilFunc_KeepsDefault(t *testing.T) {
	t.Parallel()
	svc := &fakeWalletService{}
	a, err := walletdebitor.New(svc,
		walletdebitor.WithLogger(nil),
		walletdebitor.WithIdempotencyKeyFn(nil),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := a.Debit(context.Background(), uuid.New(), 1, func(_ context.Context) error { return nil }); err != nil {
		t.Fatalf("Debit: %v", err)
	}
	reserves, _, _ := svc.snapshot()
	if reserves[0].key == "" {
		t.Fatal("default key generator was lost when WithIdempotencyKeyFn(nil) was applied")
	}
}

// Concurrent Debits against the same adapter MUST be race-free under
// -race. The adapter holds no per-call mutable state of its own;
// this test guards against accidental introduction of one.
func TestDebit_ConcurrentCalls_RaceFree(t *testing.T) {
	t.Parallel()
	svc := &fakeWalletService{}
	a := newAdapter(t, svc, walletdebitor.WithLogger(discardLogger()))

	const n = 32
	var wg sync.WaitGroup
	wg.Add(n)
	for range n {
		go func() {
			defer wg.Done()
			tenant := uuid.New()
			_ = a.Debit(context.Background(), tenant, 1, func(_ context.Context) error { return nil })
		}()
	}
	wg.Wait()

	reserves, commits, _ := svc.snapshot()
	if len(reserves) != n || len(commits) != n {
		t.Fatalf("calls: r=%d c=%d, want %d/%d", len(reserves), len(commits), n, n)
	}
}
