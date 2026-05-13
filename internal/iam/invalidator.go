package iam

import (
	"context"
	"sync"

	"github.com/google/uuid"
)

// SessionInvalidator is the port for "delete every session for this user
// EXCEPT the one currently in use". The Redis adapter implements this by
// scanning sess:user:{uid}:* and deleting every key except the one whose
// session id matches currentSessionID. The in-memory MemoryInvalidator
// below mirrors the same invariant for tests so we never have to spin up
// Redis to assert the rotation behaviour.
//
// The "preserve current" semantic is critical for the password-change /
// logout-everywhere flow — the user just clicked the action, so the tab
// they are sitting in stays alive. Logging them out of their own tab is
// the single most common reason operators learn to ignore security
// events.
//
// Implementations MUST be safe to call concurrently; a real adapter is
// a Redis SCAN+DEL loop and the in-memory fake guards its map with a
// mutex.
type SessionInvalidator interface {
	InvalidateAllExceptCurrent(ctx context.Context, userID, currentSessionID uuid.UUID) (deleted int, err error)
}

// MemoryInvalidator is an in-memory SessionInvalidator suitable for unit
// tests. It models the Redis pattern sess:user:{uid}:* as a map of sets.
// Not for production: there is no eviction, no TTL, no persistence.
type MemoryInvalidator struct {
	mu       sync.Mutex
	sessions map[uuid.UUID]map[uuid.UUID]struct{}
}

// NewMemoryInvalidator returns a ready-to-use empty MemoryInvalidator.
func NewMemoryInvalidator() *MemoryInvalidator {
	return &MemoryInvalidator{sessions: make(map[uuid.UUID]map[uuid.UUID]struct{})}
}

// Add registers a (user, session) pair. Mirrors a Redis SET on
// sess:user:{userID}:{sessionID}.
func (m *MemoryInvalidator) Add(userID, sessionID uuid.UUID) {
	m.mu.Lock()
	defer m.mu.Unlock()
	set, ok := m.sessions[userID]
	if !ok {
		set = make(map[uuid.UUID]struct{})
		m.sessions[userID] = set
	}
	set[sessionID] = struct{}{}
}

// Has reports whether (userID, sessionID) is currently tracked. Used by
// tests to assert which sessions survived InvalidateAllExceptCurrent.
func (m *MemoryInvalidator) Has(userID, sessionID uuid.UUID) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	set, ok := m.sessions[userID]
	if !ok {
		return false
	}
	_, ok = set[sessionID]
	return ok
}

// InvalidateAllExceptCurrent deletes every session for userID except
// currentSessionID and returns the number of sessions deleted. A user
// with no tracked sessions returns (0, nil), matching the Redis SCAN+DEL
// "no keys matched" result.
func (m *MemoryInvalidator) InvalidateAllExceptCurrent(_ context.Context, userID, currentSessionID uuid.UUID) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	set, ok := m.sessions[userID]
	if !ok {
		return 0, nil
	}
	deleted := 0
	for sid := range set {
		if sid == currentSessionID {
			continue
		}
		delete(set, sid)
		deleted++
	}
	return deleted, nil
}
