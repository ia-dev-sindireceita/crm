package funnel

import (
	"context"
	"errors"
	"html/template"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/adapter/httpapi/csrf"
	"github.com/pericles-luz/crm/internal/funnel"
	"github.com/pericles-luz/crm/internal/tenancy"
)

// Mover is the write-side dependency: it is satisfied by
// *funnel.Service. Declaring it here keeps the handler's dependency
// surface tiny and unit-testable.
type Mover interface {
	MoveConversation(ctx context.Context, tenantID, conversationID uuid.UUID, toStageKey string, byUserID uuid.UUID, reason string) error
}

// BoardLister returns the full board projection for tenantID. Satisfied
// by the pgx adapter; tests pass an in-memory fake.
type BoardLister interface {
	Board(ctx context.Context, tenantID uuid.UUID) (funnel.Board, error)
}

// FunnelHistoryLister returns the per-conversation funnel ledger,
// oldest-first.
type FunnelHistoryLister interface {
	ListForConversation(ctx context.Context, tenantID, conversationID uuid.UUID) ([]*funnel.Transition, error)
}

// StageResolver maps a stable stage key to the Stage row. The history
// modal uses it to render the stage label in each transition line
// (instead of dumping the raw uuid).
type StageResolver interface {
	FindByKey(ctx context.Context, tenantID uuid.UUID, key string) (*funnel.Stage, error)
}

// AssignmentEntry is the web-funnel-owned projection of one row from
// the inbox assignment_history ledger. The composition root adapts the
// inbox.Assignment domain type into this DTO so the web/funnel package
// stays inside the forbidwebboundary lens (SIN-62735): handlers under
// internal/web/* must not import the inbox domain root, only use cases
// or adapters that translate at the boundary.
type AssignmentEntry struct {
	AssignedAt time.Time
	UserID     uuid.UUID
	Reason     string
}

// AssignmentHistoryLister returns the per-conversation assignment ledger
// as a slice of AssignmentEntry DTOs.
type AssignmentHistoryLister interface {
	ListHistory(ctx context.Context, tenantID, conversationID uuid.UUID) ([]AssignmentEntry, error)
}

// CSRFTokenFn returns the request's CSRF token sourced by the auth
// middleware.
type CSRFTokenFn func(*http.Request) string

// UserIDFn returns the authenticated user id used as the actor on every
// move; returning uuid.Nil rejects the request with 401-equivalent
// because MoveConversation requires a non-nil actor.
type UserIDFn func(*http.Request) uuid.UUID

// Deps bundles the collaborators required by the handler.
type Deps struct {
	Mover             Mover
	Board             BoardLister
	StageResolver     StageResolver
	FunnelHistory     FunnelHistoryLister
	AssignmentHistory AssignmentHistoryLister
	CSRFToken         CSRFTokenFn
	UserID            UserIDFn
	Logger            *slog.Logger
}

// Handler is the HTMX funnel UI front controller. It is mounted on
// /funnel, /funnel/transitions, /funnel/conversations/:id/history, and
// /funnel/modal/close. The composition root wires it in cmd/server.
type Handler struct {
	deps Deps
}

// New wires the Handler. Returns an error if any required dependency
// is missing.
func New(deps Deps) (*Handler, error) {
	if deps.Mover == nil {
		return nil, errors.New("web/funnel: Mover is required")
	}
	if deps.Board == nil {
		return nil, errors.New("web/funnel: Board is required")
	}
	if deps.StageResolver == nil {
		return nil, errors.New("web/funnel: StageResolver is required")
	}
	if deps.FunnelHistory == nil {
		return nil, errors.New("web/funnel: FunnelHistory is required")
	}
	if deps.AssignmentHistory == nil {
		return nil, errors.New("web/funnel: AssignmentHistory is required")
	}
	if deps.CSRFToken == nil {
		return nil, errors.New("web/funnel: CSRFToken is required")
	}
	if deps.UserID == nil {
		return nil, errors.New("web/funnel: UserID is required")
	}
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	return &Handler{deps: deps}, nil
}

// Routes registers the handlers on mux.
func (h *Handler) Routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /funnel", h.board)
	mux.HandleFunc("POST /funnel/transitions", h.transition)
	mux.HandleFunc("GET /funnel/conversations/{id}/history", h.history)
	mux.HandleFunc("GET /funnel/modal/close", h.modalClose)
}

// board renders the full board shell.
func (h *Handler) board(w http.ResponseWriter, r *http.Request) {
	tenant, err := tenancy.FromContext(r.Context())
	if err != nil {
		h.fail(w, http.StatusInternalServerError, "tenant required", err)
		return
	}
	board, err := h.deps.Board.Board(r.Context(), tenant.ID)
	if err != nil {
		h.fail(w, http.StatusInternalServerError, "board read", err)
		return
	}
	token := h.deps.CSRFToken(r)
	if token == "" {
		h.fail(w, http.StatusInternalServerError, "csrf token missing", errors.New("empty csrf token"))
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := boardLayoutTmpl.Execute(w, struct {
		Board     boardView
		CSRFMeta  template.HTML
		HXHeaders template.HTMLAttr
		CSRFToken string
	}{
		Board:     toBoardView(board),
		CSRFMeta:  csrf.MetaTag(token),
		HXHeaders: csrf.HXHeadersAttr(token),
		CSRFToken: token,
	}); err != nil {
		h.deps.Logger.Error("web/funnel: render board", "err", err)
	}
}

// transition moves a conversation to a new stage and re-renders the
// card with its updated prev/next keys. The drag JS reads the response
// and HTMX swaps it into the card slot (hx-target=card,
// hx-swap=outerHTML). On error the JS reverts its optimistic DOM move.
func (h *Handler) transition(w http.ResponseWriter, r *http.Request) {
	tenant, err := tenancy.FromContext(r.Context())
	if err != nil {
		h.fail(w, http.StatusInternalServerError, "tenant required", err)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	conversationIDRaw := strings.TrimSpace(r.PostFormValue("conversation_id"))
	conversationID, err := uuid.Parse(conversationIDRaw)
	if err != nil {
		http.Error(w, "invalid conversation_id", http.StatusBadRequest)
		return
	}
	toStageKey := strings.TrimSpace(r.PostFormValue("to_stage_key"))
	if toStageKey == "" {
		http.Error(w, "to_stage_key required", http.StatusBadRequest)
		return
	}
	actor := h.deps.UserID(r)
	if actor == uuid.Nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	reason := strings.TrimSpace(r.PostFormValue("reason"))
	if err := h.deps.Mover.MoveConversation(r.Context(), tenant.ID, conversationID, toStageKey, actor, reason); err != nil {
		switch {
		case errors.Is(err, funnel.ErrStageNotFound):
			http.Error(w, "stage not found", http.StatusNotFound)
		case errors.Is(err, funnel.ErrInvalidStageKey),
			errors.Is(err, funnel.ErrInvalidConversation),
			errors.Is(err, funnel.ErrInvalidTenant),
			errors.Is(err, funnel.ErrInvalidActor):
			http.Error(w, "invalid request", http.StatusBadRequest)
		default:
			h.fail(w, http.StatusInternalServerError, "move", err)
		}
		return
	}
	// Re-fetch the board to derive prev/next for the new stage and
	// locate the moved card. One extra round trip per move, but keeps
	// the card's keyboard buttons coherent with its new position.
	board, err := h.deps.Board.Board(r.Context(), tenant.ID)
	if err != nil {
		h.fail(w, http.StatusInternalServerError, "board re-read", err)
		return
	}
	view := toBoardView(board)
	card, ok := findCardInView(view, conversationID)
	if !ok {
		// The conversation was moved but no longer surfaces on the board
		// (e.g. an event upstream closed it). Return 204 so HTMX removes
		// the card cleanly via hx-swap=delete fallback wired in the JS.
		w.WriteHeader(http.StatusNoContent)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := cardTmpl.Execute(w, card); err != nil {
		h.deps.Logger.Error("web/funnel: render card", "err", err)
	}
}

// history renders the per-conversation history modal, merging the
// funnel ledger with assignment_history into a single timeline.
func (h *Handler) history(w http.ResponseWriter, r *http.Request) {
	tenant, err := tenancy.FromContext(r.Context())
	if err != nil {
		h.fail(w, http.StatusInternalServerError, "tenant required", err)
		return
	}
	conversationID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "invalid conversation id", http.StatusBadRequest)
		return
	}
	transitions, err := h.deps.FunnelHistory.ListForConversation(r.Context(), tenant.ID, conversationID)
	if err != nil {
		h.fail(w, http.StatusInternalServerError, "funnel history", err)
		return
	}
	assignments, err := h.deps.AssignmentHistory.ListHistory(r.Context(), tenant.ID, conversationID)
	if err != nil {
		h.fail(w, http.StatusInternalServerError, "assignment history", err)
		return
	}
	stageLabels := h.resolveStageLabels(r.Context(), tenant.ID, transitions)
	events := mergeTimeline(transitions, assignments, stageLabels)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := historyModalTmpl.Execute(w, struct{ Events []timelineEvent }{events}); err != nil {
		h.deps.Logger.Error("web/funnel: render history", "err", err)
	}
}

// modalClose serves an empty body so the modal mount returns to its
// closed state.
func (h *Handler) modalClose(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
}

// resolveStageLabels looks up every distinct stage id referenced in
// transitions and returns a {stage_id -> label} map. The stages live
// behind a port, so the lookup is one query per distinct stage; the
// number of distinct stages in a conversation's history is bounded by
// the funnel size (≤ 5 in the default funnel).
func (h *Handler) resolveStageLabels(ctx context.Context, tenantID uuid.UUID, transitions []*funnel.Transition) map[uuid.UUID]string {
	seen := map[uuid.UUID]struct{}{}
	for _, t := range transitions {
		if t.FromStageID != nil {
			seen[*t.FromStageID] = struct{}{}
		}
		seen[t.ToStageID] = struct{}{}
	}
	out := map[uuid.UUID]string{}
	for _, key := range defaultStageKeys {
		st, err := h.deps.StageResolver.FindByKey(ctx, tenantID, key)
		if err != nil {
			continue
		}
		if _, ok := seen[st.ID]; ok {
			out[st.ID] = st.Label
		}
	}
	return out
}

// defaultStageKeys is the canonical key set seeded by migration 0093.
// resolveStageLabels iterates them so the history modal renders
// human-readable labels for any stage on a transition (custom stages
// added by a tenant would fall back to a uuid hash; the migration that
// introduces custom-stage management ships its own helper).
var defaultStageKeys = []string{"novo", "qualificando", "proposta", "ganho", "perdido"}

// timelineEvent is the merged-history shape the modal renders.
type timelineEvent struct {
	At   time.Time
	Kind string // "funnel" or "assignment"
	Text string
}

func mergeTimeline(transitions []*funnel.Transition, assignments []AssignmentEntry, stageLabels map[uuid.UUID]string) []timelineEvent {
	events := make([]timelineEvent, 0, len(transitions)+len(assignments))
	for _, t := range transitions {
		toLabel := stageLabels[t.ToStageID]
		if toLabel == "" {
			toLabel = t.ToStageID.String()
		}
		var text string
		if t.FromStageID == nil {
			text = "Entrou no estágio " + toLabel
		} else {
			fromLabel := stageLabels[*t.FromStageID]
			if fromLabel == "" {
				fromLabel = t.FromStageID.String()
			}
			text = "Movida de " + fromLabel + " para " + toLabel
		}
		if t.Reason != "" {
			text += " — " + t.Reason
		}
		events = append(events, timelineEvent{
			At:   t.TransitionedAt,
			Kind: "funnel",
			Text: text,
		})
	}
	for _, a := range assignments {
		events = append(events, timelineEvent{
			At:   a.AssignedAt,
			Kind: "assignment",
			Text: "Líder atribuído: " + a.UserID.String() + " (" + a.Reason + ")",
		})
	}
	sort.SliceStable(events, func(i, j int) bool { return events[i].At.Before(events[j].At) })
	return events
}

func (h *Handler) fail(w http.ResponseWriter, status int, msg string, err error) {
	h.deps.Logger.Error("web/funnel: "+msg, "err", err)
	http.Error(w, http.StatusText(status), status)
}

// toBoardView converts the domain board into the template view-model
// with per-card prev/next stage keys filled in.
func toBoardView(b funnel.Board) boardView {
	stageKeys := make([]string, 0, len(b.Columns))
	for _, col := range b.Columns {
		stageKeys = append(stageKeys, col.Stage.Key)
	}
	prevByKey, nextByKey := neighbours(stageKeys)

	view := boardView{Columns: make([]columnView, 0, len(b.Columns))}
	for _, col := range b.Columns {
		cv := columnView{
			Stage: stageView{
				ID:    col.Stage.ID.String(),
				Key:   col.Stage.Key,
				Label: col.Stage.Label,
			},
			Cards: make([]cardView, 0, len(col.Cards)),
		}
		for _, c := range col.Cards {
			cv.Cards = append(cv.Cards, cardView{
				ConversationID: c.ConversationID.String(),
				DisplayName:    c.DisplayName,
				Channel:        c.Channel,
				LastMessageAt:  c.LastMessageAt,
				StageKey:       col.Stage.Key,
				PrevKey:        prevByKey[col.Stage.Key],
				NextKey:        nextByKey[col.Stage.Key],
			})
		}
		view.Columns = append(view.Columns, cv)
	}
	return view
}

// neighbours returns ({stage -> prev_stage}, {stage -> next_stage})
// using the position order of stageKeys. The first stage has no prev;
// the last has no next.
func neighbours(stageKeys []string) (map[string]string, map[string]string) {
	prev := map[string]string{}
	next := map[string]string{}
	for i, k := range stageKeys {
		if i > 0 {
			prev[k] = stageKeys[i-1]
		}
		if i < len(stageKeys)-1 {
			next[k] = stageKeys[i+1]
		}
	}
	return prev, next
}

// findCardInView locates the card for conversationID in any column;
// returns false when the conversation no longer surfaces (e.g. its
// conversation was closed concurrently).
func findCardInView(view boardView, conversationID uuid.UUID) (cardView, bool) {
	want := conversationID.String()
	for _, col := range view.Columns {
		for _, c := range col.Cards {
			if c.ConversationID == want {
				return c, true
			}
		}
	}
	return cardView{}, false
}

// boardView is the template-shaped board.
type boardView struct {
	Columns []columnView
}

type columnView struct {
	Stage stageView
	Cards []cardView
}

type stageView struct {
	ID    string
	Key   string
	Label string
}

type cardView struct {
	ConversationID string
	DisplayName    string
	Channel        string
	LastMessageAt  time.Time
	StageKey       string
	PrevKey        string
	NextKey        string
}
