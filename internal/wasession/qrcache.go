package wasession

import (
	"sync"
	"time"

	"github.com/google/uuid"
)

// QRCache holds the latest pairing QR per tenant so the provisioning UI
// (Fase 4, SIN-66259) can render the current code on demand.
//
// The Manager only fans QR codes out on its Events channel — it keeps no
// retrievable QR state — so a request-driven UI cannot ask the Manager
// "what is the QR right now?". The cmd/server inbound pump bridges that
// gap: it Puts every EventQR here (WITHOUT logging the secret, ADR 0107
// D6) and Clears the entry once the session pairs (connected) or is
// logged out (banned), so the UI never offers a dead code.
//
// Entries expire at the QR's ExpiresAt — WhatsApp rotates a Web pairing
// code roughly every 20 seconds — so Get never returns a stale code as if
// it were live. A zero ExpiresAt is treated as non-expiring (the producer
// did not stamp a rotation deadline); the entry then lives until it is
// overwritten or Cleared.
//
// QRCache is safe for concurrent use: the pump writes while UI requests
// read.
type QRCache struct {
	mu  sync.RWMutex
	now func() time.Time
	m   map[uuid.UUID]QRCode
}

// QRCacheOption configures a QRCache.
type QRCacheOption func(*QRCache)

// WithQRClock overrides the clock used for expiry. Tests pin it; production
// leaves it at time.Now.
func WithQRClock(now func() time.Time) QRCacheOption {
	return func(c *QRCache) {
		if now != nil {
			c.now = now
		}
	}
}

// NewQRCache builds an empty cache.
func NewQRCache(opts ...QRCacheOption) *QRCache {
	c := &QRCache{
		now: time.Now,
		m:   make(map[uuid.UUID]QRCode),
	}
	for _, o := range opts {
		o(c)
	}
	return c
}

// Put stores qr as the latest pairing code for tenantID, replacing any
// previous one. A zero-value (empty Code) qr is ignored so a spurious
// emit cannot wipe a live code.
func (c *QRCache) Put(tenantID uuid.UUID, qr QRCode) {
	if qr.Code.IsZero() {
		return
	}
	c.mu.Lock()
	c.m[tenantID] = qr
	c.mu.Unlock()
}

// Get returns the latest unexpired QR for tenantID. ok is false when no
// code is stored or the stored code has expired. An expired entry is
// dropped on read so the map does not accumulate dead codes.
func (c *QRCache) Get(tenantID uuid.UUID) (QRCode, bool) {
	c.mu.RLock()
	qr, ok := c.m[tenantID]
	c.mu.RUnlock()
	if !ok {
		return QRCode{}, false
	}
	if c.expired(qr) {
		c.Clear(tenantID)
		return QRCode{}, false
	}
	return qr, true
}

// Clear removes any stored QR for tenantID. It is safe to call when no
// entry exists.
func (c *QRCache) Clear(tenantID uuid.UUID) {
	c.mu.Lock()
	delete(c.m, tenantID)
	c.mu.Unlock()
}

// expired reports whether qr's rotation deadline has passed. A zero
// ExpiresAt never expires.
func (c *QRCache) expired(qr QRCode) bool {
	if qr.ExpiresAt.IsZero() {
		return false
	}
	return c.now().After(qr.ExpiresAt)
}
