package webchat

import "sync"

// Broker is a simple in-memory pub-sub for SSE. Each session has at
// most one subscriber. Publish sends a JSON payload string to the
// subscriber for that session_id (no-op if nobody is watching).
//
// The Postgres-backed durable replay for Last-Event-ID is a follow-up
// concern; the in-memory broker satisfies the integration-test
// requirement of "client receives a message sent by the agent".
type Broker struct {
	mu   sync.Mutex
	subs map[string][]chan string
}

// NewBroker returns an empty Broker.
func NewBroker() *Broker {
	return &Broker{subs: make(map[string][]chan string)}
}

// Subscribe returns a channel that receives payloads for sessionID.
// Callers must eventually call Unsubscribe to avoid a leak.
func (b *Broker) Subscribe(sessionID string) chan string {
	ch := make(chan string, 16)
	b.mu.Lock()
	b.subs[sessionID] = append(b.subs[sessionID], ch)
	b.mu.Unlock()
	return ch
}

// Unsubscribe removes ch from the sessionID subscriber list and closes
// it so the SSE handler's select exits cleanly.
func (b *Broker) Unsubscribe(sessionID string, ch chan string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	old := b.subs[sessionID]
	next := old[:0]
	for _, s := range old {
		if s != ch {
			next = append(next, s)
		}
	}
	if len(next) == 0 {
		delete(b.subs, sessionID)
	} else {
		b.subs[sessionID] = next
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
