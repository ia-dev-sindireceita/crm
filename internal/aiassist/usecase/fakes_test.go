package usecase_test

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/aiassist"
	"github.com/pericles-luz/crm/internal/wallet"
)

// fakeRepo is a hand-rolled in-memory SummaryRepository used in the unit
// tests. The integration tests use the real Postgres adapter — the two
// surfaces are validated separately so a regression in either path
// surfaces independently. The struct is concurrency-safe so the
// integration-style "many goroutines" use case test can share it.
type fakeRepo struct {
	mu        sync.Mutex
	summaries map[uuid.UUID][]*aiassist.Summary // keyed by conversationID
	getErr    error
	saveErr   error
	invErr    error
	saved     int
	getCalls  int
}

func newFakeRepo() *fakeRepo {
	return &fakeRepo{summaries: map[uuid.UUID][]*aiassist.Summary{}}
}

func (f *fakeRepo) GetLatestValid(_ context.Context, _, conversationID uuid.UUID, now time.Time) (*aiassist.Summary, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.getCalls++
	if f.getErr != nil {
		return nil, f.getErr
	}
	list := f.summaries[conversationID]
	// Return the most recent still-valid row.
	for i := len(list) - 1; i >= 0; i-- {
		if list[i].IsValid(now) {
			// return a shallow copy to mimic the adapter handing back
			// a freshly hydrated struct
			cp := *list[i]
			return &cp, nil
		}
	}
	return nil, aiassist.ErrCacheMiss
}

func (f *fakeRepo) Save(_ context.Context, s *aiassist.Summary) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.saveErr != nil {
		return f.saveErr
	}
	cp := *s
	f.summaries[s.ConversationID] = append(f.summaries[s.ConversationID], &cp)
	f.saved++
	return nil
}

func (f *fakeRepo) InvalidateForConversation(_ context.Context, _, conversationID uuid.UUID, now time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.invErr != nil {
		return f.invErr
	}
	for _, s := range f.summaries[conversationID] {
		s.Invalidate(now)
	}
	return nil
}

// fakeWallet is an in-memory WalletClient. It enforces the contract:
// Reserve fails with ErrInsufficientFunds when amount > available;
// Commit deducts actualAmount (clamped); Release restores the
// reservation without deducting. Idempotency on the same key returns
// the prior result.
type fakeWallet struct {
	mu       sync.Mutex
	balance  int64
	reserved int64
	// reservationsByKey lets Reserve/Commit/Release dedup on the same
	// idempotency key.
	reservationsByKey map[string]*wallet.Reservation
	committed         map[uuid.UUID]int64
	released          map[uuid.UUID]bool
	reserveErr        error
	commitErr         error
	releaseErr        error
	// reserveCalls counts every Reserve invocation, including
	// idempotent retries.
	reserveCalls int
	commitCalls  int
	releaseCalls int
}

func newFakeWallet(initialBalance int64) *fakeWallet {
	return &fakeWallet{
		balance:           initialBalance,
		reservationsByKey: map[string]*wallet.Reservation{},
		committed:         map[uuid.UUID]int64{},
		released:          map[uuid.UUID]bool{},
	}
}

func (f *fakeWallet) Reserve(_ context.Context, tenantID uuid.UUID, amount int64, key string) (*wallet.Reservation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reserveCalls++
	if f.reserveErr != nil {
		return nil, f.reserveErr
	}
	if prev, ok := f.reservationsByKey[key]; ok {
		// idempotent retry of an in-flight reservation
		cp := *prev
		return &cp, nil
	}
	available := f.balance - f.reserved
	if amount > available {
		return nil, wallet.ErrInsufficientFunds
	}
	f.reserved += amount
	r := &wallet.Reservation{
		ID:             uuid.New(),
		WalletID:       uuid.New(),
		TenantID:       tenantID,
		Amount:         amount,
		IdempotencyKey: key,
		CreatedAt:      time.Now(),
	}
	f.reservationsByKey[key] = r
	return r, nil
}

func (f *fakeWallet) Commit(_ context.Context, r *wallet.Reservation, actualAmount int64, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.commitCalls++
	if f.commitErr != nil {
		return f.commitErr
	}
	if f.released[r.ID] || f.committed[r.ID] != 0 {
		// completed reservations are no-ops on retry
		return nil
	}
	if actualAmount > r.Amount {
		return wallet.ErrInvalidAmount
	}
	f.reserved -= r.Amount
	f.balance -= actualAmount
	f.committed[r.ID] = actualAmount
	return nil
}

func (f *fakeWallet) Release(_ context.Context, r *wallet.Reservation, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.releaseCalls++
	if f.releaseErr != nil {
		return f.releaseErr
	}
	if f.released[r.ID] || f.committed[r.ID] != 0 {
		return nil
	}
	f.reserved -= r.Amount
	f.released[r.ID] = true
	return nil
}

func (f *fakeWallet) snapshot() (balance, reserved int64, commits, releases int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.balance, f.reserved, f.commitCalls, f.releaseCalls
}

// fakeLLM is a controllable LLMClient. Each call returns the next
// queued response (or err) and the queued state survives concurrent
// callers.
type fakeLLM struct {
	mu        sync.Mutex
	resp      aiassist.LLMResponse
	err       error
	calls     int
	lastReq   aiassist.LLMRequest
	delay     time.Duration
	customFn  func(req aiassist.LLMRequest) (aiassist.LLMResponse, error)
	lastCalls []aiassist.LLMRequest
}

func (f *fakeLLM) Complete(ctx context.Context, req aiassist.LLMRequest) (aiassist.LLMResponse, error) {
	f.mu.Lock()
	f.calls++
	f.lastReq = req
	f.lastCalls = append(f.lastCalls, req)
	delay := f.delay
	customFn := f.customFn
	f.mu.Unlock()

	if delay > 0 {
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return aiassist.LLMResponse{}, ctx.Err()
		}
	}
	if customFn != nil {
		return customFn(req)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return aiassist.LLMResponse{}, f.err
	}
	return f.resp, nil
}

func (f *fakeLLM) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// fakePolicy lets each test plug in a policy resolver. nil policyFn
// returns a permissive policy.
type fakePolicy struct {
	policy aiassist.Policy
	err    error
	calls  int
	mu     sync.Mutex
}

func (f *fakePolicy) Resolve(_ context.Context, _ uuid.UUID, _ aiassist.Scope) (aiassist.Policy, error) {
	f.mu.Lock()
	f.calls++
	pol := f.policy
	err := f.err
	f.mu.Unlock()
	return pol, err
}

func defaultPolicy() aiassist.Policy {
	return aiassist.Policy{
		AIEnabled:       true,
		OptIn:           true,
		Anonymize:       true,
		Model:           "google/gemini-2.0-flash",
		MaxOutputTokens: 256,
	}
}

// fakeRateLimiter is a controllable RateLimiter stub.
type fakeRateLimiter struct {
	allow      bool
	retryAfter time.Duration
	err        error
	calls      int
	mu         sync.Mutex
}

func (f *fakeRateLimiter) Allow(_ context.Context, _, _ string) (bool, time.Duration, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	return f.allow, f.retryAfter, f.err
}

// fixedClock returns the same instant on every call. Tests advance the
// underlying time by swapping the value through (*fixedClock).set.
type fixedClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFixedClock(t time.Time) *fixedClock {
	return &fixedClock{t: t}
}

func (c *fixedClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fixedClock) add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// errSentinel is used to assert error-wrapping behaviour without
// importing context-specific sentinels.
var errSentinel = errors.New("aiassist test: synthetic upstream error")
