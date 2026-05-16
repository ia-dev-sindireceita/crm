package wallet_alerter

import (
	"sync"
	"time"
)

// Dedup is an in-memory bounded-time cache keyed by
// (tenant_id, occurred_at). The pair is the natural idempotency key for
// `wallet.balance.depleted`: a redelivery from JetStream carries the
// same occurred_at; a legitimate second depletion (the tenant tops up
// and runs dry again) carries a different occurred_at and is correctly
// surfaced as a second alert.
//
// The cache is not LRU — it does not need to be. Entries age out on
// Seen / Record reads via lazy pruning, and a periodic-sweep goroutine
// is intentionally omitted: the worker reads from the cache on every
// delivery, so lazy pruning is sufficient to keep the map bounded.
//
// Concurrency: Seen and Record are safe under concurrent readers from a
// queue-group subscription with parallel handlers.
type Dedup struct {
	ttl   time.Duration
	clock Clock

	mu      sync.Mutex
	entries map[string]time.Time // key → expiry timestamp
}

// NewDedup constructs a cache with the given TTL. A non-positive TTL
// is coerced to DefaultDedupTTL so the cache stays useful even when
// the caller forgets to wire a value.
func NewDedup(ttl time.Duration, clock Clock) *Dedup {
	if ttl <= 0 {
		ttl = DefaultDedupTTL
	}
	if clock == nil {
		clock = SystemClock{}
	}
	return &Dedup{
		ttl:     ttl,
		clock:   clock,
		entries: make(map[string]time.Time),
	}
}

// Seen reports whether (tenantID, occurredAt) is in the cache and not
// yet expired. Expired entries are removed as a side effect so the map
// does not grow without bound.
func (d *Dedup) Seen(tenantID string, occurredAt time.Time) bool {
	if d == nil {
		return false
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	now := d.clock.Now()
	d.pruneLocked(now)
	key := dedupKey(tenantID, occurredAt)
	expiry, ok := d.entries[key]
	if !ok {
		return false
	}
	if !expiry.After(now) {
		delete(d.entries, key)
		return false
	}
	return true
}

// Record stores (tenantID, occurredAt) with an expiry of now + ttl.
// Callers MUST only Record after a successful Notify: a Record without
// a delivery prevents redelivery retries.
func (d *Dedup) Record(tenantID string, occurredAt time.Time) {
	if d == nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	now := d.clock.Now()
	d.pruneLocked(now)
	d.entries[dedupKey(tenantID, occurredAt)] = now.Add(d.ttl)
}

// Len returns the number of live entries. Test-only convenience to
// verify pruning behaviour without exporting the internal map.
func (d *Dedup) Len() int {
	if d == nil {
		return 0
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.entries)
}

// pruneLocked drops every entry whose expiry has passed. Cheap because
// the map is bounded by the redelivery rate × TTL, and Go's range over
// a map allocates nothing per iteration.
func (d *Dedup) pruneLocked(now time.Time) {
	for k, exp := range d.entries {
		if !exp.After(now) {
			delete(d.entries, k)
		}
	}
}

// dedupKey serialises the pair into a single string with a delimiter
// that cannot appear in either component:
//
//   - tenant_id is uuid-shaped (no `|` byte)
//   - occurred_at is RFC3339Nano (no `|` byte)
//
// so a `|` joiner cannot collide between two legitimately distinct keys.
func dedupKey(tenantID string, occurredAt time.Time) string {
	return tenantID + "|" + occurredAt.UTC().Format(time.RFC3339Nano)
}
