package funnel_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/funnel"
	"github.com/pericles-luz/crm/internal/iam"
)

// fakeStatsRepo is an in-memory StatsRepository for unit testing.
type fakeStatsRepo struct {
	result funnel.StatsAggregates
	err    error
	// capture the query passed in for assertion
	lastQuery funnel.StatsQuery
}

func (f *fakeStatsRepo) Stats(_ context.Context, _ uuid.UUID, q funnel.StatsQuery) (funnel.StatsAggregates, error) {
	f.lastQuery = q
	return f.result, f.err
}

var validAgg = funnel.StatsAggregates{
	HeaderKPIs: funnel.HeaderKPIs{TotalActive: 5, WonCount: 3, LostCount: 1, WonRate: 0.75},
	Stages: []funnel.StageStats{
		{StageKey: "novo", Label: "Novo", ActiveCount: 2},
		{StageKey: "ganho", Label: "Ganho", ActiveCount: 1},
	},
	PerAttendant: []funnel.AttendantStats{{UserID: uuid.New(), ActiveCount: 2, WonCount: 1}},
	PerTeam:      []funnel.TeamStats{{TeamID: uuid.New(), ActiveCount: 5}},
	PerChannel:   []funnel.ChannelStats{{Channel: "whatsapp", ActiveCount: 3}},
}

func mustNewStatsService(t *testing.T, repo funnel.StatsRepository) *funnel.StatsService {
	t.Helper()
	svc, err := funnel.NewStatsService(funnel.StatsConfig{Repo: repo})
	if err != nil {
		t.Fatalf("NewStatsService: %v", err)
	}
	return svc
}

// --- constructor tests ---

func TestNewStatsService_NilRepo(t *testing.T) {
	t.Parallel()
	_, err := funnel.NewStatsService(funnel.StatsConfig{})
	if err == nil {
		t.Fatal("expected error for nil repo")
	}
}

func TestNewStatsService_DefaultClock(t *testing.T) {
	t.Parallel()
	repo := &fakeStatsRepo{result: validAgg}
	svc, err := funnel.NewStatsService(funnel.StatsConfig{Repo: repo})
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if svc == nil {
		t.Fatal("expected non-nil service")
	}
}

// --- RBAC: atendente forbidden ---

func TestGetStats_AtendenteIsForbidden(t *testing.T) {
	t.Parallel()
	repo := &fakeStatsRepo{result: validAgg}
	svc := mustNewStatsService(t, repo)

	_, err := svc.GetStats(context.Background(), uuid.New(), funnel.StatsQuery{
		ViewerRole: iam.RoleTenantAtendente,
		ViewerID:   uuid.New(),
	})
	if !errors.Is(err, funnel.ErrForbidden) {
		t.Errorf("expected ErrForbidden, got %v", err)
	}
}

// --- RBAC: gerente passthrough ---

func TestGetStats_GerentePassthrough(t *testing.T) {
	t.Parallel()
	repo := &fakeStatsRepo{result: validAgg}
	svc := mustNewStatsService(t, repo)

	stats, err := svc.GetStats(context.Background(), uuid.New(), funnel.StatsQuery{
		ViewerRole: iam.RoleTenantGerente,
		ViewerID:   uuid.New(),
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if stats.PerTeam == nil {
		t.Error("gerente: expected PerTeam non-nil")
	}
	if stats.PerChannel == nil {
		t.Error("gerente: expected PerChannel non-nil")
	}
}

func TestGetStats_GerenteDoesNotClampOwnerScope(t *testing.T) {
	t.Parallel()
	userID := uuid.New()
	repo := &fakeStatsRepo{result: validAgg}
	svc := mustNewStatsService(t, repo)

	requested := funnel.OwnerScope{Kind: funnel.OwnerScopeAll}
	_, err := svc.GetStats(context.Background(), uuid.New(), funnel.StatsQuery{
		ViewerRole: iam.RoleTenantGerente,
		ViewerID:   userID,
		OwnerScope: requested,
	})
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if repo.lastQuery.OwnerScope.Kind != funnel.OwnerScopeAll {
		t.Errorf("gerente: OwnerScope should not be clamped; got %v", repo.lastQuery.OwnerScope)
	}
}

// --- RBAC: lider clamp ---

func TestGetStats_LiderClampsToTeam(t *testing.T) {
	t.Parallel()
	teamID := uuid.New()
	repo := &fakeStatsRepo{result: validAgg}
	svc := mustNewStatsService(t, repo)

	_, err := svc.GetStats(context.Background(), uuid.New(), funnel.StatsQuery{
		ViewerRole:   iam.RoleTenantLider,
		ViewerID:     uuid.New(),
		ViewerTeamID: teamID,
	})
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	q := repo.lastQuery
	if q.OwnerScope.Kind != funnel.OwnerScopeTeam {
		t.Errorf("lider with team: expected OwnerScopeTeam, got %v", q.OwnerScope.Kind)
	}
	if q.OwnerScope.TeamID != teamID {
		t.Errorf("lider: TeamID = %v, want %v", q.OwnerScope.TeamID, teamID)
	}
}

func TestGetStats_LiderClampsToUserWhenNoTeam(t *testing.T) {
	t.Parallel()
	viewerID := uuid.New()
	repo := &fakeStatsRepo{result: validAgg}
	svc := mustNewStatsService(t, repo)

	_, err := svc.GetStats(context.Background(), uuid.New(), funnel.StatsQuery{
		ViewerRole:   iam.RoleTenantLider,
		ViewerID:     viewerID,
		ViewerTeamID: uuid.Nil,
	})
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	q := repo.lastQuery
	if q.OwnerScope.Kind != funnel.OwnerScopeUser {
		t.Errorf("lider without team: expected OwnerScopeUser, got %v", q.OwnerScope.Kind)
	}
	if q.OwnerScope.UserID != viewerID {
		t.Errorf("lider: UserID = %v, want %v", q.OwnerScope.UserID, viewerID)
	}
}

func TestGetStats_LiderNilsPerTeamAndPerChannel(t *testing.T) {
	t.Parallel()
	repo := &fakeStatsRepo{result: validAgg}
	svc := mustNewStatsService(t, repo)

	stats, err := svc.GetStats(context.Background(), uuid.New(), funnel.StatsQuery{
		ViewerRole: iam.RoleTenantLider,
		ViewerID:   uuid.New(),
	})
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if stats.PerTeam != nil {
		t.Error("lider: PerTeam must be nil")
	}
	if stats.PerChannel != nil {
		t.Error("lider: PerChannel must be nil")
	}
	if stats.PerAttendant == nil {
		t.Error("lider: PerAttendant must not be nil")
	}
}

// --- invalid tenant ---

func TestGetStats_NilTenantReturnsError(t *testing.T) {
	t.Parallel()
	repo := &fakeStatsRepo{result: validAgg}
	svc := mustNewStatsService(t, repo)

	_, err := svc.GetStats(context.Background(), uuid.Nil, funnel.StatsQuery{
		ViewerRole: iam.RoleTenantGerente,
	})
	if !errors.Is(err, funnel.ErrInvalidTenant) {
		t.Errorf("expected ErrInvalidTenant, got %v", err)
	}
}

// --- repo error propagation ---

func TestGetStats_RepoErrorPropagated(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("db down")
	repo := &fakeStatsRepo{err: sentinel}
	svc := mustNewStatsService(t, repo)

	_, err := svc.GetStats(context.Background(), uuid.New(), funnel.StatsQuery{
		ViewerRole: iam.RoleTenantGerente,
	})
	if !errors.Is(err, sentinel) {
		t.Errorf("expected sentinel error, got %v", err)
	}
}

// --- period window resolution ---

func TestPeriod_ResolveWindow_Last7d(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	p := funnel.Period{Kind: funnel.PeriodLast7d}
	from, to := p.ResolveWindow(now)
	if !to.Equal(now) {
		t.Errorf("to = %v, want %v", to, now)
	}
	want := now.AddDate(0, 0, -7)
	if !from.Equal(want) {
		t.Errorf("from = %v, want %v", from, want)
	}
}

func TestPeriod_ResolveWindow_Last30d(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	p := funnel.Period{Kind: funnel.PeriodLast30d}
	from, to := p.ResolveWindow(now)
	if !to.Equal(now) {
		t.Errorf("to = %v, want %v", to, now)
	}
	want := now.AddDate(0, 0, -30)
	if !from.Equal(want) {
		t.Errorf("from = %v, want %v", from, want)
	}
}

func TestPeriod_ResolveWindow_Last90d(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	p := funnel.Period{Kind: funnel.PeriodLast90d}
	from, to := p.ResolveWindow(now)
	want := now.AddDate(0, 0, -90)
	if !from.Equal(want) {
		t.Errorf("from = %v, want %v", from, want)
	}
	if !to.Equal(now) {
		t.Errorf("to = %v, want %v", to, now)
	}
}

func TestPeriod_ResolveWindow_Custom(t *testing.T) {
	t.Parallel()
	customFrom := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	customTo := time.Date(2026, 5, 15, 0, 0, 0, 0, time.UTC)
	p := funnel.Period{Kind: funnel.PeriodCustom, From: customFrom, To: customTo}
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	from, to := p.ResolveWindow(now)
	if !from.Equal(customFrom) {
		t.Errorf("from = %v, want %v", from, customFrom)
	}
	if !to.Equal(customTo) {
		t.Errorf("to = %v, want %v", to, customTo)
	}
}

func TestPeriod_ResolveWindow_ZeroKindDefaultsTo30d(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	p := funnel.Period{} // zero Kind
	from, _ := p.ResolveWindow(now)
	want := now.AddDate(0, 0, -30)
	if !from.Equal(want) {
		t.Errorf("zero kind: from = %v, want %v", from, want)
	}
}
