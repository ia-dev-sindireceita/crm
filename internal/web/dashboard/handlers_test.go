package dashboard

import (
	"context"
	"encoding/csv"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/metrics"
	"github.com/pericles-luz/crm/internal/tenancy"
)

// fakeSnapshot is an in-memory SnapshotUseCase. It records the tenant it
// was called with so the tenant-scoping assertion has something to check,
// and returns either a canned snapshot or a forced error.
type fakeSnapshot struct {
	snap    metrics.DashboardMetrics
	err     error
	gotTen  uuid.UUID
	gotZero bool
}

func (f *fakeSnapshot) Execute(_ context.Context, tenantID uuid.UUID, since time.Time) (metrics.DashboardMetrics, error) {
	f.gotTen = tenantID
	f.gotZero = since.IsZero()
	if f.err != nil {
		return metrics.DashboardMetrics{}, f.err
	}
	return f.snap, nil
}

// sampleSnapshot is a representative non-empty read-model snapshot.
func sampleSnapshot() metrics.DashboardMetrics {
	return metrics.DashboardMetrics{
		Since: time.Date(2026, 5, 17, 0, 0, 0, 0, time.UTC),
		ConversationsByState: []metrics.StateCount{
			{State: "open", Count: 12},
			{State: "closed", Count: 30},
		},
		VolumeByChannel: []metrics.ChannelCount{
			{Channel: "whatsapp", Count: 40},
			{Channel: "instagram", Count: 2},
		},
		FirstResponse: metrics.Percentiles{P50: 90 * time.Second, P90: 5 * time.Minute},
		Resolution:    metrics.Percentiles{P50: 2 * time.Hour, P90: 0},
		FunnelByStage: []metrics.StageCount{
			{Key: "novo", Label: "Novo", Position: 1, Count: 5},
			{Key: "ganho", Label: "Ganho", Position: 2, Count: 3},
		},
	}
}

// serve registers the handler routes on a mux and serves one request with
// a tenant injected into the context (mimicking the production middleware).
func serve(t *testing.T, h *Handler, method, target string, tenantID uuid.UUID, withTenant bool) *httptest.ResponseRecorder {
	t.Helper()
	mux := http.NewServeMux()
	h.Routes(mux)
	r := httptest.NewRequest(method, target, nil)
	if withTenant {
		r = r.WithContext(tenancy.WithContext(r.Context(), &tenancy.Tenant{ID: tenantID}))
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, r)
	return rec
}

func mustHandler(t *testing.T, deps Deps) *Handler {
	t.Helper()
	h, err := New(deps)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return h
}

func TestNew_RequiresSnapshot(t *testing.T) {
	t.Parallel()
	if _, err := New(Deps{}); err == nil {
		t.Fatal("New(Deps{}) = nil error, want missing-Snapshot error")
	}
	h, err := New(Deps{Snapshot: &fakeSnapshot{}})
	if err != nil {
		t.Fatalf("New with Snapshot: %v", err)
	}
	if h.deps.Logger == nil {
		t.Fatal("New must default Logger when nil")
	}
}

func TestPage_OK(t *testing.T) {
	t.Parallel()
	fake := &fakeSnapshot{snap: sampleSnapshot()}
	tenantID := uuid.New()
	rec := serve(t, mustHandler(t, Deps{Snapshot: fake}), http.MethodGet, "/dashboard", tenantID, true)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200 (body=%q)", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Fatalf("content-type=%q, want text/html", ct)
	}
	body := rec.Body.String()
	for _, want := range []string{
		`data-testid="dashboard"`,
		"Painel / relatórios",
		"Abertas", "Fechadas",
		"whatsapp", "instagram",
		"Novo", "Ganho",
		"/dashboard/export.csv",
		"proxy", // resolution proxy label is rendered
		"—",     // Resolution.P90 == 0 renders an em dash
	} {
		if !strings.Contains(body, want) {
			t.Errorf("page body missing %q", want)
		}
	}
	if fake.gotTen != tenantID {
		t.Errorf("snapshot called with tenant %v, want %v", fake.gotTen, tenantID)
	}
	if !fake.gotZero {
		t.Error("page should pass zero time.Time so the use case applies the default window")
	}
}

func TestPage_RendersStatusBadges(t *testing.T) {
	t.Parallel()
	fake := &fakeSnapshot{snap: sampleSnapshot()}
	rec := serve(t, mustHandler(t, Deps{Snapshot: fake}), http.MethodGet, "/dashboard", uuid.New(), true)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200", rec.Code)
	}
	body := rec.Body.String()
	// Conversation states render as canonical Peitho StatusBadge pills:
	// open → accent tone, closed → neutral tone.
	for _, want := range []string{
		`status-badge--peitho badge--accent">Abertas`,
		`status-badge--peitho badge--neutral">Fechadas`,
		// Peitho design-system stylesheets are linked on the page.
		`/static/css/components.css`,
		`/static/css/dashboard.css`,
		// Export affordance adopts the Peitho button primitive.
		`class="btn btn--secondary"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("page body missing %q", want)
		}
	}
}

func TestPage_EmptySnapshot_RendersEmptyState(t *testing.T) {
	t.Parallel()
	fake := &fakeSnapshot{snap: metrics.DashboardMetrics{Since: time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)}}
	rec := serve(t, mustHandler(t, Deps{Snapshot: fake}), http.MethodGet, "/dashboard", uuid.New(), true)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"Sem conversas no período.",
		"Sem volume por canal no período.",
		"Sem estágios de funil configurados.",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("empty-state body missing %q", want)
		}
	}
}

func TestPage_MissingTenant_500(t *testing.T) {
	t.Parallel()
	rec := serve(t, mustHandler(t, Deps{Snapshot: &fakeSnapshot{snap: sampleSnapshot()}}), http.MethodGet, "/dashboard", uuid.Nil, false)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d, want 500", rec.Code)
	}
}

func TestPage_SnapshotError_500(t *testing.T) {
	t.Parallel()
	fake := &fakeSnapshot{err: errors.New("boom")}
	rec := serve(t, mustHandler(t, Deps{Snapshot: fake}), http.MethodGet, "/dashboard", uuid.New(), true)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d, want 500 (body=%q)", rec.Code, rec.Body.String())
	}
}

func TestExportCSV_OK(t *testing.T) {
	t.Parallel()
	fake := &fakeSnapshot{snap: sampleSnapshot()}
	rec := serve(t, mustHandler(t, Deps{Snapshot: fake}), http.MethodGet, "/dashboard/export.csv", uuid.New(), true)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200 (body=%q)", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/csv") {
		t.Fatalf("content-type=%q, want text/csv", ct)
	}
	if cd := rec.Header().Get("Content-Disposition"); !strings.Contains(cd, "attachment") || !strings.Contains(cd, "dashboard.csv") {
		t.Fatalf("content-disposition=%q, want attachment+filename", cd)
	}

	// The body must be a well-formed CSV: a header row plus data rows.
	records, err := csv.NewReader(strings.NewReader(rec.Body.String())).ReadAll()
	if err != nil {
		t.Fatalf("csv parse: %v\nbody=%q", err, rec.Body.String())
	}
	if len(records) < 2 {
		t.Fatalf("csv has %d rows, want header + data", len(records))
	}
	if got := records[0]; got[0] != "section" || got[1] != "label" || got[2] != "value" {
		t.Fatalf("header=%v, want [section label value]", got)
	}

	// Spot-check the required channel-volume + counter rows survive the
	// round trip.
	flat := rec.Body.String()
	for _, want := range []string{
		"conversations,Abertas,12",
		"conversations,Fechadas,30",
		"channel,whatsapp,40",
		"channel,instagram,2",
		"first_response,p50_seconds,90",
		"resolution_proxy,p90_seconds,0",
		"funnel,Novo,5",
	} {
		if !strings.Contains(flat, want) {
			t.Errorf("csv missing row %q\nbody=%q", want, flat)
		}
	}
}

func TestExportCSV_MissingTenant_500(t *testing.T) {
	t.Parallel()
	rec := serve(t, mustHandler(t, Deps{Snapshot: &fakeSnapshot{snap: sampleSnapshot()}}), http.MethodGet, "/dashboard/export.csv", uuid.Nil, false)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d, want 500", rec.Code)
	}
}

func TestExportCSV_SnapshotError_500(t *testing.T) {
	t.Parallel()
	fake := &fakeSnapshot{err: errors.New("boom")}
	rec := serve(t, mustHandler(t, Deps{Snapshot: fake}), http.MethodGet, "/dashboard/export.csv", uuid.New(), true)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d, want 500", rec.Code)
	}
}

func TestStateLabel(t *testing.T) {
	t.Parallel()
	cases := map[string]string{"open": "Abertas", "closed": "Fechadas", "weird": "weird"}
	for in, want := range cases {
		if got := stateLabel(in); got != want {
			t.Errorf("stateLabel(%q)=%q, want %q", in, got, want)
		}
	}
}

func TestStateTone(t *testing.T) {
	t.Parallel()
	cases := map[string]string{"open": "accent", "closed": "neutral", "weird": "neutral"}
	for in, want := range cases {
		if got := stateTone(in); got != want {
			t.Errorf("stateTone(%q)=%q, want %q", in, got, want)
		}
	}
}

func TestDurationLabel(t *testing.T) {
	t.Parallel()
	if got := durationLabel(0); got != "—" {
		t.Errorf("durationLabel(0)=%q, want em dash", got)
	}
	if got := durationLabel(-time.Second); got != "—" {
		t.Errorf("durationLabel(negative)=%q, want em dash", got)
	}
	if got := durationLabel(90 * time.Second); got != "1m30s" {
		t.Errorf("durationLabel(90s)=%q, want 1m30s", got)
	}
}

func TestSecondsCell(t *testing.T) {
	t.Parallel()
	if got := secondsCell(0); got != "0" {
		t.Errorf("secondsCell(0)=%q, want 0", got)
	}
	if got := secondsCell(150 * time.Second); got != "150" {
		t.Errorf("secondsCell(150s)=%q, want 150", got)
	}
}

func TestChannelLabel_FallsBackOnEmpty(t *testing.T) {
	t.Parallel()
	fn := templateFuncs["channelLabel"].(func(string) string)
	if got := fn(""); got != "Desconhecido" {
		t.Errorf("channelLabel(empty)=%q, want Desconhecido", got)
	}
	if got := fn("whatsapp"); got != "whatsapp" {
		t.Errorf("channelLabel(whatsapp)=%q, want whatsapp", got)
	}
}
