package instagram_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/adapter/channels/instagram"
	"github.com/pericles-luz/crm/internal/inbox"
)

// fakeInbox mirrors the production ON CONFLICT DO NOTHING semantics
// the dedup table enforces — see whatsapp tests for the rationale.
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

func (f *fakeInbox) Persisted() []inbox.InboundEvent {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]inbox.InboundEvent, len(f.persisted))
	copy(out, f.persisted)
	return out
}

func (f *fakeInbox) FailWith(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failure = err
}

func (f *fakeInbox) Calls() int64 { return f.calls.Load() }

// fakeResolver maps ig_business_id → tenant_id deterministically.
type fakeResolver struct {
	mu      sync.Mutex
	mapping map[string]uuid.UUID
	err     error
}

func newFakeResolver() *fakeResolver {
	return &fakeResolver{mapping: map[string]uuid.UUID{}}
}

func (f *fakeResolver) Register(igBusinessID string, tenant uuid.UUID) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.mapping[igBusinessID] = tenant
}

func (f *fakeResolver) Resolve(_ context.Context, igBusinessID string) (uuid.UUID, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return uuid.Nil, f.err
	}
	id, ok := f.mapping[igBusinessID]
	if !ok {
		return uuid.Nil, instagram.ErrUnknownIGBusinessID
	}
	return id, nil
}

// fakeFlag is a per-tenant feature flag fake.
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

// fakeRateLimiter mirrors the production redis adapter contract.
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

// fakeMediaPublisher records each PublishScanRequest call.
type fakeMediaPublisher struct {
	mu    sync.Mutex
	calls []mediaCall
	err   error
}

type mediaCall struct {
	TenantID  uuid.UUID
	MessageID uuid.UUID
	Key       string
}

func newFakeMediaPublisher() *fakeMediaPublisher { return &fakeMediaPublisher{} }

func (p *fakeMediaPublisher) PublishScanRequest(_ context.Context, tenantID, messageID uuid.UUID, key string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.err != nil {
		return p.err
	}
	p.calls = append(p.calls, mediaCall{TenantID: tenantID, MessageID: messageID, Key: key})
	return nil
}

func (p *fakeMediaPublisher) Calls() []mediaCall {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]mediaCall, len(p.calls))
	copy(out, p.calls)
	return out
}

// fakeClock returns a fixed instant.
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

// nopLookup is the AssociationLookup fake for TenantResolver tests.
type nopLookup struct {
	resp uuid.UUID
	err  error
}

func (n *nopLookup) Resolve(_ context.Context, _, _ string) (uuid.UUID, error) {
	return n.resp, n.err
}

// Compile-time guards.
var (
	_ inbox.InboundChannel         = (*fakeInbox)(nil)
	_ instagram.TenantResolver     = (*fakeResolver)(nil)
	_ instagram.FeatureFlag        = (*fakeFlag)(nil)
	_ instagram.RateLimiter        = (*fakeRateLimiter)(nil)
	_ instagram.Clock              = (*fakeClock)(nil)
	_ instagram.MediaScanPublisher = (*fakeMediaPublisher)(nil)
)

var errInjected = errors.New("injected")
