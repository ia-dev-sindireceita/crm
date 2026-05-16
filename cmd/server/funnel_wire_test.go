package main

// SIN-62862 — funnel wire tests. The web/funnel handler covers its own
// behaviour exhaustively in internal/web/funnel; these tests pin the
// composition root: that buildWebFunnelHandler returns nil when the DSN
// is unset, that assembleWebFunnelHandler rejects nil ports, that the
// inboxAssignmentHistory adapter remaps inbox.Assignment rows into
// webfunnel.AssignmentEntry verbatim, and that the assembled stdlib mux
// renders the 5-column board shell on GET /funnel against in-memory
// funnel + inbox stubs.

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/adapter/httpapi/middleware"
	"github.com/pericles-luz/crm/internal/funnel"
	"github.com/pericles-luz/crm/internal/iam"
	"github.com/pericles-luz/crm/internal/inbox"
	"github.com/pericles-luz/crm/internal/tenancy"
)

// defaultStageKeys mirrors the migration-0093 seed order; the wire
// assembles a board with those five stages so the rendered shell shows
// all columns.
var funnelTestStageKeys = []string{"novo", "qualificando", "proposta", "ganho", "perdido"}

func TestBuildWebFunnelHandler_DisabledWhenDSNUnset(t *testing.T) {
	t.Parallel()
	h, cleanup := buildWebFunnelHandler(context.Background(), func(string) string { return "" })
	defer cleanup()
	if h != nil {
		t.Fatalf("expected nil handler when DATABASE_URL unset, got %T", h)
	}
}

func TestAssembleWebFunnelHandler_RejectsNilDeps(t *testing.T) {
	t.Parallel()
	store := newFakeFunnelStore(uuid.New())
	asgs := &fakeAssignmentHistory{}
	cases := map[string]struct {
		stages      funnel.StageRepository
		transitions funnel.TransitionRepository
		board       funnel.BoardReader
		assignments assignmentHistoryReader
	}{
		"nil stages":      {nil, store, store, asgs},
		"nil transitions": {store, nil, store, asgs},
		"nil board":       {store, store, nil, asgs},
		"nil assignments": {store, store, store, nil},
	}
	for name, c := range cases {
		name, c := name, c
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, err := assembleWebFunnelHandler(c.stages, c.transitions, c.board, c.assignments, nil); err == nil {
				t.Fatalf("expected error on %s, got nil", name)
			}
		})
	}
}

func TestAssembleWebFunnelHandler_MountsAllFourRoutes(t *testing.T) {
	t.Parallel()
	tenantID := uuid.New()
	conversationID := uuid.New()

	store := newFakeFunnelStore(tenantID)
	asgs := &fakeAssignmentHistory{}

	h, err := assembleWebFunnelHandler(store, store, store, asgs, nil)
	if err != nil {
		t.Fatalf("assemble: %v", err)
	}
	if h == nil {
		t.Fatalf("expected non-nil handler")
	}

	cases := []struct {
		name   string
		method string
		path   string
		want   int
	}{
		{"board renders", http.MethodGet, "/funnel", http.StatusOK},
		{"history modal", http.MethodGet, "/funnel/conversations/" + conversationID.String() + "/history", http.StatusOK},
		{"modal close", http.MethodGet, "/funnel/modal/close", http.StatusOK},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			rec := httptest.NewRecorder()
			req := withFunnelCtx(httptest.NewRequest(c.method, c.path, nil), tenantID)
			h.ServeHTTP(rec, req)
			if rec.Code != c.want {
				t.Fatalf("%s status=%d, want %d (body=%q)", c.path, rec.Code, c.want, rec.Body.String())
			}
		})
	}
}

func TestAssembleWebFunnelHandler_BoardRendersFiveColumnShell(t *testing.T) {
	t.Parallel()
	tenantID := uuid.New()
	store := newFakeFunnelStore(tenantID)
	asgs := &fakeAssignmentHistory{}

	h, err := assembleWebFunnelHandler(store, store, store, asgs, nil)
	if err != nil {
		t.Fatalf("assemble: %v", err)
	}
	rec := httptest.NewRecorder()
	req := withFunnelCtx(httptest.NewRequest(http.MethodGet, "/funnel", nil), tenantID)
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200 (body=%q)", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, key := range funnelTestStageKeys {
		if !strings.Contains(body, key) {
			t.Errorf("rendered board missing stage key %q", key)
		}
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Fatalf("content-type=%q, want text/html", ct)
	}
}

func TestAssembleWebFunnelHandler_TransitionMovesCardAndRendersFragment(t *testing.T) {
	t.Parallel()
	tenantID := uuid.New()
	conversationID := uuid.New()
	store := newFakeFunnelStore(tenantID)
	store.seedCard(conversationID, "novo", "Alice")
	asgs := &fakeAssignmentHistory{}

	h, err := assembleWebFunnelHandler(store, store, store, asgs, nil)
	if err != nil {
		t.Fatalf("assemble: %v", err)
	}

	body := strings.NewReader("conversation_id=" + conversationID.String() + "&to_stage_key=qualificando")
	req := httptest.NewRequest(http.MethodPost, "/funnel/transitions", body)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req = withFunnelCtx(req, tenantID)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200 (body=%q)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "qualificando") {
		t.Fatalf("response body missing destination stage key: %q", rec.Body.String())
	}
}

func TestInboxAssignmentHistory_MapsRowsVerbatim(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 16, 9, 0, 0, 0, time.UTC)
	userA, userB := uuid.New(), uuid.New()
	port := &fakeAssignmentPort{rows: []*inbox.Assignment{
		{
			ID:             uuid.New(),
			TenantID:       uuid.New(),
			ConversationID: uuid.New(),
			UserID:         userA,
			AssignedAt:     now,
			Reason:         inbox.LeadReason("first_inbound"),
		},
		nil, // exercises the nil-row skip path
		{
			ID:             uuid.New(),
			TenantID:       uuid.New(),
			ConversationID: uuid.New(),
			UserID:         userB,
			AssignedAt:     now.Add(time.Hour),
			Reason:         inbox.LeadReason("manual_reassign"),
		},
	}}
	adapter := inboxAssignmentHistory{port: port}

	out, err := adapter.ListHistory(context.Background(), uuid.New(), uuid.New())
	if err != nil {
		t.Fatalf("ListHistory: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("entries=%d, want 2", len(out))
	}
	if out[0].UserID != userA || out[0].AssignedAt != now || out[0].Reason != "first_inbound" {
		t.Fatalf("row 0 = %+v", out[0])
	}
	if out[1].UserID != userB || out[1].Reason != "manual_reassign" {
		t.Fatalf("row 1 = %+v", out[1])
	}
}

func TestInboxAssignmentHistory_PropagatesError(t *testing.T) {
	t.Parallel()
	wantErr := errSentinel("boom")
	adapter := inboxAssignmentHistory{port: &fakeAssignmentPort{err: wantErr}}
	if _, err := adapter.ListHistory(context.Background(), uuid.New(), uuid.New()); err != wantErr {
		t.Fatalf("err=%v, want %v", err, wantErr)
	}
}

func TestTransitionsHistoryAdapter_Delegates(t *testing.T) {
	t.Parallel()
	tenantID := uuid.New()
	store := newFakeFunnelStore(tenantID)
	adapter := transitionsHistoryAdapter{port: store}
	out, err := adapter.ListForConversation(context.Background(), tenantID, uuid.New())
	if err != nil {
		t.Fatalf("ListForConversation: %v", err)
	}
	if out != nil {
		t.Fatalf("expected nil transitions on cold store, got %v", out)
	}
}

func TestSlogFunnelPublisher_PublishIsNonFatal(t *testing.T) {
	t.Parallel()
	pub := slogFunnelPublisher{logger: discardSlog()}
	if err := pub.Publish(context.Background(), funnel.EventNameConversationMoved, struct{}{}); err != nil {
		t.Fatalf("Publish err=%v, want nil", err)
	}
}

func TestUserIDFromSessionContext(t *testing.T) {
	t.Parallel()

	t.Run("returns session user", func(t *testing.T) {
		t.Parallel()
		want := uuid.New()
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r = r.WithContext(middleware.WithSession(r.Context(), iam.Session{UserID: want}))
		if got := userIDFromSessionContext(r); got != want {
			t.Fatalf("user=%s, want %s", got, want)
		}
	})

	t.Run("nil when no session", func(t *testing.T) {
		t.Parallel()
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		if got := userIDFromSessionContext(r); got != uuid.Nil {
			t.Fatalf("user=%s, want uuid.Nil", got)
		}
	})
}

// withFunnelCtx attaches the same context the production chain installs
// (TenantScope + Auth) so the funnel handler can read tenant + session
// without going through the chi router.
func withFunnelCtx(r *http.Request, tenantID uuid.UUID) *http.Request {
	ctx := tenancy.WithContext(r.Context(), &tenancy.Tenant{ID: tenantID, Host: "tenant.example"})
	ctx = middleware.WithSession(ctx, iam.Session{
		ID:        uuid.New(),
		UserID:    uuid.New(),
		TenantID:  tenantID,
		ExpiresAt: time.Now().Add(time.Hour),
		CSRFToken: "tok-csrf",
	})
	return r.WithContext(ctx)
}

// fakeFunnelStore is a minimal in-memory implementation of the three
// funnel ports the wire consumes (StageRepository, TransitionRepository,
// BoardReader). It satisfies funnel.NewService's validation without
// reaching for pgx — funnel.Service's own unit tests cover the domain
// rules end-to-end already, this fake just makes the handler's
// composition root testable from cmd/server.
type fakeFunnelStore struct {
	tenantID    uuid.UUID
	stageByKey  map[string]*funnel.Stage
	transitions []*funnel.Transition
	cards       map[uuid.UUID]struct {
		stageKey string
		display  string
	}
}

func newFakeFunnelStore(tenantID uuid.UUID) *fakeFunnelStore {
	s := &fakeFunnelStore{
		tenantID:   tenantID,
		stageByKey: map[string]*funnel.Stage{},
		cards: map[uuid.UUID]struct {
			stageKey string
			display  string
		}{},
	}
	for i, k := range funnelTestStageKeys {
		s.stageByKey[k] = &funnel.Stage{
			ID:       uuid.New(),
			TenantID: tenantID,
			Key:      k,
			Label:    titleCase(k),
			Position: i + 1,
		}
	}
	return s
}

func (s *fakeFunnelStore) seedCard(convID uuid.UUID, stageKey, display string) {
	s.cards[convID] = struct {
		stageKey string
		display  string
	}{stageKey: stageKey, display: display}
	stage := s.stageByKey[stageKey]
	s.transitions = append(s.transitions, &funnel.Transition{
		ID:                   uuid.New(),
		TenantID:             s.tenantID,
		ConversationID:       convID,
		ToStageID:            stage.ID,
		TransitionedByUserID: uuid.New(),
		TransitionedAt:       time.Date(2026, 5, 16, 11, 0, 0, 0, time.UTC),
	})
}

func (s *fakeFunnelStore) FindByKey(_ context.Context, _ uuid.UUID, key string) (*funnel.Stage, error) {
	st, ok := s.stageByKey[key]
	if !ok {
		return nil, funnel.ErrNotFound
	}
	return st, nil
}

func (s *fakeFunnelStore) LatestForConversation(_ context.Context, _, conversationID uuid.UUID) (*funnel.Transition, error) {
	var latest *funnel.Transition
	for _, t := range s.transitions {
		if t.ConversationID != conversationID {
			continue
		}
		if latest == nil || t.TransitionedAt.After(latest.TransitionedAt) {
			latest = t
		}
	}
	if latest == nil {
		return nil, funnel.ErrNotFound
	}
	return latest, nil
}

func (s *fakeFunnelStore) Create(_ context.Context, t *funnel.Transition) error {
	clone := *t
	s.transitions = append(s.transitions, &clone)
	// keep the card's stage in sync so Board() reflects the move on the
	// re-fetch the handler does after MoveConversation
	for key, st := range s.stageByKey {
		if st.ID == t.ToStageID {
			c := s.cards[t.ConversationID]
			c.stageKey = key
			if c.display == "" {
				c.display = "Conv-" + t.ConversationID.String()[:6]
			}
			s.cards[t.ConversationID] = c
			break
		}
	}
	return nil
}

func (s *fakeFunnelStore) ListForConversation(_ context.Context, _, conversationID uuid.UUID) ([]*funnel.Transition, error) {
	var out []*funnel.Transition
	for _, t := range s.transitions {
		if t.ConversationID == conversationID {
			out = append(out, t)
		}
	}
	return out, nil
}

func (s *fakeFunnelStore) Board(_ context.Context, _ uuid.UUID) (funnel.Board, error) {
	board := funnel.Board{}
	for _, k := range funnelTestStageKeys {
		stage := s.stageByKey[k]
		col := funnel.BoardColumn{Stage: *stage}
		for convID, c := range s.cards {
			if c.stageKey != k {
				continue
			}
			col.Cards = append(col.Cards, funnel.ConversationCard{
				ConversationID: convID,
				ContactID:      uuid.New(),
				DisplayName:    c.display,
				Channel:        "whatsapp",
				LastMessageAt:  time.Date(2026, 5, 16, 12, 0, 0, 0, time.UTC),
			})
		}
		board.Columns = append(board.Columns, col)
	}
	return board, nil
}

// fakeAssignmentHistory satisfies the wire's assignmentHistoryReader
// port (and therefore webfunnel.AssignmentHistoryLister after the
// adapter wraps it). Empty by default — the history modal renders the
// empty timeline branch.
type fakeAssignmentHistory struct{}

func (f *fakeAssignmentHistory) ListHistory(_ context.Context, _, _ uuid.UUID) ([]*inbox.Assignment, error) {
	return nil, nil
}

// fakeAssignmentPort lets TestInboxAssignmentHistory_* drive the adapter
// with fixed rows / a fixed error without standing up Postgres.
type fakeAssignmentPort struct {
	rows []*inbox.Assignment
	err  error
}

func (f *fakeAssignmentPort) ListHistory(_ context.Context, _, _ uuid.UUID) ([]*inbox.Assignment, error) {
	return f.rows, f.err
}

// errSentinel is a tiny string-backed error so tests can compare with ==.
type errSentinel string

func (e errSentinel) Error() string { return string(e) }

func titleCase(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// discardSlog returns a slog.Logger that writes nowhere — the publisher
// test only cares about Publish's return value, not its log output.
func discardSlog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
