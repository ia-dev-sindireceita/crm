package funnel

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/iam"
)

// PeriodKind identifies the time-window preset.
type PeriodKind int

const (
	PeriodLast7d  PeriodKind = iota + 1
	PeriodLast30d            // default
	PeriodLast90d
	PeriodCustom
)

// Period is the time window for aggregation.
type Period struct {
	Kind PeriodKind
	From time.Time // set when Kind == PeriodCustom
	To   time.Time // set when Kind == PeriodCustom
}

// ResolveWindow returns the absolute [from, to] pair for the given now.
func (p Period) ResolveWindow(now time.Time) (from, to time.Time) {
	switch p.Kind {
	case PeriodLast7d:
		return now.AddDate(0, 0, -7), now
	case PeriodLast90d:
		return now.AddDate(0, 0, -90), now
	case PeriodCustom:
		return p.From, p.To
	default: // PeriodLast30d or zero
		return now.AddDate(0, 0, -30), now
	}
}

// OwnerScopeKind identifies how to filter by owner.
type OwnerScopeKind int

const (
	OwnerScopeAll  OwnerScopeKind = iota // no filter — all conversations
	OwnerScopeTeam                       // filter by team_id
	OwnerScopeUser                       // filter by assigned_user_id
)

// OwnerScope narrows which conversations are included.
type OwnerScope struct {
	Kind   OwnerScopeKind
	TeamID uuid.UUID
	UserID uuid.UUID
}

// StatsQuery is the full query-parameter bundle for GetStats.
// ViewerRole and ViewerID are used by the use case for RBAC clamping;
// the repository receives a clamped copy and never makes role decisions.
type StatsQuery struct {
	Period       Period
	OwnerScope   OwnerScope
	ViewerRole   iam.Role
	ViewerID     uuid.UUID
	ViewerTeamID uuid.UUID // set when viewer is lider; uuid.Nil until teams table exists
}

// StatsAggregates is the full projection set returned by StatsRepository.
// It contains all projections regardless of the viewer's role; the use
// case nils out the projections the role cannot see before returning Stats.
type StatsAggregates struct {
	HeaderKPIs   HeaderKPIs
	Stages       []StageStats
	PerAttendant []AttendantStats // capped at 50, sorted ActiveCount DESC, WonCount DESC
	PerTeam      []TeamStats
	PerChannel   []ChannelStats
}

// Stats is the use-case output shape after role-based projection nilling.
type Stats struct {
	HeaderKPIs   HeaderKPIs
	Stages       []StageStats
	PerAttendant []AttendantStats
	PerTeam      []TeamStats    // nil for lider
	PerChannel   []ChannelStats // nil for lider
}

// HeaderKPIs are the top-of-board summary numbers.
type HeaderKPIs struct {
	TotalActive  int64
	WonCount     int64
	LostCount    int64
	WonRate      float64       // WonCount/(WonCount+LostCount); zero when denominator is zero
	AvgTimeToWin time.Duration // mean of (ganho_at − first_transition_at) over period ganho events
}

// StageStats is per-column aggregate.
type StageStats struct {
	StageKey       string
	Label          string
	Position       int
	ActiveCount    int64
	AvgTimeInStage time.Duration // mean of (now − latest_transition_at) over currently-active cards
	ConvRate       *float64      // non-nil only for "ganho" and "perdido"
}

// AttendantStats is per assigned user.
type AttendantStats struct {
	UserID      uuid.UUID
	ActiveCount int64
	WonCount    int64
	LostCount   int64
}

// TeamStats is per team (reserved; empty until teams table lands).
type TeamStats struct {
	TeamID      uuid.UUID
	ActiveCount int64
	WonCount    int64
	LostCount   int64
}

// ChannelStats is per conversation.channel.
type ChannelStats struct {
	Channel     string
	ActiveCount int64
	WonCount    int64
	LostCount   int64
}

// ErrForbidden is returned when the viewer's role does not permit
// access to funnel statistics (defense-in-depth re-check in use case).
var ErrForbidden = errors.New("funnel: forbidden")

// StatsService aggregates funnel statistics (UX-F6 / SIN-63962).
// It is separate from funnel.Service (which owns mutations) because the
// two have different dependency surfaces.
type StatsService struct {
	repo StatsRepository
	now  func() time.Time
}

// StatsConfig bundles StatsService dependencies.
type StatsConfig struct {
	Repo StatsRepository
	Now  func() time.Time // defaults to time.Now().UTC() when nil
}

// NewStatsService validates cfg and returns a ready StatsService.
func NewStatsService(cfg StatsConfig) (*StatsService, error) {
	if cfg.Repo == nil {
		return nil, errors.New("funnel: StatsRepository is required")
	}
	now := cfg.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	return &StatsService{repo: cfg.Repo, now: now}, nil
}

// GetStats returns funnel statistics for tenantID, scoped to the viewer's role.
//
// RBAC rules (SIN-63961 CTO sign-off §3):
//   - atendente → ErrForbidden (defense in depth; handler gates first).
//   - lider     → OwnerScope clamped to their team (or user when team is unset);
//     PerTeam and PerChannel nilled in result.
//   - gerente   → no clamp; full projection set returned.
func (s *StatsService) GetStats(ctx context.Context, tenantID uuid.UUID, q StatsQuery) (Stats, error) {
	if tenantID == uuid.Nil {
		return Stats{}, ErrInvalidTenant
	}

	// Defense-in-depth role gate.
	if q.ViewerRole == iam.RoleTenantAtendente {
		return Stats{}, ErrForbidden
	}

	// Clamp OwnerScope for lider regardless of what the caller requested.
	clamped := q
	if q.ViewerRole == iam.RoleTenantLider {
		if q.ViewerTeamID != uuid.Nil {
			clamped.OwnerScope = OwnerScope{Kind: OwnerScopeTeam, TeamID: q.ViewerTeamID}
		} else {
			// Teams not yet provisioned — fall back to user scope.
			clamped.OwnerScope = OwnerScope{Kind: OwnerScopeUser, UserID: q.ViewerID}
		}
	}

	agg, err := s.repo.Stats(ctx, tenantID, clamped)
	if err != nil {
		return Stats{}, err
	}

	result := Stats(agg)

	// Lider cannot see cross-team aggregates.
	if q.ViewerRole == iam.RoleTenantLider {
		result.PerTeam = nil
		result.PerChannel = nil
	}

	return result, nil
}
