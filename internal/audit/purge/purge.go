// Package purge holds the LGPD retention sweep over audit_log_data.
// SIN-62424 / ADR 0004 §4: per-tenant retention drives a daily DELETE
// against audit_log_data; audit_log_security is NEVER touched.
//
// Domain code only depends on this package. The postgres-backed
// implementation lives in internal/adapter/db/postgres/audit_purge.go
// and is the only place that actually touches database/sql or pgx.
package purge

import (
	"context"
	"errors"
	"time"
)

// ErrNilStore is returned when New is called with a nil Store.
var ErrNilStore = errors.New("audit/purge: store is nil")

// Result describes the outcome of one Sweeper run. DeletedRows is the
// total number of audit_log_data rows removed across every tenant;
// TenantsSwept is the number of distinct tenants that had at least one
// row deleted. Both are zero on a clean sweep.
type Result struct {
	DeletedRows  int64
	TenantsSwept int
}

// Store is the port a Sweeper drives. PurgeExpired DELETEs every row in
// audit_log_data whose tenant's audit_data_retention_months has elapsed
// relative to `now`. Implementations MUST NEVER touch
// audit_log_security — security/identity events have a separate
// retention policy enforced upstream of any purge job.
//
// PurgeExpired runs cross-tenant: callers do not enumerate tenants and
// must rely on the implementation to scope correctly. Errors are
// propagated up; partial deletes are implementation-defined (the
// postgres adapter wraps the sweep in a single transaction).
type Store interface {
	PurgeExpired(ctx context.Context, now time.Time) (Result, error)
}

// Clock returns the current time. Tests can substitute a deterministic
// clock so retention assertions are not subject to wall-clock drift.
type Clock func() time.Time

// Sweeper is the LGPD retention use case. It is intentionally thin: it
// pins a clock and delegates the deletion to a Store. Operational
// concerns (scheduling, metrics, alerting on drift) live with the
// caller — typically a paperclip routine or a /admin/run-purge HTTP
// handler in cmd/server.
type Sweeper struct {
	store Store
	clock Clock
}

// New returns a Sweeper. A nil clock falls back to time.Now (UTC is the
// caller's responsibility — the postgres column is timestamptz so any
// TZ-aware time works).
func New(store Store, clock Clock) (*Sweeper, error) {
	if store == nil {
		return nil, ErrNilStore
	}
	if clock == nil {
		clock = func() time.Time { return time.Now().UTC() }
	}
	return &Sweeper{store: store, clock: clock}, nil
}

// Sweep runs one purge pass. It is safe to call concurrently with
// other workloads: the postgres adapter holds row-level locks for the
// duration of the DELETE, and audit_log_data is append-only so there
// is no UPDATE traffic to contend with.
func (s *Sweeper) Sweep(ctx context.Context) (Result, error) {
	return s.store.PurgeExpired(ctx, s.clock())
}
