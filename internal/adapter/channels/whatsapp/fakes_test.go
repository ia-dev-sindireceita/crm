package whatsapp_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/adapter/channels/whatsapp"
	"github.com/pericles-luz/crm/internal/inbox"
)

// fakeInbox implements inbox.InboundChannel with an atomic
// (channel, channel_external_id) → deliveries map that mirrors the
// production ON CONFLICT DO NOTHING semantics. The map IS the unit
// under test for AC #2 (replay of the same wamid 100x concurrent →
// exactly 1 persisted message): the same atomic uniqueness invariant
// that Postgres enforces with UNIQUE constraints, expressed as a
// mutex-guarded set in-process so the test can drive concurrency
// without a database.
type fakeInbox struct {
	mu        sync.Mutex
	seen      map[string]struct{}
	persisted []inbox.InboundEvent
	failure   error
	calls     atomic.Int64
}

func newFakeInbox() *fakeInbox {
	return &fakeInbox{seen: map[string]struct{}{}}
}

func (f *fakeInbox) HandleInbound(_ context.Context, ev inbox.InboundEvent) error {
	f.calls.Add(1)
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failure != nil {
		return f.failure
	}
	key := ev.Channel + ":" + ev.ChannelExternalID
	if _, ok := f.seen[key]; ok {
		return inbox.ErrInboundAlreadyProcessed
	}
	f.seen[key] = struct{}{}
	f.persisted = append(f.persisted, ev)
	return nil
}

func (f *fakeInbox) PersistedCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.persisted)
}

func (f *fakeInbox) Persisted() []inbox.InboundEvent {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]inbox.InboundEvent, len(f.persisted))
	copy(out, f.persisted)
	return out
}

func (f *fakeInbox) CallCount() int64 { return f.calls.Load() }

func (f *fakeInbox) FailWith(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failure = err
}

// fakeResolver maps phone_number_id → tenant_id deterministically.
type fakeResolver struct {
	mu      sync.Mutex
	mapping map[string]uuid.UUID
	err     error
}

func newFakeResolver() *fakeResolver {
	return &fakeResolver{mapping: map[string]uuid.UUID{}}
}

func (f *fakeResolver) Register(pn string, tenant uuid.UUID) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.mapping[pn] = tenant
}

func (f *fakeResolver) Resolve(_ context.Context, pn string) (uuid.UUID, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return uuid.Nil, f.err
	}
	id, ok := f.mapping[pn]
	if !ok {
		return uuid.Nil, whatsapp.ErrUnknownPhoneNumberID
	}
	return id, nil
}

// fakeFlag is a per-tenant feature flag fake. The default is
// "enabled for any tenant that was explicitly enabled"; tests can
// also stub a failure to exercise the warn-log path.
type fakeFlag struct {
	mu       sync.Mutex
	on       map[uuid.UUID]bool
	defaultV bool
	err      error
}

func newFakeFlag(defaultEnabled bool) *fakeFlag {
	return &fakeFlag{on: map[uuid.UUID]bool{}, defaultV: defaultEnabled}
}

func (f *fakeFlag) Set(tenant uuid.UUID, enabled bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.on[tenant] = enabled
}

func (f *fakeFlag) FailWith(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.err = err
}

func (f *fakeFlag) Enabled(_ context.Context, t uuid.UUID) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return false, f.err
	}
	if v, ok := f.on[t]; ok {
		return v, nil
	}
	return f.defaultV, nil
}

// fakeRateLimiter is an in-memory window counter that mirrors the
// production redis adapter's contract: Allow is atomic per key,
// returning (false, retryAfter) once max is exhausted within window.
type fakeRateLimiter struct {
	mu     sync.Mutex
	state  map[string]int
	limit  int
	err    error
	denied atomic.Int64
}

func newFakeRateLimiter(limit int) *fakeRateLimiter {
	return &fakeRateLimiter{state: map[string]int{}, limit: limit}
}

func (r *fakeRateLimiter) FailWith(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.err = err
}

func (r *fakeRateLimiter) Allow(_ context.Context, key string, window time.Duration, _ int) (bool, time.Duration, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.err != nil {
		return false, 0, r.err
	}
	r.state[key]++
	if r.limit > 0 && r.state[key] > r.limit {
		r.denied.Add(1)
		return false, window, nil
	}
	return true, 0, nil
}

// fakeClock returns a fixed instant — handler timestamp-window math
// is the unit under test, not the host system's wall clock.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock(t time.Time) *fakeClock { return &fakeClock{now: t} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = t
}

// nopLookup is a no-op AssociationLookup for the TenantResolverFunc
// godoc example tests (TestTenantResolverFunc_*); it just returns the
// configured response.
type nopLookup struct {
	resp uuid.UUID
	err  error
}

func (n *nopLookup) Resolve(_ context.Context, _, _ string) (uuid.UUID, error) {
	return n.resp, n.err
}

// Compile-time guards.
var (
	_ inbox.InboundChannel    = (*fakeInbox)(nil)
	_ whatsapp.TenantResolver = (*fakeResolver)(nil)
	_ whatsapp.FeatureFlag    = (*fakeFlag)(nil)
	_ whatsapp.RateLimiter    = (*fakeRateLimiter)(nil)
	_ whatsapp.Clock          = (*fakeClock)(nil)
)

var errInjected = errors.New("injected")
