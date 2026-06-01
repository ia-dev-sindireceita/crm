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

	"github.com/pericles-luz/crm/internal/branding"
	"github.com/pericles-luz/crm/internal/funnel"
	"github.com/pericles-luz/crm/internal/http/middleware/csp"
	"github.com/pericles-luz/crm/internal/iam"
	"github.com/pericles-luz/crm/internal/tenancy"
	"github.com/pericles-luz/crm/internal/web/shell"
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

// RoleFn returns the viewer's IAM role from the session context.
type RoleFn func(*http.Request) iam.Role

// TeamIDFn returns the viewer's team id from the session context.
// Returns uuid.Nil until the teams migration lands (reserved for lider scoping).
type TeamIDFn func(*http.Request) uuid.UUID

// StatsGetter is the stats use-case port exposed to the handler.
type StatsGetter interface {
	GetStats(ctx context.Context, tenantID uuid.UUID, q funnel.StatsQuery) (funnel.Stats, error)
}

// Deps bundles the collaborators required by the handler.
type Deps struct {
	Mover             Mover
	Board             BoardLister
	StageResolver     StageResolver
	FunnelHistory     FunnelHistoryLister
	AssignmentHistory AssignmentHistoryLister
	Stats             StatsGetter // optional; omit to disable GET /funnel/stats + page header stats
	CSRFToken         CSRFTokenFn
	UserID            UserIDFn
	Role              RoleFn   // required when Stats is non-nil
	TeamID            TeamIDFn // optional; returns uuid.Nil when unset
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
	if h.deps.Stats != nil {
		mux.HandleFunc("GET /funnel/stats", h.stats)
		mux.HandleFunc("GET /funnel/stats/drawer/close", h.drawerClose)
	}
}

// board renders the full board shell. SIN-63943 — composes via
// shell.Layout; embeds the stats header (KPIs + filters) when the
// viewer's role permits, and surfaces per-stage column stats.
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

	role := h.viewerRole(r)
	filters := parseFilters(r)

	stats, statsErr := h.computeStats(r, tenant.ID, role, filters)
	if statsErr != nil {
		h.fail(w, http.StatusInternalServerError, "stats", statsErr)
		return
	}
	canSeeStats := stats != nil
	canSeeTeams := canSeeStats && role == iam.RoleTenantGerente

	view := h.buildBoardView(board, stats)
	view.TenantName = tenant.Name
	view.UserDisplayName = displayNameForUser(h.deps.UserID(r))
	view.CSRFToken = token
	view.TenantThemeStyle = branding.ThemeStyleFromContext(r.Context())
	view.CSPNonce = csp.Nonce(r.Context())
	view.NavItems = buildFunnelNavItems()
	view.UserMenuItems = buildFunnelUserMenu()
	view.Stats = stats
	view.CanSeeStats = canSeeStats
	view.CanSeeTeams = canSeeTeams
	view.Filters = filters

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := boardPageTmpl.ExecuteTemplate(w, "layout", view); err != nil {
		h.deps.Logger.Error("web/funnel: render board", "err", err)
	}
}

// computeStats calls the StatsGetter when the dependency is wired and
// the viewer's role permits stats access. Atendente (and unknown roles)
// short-circuit to nil so the page never renders the analytical header
// for users who are not allowed to see it.
func (h *Handler) computeStats(r *http.Request, tenantID uuid.UUID, role iam.Role, filters filtersView) (*funnel.Stats, error) {
	if h.deps.Stats == nil {
		return nil, nil
	}
	if role != iam.RoleTenantGerente && role != iam.RoleTenantLider {
		return nil, nil
	}
	q := buildStatsQueryFromFilters(filters, role, h.deps.UserID(r), h.viewerTeamID(r))
	result, err := h.deps.Stats.GetStats(r.Context(), tenantID, q)
	if err != nil {
		if errors.Is(err, funnel.ErrForbidden) {
			return nil, nil
		}
		return nil, err
	}
	return &result, nil
}

func (h *Handler) viewerRole(r *http.Request) iam.Role {
	if h.deps.Role == nil {
		return iam.RoleTenantCommon
	}
	return h.deps.Role(r)
}

func (h *Handler) viewerTeamID(r *http.Request) uuid.UUID {
	if h.deps.TeamID == nil {
		return uuid.Nil
	}
	return h.deps.TeamID(r)
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
	view := h.buildBoardView(board, nil)
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

// drawerClose serves an empty body so the drawer mount returns to its
// closed state (SIN-63943 AC #3 close affordance).
func (h *Handler) drawerClose(w http.ResponseWriter, _ *http.Request) {
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

// buildBoardView converts the domain board into the template view-model
// with per-card prev/next stage keys filled in. When stats is non-nil,
// each column's StageStats slot is populated from the matching
// funnel.StageStats so the column header can render avg-time + conv-rate.
func (h *Handler) buildBoardView(b funnel.Board, stats *funnel.Stats) boardView {
	stageKeys := make([]string, 0, len(b.Columns))
	for _, col := range b.Columns {
		stageKeys = append(stageKeys, col.Stage.Key)
	}
	prevByKey, nextByKey := neighbours(stageKeys)

	statsByKey := map[string]funnel.StageStats{}
	if stats != nil {
		for _, s := range stats.Stages {
			statsByKey[s.StageKey] = s
		}
	}

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
		if s, ok := statsByKey[col.Stage.Key]; ok {
			cv.Stats = stageStatsView{
				HasStats:       true,
				AvgTimeInStage: s.AvgTimeInStage,
				ConvRate:       s.ConvRate,
			}
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

// boardView is the template-shaped board. Embeds the shell.Data field
// surface so shell.Layout's reflection-based helpers (shellTenantName,
// shellNavItems, …) find every chrome field by name.
type boardView struct {
	// shell.Data fields (read by shell.Layout reflection helpers)
	TenantName       string
	TenantLogo       string
	UserDisplayName  string
	NavItems         []shell.NavItem
	UserMenuItems    []shell.UserMenuItem
	CSRFToken        string
	CSPNonce         string
	TenantThemeStyle template.CSS

	// Funnel content
	Columns     []columnView
	Stats       *funnel.Stats
	CanSeeStats bool
	CanSeeTeams bool
	Filters     filtersView
}

type columnView struct {
	Stage stageView
	Cards []cardView
	Stats stageStatsView
}

type stageView struct {
	ID    string
	Key   string
	Label string
}

type stageStatsView struct {
	HasStats       bool
	AvgTimeInStage time.Duration
	ConvRate       *float64
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

// filtersView is the view-model of the period/owner filter form.
type filtersView struct {
	Period string // "7d" | "30d" | "90d" — empty defaults to 30d in the UI
	Owner  string // "" | "me"
}

// stats handles GET /funnel/stats. It enforces coarse RBAC at the door
// (atendente → 403), constructs a StatsQuery from query-string params,
// calls StatsService.GetStats, and renders the HTMX stats partial.
//
// SIN-63943 — accepts an optional `view` query param: `view=drawer`
// renders the drawer-only partial (per-attendant + per-team + per-channel
// tables); default behaviour renders the full statsTmpl (kept for
// backwards compatibility with the SIN-63962 handler tests).
func (h *Handler) stats(w http.ResponseWriter, r *http.Request) {
	tenant, err := tenancy.FromContext(r.Context())
	if err != nil {
		h.fail(w, http.StatusInternalServerError, "stats: tenant required", err)
		return
	}

	var viewerRole iam.Role
	if h.deps.Role != nil {
		viewerRole = h.deps.Role(r)
	}

	// Coarse RBAC gate: atendente cannot access stats.
	if viewerRole == iam.RoleTenantAtendente {
		http.Error(w, http.StatusText(http.StatusForbidden), http.StatusForbidden)
		return
	}

	viewerID := h.deps.UserID(r)

	var viewerTeamID uuid.UUID
	if h.deps.TeamID != nil {
		viewerTeamID = h.deps.TeamID(r)
	}

	q := buildStatsQuery(r, viewerRole, viewerID, viewerTeamID)

	result, err := h.deps.Stats.GetStats(r.Context(), tenant.ID, q)
	if err != nil {
		if errors.Is(err, funnel.ErrForbidden) {
			http.Error(w, http.StatusText(http.StatusForbidden), http.StatusForbidden)
			return
		}
		h.fail(w, http.StatusInternalServerError, "stats: get", err)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	switch r.URL.Query().Get("view") {
	case "drawer":
		data := struct {
			Stats funnel.Stats
		}{Stats: result}
		if err := statsDrawerTmpl.Execute(w, data); err != nil {
			h.deps.Logger.Error("web/funnel: stats drawer: render", "err", err)
		}
	default:
		view := statsView{
			Stats:    result,
			Period:   q.Period.Kind,
			CSPNonce: csp.Nonce(r.Context()),
		}
		if err := statsTmpl.Execute(w, view); err != nil {
			h.deps.Logger.Error("web/funnel: stats: render", "err", err)
		}
	}
}

// parseFilters lifts the period + owner query params off the request and
// normalizes them for the filter form view-model.
func parseFilters(r *http.Request) filtersView {
	period := strings.TrimSpace(r.URL.Query().Get("period"))
	switch period {
	case "7d", "30d", "90d":
		// ok
	default:
		period = ""
	}
	owner := strings.TrimSpace(r.URL.Query().Get("owner"))
	if owner != "me" {
		owner = ""
	}
	return filtersView{Period: period, Owner: owner}
}

// buildStatsQueryFromFilters maps the view-model filters back into the
// domain StatsQuery. The board handler uses this when computing
// page-header stats; the /funnel/stats handler keeps using the existing
// buildStatsQuery helper (which reads the request's query string
// directly) for backwards compatibility.
func buildStatsQueryFromFilters(f filtersView, role iam.Role, viewerID, viewerTeamID uuid.UUID) funnel.StatsQuery {
	q := funnel.StatsQuery{
		ViewerRole:   role,
		ViewerID:     viewerID,
		ViewerTeamID: viewerTeamID,
	}
	switch f.Period {
	case "7d":
		q.Period = funnel.Period{Kind: funnel.PeriodLast7d}
	case "90d":
		q.Period = funnel.Period{Kind: funnel.PeriodLast90d}
	default:
		q.Period = funnel.Period{Kind: funnel.PeriodLast30d}
	}
	if f.Owner == "me" {
		q.OwnerScope = funnel.OwnerScope{Kind: funnel.OwnerScopeUser, UserID: viewerID}
	}
	return q
}

// buildStatsQuery parses GET /funnel/stats query string params.
// Supported params: period (7d|30d|90d|custom), from/to (RFC3339 for custom).
func buildStatsQuery(r *http.Request, role iam.Role, viewerID, viewerTeamID uuid.UUID) funnel.StatsQuery {
	q := funnel.StatsQuery{
		ViewerRole:   role,
		ViewerID:     viewerID,
		ViewerTeamID: viewerTeamID,
	}

	switch r.URL.Query().Get("period") {
	case "7d":
		q.Period = funnel.Period{Kind: funnel.PeriodLast7d}
	case "90d":
		q.Period = funnel.Period{Kind: funnel.PeriodLast90d}
	case "custom":
		from, errF := time.Parse(time.RFC3339, r.URL.Query().Get("from"))
		to, errT := time.Parse(time.RFC3339, r.URL.Query().Get("to"))
		if errF == nil && errT == nil && !from.IsZero() && !to.IsZero() {
			q.Period = funnel.Period{Kind: funnel.PeriodCustom, From: from, To: to}
		} else {
			q.Period = funnel.Period{Kind: funnel.PeriodLast30d}
		}
	default: // "30d" or empty
		q.Period = funnel.Period{Kind: funnel.PeriodLast30d}
	}

	// Owner filter mirrors the board page's filter form, with the
	// service still authoritatively clamping for lider/atendente.
	if r.URL.Query().Get("owner") == "me" {
		q.OwnerScope = funnel.OwnerScope{Kind: funnel.OwnerScopeUser, UserID: viewerID}
	}

	return q
}

// statsView is the template view model for the stats partial.
type statsView struct {
	Stats    funnel.Stats
	Period   funnel.PeriodKind
	CSPNonce string
}

// buildFunnelNavItems returns the top-bar nav for the funnel page. The
// brand link points back at /hello-tenant; the nav lists the primary
// post-login destinations so AC #8 ("Migração para shell.Layout confirma
// volta para landing via top-nav") is observable in any role.
func buildFunnelNavItems() []shell.NavItem {
	return []shell.NavItem{
		{Label: "Inbox", Path: "/inbox"},
		{Label: "Funil", Path: "/funnel", Active: true},
	}
}

// buildFunnelUserMenu returns the user-menu dropdown entries common to
// authenticated funnel sessions.
func buildFunnelUserMenu() []shell.UserMenuItem {
	return []shell.UserMenuItem{
		{Label: "Sair", Path: "/logout", Form: true},
	}
}

// displayNameForUser is the placeholder display formatter for the
// user-menu button. The session does not (yet) carry a human label,
// so we render the uuid prefix — replace once a user-name resolver
// lands.
func displayNameForUser(userID uuid.UUID) string {
	if userID == uuid.Nil {
		return "Conta"
	}
	s := userID.String()
	if len(s) > 8 {
		return s[:8]
	}
	return s
}
