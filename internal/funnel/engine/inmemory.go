package engine

import (
	"context"
	"sync"

	"github.com/google/uuid"
)

// InMemoryApplications is a goroutine-safe in-process implementation of
// [ApplicationsRepo]. Unit tests use it without spinning up Postgres;
// production wiring substitutes the pgx adapter from
// internal/adapter/db/postgres/funnelapplications.
//
// The store keys on (rule_id, message_id) so the lookup mirrors the
// UNIQUE index on the funnel_rule_applications table. Tenant id is
// retained on the stored value for parity with the production adapter
// but is not part of the dedup key — the migration's UNIQUE is global
// per (rule, message), not per (tenant, rule, message).
type InMemoryApplications struct {
	mu      sync.Mutex
	entries map[applicationKey]Application
}

type applicationKey struct {
	ruleID    uuid.UUID
	messageID uuid.UUID
}

// NewInMemoryApplications returns a ready repository.
func NewInMemoryApplications() *InMemoryApplications {
	return &InMemoryApplications{entries: make(map[applicationKey]Application)}
}

// IsApplied implements the port. The receiver lock is held only across
// the map probe, never around any I/O.
func (r *InMemoryApplications) IsApplied(_ context.Context, _, ruleID, messageID uuid.UUID) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.entries[applicationKey{ruleID: ruleID, messageID: messageID}]
	return ok, nil
}

// Record implements the port. A duplicate insert returns
// [ErrAlreadyApplied] so callers can short-circuit on the race; the
// stored value on the original key is left untouched.
func (r *InMemoryApplications) Record(_ context.Context, app Application) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	key := applicationKey{ruleID: app.RuleID, messageID: app.MessageID}
	if _, exists := r.entries[key]; exists {
		return ErrAlreadyApplied
	}
	r.entries[key] = app
	return nil
}

// Len reports how many rows the repository currently holds. Test-only
// helper — production code does not call it.
func (r *InMemoryApplications) Len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.entries)
}

// Snapshot returns a copy of the stored rows. Test-only helper —
// production code does not call it. Returned in undefined order; tests
// that care about ordering sort the slice themselves.
func (r *InMemoryApplications) Snapshot() []Application {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Application, 0, len(r.entries))
	for _, app := range r.entries {
		out = append(out, app)
	}
	return out
}
