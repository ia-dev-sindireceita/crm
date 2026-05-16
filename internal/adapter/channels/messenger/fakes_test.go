package messenger_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/adapter/channels/messenger"
	"github.com/pericles-luz/crm/internal/inbox"
)

// fakeInbox implements inbox.InboundChannel with an in-memory dedup
// map. It mirrors the production ON CONFLICT DO NOTHING semantics so
// the messenger handler can be unit-tested without spinning up a
// database.
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

func (f *fakeInbox) CallCount() int64 { return f.calls.Load() }

func (f *fakeInbox) FailWith(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failure = err
}

// fakeResolver maps page_id → tenant_id deterministically.
type fakeResolver struct {
	mu      sync.Mutex
	mapping map[string]uuid.UUID
	err     error
}

func newFakeResolver() *fakeResolver {
	return &fakeResolver{mapping: map[string]uuid.UUID{}}
}

func (f *fakeResolver) Register(pageID string, tenant uuid.UUID) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.mapping[pageID] = tenant
}

func (f *fakeResolver) Resolve(_ context.Context, pageID string) (uuid.UUID, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return uuid.Nil, f.err
	}
	id, ok := f.mapping[pageID]
	if !ok {
		return uuid.Nil, messenger.ErrUnknownPageID
	}
	return id, nil
}

// fakeFlag is a per-tenant feature-flag fake.
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

func (p *fakeMediaPublisher) FailWith(err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.err = err
}

// Compile-time guards.
var (
	_ inbox.InboundChannel         = (*fakeInbox)(nil)
	_ messenger.TenantResolver     = (*fakeResolver)(nil)
	_ messenger.FeatureFlag        = (*fakeFlag)(nil)
	_ messenger.Clock              = (*fakeClock)(nil)
	_ messenger.MediaScanPublisher = (*fakeMediaPublisher)(nil)
)

var errInjected = errors.New("messenger_test: injected")
