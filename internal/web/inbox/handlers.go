package inbox

import (
	"context"
	"errors"
	"html/template"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/adapter/httpapi/csrf"
	"github.com/pericles-luz/crm/internal/branding"
	"github.com/pericles-luz/crm/internal/http/middleware/csp"
	inboxusecase "github.com/pericles-luz/crm/internal/inbox/usecase"
	"github.com/pericles-luz/crm/internal/tenancy"
	"github.com/pericles-luz/crm/internal/web/shell"
)

// maxBodyChars caps the textarea (matches the maxlength on the form).
// The schema's `message.body` column is `text` without a hard cap, but
// 4096 chars is enough for any operator-composed reply and keeps the
// request body small enough to fit in a single TLS frame.
const maxBodyChars = 4096

// ListConversationsUseCase / ListMessagesUseCase / SendOutboundUseCase
// are the minimal use-case interfaces the handler depends on. The
// concrete *inboxusecase.ListConversations etc. satisfy them. Declaring
// them here keeps the handler's dependency surface tiny and unit-test
// friendly: tests can swap in lightweight fakes without dragging the
// full domain in.
type ListConversationsUseCase interface {
	Execute(ctx context.Context, in inboxusecase.ListConversationsInput) (inboxusecase.ListConversationsResult, error)
}

// ListSummariesUseCase is the enriched read side backing GET /inbox
// (SIN-64967 read-model → SIN-64968 UI). It returns ConversationViews
// carrying the contact name, last-message snippet + direction, the
// awaiting-reply flag, and the assigned-atendente label the rich list
// renders. It is optional on Deps: when wired the list handler consumes
// it (badges, snippet, filters); when nil the handler falls back to the
// legacy ListConversations (channel + timestamp only) so deployments
// that have not wired the read-model keep rendering.
type ListSummariesUseCase interface {
	Execute(ctx context.Context, in inboxusecase.ListConversationSummariesInput) (inboxusecase.ListConversationSummariesResult, error)
}

// ListMessagesUseCase is the conversation-view read side.
type ListMessagesUseCase interface {
	Execute(ctx context.Context, in inboxusecase.ListMessagesInput) (inboxusecase.ListMessagesResult, error)
}

// SendOutboundUseCase is the outbound write side. SendForView returns
// the MessageView the handler renders into the new bubble.
type SendOutboundUseCase interface {
	SendForView(ctx context.Context, in inboxusecase.SendOutboundInput) (inboxusecase.MessageView, error)
}

// GetMessageUseCase is the single-message read-side that backs the
// realtime status partial (SIN-62736). The HTMX bubble polls this
// endpoint every few seconds while the message is in a non-final state
// (∉ {read, failed}) so the operator sees the status transitions
// without reloading the conversation pane.
type GetMessageUseCase interface {
	Execute(ctx context.Context, in inboxusecase.GetMessageInput) (inboxusecase.GetMessageResult, error)
}

// ListMessagesSinceUseCase is the incremental read-side that backs the
// conversation thread's live-refresh poll (SIN-65419). The open
// conversation pane polls every few seconds with an exclusive cursor
// (the Unix-nanosecond CreatedAt of the last message it already holds);
// the use case returns only the messages created after it so the handler
// appends the new inbound (auto-reply) bubbles without a reload. It is
// optional on Deps: when nil the live-poll route is not registered and
// the conversation view renders without the poll sentinel, so the thread
// stays static (the legacy behaviour) on deployments that have not wired
// it.
type ListMessagesSinceUseCase interface {
	Execute(ctx context.Context, in inboxusecase.ListMessagesSinceInput) (inboxusecase.ListMessagesSinceResult, error)
}

// GetConversationContextUseCase is the read side that gathers the
// conversation's channel + (future side-panel) funnel/assignment
// context (SIN-64969). The view handler uses it to feed the real
// conversation channel scope to the AI-assist policy and the customer
// panel, replacing the PR10 empty-scope stub. It is optional: when nil
// the channel falls back to empty (the policy resolver then uses its
// tenant-scope default), preserving the original behaviour.
type GetConversationContextUseCase interface {
	Execute(ctx context.Context, in inboxusecase.GetConversationContextInput) (inboxusecase.GetConversationContextResult, error)
}

// AssignConversationUseCase is the write-side port for assigning (or
// re-assigning) a conversation to an attendant (SIN-64978 / SIN-64979).
// It is optional on Deps: when nil the assign route is not registered so
// deployments that have not wired the attendant repository keep the
// original read-only surface.
type AssignConversationUseCase interface {
	Execute(ctx context.Context, in inboxusecase.AssignConversationInput) (inboxusecase.AssignConversationResult, error)
}

// ResetConversationUseCase is the write-side port for the fakellm
// training-conversation reset (SIN-65392): it deletes the conversation's
// messages and clears the channel adapter's in-memory state. It is
// optional on Deps — when nil the reset route is not registered and the
// "Apagar mensagens" button is not rendered, so deployments without the
// fake channel keep the read/send-only surface. The use case itself
// rejects any non-fakellm conversation, so the route stays at the
// ordinary inbox role level (the channel guard, not RBAC, confines it).
type ResetConversationUseCase interface {
	Execute(ctx context.Context, in inboxusecase.ResetConversationInput) (inboxusecase.ResetConversationResult, error)
}

// CloseConversationUseCase is the write-side port behind the inbox
// "Encerrar conversa" action (SIN-65473). It is optional on Deps: when
// nil the close/reopen routes are not registered and the customer-actions
// "Encerrar conversa" button is rendered disabled (the pre-feature stub).
type CloseConversationUseCase interface {
	Execute(ctx context.Context, in inboxusecase.CloseConversationInput) (inboxusecase.CloseConversationResult, error)
}

// ReopenConversationUseCase is the inverse write-side port behind the
// "Reabrir conversa" action (SIN-65473). It is wired together with
// CloseConversation — closing a conversation without a reopen path would
// trap it — and shares the same optionality.
type ReopenConversationUseCase interface {
	Execute(ctx context.Context, in inboxusecase.ReopenConversationInput) (inboxusecase.ReopenConversationResult, error)
}

// UnassignConversationUseCase is the write-side port behind the
// "— Não atribuído —" option of the Transferir select (SIN-65480): it
// returns a conversation to the unassigned state. Optional — when nil,
// the option is not rendered and the /transfer handler never routes to
// the unassign path.
type UnassignConversationUseCase interface {
	Execute(ctx context.Context, in inboxusecase.UnassignConversationInput) (inboxusecase.UnassignConversationResult, error)
}

// AssignableRow is the web-local projection of inbox.AssignableAttendant.
// Kept here (instead of importing the domain root) because the
// forbidwebboundary lint forbids web/* from importing internal/inbox
// directly. The composition root maps domain rows to this shape.
type AssignableRow struct {
	UserID      uuid.UUID
	DisplayName string
}

// ListAssignableUseCase feeds the assignment-dropdown with the tenant's
// eligible attendants (SIN-64979). It is optional: when nil the
// assignment panel renders read-only (no dropdown / no assign button).
type ListAssignableUseCase interface {
	Execute(ctx context.Context, tenantID uuid.UUID) ([]AssignableRow, error)
}

// CSRFTokenFn returns the request's CSRF token (typically sourced from
// the session via the IAM auth middleware). The empty string is a
// programming error: every handler runs after RequireAuth, which
// guarantees the session exists. The handler surfaces empty as 500
// rather than render a form with no token.
type CSRFTokenFn func(*http.Request) string

// UserIDFn returns the authenticated user id for outbound
// `sent_by_user_id` bookkeeping. Returning uuid.Nil is acceptable
// (the message is recorded without an author); the handler does not
// gate on it.
type UserIDFn func(*http.Request) uuid.UUID

// Deps bundles the handler's collaborators. The core inbox ports
// (ListConversations / ListMessages / SendOutbound / GetMessage /
// CSRFToken / UserID) are required; AIAssist is optional — when
// AIAssist.Summarizer is nil the assist button + route are not
// registered, so existing deployments keep the same surface.
type Deps struct {
	ListConversations ListConversationsUseCase
	// ListSummaries is the optional enriched read side (SIN-64968). When
	// wired the list handler renders the rich row (contact name, snippet,
	// badges) and the state/channel/"minhas" filters; when nil it falls
	// back to ListConversations. Wiring it in the composition root is what
	// upgrades GET /inbox from the bare channel+timestamp list to the full
	// UX-spec surface.
	ListSummaries ListSummariesUseCase
	ListMessages  ListMessagesUseCase
	// ListMessagesSince is the optional incremental read-side for the
	// conversation thread live-refresh poll (SIN-65419). When wired, GET
	// /inbox/conversations/{id}/messages/since is registered and the
	// conversation view renders the poll sentinel that appends new inbound
	// bubbles every few seconds; when nil the route is absent and the
	// thread stays static (legacy behaviour). It reuses the same message
	// read port as ListMessages, so wiring it carries no extra storage
	// surface.
	ListMessagesSince ListMessagesSinceUseCase
	SendOutbound      SendOutboundUseCase
	GetMessage        GetMessageUseCase
	CSRFToken         CSRFTokenFn
	UserID            UserIDFn
	Logger            *slog.Logger
	// AIAssist wires the optional SIN-62908 ai-assist feature. The
	// nested Summarizer field is the activation switch.
	AIAssist AssistDeps
	// CustomerInfo is the optional read-side port that hydrates the
	// right-rail customer panel (SIN-63939 / UX-F2). When nil the panel
	// renders its degraded state: name = "Contato sem nome", no email /
	// phone / tags / identities. The contact aggregate behind the port
	// is intentionally NOT imported from internal/contacts because the
	// forbidwebboundary lint blocks domain imports from web/* — the
	// adapter that satisfies the port projects the contact onto
	// CustomerInfo at the boundary.
	CustomerInfo CustomerInfoLoader
	// ConversationContext is the optional read-side that resolves the
	// conversation's channel (SIN-64969). When wired, the view handler
	// feeds the real channel scope to the AI-assist policy + customer
	// panel; when nil the channel falls back to empty (the original PR10
	// behaviour). It complements CustomerInfo (contact projection) — this
	// one carries the channel + funnel/assignment context.
	ConversationContext GetConversationContextUseCase
	// AssignConversation is the optional write-side for assigning a
	// conversation (SIN-64979). When wired, POST
	// /inbox/conversations/{id}/assign is registered and the context panel
	// renders the interactive assignment widget; when nil the route is
	// absent and the panel renders read-only.
	AssignConversation AssignConversationUseCase
	// ListAssignable is the optional read-side that feeds the assignment
	// dropdown (SIN-64979). When wired, the view handler loads the
	// tenant's eligible attendants and passes them to the context panel so
	// the dropdown is pre-populated; when nil the form is not rendered.
	ListAssignable ListAssignableUseCase
	// ResetConversation is the optional write-side for the fakellm
	// training-conversation reset (SIN-65392). When wired, POST
	// /inbox/conversations/{id}/reset is registered and the conversation
	// view renders the "Apagar mensagens" button for fakellm threads; when
	// nil the route is absent and the button never renders. The use case
	// rejects any non-fakellm conversation, so the reach is confined to
	// the synthetic training thread regardless of caller role.
	ResetConversation ResetConversationUseCase
	// CloseConversation / ReopenConversation are the optional write-side
	// for the inbox "Encerrar / Reabrir conversa" actions (SIN-65473). When
	// CloseConversation is wired, POST /inbox/conversations/{id}/close and
	// .../reopen are registered and the customer-actions panel renders the
	// live Encerrar / Reabrir toggle; when nil the routes are absent and the
	// "Encerrar conversa" button stays disabled. They are wired as a pair —
	// a closed conversation must always have a reopen path.
	CloseConversation  CloseConversationUseCase
	ReopenConversation ReopenConversationUseCase
	// UnassignConversation is the optional write-side for the
	// "— Não atribuído —" option of the Transferir select (SIN-65480).
	// When wired the option is rendered and POST .../transfer routes a
	// sentinel-valued submit to the unassign path; when nil the option is
	// absent and /transfer only reassigns to another attendant.
	UnassignConversation UnassignConversationUseCase
}

// CustomerInfoLoader is the read-side port that hydrates the customer
// panel for one conversation. Implementations MUST NOT return an error
// for "contact not found"; they should return a zero CustomerInfo +
// nil error so the panel degrades to the "no data" state instead of
// surfacing a 500. The handler treats a non-nil error as a true read
// failure and logs + degrades the panel.
type CustomerInfoLoader interface {
	Load(ctx context.Context, tenantID, conversationID uuid.UUID) (CustomerInfo, error)
}

// CustomerInfo is the projection the right-rail customer panel
// consumes. All fields are optional; the template renders only the
// blocks for which it has data. Tags / Identities are nil-safe.
//
// LGPD note: the projector behind the port is responsible for trimming
// the field set to the minimum the operator needs to close the sale
// (Nielsen #8 + LGPD minimização — see SIN-63939 constraints). The
// template intentionally does NOT expose CPF, full address, or other
// sensitive fields; tightening the projection at the boundary keeps
// the template free of compliance branches.
type CustomerInfo struct {
	DisplayName string
	Email       string
	Phone       string
	Tags        []string
	Identities  []CustomerIdentity
}

// CustomerIdentity is one entry in the right-rail "identidades
// vinculadas" list. Channel is the lower-case channel slug (whatsapp /
// instagram / facebook / chatbot); Handle is the human-friendly
// identifier (phone number, @handle, …) the operator recognises.
type CustomerIdentity struct {
	Channel string
	Handle  string
}

// Handler is the HTMX inbox UI front controller. It is mounted on the
// public mux at /inbox, /inbox/conversations/:id, and
// /inbox/conversations/:id/messages — see Routes.
type Handler struct {
	deps Deps
}

// New wires the Handler. Returns an error when any required dependency
// is missing; the composition root panics on that error.
func New(deps Deps) (*Handler, error) {
	if deps.ListConversations == nil {
		return nil, errors.New("web/inbox: ListConversations is required")
	}
	if deps.ListMessages == nil {
		return nil, errors.New("web/inbox: ListMessages is required")
	}
	if deps.SendOutbound == nil {
		return nil, errors.New("web/inbox: SendOutbound is required")
	}
	if deps.GetMessage == nil {
		return nil, errors.New("web/inbox: GetMessage is required")
	}
	if deps.CSRFToken == nil {
		return nil, errors.New("web/inbox: CSRFToken is required")
	}
	if deps.UserID == nil {
		return nil, errors.New("web/inbox: UserID is required")
	}
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	return &Handler{deps: deps}, nil
}

// Routes registers the inbox handlers on mux. Path patterns are Go
// 1.22 ServeMux style so the mux's longest-prefix rule wins over the
// custom-domain catch-all at "/".
//
// POST /inbox/conversations/{id}/ai-assist is conditional: it is only
// registered when AIAssist.Summarizer is wired (SIN-62908). Inbox-only
// deployments that don't enable IA keep the original four routes.
func (h *Handler) Routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /inbox", h.list)
	mux.HandleFunc("GET /inbox/conversations/{id}", h.view)
	mux.HandleFunc("POST /inbox/conversations/{id}/messages", h.send)
	mux.HandleFunc("GET /inbox/conversations/{id}/messages/{msgID}/status", h.status)
	if h.deps.ListMessagesSince != nil {
		mux.HandleFunc("GET /inbox/conversations/{id}/messages/since", h.since)
	}
	if h.deps.AIAssist.Summarizer != nil {
		mux.HandleFunc("POST /inbox/conversations/{id}/ai-assist", h.aiAssist)
	}
	if h.deps.AssignConversation != nil {
		mux.HandleFunc("POST /inbox/conversations/{id}/assign", h.assign)
		// Transfer is the operator-facing "Transferir conversa" action: a
		// reassignment of the conversation lead surfaced from the
		// customer-actions panel rather than the context chip (SIN-65473).
		// h.transfer dispatches on the submitted targetUserID: the
		// "— Não atribuído —" sentinel routes to the unassign path
		// (SIN-65480), every other value is the same reassignment pipeline
		// as assign. The route is enumerated once in router.go regardless of
		// which branch handles it, so no new route is added.
		mux.HandleFunc("POST /inbox/conversations/{id}/transfer", h.transfer)
	}
	if h.deps.ResetConversation != nil {
		mux.HandleFunc("POST /inbox/conversations/{id}/reset", h.reset)
	}
	if h.deps.CloseConversation != nil {
		mux.HandleFunc("POST /inbox/conversations/{id}/close", h.close)
		if h.deps.ReopenConversation != nil {
			mux.HandleFunc("POST /inbox/conversations/{id}/reopen", h.reopen)
		}
	}
}

// list renders the inbox list (left pane). On a full navigation it
// renders the whole shell; on an HTMX request (a filter change) it
// renders only the list region partial so the conversation + customer
// panes are left untouched. The active filters come from the query
// string; the "minhas" filter is keyed to the session user id, never a
// client-supplied id (secure-by-default).
func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	tenant, err := tenancy.FromContext(r.Context())
	if err != nil {
		h.fail(w, http.StatusInternalServerError, "tenant required", err)
		return
	}
	filter := parseInboxFilter(r)
	region, err := h.buildListRegion(r, tenant.ID, filter, uuid.Nil, false)
	if err != nil {
		h.fail(w, http.StatusInternalServerError, "list conversations", err)
		return
	}

	// HTMX filter requests swap only the list region; a full navigation
	// (or no-JS request) renders the entire shell so the surface is
	// bookmarkable with the filters applied.
	if r.Header.Get("HX-Request") == "true" {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := inboxListRegionTmpl.Execute(w, region); err != nil {
			h.deps.Logger.Error("web/inbox: render list region", "err", err)
		}
		return
	}

	token := h.deps.CSRFToken(r)
	if token == "" {
		h.fail(w, http.StatusInternalServerError, "csrf token missing", errors.New("empty csrf token"))
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := inboxLayoutTmpl.Execute(w, layoutData{
		TenantName:       tenant.Name,
		UserDisplayName:  displayNameForUser(h.deps.UserID(r)),
		NavItems:         buildInboxNavItems(),
		UserMenuItems:    buildInboxUserMenu(),
		CSRFToken:        token,
		TenantThemeStyle: branding.ThemeStyleFromContext(r.Context()),
		CSPNonce:         csp.Nonce(r.Context()),
		List:             region,
		Customer:         customerPanelData{HasConversation: false},
	}); err != nil {
		h.deps.Logger.Error("web/inbox: render layout", "err", err)
	}
}

// inboxFilters are the validated, sanitised list filters parsed from the
// query string. State defaults to "open" (the operator triages open work
// first, matching the legacy handler); an absent state param means the
// default, an explicit empty state means "all". Channel is restricted to
// the known carriers — an unknown value degrades to "" (all) rather than
// erroring the swap. AssignedMe drives the "minhas" filter using the
// session user id resolved in buildListRegion.
type inboxFilter struct {
	State   string // "", "open", "closed"
	Channel string // "", "whatsapp", "instagram", "messenger", "webchat"
	// AssignedMe drives the "atribuídas a mim" queue (assigned=me): the
	// list is filtered to the session user's conversations.
	AssignedMe bool
	// Unassigned drives the "fila / não atribuídas" queue
	// (assigned=unassigned): the list is filtered to conversations with no
	// current lead. Mutually exclusive with AssignedMe — parseInboxFilter
	// reads a single `assigned` value, so at most one is ever set, which
	// keeps the read-side use case (it rejects the unassigned+user combo)
	// from ever seeing both.
	Unassigned bool
}

// inboxFilterChannels mirrors the read-model's known carriers
// (internal/inbox.knownChannels). It is duplicated here, rather than
// imported, because the forbidwebboundary lint forbids web/* from
// importing the inbox domain root; keeping a tiny local allowlist lets
// the handler sanitise a stray channel param to "" (all) instead of
// surfacing a 400 on an otherwise harmless filter swap.
var inboxFilterChannels = map[string]struct{}{
	"whatsapp":  {},
	"instagram": {},
	"messenger": {},
	"webchat":   {},
}

// parseInboxFilter reads the state/channel/assigned filters from the
// request query string and normalises them. It never trusts a
// client-supplied user id: the "minhas" toggle is a boolean here and the
// concrete id is sourced from the session in buildListRegion.
func parseInboxFilter(r *http.Request) inboxFilter {
	q := r.URL.Query()

	state := "open"
	if q.Has("state") {
		switch v := strings.ToLower(strings.TrimSpace(q.Get("state"))); v {
		case "", "open", "closed":
			state = v
		default:
			state = "open"
		}
	}

	channel := strings.ToLower(strings.TrimSpace(q.Get("channel")))
	if channel != "" {
		if _, ok := inboxFilterChannels[channel]; !ok {
			channel = ""
		}
	}

	// The assignment queue is a single mutually-exclusive value:
	// "me" → my conversations, "unassigned" → the unattended queue,
	// anything else (incl. absent / "all") → no assignment filter.
	assigned := strings.ToLower(strings.TrimSpace(q.Get("assigned")))
	return inboxFilter{
		State:      state,
		Channel:    channel,
		AssignedMe: assigned == "me",
		Unassigned: assigned == "unassigned",
	}
}

// AssignedParam renders the assignment queue back to its `assigned=`
// query value ("me" / "unassigned" / ""). Exported so the filter
// template can carry the active queue into the state-pill links and the
// row hrefs without re-deriving it. Mutually exclusive by construction.
func (f inboxFilter) AssignedParam() string {
	switch {
	case f.AssignedMe:
		return "me"
	case f.Unassigned:
		return "unassigned"
	default:
		return ""
	}
}

// query renders the filter set back into a querystring (leading "?") so
// the row links can carry the active filters into GET
// /inbox/conversations/{id}; the view handler re-parses them to re-render
// the OOB list under the same filters (SIN-64966 §4.3, option a).
func (f inboxFilter) query() string {
	v := url.Values{}
	v.Set("state", f.State)
	v.Set("channel", f.Channel)
	v.Set("assigned", f.AssignedParam())
	return "?" + v.Encode()
}

// nonDefault reports whether any filter narrows past the default view
// (open / all channels / not-mine). It drives the empty-state copy:
// non-default + zero rows means "none with these filters" (offer a clear
// link); the default + zero rows means the tenant simply has no
// conversations yet.
func (f inboxFilter) nonDefault() bool {
	return f.State != "open" || f.Channel != "" || f.AssignedMe || f.Unassigned
}

// buildListRegion runs the read side and assembles the listRegionData the
// region template renders. It prefers the enriched ListSummaries use case
// (snippet + badges + filters); when that dep is nil it falls back to the
// legacy ListConversations (channel + timestamp only, open conversations)
// so the surface still renders. activeID marks the open conversation's
// row; oob flags the region for an out-of-band swap.
func (h *Handler) buildListRegion(r *http.Request, tenantID uuid.UUID, f inboxFilter, activeID uuid.UUID, oob bool) (listRegionData, error) {
	region := listRegionData{
		Filters:     f,
		HasFilters:  f.nonDefault(),
		OOB:         oob,
		FilterQuery: f.query(),
	}

	if h.deps.ListSummaries == nil {
		// Legacy fallback: the enriched read-model is not wired. Only the
		// open-state list is available, with no snippet/badges/filters.
		res, err := h.deps.ListConversations.Execute(r.Context(), inboxusecase.ListConversationsInput{
			TenantID: tenantID,
			State:    "open",
		})
		if err != nil {
			return listRegionData{}, err
		}
		region.Filters = inboxFilter{State: "open"}
		region.HasFilters = false
		region.FilterQuery = region.Filters.query()
		region.Items = make([]listRow, 0, len(res.Items))
		for _, c := range res.Items {
			region.Items = append(region.Items, listRow{
				ID:            c.ID,
				Channel:       c.Channel,
				LastMessageAt: c.LastMessageAt,
				Active:        c.ID == activeID,
			})
		}
		return region, nil
	}

	var assignedUserID uuid.UUID
	if f.AssignedMe {
		// The "minhas" filter is keyed to the authenticated session user;
		// a client-supplied id is never honoured. UserID may return
		// uuid.Nil when the session carries no user claim — the use case
		// then treats it as "no assignee filter", which stays tenant-scoped
		// (no cross-tenant leak), so this degrades safely.
		assignedUserID = h.deps.UserID(r)
	}

	res, err := h.deps.ListSummaries.Execute(r.Context(), inboxusecase.ListConversationSummariesInput{
		TenantID: tenantID,
		State:    f.State,
		Channel:  f.Channel,
		// AssignedUserID and Unassigned are mutually exclusive (the filter
		// carries a single `assigned` value), so the read-side use case
		// never sees the combination it rejects.
		AssignedUserID: assignedUserID,
		Unassigned:     f.Unassigned,
	})
	if err != nil {
		return listRegionData{}, err
	}
	region.Items = make([]listRow, 0, len(res.Items))
	for _, c := range res.Items {
		row := listRow{
			ID:            c.ID,
			Channel:       c.Channel,
			ContactName:   c.ContactDisplayName,
			Snippet:       c.LastMessageSnippet,
			OutboundLast:  c.LastMessageDirection == "out",
			AwaitingReply: c.AwaitingReply,
			Closed:        c.State == "closed",
			LastMessageAt: c.LastMessageAt,
			Active:        c.ID == activeID,
		}
		if c.AssignedUserLabel != nil {
			row.AssigneeLabel = *c.AssignedUserLabel
			row.AssigneeInitials = initials(*c.AssignedUserLabel)
		}
		region.Items = append(region.Items, row)
	}
	return region, nil
}

// view renders a single conversation pane (thread + compose form +
// optional AI-assist button + panel placeholder).
func (h *Handler) view(w http.ResponseWriter, r *http.Request) {
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
	res, err := h.deps.ListMessages.Execute(r.Context(), inboxusecase.ListMessagesInput{
		TenantID:       tenant.ID,
		ConversationID: conversationID,
	})
	if err != nil {
		if errors.Is(err, inboxusecase.ErrNotFound) {
			http.Error(w, http.StatusText(http.StatusNotFound), http.StatusNotFound)
			return
		}
		h.fail(w, http.StatusInternalServerError, "list messages", err)
		return
	}
	token := h.deps.CSRFToken(r)
	if token == "" {
		h.fail(w, http.StatusInternalServerError, "csrf token missing", errors.New("empty csrf token"))
		return
	}

	// Resolve the real conversation channel for the AI-assist policy
	// scope, the customer panel, and the view template (SIN-64969,
	// replacing the PR10 empty-scope stub). The read is best-effort:
	// ListMessages above already 404s a missing conversation, so a
	// failure here degrades to the empty scope rather than failing the
	// whole pane — the policy resolver then falls through to its
	// tenant-scope default, which is the safe behaviour.
	channel := ""
	var contextPanel contextPanelData
	// closed reflects the conversation lifecycle state (SIN-65473): it
	// drives the customer-actions Encerrar / Reabrir toggle and the compose
	// region. It stays false (open) when the context read is skipped or
	// fails, matching the graceful-degradation posture of the rest of this
	// handler.
	var closed bool
	if h.deps.ConversationContext != nil {
		ctxRes, err := h.deps.ConversationContext.Execute(r.Context(), inboxusecase.GetConversationContextInput{
			TenantID:       tenant.ID,
			ConversationID: conversationID,
		})
		if err != nil {
			h.deps.Logger.Warn("web/inbox: conversation context read", "err", err)
		} else {
			channel = ctxRes.Context.Channel
			closed = ctxRes.Context.Closed
			contextPanel = newContextPanelData(ctxRes.Context)
		}
	}

	// Enrich the context panel with the assignable-attendant list when
	// the dep is wired (SIN-64979). Best-effort: a load failure only
	// costs the interactive widget; the panel still renders read-only.
	// The same list feeds the customer-actions "Transferir conversa" form
	// (SIN-65473), so it is captured for the customer panel below.
	var assignees []AssignableRow
	if h.deps.ListAssignable != nil {
		contextPanel.ConversationIDStr = conversationID.String()
		rows, err := h.deps.ListAssignable.Execute(r.Context(), tenant.ID)
		if err != nil {
			h.deps.Logger.Warn("web/inbox: list assignable", "err", err)
		} else {
			assignees = rows
			contextPanel.Assignees = rows
			if uid := h.deps.UserID(r); uid != uuid.Nil {
				contextPanel.CurrentUserID = uid.String()
			}
			// Resolve the current assignee's display name from the
			// dropdown list so the badge reads a name, not an ID.
			contextPanel.AssignedDisplayName = assigneeDisplayName(rows, contextPanel.AssignedUserID)
		}
	}

	// Pre-render the assist button when the feature is wired. The team
	// scope stays empty (conversations carry no team affinity in v1);
	// the channel scope is now the real conversation channel.
	var assistHTML template.HTML
	if h.deps.AIAssist.Summarizer != nil {
		var buf strings.Builder
		if err := h.renderAssistButton(r.Context(), &buf, tenant.ID, conversationID, channel, "", token); err != nil {
			h.deps.Logger.Error("web/inbox: render assist button", "err", err)
		} else {
			assistHTML = template.HTML(buf.String())
		}
	}

	// Staleness hint (SIN-65474): when the conversation already has a
	// valid summary and a message arrived after it was generated, flag
	// the panel so the operator sees a "há novas mensagens" affordance
	// next to the refresh control. Computed entirely server-side (no
	// client polling) by comparing the newest message's CreatedAt to the
	// last summary's generated_at. Best-effort: a read error only drops
	// the hint, never the panel.
	assistStale := h.assistStale(r.Context(), tenant.ID, conversationID, res.Items)

	// Hydrate the customer panel through the optional loader. When the
	// loader is nil or errors we degrade to the empty CustomerInfo so
	// the panel still renders (with "Contato sem nome" + no metadata).
	var customer CustomerInfo
	if h.deps.CustomerInfo != nil {
		c, err := h.deps.CustomerInfo.Load(r.Context(), tenant.ID, conversationID)
		if err != nil {
			h.deps.Logger.Warn("web/inbox: customer info load failed", "err", err)
		} else {
			customer = c
		}
	}

	// Render the customer panel into a buffer and pass it to the view
	// template — the conversation view appends the customer-panel
	// markup with hx-swap-oob so HTMX swaps both panes in one round
	// trip.
	var customerHTML template.HTML
	{
		var buf strings.Builder
		if err := customerPanelTmpl.Execute(&buf, customerPanelData{
			HasConversation: true,
			ConversationID:  conversationID,
			Channel:         channel,
			Contact:         customer,
			AssistButton:    assistHTML,
			AssistStale:     assistStale,
			// Transfer + close actions (SIN-65473). Assignees enables the
			// "Transferir conversa" form (reusing the assign use case);
			// CanClose enables the Encerrar / Reabrir toggle; Closed selects
			// which side of the toggle renders.
			Assignees:   assignees,
			CanClose:    h.deps.CloseConversation != nil,
			CanUnassign: h.deps.UnassignConversation != nil,
			Closed:      closed,
		}); err != nil {
			h.deps.Logger.Error("web/inbox: render customer panel", "err", err)
		} else {
			customerHTML = template.HTML(buf.String())
		}
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := conversationViewTmpl.Execute(w, viewData{
		ConversationID: conversationID,
		Channel:        channel,
		Messages:       res.Items,
		CSRFInput:      csrf.FormHidden(token),
		CustomerPanel:  customerHTML,
		Context:        contextPanel,
		// Show the destructive "Apagar mensagens" action only for the
		// fakellm training thread and only when the reset use case is
		// wired (SIN-65392). Both conditions resolve server-side so the
		// button is absent — not merely hidden — on real conversations.
		ShowReset: h.deps.ResetConversation != nil && channel == inboxusecase.TrainingChannel,
		// Live-thread poll (SIN-65419): rendered only when the incremental
		// read side is wired. The cursor is the newest message's CreatedAt
		// so the first poll only fetches strictly-newer inbound replies.
		ShowLivePoll:   h.deps.ListMessagesSince != nil,
		LivePollCursor: liveCursor(res.Items),
		// Closed gates the compose region (SIN-65473): a closed conversation
		// renders the "conversa encerrada" notice instead of the form.
		Closed: closed,
	}); err != nil {
		h.deps.Logger.Error("web/inbox: render view", "err", err)
		return
	}

	// Out-of-band refresh of the list region so the opened conversation's
	// row gets the active marker (aria-current + accent) and the list
	// reflects the latest ordering/awaiting state — all server-driven, no
	// inline JS (SIN-64966 §4.3). The active filters ride in on the row's
	// query params (option a), so the OOB list stays under the same
	// filter set instead of silently resetting. Best-effort: a read error
	// here only costs the active marker (re-applied on the next list
	// load), so it must not fail the conversation swap. Skipped when the
	// enriched read side is not wired (the legacy list cannot be filtered).
	if h.deps.ListSummaries != nil {
		region, err := h.buildListRegion(r, tenant.ID, parseInboxFilter(r), conversationID, true)
		if err != nil {
			h.deps.Logger.Warn("web/inbox: oob list refresh", "err", err)
			return
		}
		if err := inboxListRegionTmpl.Execute(w, region); err != nil {
			h.deps.Logger.Error("web/inbox: render oob list", "err", err)
		}
	}
}

// send orchestrates an outbound message. Renders the new message bubble
// (HTMX swap target = #conversation-thread, hx-swap=beforeend) on
// success; returns 400 / 404 / 409 / 413 / 500 on failure.
func (h *Handler) send(w http.ResponseWriter, r *http.Request) {
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
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	body := strings.TrimSpace(r.PostFormValue("body"))
	if body == "" {
		http.Error(w, "body required", http.StatusBadRequest)
		return
	}
	if len(body) > maxBodyChars {
		http.Error(w, "body too long", http.StatusRequestEntityTooLarge)
		return
	}
	var sentByUserID *uuid.UUID
	if u := h.deps.UserID(r); u != uuid.Nil {
		uid := u
		sentByUserID = &uid
	}
	msg, err := h.deps.SendOutbound.SendForView(r.Context(), inboxusecase.SendOutboundInput{
		TenantID:       tenant.ID,
		ConversationID: conversationID,
		Body:           body,
		SentByUserID:   sentByUserID,
	})
	if err != nil {
		switch {
		case errors.Is(err, inboxusecase.ErrNotFound):
			http.Error(w, http.StatusText(http.StatusNotFound), http.StatusNotFound)
		case errors.Is(err, inboxusecase.ErrConversationClosed):
			http.Error(w, http.StatusText(http.StatusConflict), http.StatusConflict)
		default:
			h.fail(w, http.StatusInternalServerError, "send outbound", err)
		}
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := messageBubbleTmpl.Execute(w, msg); err != nil {
		h.deps.Logger.Error("web/inbox: render bubble", "err", err)
		return
	}
	// Out-of-band textarea reset: htmx swaps the live #compose-body by id
	// (pure DOM replacement, no eval), clearing the field after a
	// successful send. Replaces the old hx-on::after-request="this.reset()"
	// which threw EvalError under the prod strict CSP (SIN-65068).
	if err := composeTextareaTmpl.Execute(w, true); err != nil {
		h.deps.Logger.Error("web/inbox: render compose reset", "err", err)
	}
	// Out-of-band live-poll cursor advance (SIN-65419): the bubble above is
	// appended client-side, so the live thread poll must skip past the
	// just-sent message or its next tick would re-fetch and duplicate it.
	// Replacing the sentinel by id (hx-swap-oob="true") moves the cursor to
	// this message's CreatedAt; the subsequent poll then returns only the
	// later inbound reply. Emitted only when the live poll is wired.
	if h.deps.ListMessagesSince != nil {
		if err := threadLivePollTmpl.Execute(w, threadLivePollData{
			ConversationID: conversationID,
			Cursor:         strconv.FormatInt(msg.CreatedAt.UnixNano(), 10),
			OOB:            true,
		}); err != nil {
			h.deps.Logger.Error("web/inbox: render live poll cursor advance", "err", err)
		}
	}
}

// status is the realtime message-status partial that backs the bubble's
// hx-trigger="every 3s" polling loop (SIN-62736, ADR 0095). The handler
// looks up the message under the tenant + conversation scope and:
//
//   - returns 204 No Content when the caller's ?currentStatus= query
//     param matches the persisted status, or
//   - returns 200 + a re-rendered message_bubble partial when the status
//     changed. Final states (read/failed) render a bubble without the
//     polling attrs so HTMX's outerHTML swap stops the loop.
//
// The no-change response MUST be 204 (not 304): htmx 2.x's default
// responseHandling maps the "[23].." status range — which includes 304 —
// to swap:true, so a 304 with an empty body would make the outerHTML swap
// replace the polling <li> with nothing, deleting the outbound bubble from
// the thread (SIN-65389/SIN-65393). 204 is the only code htmx treats as an
// explicit no-swap, leaving the existing element (and its poll attrs)
// intact so it keeps polling and stays visible.
//
// Cache-Control: no-store keeps intermediate caches (CDN, browser) from
// pinning the partial — every poll MUST hit the origin so a freshly
// reconciled status surfaces in the UI within the next poll window.
func (h *Handler) status(w http.ResponseWriter, r *http.Request) {
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
	messageID, err := uuid.Parse(r.PathValue("msgID"))
	if err != nil {
		http.Error(w, "invalid message id", http.StatusBadRequest)
		return
	}
	res, err := h.deps.GetMessage.Execute(r.Context(), inboxusecase.GetMessageInput{
		TenantID:       tenant.ID,
		ConversationID: conversationID,
		MessageID:      messageID,
	})
	if err != nil {
		if errors.Is(err, inboxusecase.ErrNotFound) {
			http.Error(w, http.StatusText(http.StatusNotFound), http.StatusNotFound)
			return
		}
		h.fail(w, http.StatusInternalServerError, "get message", err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	if r.URL.Query().Get("currentStatus") == res.Message.Status {
		// No status change: 204 No Content. htmx 2.x treats 204 as an
		// explicit no-swap, so the existing <li> (with its every-3s poll
		// attrs) is left untouched. Returning 304 here would be swapped by
		// htmx's "[23].." default into outerHTML and delete the bubble
		// (SIN-65389/SIN-65393).
		w.WriteHeader(http.StatusNoContent)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := messageBubbleTmpl.Execute(w, res.Message); err != nil {
		h.deps.Logger.Error("web/inbox: render status bubble", "err", err)
	}
}

// since is the conversation thread live-refresh poll (SIN-65419). The
// open conversation pane polls this endpoint every few seconds through a
// hidden sentinel element that carries an exclusive cursor — the
// Unix-nanosecond CreatedAt of the last message the client already holds.
// The handler returns only the messages created after the cursor:
//
//   - 204 No Content when nothing is newer (the common case), so htmx
//     leaves the sentinel (and its poll trigger) untouched and keeps
//     polling. As with the status poll, the no-change code MUST be 204
//     and never 304: htmx 2.x swaps the "[23].." status range, so a 304
//     would be applied and wipe the sentinel (SIN-65389/SIN-65393).
//   - 200 + the new bubbles (OOB-appended to #conversation-thread) plus a
//     fresh sentinel carrying the advanced cursor, when there are new
//     messages.
//
// Security: the read is tenant-scoped (tenancy.FromContext + the
// use-case's tenant predicate), so it can never surface another tenant's
// messages; an unknown/RLS-hidden conversation collapses to 404 — the
// same opaque response the view and status handlers give. A malformed
// cursor is rejected at the boundary with 400.
//
// Cache-Control: no-store keeps every poll hitting the origin so a freshly
// persisted inbound reply surfaces within the next poll window.
func (h *Handler) since(w http.ResponseWriter, r *http.Request) {
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
	// The cursor is the Unix-nanosecond CreatedAt of the last message the
	// client holds. Absent ("") means the client thread is empty → cursor
	// 0, which the use case treats as "return all" (the correct first
	// fill). A present-but-unparseable cursor is a malformed request.
	var afterNanos int64
	if raw := r.URL.Query().Get("after"); raw != "" {
		afterNanos, err = strconv.ParseInt(raw, 10, 64)
		if err != nil {
			http.Error(w, "invalid cursor", http.StatusBadRequest)
			return
		}
	}
	res, err := h.deps.ListMessagesSince.Execute(r.Context(), inboxusecase.ListMessagesSinceInput{
		TenantID:       tenant.ID,
		ConversationID: conversationID,
		AfterUnixNano:  afterNanos,
	})
	if err != nil {
		if errors.Is(err, inboxusecase.ErrNotFound) {
			http.Error(w, http.StatusText(http.StatusNotFound), http.StatusNotFound)
			return
		}
		h.fail(w, http.StatusInternalServerError, "list messages since", err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	if len(res.Items) == 0 {
		// Nothing new: 204 No Content. htmx treats 204 as an explicit
		// no-swap, so the sentinel (with its every-3s poll attrs and the
		// unchanged cursor) is left intact and keeps polling. A re-poll with
		// the same cursor lands here, so the response is idempotent.
		w.WriteHeader(http.StatusNoContent)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := threadLiveUpdateTmpl.Execute(w, threadLiveUpdate{
		Poll: threadLivePollData{
			ConversationID: conversationID,
			Cursor:         liveCursor(res.Items),
		},
		Messages: res.Items,
	}); err != nil {
		h.deps.Logger.Error("web/inbox: render thread live update", "err", err)
	}
}

// assign handles POST /inbox/conversations/{id}/assign (SIN-64979).
// It validates the form-supplied targetUserID (UUID shape only — tenant
// and role checks are the use-case's responsibility), delegates to
// AssignConversation, then re-renders the assignment panel partial so
// HTMX can swap it in place without a full conversation reload.
// ErrAlreadyAssigned is treated as a successful idempotent no-op: the
// panel re-renders with the unchanged assignee so the operator sees
// visual confirmation that the assignment is still in effect.
func (h *Handler) assign(w http.ResponseWriter, r *http.Request) {
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
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	targetStr := strings.TrimSpace(r.PostFormValue("targetUserID"))
	if targetStr == "" {
		http.Error(w, "targetUserID required", http.StatusBadRequest)
		return
	}
	targetUserID, err := uuid.Parse(targetStr)
	if err != nil {
		http.Error(w, "targetUserID must be a valid UUID", http.StatusBadRequest)
		return
	}

	_, err = h.deps.AssignConversation.Execute(r.Context(), inboxusecase.AssignConversationInput{
		TenantID:       tenant.ID,
		ConversationID: conversationID,
		TargetUserID:   targetUserID,
	})
	if err != nil {
		switch {
		case errors.Is(err, inboxusecase.ErrAlreadyAssigned):
			// idempotent no-op: fall through to re-render the panel
		case errors.Is(err, inboxusecase.ErrNotFound):
			http.Error(w, http.StatusText(http.StatusNotFound), http.StatusNotFound)
			return
		case errors.Is(err, inboxusecase.ErrUserNotAssignable):
			http.Error(w, http.StatusText(http.StatusForbidden), http.StatusForbidden)
			return
		default:
			h.fail(w, http.StatusInternalServerError, "assign conversation", err)
			return
		}
	}

	// Re-render the assignment panel partial. Load the fresh attendant
	// list for the dropdown; on failure degrade to read-only.
	panel := h.buildAssignPanel(r.Context(), tenant.ID, conversationID, targetUserID)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := conversationAssignmentTmpl.Execute(w, panel); err != nil {
		h.deps.Logger.Error("web/inbox: render assign panel", "err", err)
	}
}

// reset handles POST /inbox/conversations/{id}/reset (SIN-65392). It
// deletes every message of a fakellm training conversation and clears the
// channel adapter's in-memory state, then returns the now-empty thread
// partial so HTMX swaps it in place (hx-target="#conversation-thread",
// hx-swap="outerHTML").
//
// Security: the underlying use case rejects any conversation whose
// channel is not the fakellm training channel with
// ErrConversationNotResettable, which the handler maps to 404 — identical
// to the unknown-conversation response — so the endpoint leaks no signal
// about real customer conversations. A missing/RLS-hidden id is likewise
// 404. The operation is idempotent: resetting an already-empty thread
// returns 200 with the empty partial.
//
// CSRF: the request rides the same protections as POST .../messages — the
// chi stack applies the CSRF middleware to the whole /inbox subtree and
// the form carries the hidden token field — so no extra check is needed
// here.
func (h *Handler) reset(w http.ResponseWriter, r *http.Request) {
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
	if _, err := h.deps.ResetConversation.Execute(r.Context(), inboxusecase.ResetConversationInput{
		TenantID:       tenant.ID,
		ConversationID: conversationID,
	}); err != nil {
		switch {
		case errors.Is(err, inboxusecase.ErrNotFound),
			errors.Is(err, inboxusecase.ErrConversationNotResettable):
			// Both collapse to 404 so the endpoint never confirms the
			// existence of a non-training conversation.
			http.Error(w, http.StatusText(http.StatusNotFound), http.StatusNotFound)
		default:
			h.fail(w, http.StatusInternalServerError, "reset conversation", err)
		}
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// Render the thread template with no messages → an empty
	// <ol id="conversation-thread"> that HTMX swaps in via outerHTML.
	if err := conversationThreadTmpl.Execute(w, nil); err != nil {
		h.deps.Logger.Error("web/inbox: render empty thread", "err", err)
	}
}

// stateActionData drives the standalone conversation_state_action render
// emitted by the close / reopen handlers (SIN-65473). The customer panel
// renders the same partial inline from customerPanelData, which exposes the
// same two field names.
type stateActionData struct {
	ConversationID uuid.UUID
	Closed         bool
}

// close handles POST /inbox/conversations/{id}/close (SIN-65473). It flips
// the conversation to the closed lifecycle state via CloseConversation,
// then re-renders the Encerrar / Reabrir toggle (hx-target
// "#conversation-state-action") and out-of-band swaps the compose region so
// the now-dead outbound form is replaced by the "conversa encerrada"
// notice in the same response.
//
// Security: tenant scope comes from the request context; the use case loads
// the conversation under that scope so an unknown / RLS-hidden id collapses
// to 404 (the IDOR guard), identical to assign/reset. The route inherits
// the /inbox subtree's RequireAuth → RequireAction(ActionTenantInboxRead) →
// RequireCSRF envelope from the chi router; the form carries no extra token
// because the layout's hx-headers propagates X-CSRF-Token on every HTMX
// request. Closing is idempotent (double-click safe).
func (h *Handler) close(w http.ResponseWriter, r *http.Request) {
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
	if _, err := h.deps.CloseConversation.Execute(r.Context(), inboxusecase.CloseConversationInput{
		TenantID:       tenant.ID,
		ConversationID: conversationID,
	}); err != nil {
		if errors.Is(err, inboxusecase.ErrNotFound) {
			http.Error(w, http.StatusText(http.StatusNotFound), http.StatusNotFound)
			return
		}
		h.fail(w, http.StatusInternalServerError, "close conversation", err)
		return
	}
	h.renderStateAction(w, r, conversationID, true)
}

// reopen handles POST /inbox/conversations/{id}/reopen (SIN-65473), the
// inverse of close: it lifts a closed conversation back to open and
// re-renders the toggle + compose region (now the live outbound form).
// Same security envelope and idempotency posture as close.
func (h *Handler) reopen(w http.ResponseWriter, r *http.Request) {
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
	if _, err := h.deps.ReopenConversation.Execute(r.Context(), inboxusecase.ReopenConversationInput{
		TenantID:       tenant.ID,
		ConversationID: conversationID,
	}); err != nil {
		if errors.Is(err, inboxusecase.ErrNotFound) {
			http.Error(w, http.StatusText(http.StatusNotFound), http.StatusNotFound)
			return
		}
		h.fail(w, http.StatusInternalServerError, "reopen conversation", err)
		return
	}
	h.renderStateAction(w, r, conversationID, false)
}

// transferUnassignValue is the sentinel option value the Transferir
// select submits for "— Não atribuído —" (SIN-65480). It is deliberately
// NOT a UUID so the assign path's uuid.Parse rejects it if it ever leaks
// there; h.transfer intercepts it first and routes to the unassign use
// case. The customerPanelTmpl renders the matching <option value="...">,
// and a guard test pins the two literals together.
const transferUnassignValue = "unassigned"

// transfer handles POST /inbox/conversations/{id}/transfer. It dispatches
// on the submitted targetUserID: the transferUnassignValue sentinel
// routes to the unassign path (SIN-65480); any other value is the same
// reassignment operation as assign (SIN-65473) and is delegated verbatim
// to h.assign. ParseForm is idempotent, so re-parsing inside h.assign is
// safe.
func (h *Handler) transfer(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(r.PostFormValue("targetUserID")) == transferUnassignValue {
		h.unassign(w, r)
		return
	}
	h.assign(w, r)
}

// unassign handles the "— Não atribuído —" branch of POST .../transfer
// (SIN-65480): it returns the conversation to the unassigned state via
// UnassignConversation, then re-renders the assignment-section partial so
// HTMX swaps the chip to "Não atribuída" in place. A nil use case (the
// option should not have rendered) collapses to 404 so a hand-crafted
// POST cannot reach an unwired path. ErrConversationClosed maps to 409
// (the close gate); ErrNotFound (unknown / cross-tenant id, IDOR) to 404.
func (h *Handler) unassign(w http.ResponseWriter, r *http.Request) {
	if h.deps.UnassignConversation == nil {
		http.Error(w, http.StatusText(http.StatusNotFound), http.StatusNotFound)
		return
	}
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
	if _, err := h.deps.UnassignConversation.Execute(r.Context(), inboxusecase.UnassignConversationInput{
		TenantID:       tenant.ID,
		ConversationID: conversationID,
	}); err != nil {
		switch {
		case errors.Is(err, inboxusecase.ErrNotFound):
			http.Error(w, http.StatusText(http.StatusNotFound), http.StatusNotFound)
			return
		case errors.Is(err, inboxusecase.ErrConversationClosed):
			http.Error(w, http.StatusText(http.StatusConflict), http.StatusConflict)
			return
		default:
			h.fail(w, http.StatusInternalServerError, "unassign conversation", err)
			return
		}
	}

	// Re-render the assignment panel in the unassigned state (assignedUserID
	// = uuid.Nil → "Não atribuída"). Same partial and swap target as the
	// assign path so the OOB chip swap is symmetric.
	panel := h.buildAssignPanel(r.Context(), tenant.ID, conversationID, uuid.Nil)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := conversationAssignmentTmpl.Execute(w, panel); err != nil {
		h.deps.Logger.Error("web/inbox: render unassign panel", "err", err)
	}
}

// renderStateAction writes the close/reopen response: the
// conversation_state_action toggle as the primary swap target plus an
// out-of-band re-render of the compose region (enabled when open, replaced
// by the closed notice when closed). Both fragments are coherent with the
// new lifecycle state so the operator sees the action reflected without a
// full conversation reload.
func (h *Handler) renderStateAction(w http.ResponseWriter, r *http.Request, conversationID uuid.UUID, closed bool) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := conversationStateActionTmpl.Execute(w, stateActionData{
		ConversationID: conversationID,
		Closed:         closed,
	}); err != nil {
		h.deps.Logger.Error("web/inbox: render state action", "err", err)
		return
	}
	// Out-of-band compose swap so the outbound form toggles with the state.
	// The CSRF hidden field mirrors the conversation view; an empty token is
	// non-fatal here because the layout's hx-headers carries the token on
	// the HTMX request regardless.
	if err := conversationComposeTmpl.Execute(w, composeView{
		ConversationID: conversationID,
		CSRFInput:      csrf.FormHidden(h.deps.CSRFToken(r)),
		Closed:         closed,
		OOB:            true,
	}); err != nil {
		h.deps.Logger.Error("web/inbox: render compose oob", "err", err)
	}
}

// buildAssignPanel assembles the contextPanelData subset the assignment
// partial needs. It calls ListAssignable (when wired) to populate the
// dropdown and resolves the new assignee's display name from the list.
func (h *Handler) buildAssignPanel(ctx context.Context, tenantID, conversationID, assignedUserID uuid.UUID) contextPanelData {
	panel := contextPanelData{
		ConversationIDStr: conversationID.String(),
		Assigned:          assignedUserID != uuid.Nil,
		AssignedUserID:    assignedUserID.String(),
	}
	if h.deps.ListAssignable == nil {
		return panel
	}
	assignees, err := h.deps.ListAssignable.Execute(ctx, tenantID)
	if err != nil {
		h.deps.Logger.Warn("web/inbox: list assignable for assign panel", "err", err)
		return panel
	}
	panel.Assignees = assignees
	panel.AssignedDisplayName = assigneeDisplayName(assignees, assignedUserID.String())
	return panel
}

// assigneeDisplayName resolves an attendant's human label from the
// assignable list by user id (string form, as the templates carry it).
// Returns "" when the id is empty or not present in the list (no
// directory match), so the caller falls back to the unassigned styling.
// Shared by the conversation view and the post-assign panel rebuild so
// the lookup lives in one place.
func assigneeDisplayName(assignees []AssignableRow, userID string) string {
	if userID == "" {
		return ""
	}
	for _, a := range assignees {
		if a.UserID.String() == userID {
			return a.DisplayName
		}
	}
	return ""
}

// liveCursor renders the live-poll cursor for a message slice: the
// Unix-nanosecond CreatedAt of the last (newest) message, as a decimal
// string. An integer cursor sidesteps all URL-escaping and timezone
// pitfalls a formatted timestamp would carry in the poll's hx-get query.
// Returns "" for an empty slice so a conversation that rendered with no
// messages emits an empty cursor — the since handler then treats it as
// "return all" (the correct first fill). Messages are oldest-first, so
// the last element is the newest.
func liveCursor(items []inboxusecase.MessageView) string {
	if len(items) == 0 {
		return ""
	}
	return strconv.FormatInt(items[len(items)-1].CreatedAt.UnixNano(), 10)
}

// buildInboxNavItems returns the SidebarNav primary nav for the inbox
// page (SIN-65104). It mirrors funnel's buildFunnelNavItems so the two
// post-login surfaces share one nav, with "Inbox" marked active here so
// the shell stamps aria-current="page" on it. The brand link back to
// /hello-tenant is owned by the shell layout.
func buildInboxNavItems() []shell.NavItem {
	return []shell.NavItem{
		{Label: "Inbox", Path: "/inbox", Active: true},
		{Label: "Funil", Path: "/funnel"},
	}
}

// buildInboxUserMenu returns the user-menu dropdown entries for an
// authenticated inbox session (logout only, matching funnel).
func buildInboxUserMenu() []shell.UserMenuItem {
	return []shell.UserMenuItem{
		{Label: "Sair", Path: "/logout", Form: true},
	}
}

// displayNameForUser is the placeholder display formatter for the
// user-menu button. The session does not (yet) carry a human label, so
// we render the uuid prefix — replace once a user-name resolver lands.
// Mirrors internal/web/funnel.displayNameForUser; kept local because the
// two web packages do not share a helper module.
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

// fail centralises the error reporting + log path. The response body
// never carries the underlying error text — error detail goes to logs.
func (h *Handler) fail(w http.ResponseWriter, status int, msg string, err error) {
	h.deps.Logger.Error("web/inbox: "+msg, "err", err)
	http.Error(w, http.StatusText(status), status)
}

// listRow is the row shape consumed by the conversation_list template.
// The legacy fallback path populates only ID / Channel / LastMessageAt;
// the enriched path fills the contact name, snippet (+ direction), the
// awaiting-reply / closed flags, and the assignee label + initials.
type listRow struct {
	ID               uuid.UUID
	Channel          string
	ContactName      string
	Snippet          string
	OutboundLast     bool // last message was outbound → "Você:" snippet prefix
	AwaitingReply    bool // last message inbound → waiting badge + accent
	Closed           bool // conversation state == "closed" → closed badge
	AssigneeLabel    string
	AssigneeInitials string
	LastMessageAt    time.Time
	Active           bool // this row is the open conversation (aria-current)
}

// listRegionData drives the inbox_list_region template (filter bar +
// conversation list). It is the single swap target for filter changes
// and for the OOB row-active refresh, so it carries both the current
// filters and the row set. FilterQuery is the encoded "?state=…" the row
// links append so opening a conversation preserves the active filters.
type listRegionData struct {
	Filters     inboxFilter
	Items       []listRow
	HasFilters  bool   // non-default filters active → "none with filters" empty copy
	OOB         bool   // render the wrapper with hx-swap-oob="true"
	FilterQuery string // "?state=…&channel=…&assigned=…" for row links
}

// layoutData drives the full-page inbox shell template. SIN-65104 wraps
// the inbox in the global SidebarNav app-shell (internal/web/shell), so
// the struct now carries the shell.Data chrome fields by name — the shell
// layout's reflection helpers (shellTenantName, shellNavItems, …) read
// them off this struct verbatim. The inbox content (list + customer
// panes) rides alongside in List/Customer.
type layoutData struct {
	// shell.Data chrome fields (read by shell.Layout reflection helpers).
	TenantName      string
	TenantLogo      string
	UserDisplayName string
	NavItems        []shell.NavItem
	UserMenuItems   []shell.UserMenuItem
	// CSRFToken feeds the shell's <meta>, hx-headers, and form-hidden
	// slots. Empty is a programming error the handler rejects as 500.
	CSRFToken        string
	TenantThemeStyle template.CSS
	// CSPNonce carries the per-request CSP nonce (SIN-63275). Empty
	// when csp.Middleware is absent — the template still emits the
	// attribute so the browser blocks the inline tag (fail-closed).
	CSPNonce string

	// Inbox content panes.
	List     listRegionData
	Customer customerPanelData
}

// viewData drives the middle (conversation) pane template. The
// customer pane is delivered alongside it as an OOB swap (CustomerPanel
// is pre-rendered HTML appended to the response body).
type viewData struct {
	ConversationID uuid.UUID
	Channel        string
	Messages       []inboxusecase.MessageView
	CSRFInput      template.HTML
	CustomerPanel  template.HTML
	// ShowReset gates the destructive "Apagar mensagens" button
	// (SIN-65392). It is true only for the fakellm training channel with
	// the reset use case wired; the template renders the button solely on
	// this flag so the destructive action is structurally absent from
	// every real conversation.
	ShowReset bool
	// ShowLivePoll gates the conversation thread live-refresh sentinel
	// (SIN-65419). True only when the ListMessagesSince use case is wired;
	// the template renders the hidden poll element solely on this flag so
	// unwired deployments keep the static thread. LivePollCursor seeds the
	// sentinel's exclusive cursor (the newest message's Unix-nanosecond
	// CreatedAt, "" when the thread is empty).
	ShowLivePoll   bool
	LivePollCursor string
	// Context drives the conversation context side panel (SIN-64970):
	// contact identity, channel, funnel stage, and assignment state. Its
	// zero value (HasContext=false) renders the panel's degraded
	// "contexto indisponível" state, so a skipped or failed context read
	// never breaks the conversation pane.
	Context contextPanelData
	// Closed reflects the conversation lifecycle state (SIN-65473). When
	// true the compose region renders the "conversa encerrada" notice
	// instead of the outbound form — sending is already rejected server-side
	// (ErrConversationClosed → 409), this hides the dead form. The close /
	// reopen handlers swap the region out-of-band to keep it coherent.
	Closed bool
}

// threadLivePollData drives the conversation thread live-refresh sentinel
// (SIN-65419) — the hidden element that polls GET
// .../messages/since every few seconds. Cursor is the exclusive
// Unix-nanosecond cursor (the newest message the client holds); OOB is
// true when the element is re-emitted as an out-of-band swap (the send
// handler advancing the cursor past a just-sent message) and false for
// the in-place renders (initial view + the poll's own outerHTML refresh).
type threadLivePollData struct {
	ConversationID uuid.UUID
	Cursor         string
	OOB            bool
}

// threadLiveUpdate drives the since handler's 200 response: a fresh
// sentinel (Poll, carrying the advanced cursor, replacing the old one in
// place) plus the new message bubbles, OOB-appended to the thread so they
// land at the end regardless of where the sentinel sits.
type threadLiveUpdate struct {
	Poll     threadLivePollData
	Messages []inboxusecase.MessageView
}

// contextPanelData is the web-local projection of
// inboxusecase.ConversationContextView (SIN-64970, frontend half of
// SIN-64959) that backs the conversation context side panel. Keeping it
// local — rather than handing the use-case view straight to the template
// — decouples the template from the use-case shape and lets each block
// degrade independently: an empty ContactName, nil Identities, an empty
// FunnelStageName, or Assigned=false each collapse their own section
// instead of breaking the layout (partial-data tolerance).
//
// HasContext is false when the context read was skipped (no wired
// ConversationContext use case) or failed; the panel then renders its
// "contexto indisponível" state rather than a half-empty card.
type contextPanelData struct {
	HasContext      bool
	Channel         string
	ContactName     string
	Identities      []contextIdentity
	FunnelStageName string
	FunnelStageKey  string
	Assigned        bool
	AssignedUserID  string
	// Assignment widget (SIN-64979). Non-nil Assignees enables the
	// interactive dropdown + "Atribuir a mim" button; nil leaves the
	// section read-only. ConversationIDStr drives the form action URL.
	// CurrentUserID enables the "Atribuir a mim" shortcut (empty when
	// the session has no user claim). AssignedDisplayName is the
	// resolved label for the current lead (empty when unresolved).
	Assignees           []AssignableRow
	ConversationIDStr   string
	CurrentUserID       string
	AssignedDisplayName string
}

// contextIdentity is one contact channel identity (e.g. a WhatsApp
// phone) shown in the side panel's "Identidades" list.
type contextIdentity struct {
	Channel    string
	ExternalID string
}

// newContextPanelData projects a use-case ConversationContextView onto
// the template-facing contextPanelData. It is total: every field maps
// straight through and the optional AssignedUserID pointer is rendered
// as its string form only when present, so the template never has to
// dereference a pointer.
func newContextPanelData(v inboxusecase.ConversationContextView) contextPanelData {
	d := contextPanelData{
		HasContext:        true,
		Channel:           v.Channel,
		ContactName:       v.ContactDisplayName,
		FunnelStageName:   v.FunnelStageName,
		FunnelStageKey:    v.FunnelStageKey,
		Assigned:          v.Assigned,
		ConversationIDStr: v.ConversationID.String(),
	}
	if v.AssignedUserID != nil {
		d.AssignedUserID = v.AssignedUserID.String()
	}
	if len(v.ContactIdentities) > 0 {
		d.Identities = make([]contextIdentity, 0, len(v.ContactIdentities))
		for _, id := range v.ContactIdentities {
			d.Identities = append(d.Identities, contextIdentity{
				Channel:    id.Channel,
				ExternalID: id.ExternalID,
			})
		}
	}
	return d
}

// customerPanelData drives the right-rail customer panel. HasConversation
// is false on the initial empty render (the layout placeholder) and true
// when the view handler hydrates the panel for a selected conversation.
type customerPanelData struct {
	HasConversation bool
	ConversationID  uuid.UUID
	Channel         string
	Contact         CustomerInfo
	// AssistButton is the pre-rendered HTMX fragment for the SIN-62908
	// "Resumir + sugerir 3 respostas" button — empty when the assist
	// feature is not wired (the panel falls back to a disabled-state
	// hint then).
	AssistButton template.HTML
	// AssistStale is true when a valid summary exists but a newer message
	// has arrived since it was generated (SIN-65474). The panel then
	// renders a "há novas mensagens" hint next to the refresh control.
	AssistStale bool
	// Assignees feeds the customer-actions "Transferir conversa" form
	// (SIN-65473). When non-empty the panel renders the transfer select +
	// submit (posting to .../transfer, which reuses the assign use case);
	// when empty the button stays disabled. Mirrors the context panel's
	// assignment dropdown so both surfaces share the eligible-attendant list.
	Assignees []AssignableRow
	// CanClose enables the customer-actions Encerrar / Reabrir toggle
	// (SIN-65473). False leaves the "Encerrar conversa" button disabled (the
	// pre-feature stub) when the close use case is not wired.
	CanClose bool
	// CanUnassign renders the "— Não atribuído —" option inside the
	// Transferir select (SIN-65480). False (the unassign use case not wired)
	// omits the option entirely — deny-by-default: an action the server
	// cannot perform is never offered.
	CanUnassign bool
	// Closed reflects the conversation lifecycle state so the toggle renders
	// "Reabrir conversa" + a closed badge instead of "Encerrar conversa",
	// and the compose region renders the closed notice instead of the form.
	Closed bool
}

// assistStale reports whether the conversation's latest valid summary is
// older than its newest message. It returns false when the reader port
// is not wired, when no valid summary exists yet, when the thread is
// empty, or on any read error (the hint is a best-effort affordance, not
// a correctness gate). The comparison is strict — a message generated in
// the same instant as the summary is not "newer".
func (h *Handler) assistStale(ctx context.Context, tenantID, conversationID uuid.UUID, items []inboxusecase.MessageView) bool {
	if h.deps.AIAssist.SummaryReader == nil || len(items) == 0 {
		return false
	}
	generatedAt, exists, err := h.deps.AIAssist.SummaryReader.LatestSummaryGeneratedAt(ctx, tenantID, conversationID)
	if err != nil {
		h.deps.Logger.Warn("web/inbox: assist staleness read", "err", err)
		return false
	}
	if !exists {
		return false
	}
	// Items are ordered oldest-first (same contract liveCursor relies on),
	// so the last element is the newest message.
	newest := items[len(items)-1].CreatedAt
	return newest.After(generatedAt)
}
