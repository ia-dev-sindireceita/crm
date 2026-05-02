package grant

import (
	"context"
	"time"
)

// Repo is the persistence port for grants. Adapters: memory (tests),
// postgres (production). Aggregations are server-computed (the policy
// never scans the table itself) so the adapter can use the cheapest
// query that respects the rolling window.
type Repo interface {
	// Save persists a grant in its current Status. Adapters must reject
	// duplicate IDs.
	Save(ctx context.Context, g Grant) error

	// SubscriptionWindowSum returns the sum of granted+approved tokens
	// for the given subscription since `since` (inclusive). Pending or
	// denied grants are excluded — only realised tokens count toward
	// the cap.
	SubscriptionWindowSum(ctx context.Context, subscriptionID string, since time.Time) (int64, error)

	// MasterWindowSum returns the sum of granted+approved tokens issued
	// by the given master since `since` (inclusive). Same exclusion rule
	// as SubscriptionWindowSum.
	MasterWindowSum(ctx context.Context, masterID string, since time.Time) (int64, error)

	// FindByID fetches a grant. Returns (Grant, true, nil) on hit,
	// (zero, false, nil) on miss, error on transport failure.
	FindByID(ctx context.Context, id string) (Grant, bool, error)

	// UpdateDecision records the ratify-flow outcome on a pending grant:
	// status must be StatusApproved or StatusCancelled. Adapters must
	// reject transitions from non-pending grants.
	UpdateDecision(ctx context.Context, id string, status Status, decidedBy string, decidedAt time.Time) error
}

// AuditEntryKind identifies the audit event type.
type AuditEntryKind string

const (
	AuditGranted        AuditEntryKind = "granted"
	AuditDeniedCap      AuditEntryKind = "denied_cap_exceeded"
	AuditPending        AuditEntryKind = "pending_approval"
	AuditApproved       AuditEntryKind = "approved"
	AuditCancelled      AuditEntryKind = "cancelled"
	AuditAlertEmitted   AuditEntryKind = "alert_emitted"
	AuditValidationFail AuditEntryKind = "validation_failed"
)

// AuditEntry is a structured audit-log record. Adapter is responsible
// for persisting it to the audit_log infrastructure (SIN-62192).
type AuditEntry struct {
	Kind         AuditEntryKind
	OccurredAt   time.Time
	GrantID      string
	Principal    string
	IPAddress    string
	Request      Request
	Decision     Decision
	BreachReason []string
	// Note carries free-text context (e.g. ratify decision rationale).
	Note string
}

// AuditLogger is the audit-log port. Adapters: memory (tests), postgres
// (production audit_log table).
type AuditLogger interface {
	Log(ctx context.Context, entry AuditEntry) error
}

// Alert is the Slack alert payload (AC #5).
type Alert struct {
	MasterID   string
	TenantID   string
	Amount     int64
	Reason     string
	Decision   Status
	BreachOf   []string
	OccurredAt time.Time
}

// AlertNotifier is the Slack adapter port. Failures must not block the
// grant decision — adapters log and return the error, the service swallows.
type AlertNotifier interface {
	Notify(ctx context.Context, alert Alert) error
}

// Clock abstracts time.Now for testability and rolling-window math.
type Clock interface {
	Now() time.Time
}

// IDGenerator produces unique grant IDs. Adapters: ULID/UUID in production,
// deterministic counter in tests.
type IDGenerator interface {
	NewID() string
}

// Principal carries the authenticated caller identity for audit purposes.
// The HTTP adapter populates it from the request context after authn.
type Principal struct {
	// MasterID is the authenticated master's id (used as audit principal).
	MasterID string
	// IPAddress is the client IP — adapter is responsible for correctly
	// resolving it from headers (X-Forwarded-For trust chain).
	IPAddress string
}
