// Package metrics is the pgx-backed adapter for the metrics.Reader port
// (SIN-65007). It computes the managerial dashboard's aggregated read-
// model in a single tenant-scoped transaction so every aggregation sees a
// consistent snapshot and RLS isolates the tenant.
//
// The package lives under internal/adapter/db/postgres/ so the
// forbidimport / notenant analyzers allow it to import pgx and call pgx
// methods directly. Every query routes through the sibling
// postgres.WithTenant helper, which sets the app.tenant_id GUC the RLS
// policies on conversation / message / funnel_* gate on.
//
// Tables read (all read-only, no write-path mutation):
//   - conversation (migration 0088): state, channel, created_at,
//     last_message_at.
//   - message      (migration 0088): direction, created_at — outbound
//     timestamps drive time-to-first-response.
//   - funnel_stage + funnel_transition (migration 0093): current-stage
//     distribution via DISTINCT ON (conversation_id) latest transition.
package metrics

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pericles-luz/crm/internal/adapter/db/postgres"
	domain "github.com/pericles-luz/crm/internal/metrics"
)

// Compile-time assertion: Store satisfies the read port. If the port
// grows or shrinks the build fails here before any caller notices.
var _ domain.Reader = (*Store)(nil)

// Store is the pgx-backed metrics adapter. Construct via New(pool); the
// pool MUST be the app_runtime pool so the RLS policies apply.
type Store struct {
	pool postgres.TxBeginner
}

// New wraps pool and returns a ready-to-use Store. A nil pool yields
// postgres.ErrNilPool.
func New(pool *pgxpool.Pool) (*Store, error) {
	if pool == nil {
		return nil, postgres.ErrNilPool
	}
	return &Store{pool: pool}, nil
}

// Snapshot computes the full DashboardMetrics for tenantID over the window
// [since, now]. All five aggregations run inside one WithTenant
// transaction so they reflect the same instant and share the RLS scope.
// The aggregations run as an ordered list of named steps so a failure in
// any one is wrapped with a stable label and aborts the snapshot.
func (s *Store) Snapshot(ctx context.Context, tenantID uuid.UUID, since time.Time) (domain.DashboardMetrics, error) {
	if tenantID == uuid.Nil {
		return domain.DashboardMetrics{}, fmt.Errorf("metrics/postgres: Snapshot: tenant id is nil")
	}
	out := domain.DashboardMetrics{Since: since}
	steps := []struct {
		name string
		run  func(pgx.Tx) error
	}{
		{"conversations by state", func(tx pgx.Tx) error {
			v, err := conversationsByState(ctx, tx, since)
			out.ConversationsByState = v
			return err
		}},
		{"volume by channel", func(tx pgx.Tx) error {
			v, err := volumeByChannel(ctx, tx, since)
			out.VolumeByChannel = v
			return err
		}},
		{"first response", func(tx pgx.Tx) error {
			v, err := firstResponse(ctx, tx, since)
			out.FirstResponse = v
			return err
		}},
		{"resolution", func(tx pgx.Tx) error {
			v, err := resolution(ctx, tx, since)
			out.Resolution = v
			return err
		}},
		{"funnel by stage", func(tx pgx.Tx) error {
			v, err := funnelByStage(ctx, tx, since)
			out.FunnelByStage = v
			return err
		}},
	}
	err := postgres.WithTenant(ctx, s.pool, tenantID, func(tx pgx.Tx) error {
		for _, step := range steps {
			if err := step.run(tx); err != nil {
				return fmt.Errorf("%s: %w", step.name, err)
			}
		}
		return nil
	})
	if err != nil {
		return domain.DashboardMetrics{}, fmt.Errorf("metrics/postgres: Snapshot: %w", err)
	}
	return out, nil
}

// queryRows runs query (bound to a single `since` parameter) and projects
// every row through scan into a slice. It centralises the query/scan/Err
// error handling shared by the count and distribution aggregations.
func queryRows[T any](ctx context.Context, tx pgx.Tx, query string, since time.Time, scan func(pgx.Rows) (T, error)) ([]T, error) {
	rows, err := tx.Query(ctx, query, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []T
	for rows.Next() {
		v, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// conversationsByState returns the open/closed split for conversations
// created in the window, ordered by state so the result is deterministic.
func conversationsByState(ctx context.Context, tx pgx.Tx, since time.Time) ([]domain.StateCount, error) {
	return queryRows(ctx, tx, `
		SELECT state, COUNT(*)
		  FROM conversation
		 WHERE created_at >= $1
		 GROUP BY state
		 ORDER BY state
	`, since, func(rows pgx.Rows) (domain.StateCount, error) {
		var c domain.StateCount
		err := rows.Scan(&c.State, &c.Count)
		return c, err
	})
}

// volumeByChannel returns conversation volume per carrier channel for
// conversations created in the window, ordered by channel.
func volumeByChannel(ctx context.Context, tx pgx.Tx, since time.Time) ([]domain.ChannelCount, error) {
	return queryRows(ctx, tx, `
		SELECT channel, COUNT(*)
		  FROM conversation
		 WHERE created_at >= $1
		 GROUP BY channel
		 ORDER BY channel
	`, since, func(rows pgx.Rows) (domain.ChannelCount, error) {
		var c domain.ChannelCount
		err := rows.Scan(&c.Channel, &c.Count)
		return c, err
	})
}

// firstResponse computes the p50/p90 time-to-first-response over
// conversations in the window that received at least one outbound message.
// The per-conversation sample is MIN(out.created_at) - conversation.created_at,
// expressed in seconds so percentile_cont operates on double precision
// rather than interval. COALESCE collapses the empty-sample NULL to 0.
func firstResponse(ctx context.Context, tx pgx.Tx, since time.Time) (domain.Percentiles, error) {
	return percentilesOf(ctx, tx, `
		WITH frt AS (
			SELECT EXTRACT(EPOCH FROM (MIN(m.created_at) - c.created_at))::double precision AS secs
			  FROM conversation c
			  JOIN message m
			    ON m.conversation_id = c.id
			   AND m.direction = 'out'
			 WHERE c.created_at >= $1
			 GROUP BY c.id, c.created_at
		)
		SELECT
			COALESCE(percentile_cont(0.5) WITHIN GROUP (ORDER BY secs), 0),
			COALESCE(percentile_cont(0.9) WITHIN GROUP (ORDER BY secs), 0)
		  FROM frt
	`, since)
}

// resolution computes the p50/p90 of a PROXY for time-to-resolution.
// There is no closed_at column today, so for closed conversations in the
// window we approximate the resolution span as last_message_at -
// created_at. This is a documented approximation, not an authoritative
// SLA; adding a real closed_at to the write-path is a separate follow-up.
func resolution(ctx context.Context, tx pgx.Tx, since time.Time) (domain.Percentiles, error) {
	return percentilesOf(ctx, tx, `
		WITH res AS (
			SELECT EXTRACT(EPOCH FROM (last_message_at - created_at))::double precision AS secs
			  FROM conversation
			 WHERE state = 'closed'
			   AND created_at >= $1
			   AND last_message_at IS NOT NULL
		)
		SELECT
			COALESCE(percentile_cont(0.5) WITHIN GROUP (ORDER BY secs), 0),
			COALESCE(percentile_cont(0.9) WITHIN GROUP (ORDER BY secs), 0)
		  FROM res
	`, since)
}

// percentilesOf runs a query that selects exactly two double-precision
// seconds values (p50, p90) and converts them to durations.
func percentilesOf(ctx context.Context, tx pgx.Tx, query string, since time.Time) (domain.Percentiles, error) {
	var p50, p90 float64
	if err := tx.QueryRow(ctx, query, since).Scan(&p50, &p90); err != nil {
		return domain.Percentiles{}, err
	}
	return domain.Percentiles{
		P50: secondsToDuration(p50),
		P90: secondsToDuration(p90),
	}, nil
}

// secondsToDuration converts fractional seconds to a time.Duration,
// rounding to the nearest nanosecond.
func secondsToDuration(secs float64) time.Duration {
	return time.Duration(secs * float64(time.Second))
}

// funnelByStage returns the current-stage distribution over the window's
// conversations. The current stage is the to_stage_id of each
// conversation's latest transition (DISTINCT ON ... ORDER BY
// transitioned_at DESC, the same pattern as funnel/board.go). Every
// tenant stage is reported via LEFT JOIN, including zero-count stages, so
// the dashboard renders the full funnel ordered by position.
func funnelByStage(ctx context.Context, tx pgx.Tx, since time.Time) ([]domain.StageCount, error) {
	return queryRows(ctx, tx, `
		WITH latest AS (
			SELECT DISTINCT ON (t.conversation_id)
			       t.conversation_id, t.to_stage_id
			  FROM funnel_transition t
			  JOIN conversation c
			    ON c.id = t.conversation_id
			   AND c.created_at >= $1
			 ORDER BY t.conversation_id, t.transitioned_at DESC
		)
		SELECT s.id, s.key, s.label, s.position, COUNT(l.conversation_id)
		  FROM funnel_stage s
		  LEFT JOIN latest l ON l.to_stage_id = s.id
		 GROUP BY s.id, s.key, s.label, s.position
		 ORDER BY s.position ASC
	`, since, func(rows pgx.Rows) (domain.StageCount, error) {
		var sc domain.StageCount
		err := rows.Scan(&sc.StageID, &sc.Key, &sc.Label, &sc.Position, &sc.Count)
		return sc, err
	})
}
