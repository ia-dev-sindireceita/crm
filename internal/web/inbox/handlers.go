package inbox

import (
	"context"
	"errors"
	"html/template"
	"log/slog"
	"net/http"
	"net/url"
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
	SendOutbound  SendOutboundUseCase
	GetMessage    GetMessageUseCase
	CSRFToken     CSRFTokenFn
	UserID        UserIDFn
	Logger        *slog.Logger
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
	if h.deps.AIAssist.Summarizer != nil {
		mux.HandleFunc("POST /inbox/conversations/{id}/ai-assist", h.aiAssist)
	}
	if h.deps.AssignConversation != nil {
		mux.HandleFunc("POST /inbox/conversations/{id}/assign", h.assign)
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
	if h.deps.ConversationContext != nil {
		ctxRes, err := h.deps.ConversationContext.Execute(r.Context(), inboxusecase.GetConversationContextInput{
			TenantID:       tenant.ID,
			ConversationID: conversationID,
		})
		if err != nil {
			h.deps.Logger.Warn("web/inbox: conversation context read", "err", err)
		} else {
			channel = ctxRes.Context.Channel
			contextPanel = newContextPanelData(ctxRes.Context)
		}
	}

	// Enrich the context panel with the assignable-attendant list when
	// the dep is wired (SIN-64979). Best-effort: a load failure only
	// costs the interactive widget; the panel still renders read-only.
	if h.deps.ListAssignable != nil {
		contextPanel.ConversationIDStr = conversationID.String()
		assignees, err := h.deps.ListAssignable.Execute(r.Context(), tenant.ID)
		if err != nil {
			h.deps.Logger.Warn("web/inbox: list assignable", "err", err)
		} else {
			contextPanel.Assignees = assignees
			if uid := h.deps.UserID(r); uid != uuid.Nil {
				contextPanel.CurrentUserID = uid.String()
			}
			// Resolve the current assignee's display name from the
			// dropdown list so the badge reads a name, not an ID.
			contextPanel.AssignedDisplayName = assigneeDisplayName(assignees, contextPanel.AssignedUserID)
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
}

// status is the realtime message-status partial that backs the bubble's
// hx-trigger="every 3s" polling loop (SIN-62736, ADR 0095). The handler
// looks up the message under the tenant + conversation scope and:
//
//   - returns 304 Not Modified when the caller's ?currentStatus= query
//     param matches the persisted status (HTMX's default no-swap), or
//   - returns 200 + a re-rendered message_bubble partial when the status
//     changed. Final states (read/failed) render a bubble without the
//     polling attrs so HTMX's outerHTML swap stops the loop.
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
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := messageBubbleTmpl.Execute(w, res.Message); err != nil {
		h.deps.Logger.Error("web/inbox: render status bubble", "err", err)
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
	// Context drives the conversation context side panel (SIN-64970):
	// contact identity, channel, funnel stage, and assignment state. Its
	// zero value (HasContext=false) renders the panel's degraded
	// "contexto indisponível" state, so a skipped or failed context read
	// never breaks the conversation pane.
	Context contextPanelData
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
}
