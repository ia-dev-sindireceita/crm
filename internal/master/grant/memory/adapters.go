// Package memory provides in-memory implementations of the grant ports
// (Repo, AuditLogger, AlertNotifier, Clock, IDGenerator). They are used
// for tests and local development. Behaviour matches the production
// Postgres adapter for window-sum aggregation: the rolling-window math
// is identical (granted+approved tokens, rows with CreatedAt or
// DecidedAt strictly inside the window).
package memory

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pericles-luz/crm/internal/master/grant"
)

// Repo is an in-memory grant.Repo implementation.
type Repo struct {
	mu     sync.Mutex
	grants []grant.Grant
}

// NewRepo returns an empty Repo.
func NewRepo() *Repo { return &Repo{} }

// Save appends a grant. Duplicate IDs return an error.
func (r *Repo) Save(_ context.Context, g grant.Grant) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, existing := range r.grants {
		if existing.ID == g.ID {
			return fmt.Errorf("memory: duplicate grant id %q", g.ID)
		}
	}
	r.grants = append(r.grants, g)
	return nil
}

// SubscriptionWindowSum sums granted+approved tokens for the subscription
// since `since` (inclusive).
func (r *Repo) SubscriptionWindowSum(_ context.Context, subscriptionID string, since time.Time) (int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var total int64
	for _, g := range r.grants {
		if g.SubscriptionID != subscriptionID {
			continue
		}
		if !countsTowardCap(g) {
			continue
		}
		if effectiveTime(g).Before(since) {
			continue
		}
		total += g.Amount
	}
	return total, nil
}

// MasterWindowSum sums granted+approved tokens for the master since
// `since` (inclusive).
func (r *Repo) MasterWindowSum(_ context.Context, masterID string, since time.Time) (int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var total int64
	for _, g := range r.grants {
		if g.MasterID != masterID {
			continue
		}
		if !countsTowardCap(g) {
			continue
		}
		if effectiveTime(g).Before(since) {
			continue
		}
		total += g.Amount
	}
	return total, nil
}

// FindByID returns the grant matching id.
func (r *Repo) FindByID(_ context.Context, id string) (grant.Grant, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, g := range r.grants {
		if g.ID == id {
			return g, true, nil
		}
	}
	return grant.Grant{}, false, nil
}

// UpdateDecision applies a ratify-flow outcome to a pending grant.
func (r *Repo) UpdateDecision(_ context.Context, id string, status grant.Status, decidedBy string, decidedAt time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i, g := range r.grants {
		if g.ID != id {
			continue
		}
		if g.Status != grant.StatusPendingApproval {
			return errors.New("memory: grant not pending")
		}
		if status != grant.StatusApproved && status != grant.StatusCancelled {
			return fmt.Errorf("memory: invalid decision status %q", status)
		}
		r.grants[i].Status = status
		r.grants[i].DecidedBy = decidedBy
		r.grants[i].DecidedAt = decidedAt
		return nil
	}
	return errors.New("memory: grant not found")
}

// Snapshot returns a copy of the stored grants for assertions.
func (r *Repo) Snapshot() []grant.Grant {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]grant.Grant, len(r.grants))
	copy(out, r.grants)
	return out
}

func countsTowardCap(g grant.Grant) bool {
	return g.Status == grant.StatusGranted || g.Status == grant.StatusApproved
}

// effectiveTime is the timestamp used for rolling-window inclusion.
// Approved grants count from their decision time; granted grants count
// from their creation time.
func effectiveTime(g grant.Grant) time.Time {
	if g.Status == grant.StatusApproved && !g.DecidedAt.IsZero() {
		return g.DecidedAt
	}
	return g.CreatedAt
}

// AuditLogger is an in-memory grant.AuditLogger.
type AuditLogger struct {
	mu      sync.Mutex
	entries []grant.AuditEntry
}

// NewAuditLogger returns an empty audit logger.
func NewAuditLogger() *AuditLogger { return &AuditLogger{} }

// Log appends the entry.
func (a *AuditLogger) Log(_ context.Context, entry grant.AuditEntry) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.entries = append(a.entries, entry)
	return nil
}

// Entries returns a copy of the audit trail.
func (a *AuditLogger) Entries() []grant.AuditEntry {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]grant.AuditEntry, len(a.entries))
	copy(out, a.entries)
	return out
}

// CountKind returns how many entries match the given kind.
func (a *AuditLogger) CountKind(kind grant.AuditEntryKind) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	var n int
	for _, e := range a.entries {
		if e.Kind == kind {
			n++
		}
	}
	return n
}

// AlertNotifier is an in-memory grant.AlertNotifier. A non-nil failOn
// substitutes a controlled error for tests.
type AlertNotifier struct {
	mu     sync.Mutex
	alerts []grant.Alert
	failOn error
}

// NewAlertNotifier returns an empty notifier.
func NewAlertNotifier() *AlertNotifier { return &AlertNotifier{} }

// SetFailure makes subsequent Notify calls return err. Pass nil to clear.
func (n *AlertNotifier) SetFailure(err error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.failOn = err
}

// Notify records the alert (or returns the configured failure).
func (n *AlertNotifier) Notify(_ context.Context, alert grant.Alert) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.failOn != nil {
		return n.failOn
	}
	n.alerts = append(n.alerts, alert)
	return nil
}

// Alerts returns a copy of the captured alerts.
func (n *AlertNotifier) Alerts() []grant.Alert {
	n.mu.Lock()
	defer n.mu.Unlock()
	out := make([]grant.Alert, len(n.alerts))
	copy(out, n.alerts)
	return out
}

// FixedClock is a controllable grant.Clock for tests.
type FixedClock struct {
	mu  sync.Mutex
	now time.Time
}

// NewFixedClock seeds the clock at start.
func NewFixedClock(start time.Time) *FixedClock { return &FixedClock{now: start} }

// Now returns the current fake time.
func (c *FixedClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

// Advance moves the clock forward by d.
func (c *FixedClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// Set jumps the clock to t.
func (c *FixedClock) Set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = t
}

// SequenceIDs is a deterministic grant.IDGenerator producing g1, g2, ...
type SequenceIDs struct {
	prefix string
	n      atomic.Int64
}

// NewSequenceIDs returns a generator with the given prefix.
func NewSequenceIDs(prefix string) *SequenceIDs {
	return &SequenceIDs{prefix: prefix}
}

// NewID returns the next id in the sequence.
func (g *SequenceIDs) NewID() string {
	v := g.n.Add(1)
	return g.prefix + strconv.FormatInt(v, 10)
}
