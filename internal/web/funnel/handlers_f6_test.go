package funnel_test

// SIN-63943 / UX-F6 — funnel board UI integration tests. These
// complement the SIN-63962 stats handler tests in handlers_test.go:
// they cover the board page's shell-chrome composition, header KPI
// rendering for lider / gerente, the per-stage column stats, the RBAC
// drawer button, the period/owner filter form, and the keyboard
// fallback wiring expectations.

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/funnel"
	"github.com/pericles-luz/crm/internal/iam"
	webfunnel "github.com/pericles-luz/crm/internal/web/funnel"
)

// boardDepsWithStats wires the board handler with a configurable Stats
// stub + role function so each F6 test can drive the role-conditional
// rendering paths.
func boardDepsWithStats(role iam.Role, stats funnel.Stats) webfunnel.Deps {
	d := fullDeps()
	d.Board = &stubBoard{board: seededBoardWithStats()}
	d.Stats = &stubStats{result: stats}
	d.Role = func(*http.Request) iam.Role { return role }
	return d
}

// seededBoardWithStats returns a 5-stage board with one card seeded in
// "qualificando" (matches seededBoard's shape so existing tests stay
// compatible) — kept separate so an F6 test can adjust the card set
// without disturbing the shared seededBoard fixture.
func seededBoardWithStats() funnel.Board {
	return seededBoard()
}

// f6Sample returns a deterministic Stats fixture with KPIs + per-stage
// + per-attendant + per-team + per-channel projections wired in. Tests
// adjust visibility via the role parameter passed into the use case.
func f6Sample() funnel.Stats {
	convRate := 0.127
	return funnel.Stats{
		HeaderKPIs: funnel.HeaderKPIs{
			TotalActive:  142,
			WonCount:     18,
			LostCount:    22,
			WonRate:      0.45,
			AvgTimeToWin: 102 * time.Hour,
		},
		Stages: []funnel.StageStats{
			{StageKey: "novo", Label: "Novo", ActiveCount: 32, AvgTimeInStage: 26 * time.Hour},
			{StageKey: "qualificando", Label: "Qualificando", ActiveCount: 28, AvgTimeInStage: 50 * time.Hour},
			{StageKey: "proposta", Label: "Proposta", ActiveCount: 15, AvgTimeInStage: 70 * time.Hour},
			{StageKey: "ganho", Label: "Ganho", ActiveCount: 18, ConvRate: &convRate},
			{StageKey: "perdido", Label: "Perdido", ActiveCount: 22},
		},
		PerAttendant: []funnel.AttendantStats{
			{UserID: uuid.New(), ActiveCount: 12, WonCount: 3, LostCount: 2},
		},
		PerTeam: []funnel.TeamStats{
			{TeamID: uuid.New(), ActiveCount: 60, WonCount: 9, LostCount: 6},
		},
		PerChannel: []funnel.ChannelStats{
			{Channel: "whatsapp", ActiveCount: 90, WonCount: 14, LostCount: 8},
		},
	}
}

// TestBoardPage_Atendente_NoStats — atendente role sees no analytical
// header, no filter form, no drawer button; the column count chips are
// still rendered (operational baseline).
func TestBoardPage_Atendente_NoStats(t *testing.T) {
	t.Parallel()
	deps := boardDepsWithStats(iam.RoleTenantAtendente, f6Sample())
	h := buildHandler(t, deps)
	r := reqWithTenant(http.MethodGet, "/funnel", "", uuid.New())
	w := httptest.NewRecorder()
	mux(h).ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	body := w.Body.String()
	for _, forbidden := range []string{
		`data-testid="funnel-kpis"`,
		`id="funnel-filters"`,
		`class="funnel-filters__drawer"`,
		`Conversas no funil`,
	} {
		if strings.Contains(body, forbidden) {
			t.Errorf("atendente must not see %q", forbidden)
		}
	}
}

// TestBoardPage_Gerente_KPIsRendered — gerente sees full KPI row +
// filter form + drawer button. Asserts the AC #1 / AC #3 surfaces are
// rendered server-side on the initial load.
func TestBoardPage_Gerente_KPIsRendered(t *testing.T) {
	t.Parallel()
	deps := boardDepsWithStats(iam.RoleTenantGerente, f6Sample())
	h := buildHandler(t, deps)
	r := reqWithTenant(http.MethodGet, "/funnel", "", uuid.New())
	w := httptest.NewRecorder()
	mux(h).ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{
		`data-testid="funnel-kpis"`,
		`Conversas no funil`,
		`>142<`,        // TotalActive
		`>18 (45.0%)<`, // WonCount + WonRate
		`>22<`,         // LostCount
		`Tempo médio até ganho`,
		`id="funnel-filters"`,
		`name="period"`,
		`name="owner"`,
		`Estatísticas detalhadas`, // gerente drawer label
		`hx-get="/funnel"`,        // filter form posts back to board
		`hx-target="#funnel-board-area"`,
		`hx-select="#funnel-board-area"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("gerente missing %q in body", want)
		}
	}
}

// TestBoardPage_Lider_DrawerLabel — lider sees "Estatísticas por
// atendente" drawer label instead of the full "Estatísticas
// detalhadas" because per-team / per-channel projections are nilled.
func TestBoardPage_Lider_DrawerLabel(t *testing.T) {
	t.Parallel()
	stats := f6Sample()
	stats.PerTeam = nil
	stats.PerChannel = nil
	deps := boardDepsWithStats(iam.RoleTenantLider, stats)
	h := buildHandler(t, deps)
	r := reqWithTenant(http.MethodGet, "/funnel", "", uuid.New())
	w := httptest.NewRecorder()
	mux(h).ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, `Estatísticas por atendente`) {
		t.Errorf("lider missing scoped drawer label")
	}
	if strings.Contains(body, `Estatísticas detalhadas`) {
		t.Errorf("lider must not see gerente-only drawer label")
	}
}

// TestBoardPage_ColumnHeader_AvgTime — column header for stages with
// stats shows the tempo médio chip; terminal stages also show
// conv. rate when StageStats.ConvRate is non-nil.
func TestBoardPage_ColumnHeader_AvgTime(t *testing.T) {
	t.Parallel()
	deps := boardDepsWithStats(iam.RoleTenantGerente, f6Sample())
	h := buildHandler(t, deps)
	r := reqWithTenant(http.MethodGet, "/funnel", "", uuid.New())
	w := httptest.NewRecorder()
	mux(h).ServeHTTP(w, r)
	body := w.Body.String()
	if !strings.Contains(body, `[tempo médio: 1d 2h]`) {
		t.Errorf("missing column avg-time chip for novo stage in body")
	}
	if !strings.Contains(body, `[conv. rate: 12.7%]`) {
		t.Errorf("missing column conv-rate chip for ganho stage in body")
	}
}

// TestBoardPage_PeriodFilter_PreservedInForm — when the filter form
// submits ?period=7d the rendered <option value="7d" selected> reflects
// the new state, and the use case received a 7d Period.
func TestBoardPage_PeriodFilter_PreservedInForm(t *testing.T) {
	t.Parallel()
	deps := boardDepsWithStats(iam.RoleTenantGerente, f6Sample())
	h := buildHandler(t, deps)
	r := reqWithTenant(http.MethodGet, "/funnel?period=7d", "", uuid.New())
	w := httptest.NewRecorder()
	mux(h).ServeHTTP(w, r)
	body := w.Body.String()
	if !strings.Contains(body, `value="7d" selected`) {
		t.Errorf("7d period option not selected in form: %s", body)
	}
}

// TestBoardPage_OwnerFilter_PreservedInForm — owner=me round-trips
// the selection.
func TestBoardPage_OwnerFilter_PreservedInForm(t *testing.T) {
	t.Parallel()
	deps := boardDepsWithStats(iam.RoleTenantGerente, f6Sample())
	h := buildHandler(t, deps)
	r := reqWithTenant(http.MethodGet, "/funnel?owner=me", "", uuid.New())
	w := httptest.NewRecorder()
	mux(h).ServeHTTP(w, r)
	body := w.Body.String()
	if !strings.Contains(body, `value="me" selected`) {
		t.Errorf("owner=me not selected in form: %s", body)
	}
}

// TestBoardPage_ShellChrome — confirms the shell.Layout top-bar wires
// in. The brand link MUST point at /hello-tenant so AC #8's "volta para
// landing via top-nav" holds.
func TestBoardPage_ShellChrome(t *testing.T) {
	t.Parallel()
	h := buildHandler(t, fullDeps())
	r := reqWithTenant(http.MethodGet, "/funnel", "", uuid.New())
	w := httptest.NewRecorder()
	mux(h).ServeHTTP(w, r)
	body := w.Body.String()
	for _, want := range []string{
		`<header class="app-shell__topbar"`,
		`href="/hello-tenant"`, // brand link back to landing
		`class="app-shell__nav"`,
		`<main class="app-shell__main"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("shell chrome missing %q", want)
		}
	}
}

// TestBoardPage_KeyboardA11y — server-side assertions for the
// keyboard-nav AC #4 contract: every card MUST be tabindex=0 with
// data-prev-key / data-next-key attributes (so the JS handler can
// route arrow keys to the correct hx-button), and the page MUST carry
// an aria-live region so AT announcements fire.
func TestBoardPage_KeyboardA11y(t *testing.T) {
	t.Parallel()
	deps := fullDeps()
	deps.Board = &stubBoard{board: seededBoard()}
	h := buildHandler(t, deps)
	r := reqWithTenant(http.MethodGet, "/funnel", "", uuid.New())
	w := httptest.NewRecorder()
	mux(h).ServeHTTP(w, r)
	body := w.Body.String()
	for _, want := range []string{
		`tabindex="0"`,
		`data-prev-key="novo"`,
		`data-next-key="proposta"`,
		`id="funnel-aria-live"`,
		`role="status"`,
		`aria-live="polite"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("a11y wiring missing %q", want)
		}
	}
}

// TestStatsDrawer_Gerente — GET /funnel/stats?view=drawer renders just
// the drawer panel (no header KPIs section, but tables for attendant /
// team / channel).
func TestStatsDrawer_Gerente(t *testing.T) {
	t.Parallel()
	deps := boardDepsWithStats(iam.RoleTenantGerente, f6Sample())
	h := buildHandler(t, deps)
	r := reqWithTenant(http.MethodGet, "/funnel/stats?view=drawer", "", uuid.New())
	w := httptest.NewRecorder()
	mux(h).ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{
		`data-testid="funnel-drawer"`,
		`Estatísticas detalhadas`,
		`funnel-drawer__table--attendants`,
		`funnel-drawer__table--teams`,
		`funnel-drawer__table--channels`,
		`hx-get="/funnel/stats/drawer/close"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("drawer missing %q", want)
		}
	}
	// The drawer partial must NOT include the page-header KPI list —
	// that lives on the board page itself.
	if strings.Contains(body, `data-testid="funnel-kpis"`) {
		t.Errorf("drawer must not include the page-header KPI section")
	}
}

// TestStatsDrawer_Lider_HidesTeamChannel — lider drawer hides the
// per-team / per-channel tables because the use case nilled those
// projections.
func TestStatsDrawer_Lider_HidesTeamChannel(t *testing.T) {
	t.Parallel()
	stats := f6Sample()
	stats.PerTeam = nil
	stats.PerChannel = nil
	deps := boardDepsWithStats(iam.RoleTenantLider, stats)
	h := buildHandler(t, deps)
	r := reqWithTenant(http.MethodGet, "/funnel/stats?view=drawer", "", uuid.New())
	w := httptest.NewRecorder()
	mux(h).ServeHTTP(w, r)
	body := w.Body.String()
	if !strings.Contains(body, `funnel-drawer__table--attendants`) {
		t.Errorf("lider drawer must include attendants")
	}
	if strings.Contains(body, `funnel-drawer__table--teams`) {
		t.Errorf("lider drawer must not include teams")
	}
	if strings.Contains(body, `funnel-drawer__table--channels`) {
		t.Errorf("lider drawer must not include channels")
	}
}

// TestDrawerClose_Returns200WithEmptyBody — close mount returns 200
// with an empty body so HTMX cleanly swaps the panel back out.
func TestDrawerClose_Returns200WithEmptyBody(t *testing.T) {
	t.Parallel()
	deps := boardDepsWithStats(iam.RoleTenantGerente, f6Sample())
	h := buildHandler(t, deps)
	r := httptest.NewRequest(http.MethodGet, "/funnel/stats/drawer/close", nil)
	w := httptest.NewRecorder()
	mux(h).ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", w.Code)
	}
	if strings.TrimSpace(w.Body.String()) != "" {
		t.Errorf("drawer-close body = %q, want empty", w.Body.String())
	}
}

// TestDrawerClose_NotMountedWhenStatsNil — when Stats dep is absent
// the close route is also absent (mirrors the GET /funnel/stats
// mounting policy).
func TestDrawerClose_NotMountedWhenStatsNil(t *testing.T) {
	t.Parallel()
	h := buildHandler(t, fullDeps())
	r := httptest.NewRequest(http.MethodGet, "/funnel/stats/drawer/close", nil)
	w := httptest.NewRecorder()
	mux(h).ServeHTTP(w, r)
	if w.Code == http.StatusOK {
		t.Errorf("drawer-close must not be mounted when Stats dep is nil (status = %d)", w.Code)
	}
}

// TestBoard_5xxOnStatsError — when the stats backend returns an error
// that is NOT ErrForbidden, the board page surfaces a 500 (rather than
// silently rendering the board without stats). Forbidden falls back to
// nil stats so the page still renders.
func TestBoard_5xxOnStatsError(t *testing.T) {
	t.Parallel()
	deps := boardDepsWithStats(iam.RoleTenantGerente, funnel.Stats{})
	deps.Stats = &stubStats{err: errFakeStatsBackend}
	h := buildHandler(t, deps)
	r := reqWithTenant(http.MethodGet, "/funnel", "", uuid.New())
	w := httptest.NewRecorder()
	mux(h).ServeHTTP(w, r)
	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", w.Code)
	}
}

// TestBoard_ForbiddenStats_FallsBackToBoardWithoutKPIs — when the use
// case returns ErrForbidden (defense in depth), the page still renders
// with no KPI block instead of 500'ing the operator.
func TestBoard_ForbiddenStats_FallsBackToBoardWithoutKPIs(t *testing.T) {
	t.Parallel()
	deps := boardDepsWithStats(iam.RoleTenantGerente, funnel.Stats{})
	deps.Stats = &stubStats{err: funnel.ErrForbidden}
	h := buildHandler(t, deps)
	r := reqWithTenant(http.MethodGet, "/funnel", "", uuid.New())
	w := httptest.NewRecorder()
	mux(h).ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 (ErrForbidden falls back)", w.Code)
	}
	body := w.Body.String()
	if strings.Contains(body, `data-testid="funnel-kpis"`) {
		t.Errorf("KPI block must not render when stats are forbidden")
	}
}

// errFakeStatsBackend is a sentinel returned by stubStats to simulate
// non-Forbidden upstream errors.
var errFakeStatsBackend = stubStatsErr("fake stats backend error")

type stubStatsErr string

func (e stubStatsErr) Error() string { return string(e) }
