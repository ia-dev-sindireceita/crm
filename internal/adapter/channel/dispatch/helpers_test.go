package dispatch

import (
	"context"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/inbox"
)

// recordingChannel is a fake inbox.OutboundChannel that records every
// call and returns a scripted (wamid, err). It is the seam under test for
// the decorators — the real whatsapp.Sender is exercised in the
// integration test (dispatch_test package).
type recordingChannel struct {
	mu    sync.Mutex
	wamid string
	err   error
	calls []inbox.OutboundMessage
	// block, when non-nil, is received-from before the call returns so a
	// test can hold a send "in-flight" while a second goroutine runs.
	block chan struct{}
}

func newRecordingChannel(wamid string) *recordingChannel {
	return &recordingChannel{wamid: wamid}
}

func (c *recordingChannel) SendMessage(_ context.Context, m inbox.OutboundMessage) (string, error) {
	c.mu.Lock()
	c.calls = append(c.calls, m)
	block := c.block
	err := c.err
	wamid := c.wamid
	c.mu.Unlock()
	if block != nil {
		<-block
	}
	if err != nil {
		return "", err
	}
	return wamid, nil
}

func (c *recordingChannel) callCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.calls)
}

// fakeLimiter is a scripted RateLimiter. allow controls the verdict; err
// is returned as the limiter error; calls counts invocations.
type fakeLimiter struct {
	mu         sync.Mutex
	allow      bool
	retryAfter time.Duration
	err        error
	keys       []string
}

func (l *fakeLimiter) Allow(_ context.Context, key string, _ time.Duration, _ int) (bool, time.Duration, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.keys = append(l.keys, key)
	return l.allow, l.retryAfter, l.err
}

func (l *fakeLimiter) callCount() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.keys)
}

func newOutboundMessage(tenant uuid.UUID, channel, key string) inbox.OutboundMessage {
	return inbox.OutboundMessage{
		TenantID:       tenant,
		ConversationID: uuid.New(),
		Channel:        channel,
		ToExternalID:   "+5511999990001",
		Body:           "olá",
		IdempotencyKey: key,
	}
}
