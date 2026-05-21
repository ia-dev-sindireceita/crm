package lgpd

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// DeletionStatus is the lifecycle of an LGPD article-18 erasure request.
//
// pending   — handler accepted the request, retention_until set in the
//
//	future, worker has not yet finalised.
//
// completed — worker has anonymised the contact and dropped all
//
//	non-fiscal personal data; fiscal rows still live until
//	their own retention window expires.
//
// failed    — worker hit a non-recoverable error while purging; an
//
//	operator must intervene. The row is kept for audit.
type DeletionStatus string

const (
	DeletionStatusPending   DeletionStatus = "pending"
	DeletionStatusCompleted DeletionStatus = "completed"
	DeletionStatusFailed    DeletionStatus = "failed"
)

// Valid reports whether s is in the controlled vocabulary. The CHECK
// constraint on lgpd_deletion_request.status mirrors this set.
func (s DeletionStatus) Valid() bool {
	switch s {
	case DeletionStatusPending, DeletionStatusCompleted, DeletionStatusFailed:
		return true
	}
	return false
}

// DeletionRequest is one row of lgpd_deletion_request. The handler
// builds one on POST /admin/lgpd/delete and persists via
// DeletionRepository.Upsert; the worker reads ready rows via
// DeletionRepository.ListReady and marks them completed via
// MarkCompleted.
type DeletionRequest struct {
	ID                uuid.UUID
	TenantID          uuid.UUID
	ContactID         uuid.UUID
	RequestedByUserID uuid.UUID
	Justification     string
	Status            DeletionStatus
	RetentionUntil    time.Time
	CompletedAt       *time.Time
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

// Validate enforces the invariants the storage layer cannot:
// non-nil tenant/contact, non-empty justification, status in the
// controlled vocabulary, retention_until in the future for pending
// rows.
func (r DeletionRequest) Validate() error {
	if r.TenantID == uuid.Nil {
		return fmt.Errorf("lgpd: tenant_id is required")
	}
	if r.ContactID == uuid.Nil {
		return fmt.Errorf("lgpd: contact_id is required")
	}
	if r.Justification == "" {
		return fmt.Errorf("lgpd: justification is required")
	}
	if !r.Status.Valid() {
		return fmt.Errorf("lgpd: invalid status %q", r.Status)
	}
	if r.RetentionUntil.IsZero() {
		return fmt.Errorf("lgpd: retention_until is required")
	}
	return nil
}

// ErrDeletionRequestNotFound is returned by DeletionRepository.Get when
// no row matches.
var ErrDeletionRequestNotFound = errors.New("lgpd: deletion request not found")

// DeletionRepository is the port the handler and worker depend on.
// Implementations live in internal/adapter/db/postgres.
type DeletionRepository interface {
	// Upsert persists a request. When a pending row already exists for
	// (tenant_id, contact_id) the existing row is updated in place
	// (justification + retention_until + requested_by_user_id refresh);
	// the returned DeletionRequest reflects post-write state. This is
	// what makes the endpoint idempotent (AC #2).
	Upsert(ctx context.Context, req DeletionRequest) (DeletionRequest, error)

	// Get returns the row matching id or ErrDeletionRequestNotFound.
	Get(ctx context.Context, id uuid.UUID) (DeletionRequest, error)

	// ListReady returns pending rows whose retention_until is at or
	// before `at`. The worker uses this to find rows ready to finalise.
	// The slice is bounded by `limit` so a backlog cannot starve a
	// single run.
	ListReady(ctx context.Context, at time.Time, limit int) ([]DeletionRequest, error)

	// MarkCompleted flips status to 'completed' and stamps completed_at.
	// Idempotent: calling twice with the same id and the second call
	// becomes a no-op (already in terminal state).
	MarkCompleted(ctx context.Context, id uuid.UUID, at time.Time) error

	// MarkFailed flips status to 'failed' and stores the error reason
	// on updated_at; the row is kept for audit.
	MarkFailed(ctx context.Context, id uuid.UUID, at time.Time) error
}

// DeletionLister is the small read port the admin UI page depends on
// to render /admin/lgpd/requests (SIN-63191 / Fase 6 PR4). Kept
// separate from DeletionRepository so existing repository consumers
// (the JSON handler and the retention worker) do not have to satisfy
// a new method — the pg adapter implements both interfaces, callers
// pick the narrower one. accept-broad / return-narrow.
type DeletionLister interface {
	// ListByTenant returns rows for `tenant`, optionally filtered by
	// status (pass DeletionStatus("") for "all"). Ordered by created_at
	// DESC so the newest requests surface first in the admin UI list.
	// Bounded by limit.
	ListByTenant(ctx context.Context, tenant uuid.UUID, status DeletionStatus, limit int) ([]DeletionRequest, error)
}

// InRetention is the synthetic UI status surfaced on the
// /admin/lgpd/requests page for rows whose worker has not yet
// finalised them but whose `retention_until` is still in the future.
// It is NOT persisted: the table only carries pending/completed/failed
// per migration 0107. The handler derives this label from
// (Status == pending && RetentionUntil > now).
const InRetention = "in_retention"

// RetentionPolicy decides how long fiscal/billing data must live after
// an erasure request. The default 5 years matches Brazilian fiscal
// legislation (CTN art. 173 + Decreto 9.580/2018) and is configurable
// via the LGPD_FISCAL_RETENTION_YEARS env var on the binary.
//
// A zero RetentionPolicy is invalid; use NewRetentionPolicy.
type RetentionPolicy struct {
	FiscalYears int
}

// DefaultFiscalRetentionYears is the legislated baseline.
const DefaultFiscalRetentionYears = 5

// NewRetentionPolicy validates years (must be > 0) and returns a usable
// policy. years == 0 falls back to DefaultFiscalRetentionYears so the
// handler can hand through an unset env without panicking.
func NewRetentionPolicy(years int) (RetentionPolicy, error) {
	if years < 0 {
		return RetentionPolicy{}, fmt.Errorf("lgpd: fiscal retention years cannot be negative")
	}
	if years == 0 {
		years = DefaultFiscalRetentionYears
	}
	return RetentionPolicy{FiscalYears: years}, nil
}

// RetentionUntil reports the timestamp at which a request created at
// `now` becomes ready for the worker to finalise. Adding years instead
// of a fixed Duration keeps the policy honest across leap years.
func (p RetentionPolicy) RetentionUntil(now time.Time) time.Time {
	return now.AddDate(p.FiscalYears, 0, 0)
}
