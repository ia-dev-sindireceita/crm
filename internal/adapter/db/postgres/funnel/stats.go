package funnel

// Stats aggregation adapter for funnel.StatsRepository (UX-F6 / SIN-63962).
// Single-CTE query preferred; falls back to two queries when the plan is
// cleaner that way. RLS tenant scope is set via postgres.WithTenant so
// app.tenant_id is always in scope.

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/pericles-luz/crm/internal/adapter/db/postgres"
	domain "github.com/pericles-luz/crm/internal/funnel"
)

// Compile-time assertion: Store satisfies the StatsRepository port.
var _ domain.StatsRepository = (*Store)(nil)

// terminalKeys are the stage keys that represent a closed funnel outcome.
// Defined as constants so the adapter and test fixtures share one source.
const (
	stageKeyGanho   = "ganho"
	stageKeyPerdido = "perdido"
)

// attendantCap is the maximum number of attendant rows returned.
const attendantCap = 50

// Stats implements domain.StatsRepository. It runs two queries inside a
// single WithTenant transaction:
//
//  1. Current-state query — counts active conversations by stage / attendant
//     / channel as of now; computes AvgTimeInStage.
//  2. Period-transition query — counts won/lost transitions in [from, to];
//     computes WonRate and AvgTimeToWin.
//
// Two queries are used because mixing DISTINCT ON (current state) with
// period aggregation in a single CTE produces unnecessarily complex
// query plans on the tenant × conversation_id cardinality we expect.
func (s *Store) Stats(ctx context.Context, tenantID uuid.UUID, q domain.StatsQuery) (domain.StatsAggregates, error) {
	if tenantID == uuid.Nil {
		return domain.StatsAggregates{}, fmt.Errorf("funnel/stats: tenantID is nil")
	}

	var agg domain.StatsAggregates
	err := postgres.WithTenant(ctx, s.pool, tenantID, func(tx pgx.Tx) error {
		now := time.Now().UTC()
		from, to := q.Period.ResolveWindow(now)

		// --- current-state query ---
		if err := queryCurrentState(ctx, tx, q, now, &agg); err != nil {
			return fmt.Errorf("funnel/stats: current state: %w", err)
		}

		// --- period-transition query ---
		if err := queryPeriodTransitions(ctx, tx, q, from, to, &agg); err != nil {
			return fmt.Errorf("funnel/stats: period transitions: %w", err)
		}

		// Compute WonRate.
		denom := agg.HeaderKPIs.WonCount + agg.HeaderKPIs.LostCount
		if denom > 0 {
			agg.HeaderKPIs.WonRate = float64(agg.HeaderKPIs.WonCount) / float64(denom)
		}

		// Compute ConvRate for terminal stages.
		if denom > 0 {
			for i := range agg.Stages {
				key := agg.Stages[i].StageKey
				if key == stageKeyGanho || key == stageKeyPerdido {
					var cnt int64
					if key == stageKeyGanho {
						cnt = agg.HeaderKPIs.WonCount
					} else {
						cnt = agg.HeaderKPIs.LostCount
					}
					rate := float64(cnt) / float64(denom)
					agg.Stages[i].ConvRate = &rate
				}
			}
		}

		return nil
	})
	return agg, err
}

// queryCurrentState populates HeaderKPIs.TotalActive, StageStats.ActiveCount /
// AvgTimeInStage, PerAttendant.ActiveCount, and PerChannel.ActiveCount.
//
// The SQL groups by (stage, attendant, channel) so each non-terminal stage
// can yield multiple rows. AvgTimeInStage is the weighted mean across all
// sub-groups for the stage, computed as
// SUM(seconds-in-stage across rows) / SUM(active counts across rows).
// We return SUM (not AVG) from SQL and divide in Go so the weights are
// correct regardless of how the GROUP BY splits the stage.
func queryCurrentState(
	ctx context.Context,
	tx pgx.Tx,
	q domain.StatsQuery,
	now time.Time,
	agg *domain.StatsAggregates,
) error {
	// Build owner-scope WHERE clause.
	var scopeArgs []any
	scopeWhere := ""
	if q.OwnerScope.Kind == domain.OwnerScopeUser {
		scopeArgs = append(scopeArgs, q.OwnerScope.UserID)
		scopeWhere = fmt.Sprintf("AND c.assigned_user_id = $%d", len(scopeArgs)+1) // +1 for $1=now
	}

	// Parameters: $1=now, $2..=scope args.
	allArgs := append([]any{now}, scopeArgs...)

	const qCurrentState = `
WITH latest_trans AS (
    SELECT DISTINCT ON (t.conversation_id)
        t.conversation_id,
        t.to_stage_id,
        t.transitioned_at
    FROM funnel_transition t
    ORDER BY t.conversation_id, t.transitioned_at DESC
)
SELECT
    s.key       AS stage_key,
    s.label,
    s.position,
    COUNT(*)    AS active_count,
    EXTRACT(EPOCH FROM SUM($1::timestamptz - lt.transitioned_at))::bigint AS sum_seconds,
    c.assigned_user_id,
    c.channel
FROM conversation c
JOIN latest_trans lt ON lt.conversation_id = c.id
JOIN funnel_stage s  ON s.id = lt.to_stage_id
WHERE c.state = 'open'
  AND s.key NOT IN ('ganho', 'perdido')
  %s
GROUP BY s.key, s.label, s.position, c.assigned_user_id, c.channel
ORDER BY s.position, s.key
`
	rows, err := tx.Query(ctx, fmt.Sprintf(qCurrentState, scopeWhere), allArgs...)
	if err != nil {
		return err
	}
	defer rows.Close()

	// Running per-stage accumulator for the weighted mean.
	type stageAcc struct {
		stage      *domain.StageStats
		sumSeconds int64
		count      int64
	}
	stageMap := map[string]*stageAcc{}
	attendantMap := map[uuid.UUID]*domain.AttendantStats{}
	channelMap := map[string]*domain.ChannelStats{}
	var totalActive int64

	for rows.Next() {
		var (
			stageKey    string
			label       string
			position    int
			count       int64
			sumSeconds  int64
			assignedUID *uuid.UUID
			channel     string
		)
		if err := rows.Scan(&stageKey, &label, &position, &count, &sumSeconds, &assignedUID, &channel); err != nil {
			return err
		}
		totalActive += count

		// Stage bucket — accumulate weighted-avg components.
		if st, ok := stageMap[stageKey]; ok {
			st.stage.ActiveCount += count
			st.sumSeconds += sumSeconds
			st.count += count
		} else {
			stageMap[stageKey] = &stageAcc{
				stage: &domain.StageStats{
					StageKey:    stageKey,
					Label:       label,
					Position:    position,
					ActiveCount: count,
				},
				sumSeconds: sumSeconds,
				count:      count,
			}
		}

		// Attendant bucket.
		if assignedUID != nil {
			if att, ok := attendantMap[*assignedUID]; ok {
				att.ActiveCount += count
			} else {
				attendantMap[*assignedUID] = &domain.AttendantStats{
					UserID:      *assignedUID,
					ActiveCount: count,
				}
			}
		}

		// Channel bucket.
		if ch, ok := channelMap[channel]; ok {
			ch.ActiveCount += count
		} else {
			channelMap[channel] = &domain.ChannelStats{
				Channel:     channel,
				ActiveCount: count,
			}
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}

	agg.HeaderKPIs.TotalActive = totalActive
	for _, acc := range stageMap {
		if acc.count > 0 {
			avgSec := acc.sumSeconds / acc.count
			acc.stage.AvgTimeInStage = time.Duration(avgSec) * time.Second
		}
		agg.Stages = append(agg.Stages, *acc.stage)
	}
	for _, att := range attendantMap {
		agg.PerAttendant = append(agg.PerAttendant, *att)
	}
	for _, ch := range channelMap {
		agg.PerChannel = append(agg.PerChannel, *ch)
	}
	return nil
}

// queryPeriodTransitions populates HeaderKPIs.WonCount / LostCount /
// AvgTimeToWin, plus per-attendant / per-channel won/lost counts.
func queryPeriodTransitions(
	ctx context.Context,
	tx pgx.Tx,
	q domain.StatsQuery,
	from, to time.Time,
	agg *domain.StatsAggregates,
) error {
	var scopeArgs []any
	scopeWhere := ""
	if q.OwnerScope.Kind == domain.OwnerScopeUser {
		scopeArgs = append(scopeArgs, q.OwnerScope.UserID)
		// $1=from $2=to $3..=scope
		scopeWhere = fmt.Sprintf("AND c.assigned_user_id = $%d", 2+len(scopeArgs))
	}

	// $1=from, $2=to, then scope args
	allArgs := append([]any{from, to}, scopeArgs...)

	const qPeriod = `
WITH period_terminal AS (
    SELECT
        t.conversation_id,
        s.key       AS stage_key,
        t.transitioned_at,
        c.assigned_user_id,
        c.channel
    FROM funnel_transition t
    JOIN funnel_stage s    ON s.id = t.to_stage_id
    JOIN conversation  c   ON c.id = t.conversation_id
    WHERE t.transitioned_at BETWEEN $1 AND $2
      AND s.key IN ('ganho', 'perdido')
      %s
),
first_trans AS (
    SELECT DISTINCT ON (conversation_id)
        conversation_id,
        transitioned_at AS first_at
    FROM funnel_transition
    ORDER BY conversation_id, transitioned_at ASC
)
SELECT
    pt.stage_key,
    COUNT(*)                                                         AS cnt,
    pt.assigned_user_id,
    pt.channel,
    AVG(CASE WHEN pt.stage_key = 'ganho'
             THEN EXTRACT(EPOCH FROM (pt.transitioned_at - ft.first_at))
             ELSE NULL END)                                          AS avg_ttw_s
FROM period_terminal pt
LEFT JOIN first_trans ft ON ft.conversation_id = pt.conversation_id
GROUP BY pt.stage_key, pt.assigned_user_id, pt.channel
`
	rows, err := tx.Query(ctx, fmt.Sprintf(qPeriod, scopeWhere), allArgs...)
	if err != nil {
		return err
	}
	defer rows.Close()

	var wonCount, lostCount int64
	var totalTTWSeconds float64
	var ttwCount int64

	// helpers to accumulate into existing attendant / channel entries
	attendantMap := map[uuid.UUID]*domain.AttendantStats{}
	for i := range agg.PerAttendant {
		attendantMap[agg.PerAttendant[i].UserID] = &agg.PerAttendant[i]
	}
	channelMap := map[string]*domain.ChannelStats{}
	for i := range agg.PerChannel {
		channelMap[agg.PerChannel[i].Channel] = &agg.PerChannel[i]
	}

	for rows.Next() {
		var (
			stageKey    string
			cnt         int64
			assignedUID *uuid.UUID
			channel     string
			avgTTWs     *float64
		)
		if err := rows.Scan(&stageKey, &cnt, &assignedUID, &channel, &avgTTWs); err != nil {
			return err
		}

		switch stageKey {
		case stageKeyGanho:
			wonCount += cnt
			if avgTTWs != nil {
				totalTTWSeconds += *avgTTWs * float64(cnt)
				ttwCount += cnt
			}
		case stageKeyPerdido:
			lostCount += cnt
		}

		// Merge into attendant map.
		if assignedUID != nil {
			if att, ok := attendantMap[*assignedUID]; ok {
				if stageKey == stageKeyGanho {
					att.WonCount += cnt
				} else {
					att.LostCount += cnt
				}
			} else {
				entry := &domain.AttendantStats{UserID: *assignedUID}
				if stageKey == stageKeyGanho {
					entry.WonCount = cnt
				} else {
					entry.LostCount = cnt
				}
				agg.PerAttendant = append(agg.PerAttendant, *entry)
				attendantMap[*assignedUID] = &agg.PerAttendant[len(agg.PerAttendant)-1]
			}
		}

		// Merge into channel map.
		if ch, ok := channelMap[channel]; ok {
			if stageKey == stageKeyGanho {
				ch.WonCount += cnt
			} else {
				ch.LostCount += cnt
			}
		} else {
			entry := domain.ChannelStats{Channel: channel}
			if stageKey == stageKeyGanho {
				entry.WonCount = cnt
			} else {
				entry.LostCount = cnt
			}
			agg.PerChannel = append(agg.PerChannel, entry)
			channelMap[channel] = &agg.PerChannel[len(agg.PerChannel)-1]
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}

	agg.HeaderKPIs.WonCount = wonCount
	agg.HeaderKPIs.LostCount = lostCount
	if ttwCount > 0 {
		agg.HeaderKPIs.AvgTimeToWin = time.Duration(totalTTWSeconds/float64(ttwCount)) * time.Second
	}

	// Apply attendant cap (50, sorted by ActiveCount DESC, WonCount DESC).
	if len(agg.PerAttendant) > attendantCap {
		// sort in-place
		sortAttendants(agg.PerAttendant)
		agg.PerAttendant = agg.PerAttendant[:attendantCap]
	}

	return nil
}

// sortAttendants sorts AttendantStats by ActiveCount DESC, WonCount DESC.
func sortAttendants(a []domain.AttendantStats) {
	sort.Slice(a, func(i, j int) bool {
		if a[i].ActiveCount != a[j].ActiveCount {
			return a[i].ActiveCount > a[j].ActiveCount
		}
		return a[i].WonCount > a[j].WonCount
	})
}
