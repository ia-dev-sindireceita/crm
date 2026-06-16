// Package metrics is the read-only aggregation domain that feeds the
// managerial dashboard (SIN-65007, backend half of the Dashboard /
// relatórios epic SIN-64963).
//
// It is a pure read-model: the domain owns the *shape* of the aggregated
// snapshot and the read port (Reader); the concrete aggregation lives in
// the Postgres adapter at internal/adapter/db/postgres/metrics, tenant-
// scoped via postgres.WithTenant so RLS isolates every tenant. No code in
// this package imports a storage driver — Hexagonal by construction.
//
// All counts and percentiles are computed over a time window: the caller
// passes a `since` instant and the adapter restricts every aggregation to
// conversations created at or after it. The use-case layer defaults the
// window to the last 30 days (internal/metrics/usecase).
package metrics

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// StateCount is the number of conversations in one lifecycle state
// (open|closed) within the window. State carries the raw column value so
// the dashboard view layer owns its own labelling.
type StateCount struct {
	State string
	Count int64
}

// ChannelCount is the conversation volume for one carrier channel
// (e.g. "whatsapp") within the window.
type ChannelCount struct {
	Channel string
	Count   int64
}

// StageCount is the number of conversations whose *current* funnel stage
// (the to_stage_id of their latest transition) is this stage, within the
// window. Every tenant stage is reported, including those with a zero
// count, ordered by Position so the dashboard renders the funnel
// left-to-right exactly like the board.
type StageCount struct {
	StageID  uuid.UUID
	Key      string
	Label    string
	Position int
	Count    int64
}

// Percentiles holds the median (p50) and p90 of a duration distribution.
// Both are zero when the underlying sample is empty — the dashboard
// renders "—" rather than a misleading 0s in that case, but the read
// model stays a plain value type.
type Percentiles struct {
	P50 time.Duration
	P90 time.Duration
}

// DashboardMetrics is the full aggregated snapshot the managerial
// dashboard renders. Every slice is ordered deterministically by the
// adapter (state/channel alphabetically, stages by position) so the view
// layer never has to sort.
type DashboardMetrics struct {
	// Since is the inclusive lower bound of the aggregation window,
	// echoed back so the view can label the period it reflects.
	Since time.Time

	// ConversationsByState is the open/closed split within the window.
	ConversationsByState []StateCount

	// VolumeByChannel is conversation volume per carrier within the window.
	VolumeByChannel []ChannelCount

	// FirstResponse is the SLA distribution of time-to-first-response:
	// per conversation, MIN(outbound.created_at) - conversation.created_at,
	// over conversations in the window that received at least one reply.
	FirstResponse Percentiles

	// Resolution is a PROXY for time-to-resolution: for closed
	// conversations, last_message_at - created_at. There is no closed_at
	// column today; adding one to the write-path is a separate follow-up.
	// Treat this as an approximation, not an authoritative SLA.
	Resolution Percentiles

	// FunnelByStage is the current-stage distribution across the window's
	// conversations, one entry per tenant stage ordered by position.
	FunnelByStage []StageCount
}

// Reader is the read port the dashboard use case depends on. The Postgres
// adapter satisfies it; unit tests substitute an in-memory fake.
type Reader interface {
	// Snapshot returns the aggregated dashboard metrics for tenantID over
	// the window [since, now]. tenantID MUST be non-nil; the adapter runs
	// every aggregation inside postgres.WithTenant so RLS isolates the
	// tenant. A zero `since` is the caller's responsibility to resolve —
	// the use case applies the default window before calling here.
	Snapshot(ctx context.Context, tenantID uuid.UUID, since time.Time) (DashboardMetrics, error)
}
