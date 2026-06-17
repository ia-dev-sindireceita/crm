// Package usecase holds the read-only dashboard use cases over the
// metrics read-model (SIN-65007). It depends only on the metrics.Reader
// port, never on a storage driver, so it composes with either the
// Postgres adapter or an in-memory fake.
package usecase

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/metrics"
)

// DefaultWindow is the lookback applied when the caller does not pin an
// explicit `since`: the dashboard reflects the last 30 days by default.
const DefaultWindow = 30 * 24 * time.Hour

// ErrNilReader is returned by NewGetDashboard when the reader port is nil.
var ErrNilReader = errors.New("metrics/usecase: reader is nil")

// GetDashboard is the read-side use case that produces the managerial
// dashboard snapshot for a tenant. It resolves the aggregation window
// (defaulting to the last DefaultWindow) and delegates the heavy lifting
// to the metrics.Reader port.
type GetDashboard struct {
	reader metrics.Reader
	now    func() time.Time
}

// NewGetDashboard wires the use case to a reader. A nil reader is a
// programming error and yields ErrNilReader.
func NewGetDashboard(reader metrics.Reader) (*GetDashboard, error) {
	if reader == nil {
		return nil, ErrNilReader
	}
	return &GetDashboard{
		reader: reader,
		now:    func() time.Time { return time.Now().UTC() },
	}, nil
}

// WithClock returns a copy of uc that reads "now" from fn. Tests use it to
// make the default-window resolution deterministic. fn MUST NOT be nil.
func (uc *GetDashboard) WithClock(fn func() time.Time) *GetDashboard {
	cp := *uc
	cp.now = fn
	return &cp
}

// Execute returns the dashboard snapshot for tenantID. When since is the
// zero value the use case applies the default 30-day window relative to
// the use-case clock; otherwise the caller-supplied window is honoured.
// tenantID is validated here so the boundary rejects uuid.Nil before any
// query runs.
func (uc *GetDashboard) Execute(ctx context.Context, tenantID uuid.UUID, since time.Time) (metrics.DashboardMetrics, error) {
	if tenantID == uuid.Nil {
		return metrics.DashboardMetrics{}, errors.New("metrics/usecase: tenant id is nil")
	}
	if since.IsZero() {
		since = uc.now().Add(-DefaultWindow)
	}
	return uc.reader.Snapshot(ctx, tenantID, since)
}
