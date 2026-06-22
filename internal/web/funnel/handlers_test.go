package funnel_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/funnel"
	"github.com/pericles-luz/crm/internal/iam"
	"github.com/pericles-luz/crm/internal/tenancy"
	webfunnel "github.com/pericles-luz/crm/internal/web/funnel"
)

// stubMover captures MoveConversation args and returns the configured
// error to drive each handler branch.
type stubMover struct {
	mu             sync.Mutex
	called         bool
	tenantID       uuid.UUID
	conversationID uuid.UUID
	toStageKey     string
	actor          uuid.UUID
	reason         string
	err            error
}

func (s *stubMover) MoveConversation(_ context.Context, tenantID, conversationID uuid.UUID, toStageKey string, actor uuid.UUID, reason string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.called = true
	s.tenantID, s.conversationID, s.toStageKey, s.actor, s.reason = tenantID, conversationID, toStageKey, actor, reason
	return s.err
}

// stubBoard returns a fixed board plus an optional error.
type stubBoard struct {
	mu     sync.Mutex
	called int
	board  funnel.Board
	err    error
}

func (s *stubBoard) Board(_ context.Context, _ uuid.UUID) (funnel.Board, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.called++
	return s.board, s.err
}

type stubStage struct {
	mu     sync.Mutex
	stages map[string]*funnel.Stage
	err    error
}

func (s *stubStage) FindByKey(_ context.Context, _ uuid.UUID, key string) (*funnel.Stage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return nil, s.err
	}
	st, ok := s.stages[key]
	if !ok {
		return nil, funnel.ErrNotFound
	}
	return st, nil
}

type stubFunnelHistory struct {
	mu          sync.Mutex
	transitions []*funnel.Transition
	err         error
}

func (s *stubFunnelHistory) ListForConversation(_ context.Context, _, _ uuid.UUID) ([]*funnel.Transition, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.transitions, s.err
}

type stubAssignments struct {
	mu          sync.Mutex
	assignments []webfunnel.AssignmentEntry
	err         error
}

func (s *stubAssignments) ListHistory(_ context.Context, _, _ uuid.UUID) ([]webfunnel.AssignmentEntry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.assignments, s.err
}

// fullDeps returns a Deps struct that constructs cleanly; individual
// tests override fields they care about.
func fullDeps() webfunnel.Deps {
	return webfunnel.Deps{
		Mover:             &stubMover{},
		Board:             &stubBoard{},
		StageResolver:     &stubStage{stages: map[string]*funnel.Stage{}},
		FunnelHistory:     &stubFunnelHistory{},
		AssignmentHistory: &stubAssignments{},
		CSRFToken:         func(*http.Request) string { return "tok" },
		UserID:            func(*http.Request) uuid.UUID { return uuid.New() },
	}
}

// titleASCII upper-cases the first byte; good enough for our test seeds.
func titleASCII(s string) string {
	if s == "" {
		return s
	}
	if s[0] >= 'a' && s[0] <= 'z' {
		return string(s[0]-32) + s[1:]
	}
	return s
}

func reqWithTenant(method, target string, body string, tenantID uuid.UUID) *http.Request {
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, target, nil)
	} else {
		r = httptest.NewRequest(method, target, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	tenant := &tenancy.Tenant{ID: tenantID}
	return r.WithContext(tenancy.WithContext(r.Context(), tenant))
}

func TestNew_RequiresAllDeps(t *testing.T) {
	t.Parallel()
	// Full deps construct cleanly.
	if _, err := webfunnel.New(fullDeps()); err != nil {
		t.Fatalf("New(full): %v", err)
	}
	mutators := map[string]func(*webfunnel.Deps){
		"missing Mover":             func(d *webfunnel.Deps) { d.Mover = nil },
		"missing Board":             func(d *webfunnel.Deps) { d.Board = nil },
		"missing StageResolver":     func(d *webfunnel.Deps) { d.StageResolver = nil },
		"missing FunnelHistory":     func(d *webfunnel.Deps) { d.FunnelHistory = nil },
		"missing AssignmentHistory": func(d *webfunnel.Deps) { d.AssignmentHistory = nil },
		"missing CSRFToken":         func(d *webfunnel.Deps) { d.CSRFToken = nil },
		"missing UserID":            func(d *webfunnel.Deps) { d.UserID = nil },
	}
	for name, mut := range mutators {
		name, mut := name, mut
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			deps := fullDeps()
			mut(&deps)
			if _, err := webfunnel.New(deps); err == nil {
				t.Errorf("New(%s) err = nil, want error", name)
			}
		})
	}
}

func buildHandler(t *testing.T, deps webfunnel.Deps) *webfunnel.Handler {
	t.Helper()
	h, err := webfunnel.New(deps)
	if err != nil {
		t.Fatalf("webfunnel.New: %v", err)
	}
	return h
}

func mux(h *webfunnel.Handler) *http.ServeMux {
	m := http.NewServeMux()
	h.Routes(m)
	return m
}

func seededBoard() funnel.Board {
	stageIDs := map[string]uuid.UUID{
		"novo":         uuid.New(),
		"qualificando": uuid.New(),
		"proposta":     uuid.New(),
		"ganho":        uuid.New(),
		"perdido":      uuid.New(),
	}
	keys := []string{"novo", "qualificando", "proposta", "ganho", "perdido"}
	board := funnel.Board{}
	for i, k := range keys {
		board.Columns = append(board.Columns, funnel.BoardColumn{
			Stage: funnel.Stage{
				ID:       stageIDs[k],
				Key:      k,
				Label:    titleASCII(k),
				Position: i + 1,
			},
		})
	}
	// Seed a card in "qualificando".
	convID := uuid.New()
	board.Columns[1].Cards = []funnel.ConversationCard{
		{
			ConversationID: convID,
			ContactID:      uuid.New(),
			DisplayName:    "Alice",
			Channel:        "whatsapp",
			LastMessageAt:  time.Date(2026, 5, 16, 12, 0, 0, 0, time.UTC),
		},
	}
	return board
}

func TestBoard_RendersFiveColumnsWithCSRF(t *testing.T) {
	t.Parallel()
	deps := fullDeps()
	board := seededBoard()
	deps.Board = &stubBoard{board: board}
	h := buildHandler(t, deps)
	tenantID := uuid.New()
	r := reqWithTenant(http.MethodGet, "/funnel", "", tenantID)
	w := httptest.NewRecorder()
	mux(h).ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	for _, want := range []string{
		"funnel-board",
		`data-stage-key="novo"`,
		`data-stage-key="qualificando"`,
		`data-stage-key="proposta"`,
		`data-stage-key="ganho"`,
		`data-stage-key="perdido"`,
		"Alice",
		"whatsapp",
		`href="/static/css/funnel.css"`,
		`src="/static/js/funnel-board.js"`,
		"hx-post=\"/funnel/transitions\"",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("board body missing %q", want)
		}
	}
}

func TestBoard_5xxOnBoardError(t *testing.T) {
	t.Parallel()
	deps := fullDeps()
	deps.Board = &stubBoard{err: errors.New("boom")}
	h := buildHandler(t, deps)
	r := reqWithTenant(http.MethodGet, "/funnel", "", uuid.New())
	w := httptest.NewRecorder()
	mux(h).ServeHTTP(w, r)
	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", w.Code)
	}
}

func TestBoard_5xxOnMissingCSRF(t *testing.T) {
	t.Parallel()
	deps := fullDeps()
	deps.CSRFToken = func(*http.Request) string { return "" }
	h := buildHandler(t, deps)
	r := reqWithTenant(http.MethodGet, "/funnel", "", uuid.New())
	w := httptest.NewRecorder()
	mux(h).ServeHTTP(w, r)
	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", w.Code)
	}
}

func TestBoard_5xxOnMissingTenant(t *testing.T) {
	t.Parallel()
	h := buildHandler(t, fullDeps())
	r := httptest.NewRequest(http.MethodGet, "/funnel", nil) // no tenant in ctx
	w := httptest.NewRecorder()
	mux(h).ServeHTTP(w, r)
	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", w.Code)
	}
}

func TestTransition_CallsMoverAndRendersCard(t *testing.T) {
	t.Parallel()
	deps := fullDeps()
	board := seededBoard()
	tenantID := uuid.New()
	actorID := uuid.New()
	convID := board.Columns[1].Cards[0].ConversationID
	deps.Board = &stubBoard{board: board}
	mover := &stubMover{}
	deps.Mover = mover
	deps.UserID = func(*http.Request) uuid.UUID { return actorID }
	h := buildHandler(t, deps)

	body := "conversation_id=" + convID.String() + "&to_stage_key=qualificando&reason=triagem"
	r := reqWithTenant(http.MethodPost, "/funnel/transitions", body, tenantID)
	w := httptest.NewRecorder()
	mux(h).ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	if !mover.called {
		t.Fatal("MoveConversation not called")
	}
	if mover.toStageKey != "qualificando" {
		t.Errorf("toStageKey = %q, want %q", mover.toStageKey, "qualificando")
	}
	if mover.conversationID != convID {
		t.Errorf("conversationID = %v, want %v", mover.conversationID, convID)
	}
	if mover.actor != actorID {
		t.Errorf("actor = %v, want %v", mover.actor, actorID)
	}
	if mover.reason != "triagem" {
		t.Errorf("reason = %q, want %q", mover.reason, "triagem")
	}
	// The response is a card partial — must include the card's id and
	// keyboard buttons (prev = novo, next = proposta in this seed).
	out := w.Body.String()
	if !strings.Contains(out, "card-"+convID.String()) {
		t.Errorf("missing card id in response: %s", out)
	}
	if !strings.Contains(out, "← novo") {
		t.Errorf("missing prev key in response: %s", out)
	}
	if !strings.Contains(out, "proposta →") {
		t.Errorf("missing next key in response: %s", out)
	}
}

func TestTransition_ErrorBranches(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		body       string
		userID     uuid.UUID
		moverErr   error
		wantStatus int
	}{
		{"invalid form / bad uuid", "conversation_id=not-uuid&to_stage_key=novo", uuid.New(), nil, http.StatusBadRequest},
		{"missing stage key", "conversation_id=" + uuid.NewString() + "&to_stage_key=", uuid.New(), nil, http.StatusBadRequest},
		{"zero user → unauthorized", "conversation_id=" + uuid.NewString() + "&to_stage_key=novo", uuid.Nil, nil, http.StatusUnauthorized},
		{"stage not found", "conversation_id=" + uuid.NewString() + "&to_stage_key=imaginary", uuid.New(), funnel.ErrStageNotFound, http.StatusNotFound},
		{"invalid stage key from service", "conversation_id=" + uuid.NewString() + "&to_stage_key=novo", uuid.New(), funnel.ErrInvalidStageKey, http.StatusBadRequest},
		{"upstream 500", "conversation_id=" + uuid.NewString() + "&to_stage_key=novo", uuid.New(), errors.New("boom"), http.StatusInternalServerError},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			deps := fullDeps()
			deps.UserID = func(*http.Request) uuid.UUID { return c.userID }
			deps.Mover = &stubMover{err: c.moverErr}
			h := buildHandler(t, deps)
			r := reqWithTenant(http.MethodPost, "/funnel/transitions", c.body, uuid.New())
			w := httptest.NewRecorder()
			mux(h).ServeHTTP(w, r)
			if w.Code != c.wantStatus {
				t.Errorf("status = %d, want %d (body=%q)", w.Code, c.wantStatus, w.Body.String())
			}
		})
	}
}

func TestTransition_NoContentWhenCardDropsOffBoard(t *testing.T) {
	t.Parallel()
	deps := fullDeps()
	// Mover succeeds, but the re-fetched board does not contain the card
	// (simulates a conversation that was closed between move and read).
	deps.Board = &stubBoard{board: funnel.Board{Columns: []funnel.BoardColumn{{Stage: funnel.Stage{Key: "novo", Position: 1}}}}}
	h := buildHandler(t, deps)
	body := "conversation_id=" + uuid.NewString() + "&to_stage_key=novo"
	r := reqWithTenant(http.MethodPost, "/funnel/transitions", body, uuid.New())
	w := httptest.NewRecorder()
	mux(h).ServeHTTP(w, r)
	if w.Code != http.StatusNoContent {
		t.Errorf("status = %d, want 204", w.Code)
	}
}

func TestTransition_5xxOnBoardReReadError(t *testing.T) {
	t.Parallel()
	deps := fullDeps()
	stub := &stubBoard{err: errors.New("read failed")}
	deps.Board = stub
	h := buildHandler(t, deps)
	body := "conversation_id=" + uuid.NewString() + "&to_stage_key=novo"
	r := reqWithTenant(http.MethodPost, "/funnel/transitions", body, uuid.New())
	w := httptest.NewRecorder()
	mux(h).ServeHTTP(w, r)
	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", w.Code)
	}
}

func TestHistory_RendersMergedTimeline(t *testing.T) {
	t.Parallel()
	tenantID := uuid.New()
	conversationID := uuid.New()
	novoID := uuid.New()
	ganhoID := uuid.New()
	user := uuid.New()

	transitions := []*funnel.Transition{
		{
			ID:                   uuid.New(),
			TenantID:             tenantID,
			ConversationID:       conversationID,
			ToStageID:            novoID,
			TransitionedByUserID: user,
			TransitionedAt:       time.Date(2026, 5, 16, 10, 0, 0, 0, time.UTC),
			Reason:               "intake",
		},
		{
			ID:                   uuid.New(),
			TenantID:             tenantID,
			ConversationID:       conversationID,
			FromStageID:          &novoID,
			ToStageID:            ganhoID,
			TransitionedByUserID: user,
			TransitionedAt:       time.Date(2026, 5, 16, 12, 0, 0, 0, time.UTC),
		},
	}
	assignments := []webfunnel.AssignmentEntry{
		{
			AssignedAt: time.Date(2026, 5, 16, 11, 0, 0, 0, time.UTC),
			UserID:     user,
			Reason:     "lead",
		},
	}
	deps := fullDeps()
	deps.FunnelHistory = &stubFunnelHistory{transitions: transitions}
	deps.AssignmentHistory = &stubAssignments{assignments: assignments}
	deps.StageResolver = &stubStage{stages: map[string]*funnel.Stage{
		"novo":  {ID: novoID, Key: "novo", Label: "Novo"},
		"ganho": {ID: ganhoID, Key: "ganho", Label: "Ganho"},
	}}
	h := buildHandler(t, deps)
	r := reqWithTenant(http.MethodGet, "/funnel/conversations/"+conversationID.String()+"/history", "", tenantID)
	w := httptest.NewRecorder()
	mux(h).ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	for _, want := range []string{
		"funnel-modal__panel",
		"Entrou no estágio Novo",
		"Movida de Novo para Ganho",
		"Líder atribuído",
		"intake",
		`datetime="2026-05-16T10:00:00Z"`,
		`datetime="2026-05-16T12:00:00Z"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("history body missing %q", want)
		}
	}
	// Ordering: novo (10:00) before assignment (11:00) before ganho (12:00).
	iNovo := strings.Index(body, "Entrou no estágio Novo")
	iAssign := strings.Index(body, "Líder atribuído")
	iGanho := strings.Index(body, "Movida de Novo para Ganho")
	if !(iNovo < iAssign && iAssign < iGanho) {
		t.Errorf("timeline not chronological: novo=%d, assign=%d, ganho=%d", iNovo, iAssign, iGanho)
	}
}

func TestHistory_EmptyTimelineRendersFallback(t *testing.T) {
	t.Parallel()
	deps := fullDeps()
	h := buildHandler(t, deps)
	r := reqWithTenant(http.MethodGet, "/funnel/conversations/"+uuid.NewString()+"/history", "", uuid.New())
	w := httptest.NewRecorder()
	mux(h).ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "Sem histórico.") {
		t.Errorf("missing empty-state copy: %s", w.Body.String())
	}
}

func TestHistory_4xxOnBadID(t *testing.T) {
	t.Parallel()
	h := buildHandler(t, fullDeps())
	r := reqWithTenant(http.MethodGet, "/funnel/conversations/not-uuid/history", "", uuid.New())
	w := httptest.NewRecorder()
	mux(h).ServeHTTP(w, r)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

func TestHistory_5xxOnFunnelError(t *testing.T) {
	t.Parallel()
	deps := fullDeps()
	deps.FunnelHistory = &stubFunnelHistory{err: errors.New("fail")}
	h := buildHandler(t, deps)
	r := reqWithTenant(http.MethodGet, "/funnel/conversations/"+uuid.NewString()+"/history", "", uuid.New())
	w := httptest.NewRecorder()
	mux(h).ServeHTTP(w, r)
	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", w.Code)
	}
}

func TestHistory_5xxOnAssignmentError(t *testing.T) {
	t.Parallel()
	deps := fullDeps()
	deps.AssignmentHistory = &stubAssignments{err: errors.New("fail")}
	h := buildHandler(t, deps)
	r := reqWithTenant(http.MethodGet, "/funnel/conversations/"+uuid.NewString()+"/history", "", uuid.New())
	w := httptest.NewRecorder()
	mux(h).ServeHTTP(w, r)
	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", w.Code)
	}
}

func TestHistory_5xxOnMissingTenant(t *testing.T) {
	t.Parallel()
	h := buildHandler(t, fullDeps())
	r := httptest.NewRequest(http.MethodGet, "/funnel/conversations/"+uuid.NewString()+"/history", nil)
	w := httptest.NewRecorder()
	mux(h).ServeHTTP(w, r)
	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", w.Code)
	}
}

func TestTransition_5xxOnMissingTenant(t *testing.T) {
	t.Parallel()
	h := buildHandler(t, fullDeps())
	body := "conversation_id=" + uuid.NewString() + "&to_stage_key=novo"
	r := httptest.NewRequest(http.MethodPost, "/funnel/transitions", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	mux(h).ServeHTTP(w, r)
	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", w.Code)
	}
}

func TestModalClose_Returns200WithEmptyBody(t *testing.T) {
	t.Parallel()
	h := buildHandler(t, fullDeps())
	r := httptest.NewRequest(http.MethodGet, "/funnel/modal/close", nil)
	w := httptest.NewRecorder()
	mux(h).ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", w.Code)
	}
	if got := strings.TrimSpace(w.Body.String()); got != "" {
		t.Errorf("body = %q, want empty", got)
	}
}

// TestBoardSnapshot is a coarse-grained snapshot check: it pins a few
// stable substrings of the rendered board so a careless template
// refactor lights up a test rather than a code review surprise.
func TestBoardSnapshot(t *testing.T) {
	t.Parallel()
	deps := fullDeps()
	deps.Board = &stubBoard{board: seededBoard()}
	h := buildHandler(t, deps)
	r := reqWithTenant(http.MethodGet, "/funnel", "", uuid.New())
	w := httptest.NewRecorder()
	mux(h).ServeHTTP(w, r)
	body := w.Body.String()
	// SIN-63943 / AC #8 — board page now composes via shell.Layout,
	// which owns the page-level <main class="app-shell__main"> chrome
	// (CTO ACK comment 87f0e262). The legacy <main class="funnel-shell">
	// wrapper was nested inside the new shell-owned main and demoted to
	// a <section>; the added sidebar pin asserts the shell chrome is
	// actually wired so future careless refactors that drop the
	// shell.MustParse composition still light up the snapshot.
	// (SIN-65092: top-bar → SidebarNav.)
	wantStable := []string{
		`<title>Funil</title>`,
		`<main class="app-shell__main"`,
		`class="app-shell__sidebar"`,
		`<section class="funnel-columns"`,
		`aria-label="Estágios do funil"`,
		`hx-target="#funnel-modal"`,
		`hx-swap="outerHTML"`,
		`aria-label="Histórico"`,
	}
	for _, s := range wantStable {
		if !strings.Contains(body, s) {
			t.Errorf("snapshot missing %q\n--- body ---\n%s", s, body)
		}
	}
}

// --- Stats handler tests ---

type stubStats struct {
	result funnel.Stats
	err    error
}

func (s *stubStats) GetStats(_ context.Context, _ uuid.UUID, _ funnel.StatsQuery) (funnel.Stats, error) {
	return s.result, s.err
}

func depsWithStats(role iam.Role) webfunnel.Deps {
	d := fullDeps()
	d.Stats = &stubStats{
		result: funnel.Stats{
			HeaderKPIs: funnel.HeaderKPIs{TotalActive: 7, WonCount: 3, LostCount: 1, WonRate: 0.75},
			Stages: []funnel.StageStats{
				{StageKey: "novo", Label: "Novo", ActiveCount: 4},
				{StageKey: "ganho", Label: "Ganho", ActiveCount: 0},
			},
			PerAttendant: []funnel.AttendantStats{{UserID: uuid.New(), ActiveCount: 3, WonCount: 2}},
			PerTeam:      []funnel.TeamStats{{TeamID: uuid.New(), ActiveCount: 7}},
			PerChannel:   []funnel.ChannelStats{{Channel: "whatsapp", ActiveCount: 5}},
		},
	}
	d.Role = func(*http.Request) iam.Role { return role }
	return d
}

func TestStats_Atendente_Gets403(t *testing.T) {
	t.Parallel()
	deps := depsWithStats(iam.RoleTenantAtendente)
	h := buildHandler(t, deps)
	r := reqWithTenant(http.MethodGet, "/funnel/stats", "", uuid.New())
	w := httptest.NewRecorder()
	mux(h).ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Errorf("atendente: status = %d, want 403", w.Code)
	}
}

func TestStats_Gerente_Gets200WithAllTables(t *testing.T) {
	t.Parallel()
	deps := depsWithStats(iam.RoleTenantGerente)
	h := buildHandler(t, deps)
	r := reqWithTenant(http.MethodGet, "/funnel/stats", "", uuid.New())
	w := httptest.NewRecorder()
	mux(h).ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Errorf("gerente: status = %d, want 200", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{
		"funnel-stats__stages",
		"funnel-stats__attendants",
		"funnel-stats__teams",
		"funnel-stats__channels",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("gerente: response missing %q", want)
		}
	}
}

func TestStats_Lider_Gets200AndMissingTeamChannel(t *testing.T) {
	t.Parallel()
	deps := depsWithStats(iam.RoleTenantLider)
	// For lider, service returns nil PerTeam/PerChannel — simulate that.
	deps.Stats = &stubStats{
		result: funnel.Stats{
			HeaderKPIs:   funnel.HeaderKPIs{TotalActive: 2, WonCount: 1},
			Stages:       []funnel.StageStats{{StageKey: "novo", Label: "Novo", ActiveCount: 2}},
			PerAttendant: []funnel.AttendantStats{{UserID: uuid.New(), ActiveCount: 2, WonCount: 1}},
			PerTeam:      nil, // lider sees nil
			PerChannel:   nil,
		},
	}
	h := buildHandler(t, deps)
	r := reqWithTenant(http.MethodGet, "/funnel/stats", "", uuid.New())
	w := httptest.NewRecorder()
	mux(h).ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Errorf("lider: status = %d, want 200", w.Code)
	}
	body := w.Body.String()
	if strings.Contains(body, "funnel-stats__teams") {
		t.Error("lider: teams table must not appear")
	}
	if strings.Contains(body, "funnel-stats__channels") {
		t.Error("lider: channels table must not appear")
	}
	if !strings.Contains(body, "funnel-stats__attendants") {
		t.Error("lider: attendants table must appear")
	}
}

func TestStats_RepoError_Returns500(t *testing.T) {
	t.Parallel()
	deps := depsWithStats(iam.RoleTenantGerente)
	deps.Stats = &stubStats{err: errors.New("db gone")}
	h := buildHandler(t, deps)
	r := reqWithTenant(http.MethodGet, "/funnel/stats", "", uuid.New())
	w := httptest.NewRecorder()
	mux(h).ServeHTTP(w, r)
	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", w.Code)
	}
}

func TestStats_WithPeriod7d(t *testing.T) {
	t.Parallel()
	deps := depsWithStats(iam.RoleTenantGerente)
	h := buildHandler(t, deps)
	r := reqWithTenant(http.MethodGet, "/funnel/stats?period=7d", "", uuid.New())
	w := httptest.NewRecorder()
	mux(h).ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", w.Code)
	}
}

func TestStats_WithPeriod90d(t *testing.T) {
	t.Parallel()
	deps := depsWithStats(iam.RoleTenantGerente)
	h := buildHandler(t, deps)
	r := reqWithTenant(http.MethodGet, "/funnel/stats?period=90d", "", uuid.New())
	w := httptest.NewRecorder()
	mux(h).ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", w.Code)
	}
}

func TestStats_WithCustomPeriod(t *testing.T) {
	t.Parallel()
	deps := depsWithStats(iam.RoleTenantGerente)
	h := buildHandler(t, deps)
	r := reqWithTenant(http.MethodGet,
		"/funnel/stats?period=custom&from=2026-05-01T00:00:00Z&to=2026-05-31T23:59:59Z",
		"", uuid.New())
	w := httptest.NewRecorder()
	mux(h).ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", w.Code)
	}
}

func TestStats_InvalidCustomPeriod_DefaultsTo30d(t *testing.T) {
	t.Parallel()
	deps := depsWithStats(iam.RoleTenantGerente)
	h := buildHandler(t, deps)
	r := reqWithTenant(http.MethodGet,
		"/funnel/stats?period=custom&from=bad&to=alsobad",
		"", uuid.New())
	w := httptest.NewRecorder()
	mux(h).ServeHTTP(w, r)
	// Bad custom period falls back to 30d — still 200
	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", w.Code)
	}
}

func TestStats_ForbiddenError_Returns403(t *testing.T) {
	t.Parallel()
	deps := depsWithStats(iam.RoleTenantGerente)
	deps.Stats = &stubStats{err: funnel.ErrForbidden}
	h := buildHandler(t, deps)
	r := reqWithTenant(http.MethodGet, "/funnel/stats", "", uuid.New())
	w := httptest.NewRecorder()
	mux(h).ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Errorf("ErrForbidden: status = %d, want 403", w.Code)
	}
}

func TestStats_NotMountedWhenStatsNil(t *testing.T) {
	t.Parallel()
	deps := fullDeps() // Stats is nil
	h := buildHandler(t, deps)
	r := reqWithTenant(http.MethodGet, "/funnel/stats", "", uuid.New())
	w := httptest.NewRecorder()
	mux(h).ServeHTTP(w, r)
	// Route not mounted — go's default mux returns 404 or 405.
	if w.Code == http.StatusOK {
		t.Error("stats route must not be mounted when Stats dep is nil")
	}
}

// fakeUserDir is an in-memory userlabel.Directory for the top-bar label
// assertions (SIN-65578): labels maps user id → label; absent ids resolve
// to no label so Resolve falls back to "Conta".
type fakeUserDir struct {
	labels map[uuid.UUID]string
}

func (f fakeUserDir) LabelsByID(_ context.Context, _ uuid.UUID, ids []uuid.UUID) (map[uuid.UUID]string, error) {
	out := make(map[uuid.UUID]string, len(ids))
	for _, id := range ids {
		if l, ok := f.labels[id]; ok {
			out[id] = l
		}
	}
	return out, nil
}

// TestBoard_RendersResolvedUserLabel pins the SIN-65578 fix: the top-bar
// account button renders the logged-in user's resolved label (email
// local-part), not the uuid prefix the old displayNameForUser stub
// produced.
func TestBoard_RendersResolvedUserLabel(t *testing.T) {
	t.Parallel()
	userID := uuid.New()
	deps := fullDeps()
	deps.Board = &stubBoard{board: seededBoard()}
	deps.UserID = func(*http.Request) uuid.UUID { return userID }
	deps.UserLabels = fakeUserDir{labels: map[uuid.UUID]string{userID: "agent"}}

	h := buildHandler(t, deps)
	r := reqWithTenant(http.MethodGet, "/funnel", "", uuid.New())
	w := httptest.NewRecorder()
	mux(h).ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "agent") {
		t.Errorf("board body missing resolved label %q", "agent")
	}
	// The raw uuid prefix must never leak into the shell.
	if strings.Contains(body, userID.String()[:8]) {
		t.Errorf("board body leaked uuid prefix %q (stub regression)", userID.String()[:8])
	}
}

// TestBoard_FallsBackToContaWithoutDirectory pins that an unwired
// directory degrades the top bar to "Conta" rather than the uuid prefix.
func TestBoard_FallsBackToContaWithoutDirectory(t *testing.T) {
	t.Parallel()
	userID := uuid.New()
	deps := fullDeps()
	deps.Board = &stubBoard{board: seededBoard()}
	deps.UserID = func(*http.Request) uuid.UUID { return userID }
	deps.UserLabels = nil // unwired

	h := buildHandler(t, deps)
	r := reqWithTenant(http.MethodGet, "/funnel", "", uuid.New())
	w := httptest.NewRecorder()
	mux(h).ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if strings.Contains(body, userID.String()[:8]) {
		t.Errorf("board body leaked uuid prefix %q with nil directory", userID.String()[:8])
	}
	if !strings.Contains(body, "Conta") {
		t.Errorf("board body missing %q fallback with nil directory", "Conta")
	}
}
