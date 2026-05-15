package quarantine

import (
	"context"
	"errors"
	"sync"
)

// Memory is the in-process Quarantiner used by unit tests and by the
// worker_test fakes. It keeps two maps — runtime + quarantine — so a
// test can pre-load a key, call Move, and then assert which bucket it
// landed in. Production paths never reach for this implementation.
type Memory struct {
	mu         sync.Mutex
	runtime    map[string][]byte
	quarantine map[string][]byte
}

// NewMemory returns a Memory ready for use. Both maps start empty;
// callers seed objects via Put before calling Move.
func NewMemory() *Memory {
	return &Memory{
		runtime:    map[string][]byte{},
		quarantine: map[string][]byte{},
	}
}

// Put seeds an object into the runtime bucket. Test helper.
func (m *Memory) Put(key string, body []byte) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.runtime[key] = append([]byte(nil), body...)
}

// Quarantined reports whether key currently lives in the quarantine
// bucket. Returns the body and true on hit, nil and false on miss.
func (m *Memory) Quarantined(key string) ([]byte, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.quarantine[key]
	if !ok {
		return nil, false
	}
	return append([]byte(nil), v...), true
}

// Runtime reports whether key still lives in the runtime bucket.
func (m *Memory) Runtime(key string) ([]byte, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.runtime[key]
	if !ok {
		return nil, false
	}
	return append([]byte(nil), v...), true
}

// errMissingKey is returned by Move when key does not exist in the
// runtime bucket AND is not already in quarantine. Real adapters return
// their own transport errors; this sentinel is only here so the
// idempotency table-test can distinguish "moved" from "missing".
var errMissingKey = errors.New("quarantine/memory: key not in runtime or quarantine bucket")

// Move implements Quarantiner. The operation is idempotent:
//
//   - key in runtime, not in quarantine → copy+delete, return nil.
//   - key already in quarantine → no-op, return nil (worker redelivery).
//   - key in neither → return errMissingKey so callers can distinguish
//     genuine misses from transport flakes.
func (m *Memory) Move(_ context.Context, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.quarantine[key]; ok {
		return nil
	}
	body, ok := m.runtime[key]
	if !ok {
		return errMissingKey
	}
	m.quarantine[key] = body
	delete(m.runtime, key)
	return nil
}
