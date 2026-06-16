package webchat

import "sync"

// Connection caps for the SSE stream (ADR-0021 D5, threat T5 "SSE pool
// exhaustion"). A single valid session — or many sessions behind one
// IP — must not be able to open unbounded concurrent streams, each of
// which holds a goroutine plus a 16-slot channel.
const (
	// maxStreamsPerSession is the concurrent-stream cap per session_id.
	maxStreamsPerSession = 1
	// maxStreamsPerTenantIP is the concurrent-stream cap per
	// (tenant × IP), keyed by the session's LGPD-safe ip_hash.
	maxStreamsPerTenantIP = 5
)

// Broker is a simple in-memory pub-sub for SSE. Concurrency is bounded:
// at most maxStreamsPerSession subscribers per session_id and
// maxStreamsPerTenantIP subscribers per (tenant × IP) bucket. Publish
// sends a JSON payload string to every subscriber for that session_id
// (no-op if nobody is watching).
//
// The Postgres-backed durable replay for Last-Event-ID is a follow-up
// concern; the in-memory broker satisfies the integration-test
// requirement of "client receives a message sent by the agent".
type Broker struct {
	mu       sync.Mutex
	subs     map[string][]chan string
	ipCounts map[string]int
}

// NewBroker returns an empty Broker.
func NewBroker() *Broker {
	return &Broker{
		subs:     make(map[string][]chan string),
		ipCounts: make(map[string]int),
	}
}

// Subscribe registers a subscriber for sessionID and returns its
// channel. ipKey is the (tenant × IP) bucket (the session's ip_hash);
// pass "" to opt a subscriber out of the per-IP cap.
//
// It returns (nil, false) when admitting the subscriber would exceed
// the per-session or the per-(tenant × IP) concurrent-stream cap. On
// success the caller MUST eventually call Unsubscribe with the same
// sessionID and ipKey to release the slot and avoid a leak.
func (b *Broker) Subscribe(sessionID, ipKey string) (chan string, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.subs[sessionID]) >= maxStreamsPerSession {
		return nil, false
	}
	if ipKey != "" && b.ipCounts[ipKey] >= maxStreamsPerTenantIP {
		return nil, false
	}
	ch := make(chan string, 16)
	b.subs[sessionID] = append(b.subs[sessionID], ch)
	if ipKey != "" {
		b.ipCounts[ipKey]++
	}
	return ch, true
}

// Unsubscribe removes ch from the sessionID subscriber list, releases
// the ipKey slot, and closes ch so the SSE handler's select exits
// cleanly. ipKey must match the value passed to Subscribe.
func (b *Broker) Unsubscribe(sessionID, ipKey string, ch chan string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	old := b.subs[sessionID]
	next := old[:0]
	removed := false
	for _, s := range old {
		if s != ch {
			next = append(next, s)
		} else {
			removed = true
		}
	}
	if len(next) == 0 {
		delete(b.subs, sessionID)
	} else {
		b.subs[sessionID] = next
	}
	if removed && ipKey != "" {
		if b.ipCounts[ipKey] <= 1 {
			delete(b.ipCounts, ipKey)
		} else {
			b.ipCounts[ipKey]--
		}
	}
	close(ch)
}

// Publish sends payload to every subscriber for sessionID. It is
// non-blocking: a full subscriber channel is skipped.
func (b *Broker) Publish(sessionID, payload string) {
	b.mu.Lock()
	chans := b.subs[sessionID]
	b.mu.Unlock()
	for _, ch := range chans {
		select {
		case ch <- payload:
		default:
		}
	}
}
