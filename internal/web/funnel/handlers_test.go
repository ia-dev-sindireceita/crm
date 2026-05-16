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
	wantStable := []string{
		`<title>Funil</title>`,
		`<main class="funnel-shell"`,
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
