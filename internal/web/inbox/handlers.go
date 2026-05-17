package inbox

import (
	"context"
	"errors"
	"html/template"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/adapter/httpapi/csrf"
	inboxusecase "github.com/pericles-luz/crm/internal/inbox/usecase"
	"github.com/pericles-luz/crm/internal/tenancy"
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
	ListMessages      ListMessagesUseCase
	SendOutbound      SendOutboundUseCase
	GetMessage        GetMessageUseCase
	CSRFToken         CSRFTokenFn
	UserID            UserIDFn
	Logger            *slog.Logger
	// AIAssist wires the optional SIN-62908 ai-assist feature. The
	// nested Summarizer field is the activation switch.
	AIAssist AssistDeps
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
}

// list renders the full inbox shell (left list + empty right pane).
func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	tenant, err := tenancy.FromContext(r.Context())
	if err != nil {
		h.fail(w, http.StatusInternalServerError, "tenant required", err)
		return
	}
	res, err := h.deps.ListConversations.Execute(r.Context(), inboxusecase.ListConversationsInput{
		TenantID: tenant.ID,
		State:    "open",
	})
	if err != nil {
		h.fail(w, http.StatusInternalServerError, "list conversations", err)
		return
	}
	rows := make([]listRow, 0, len(res.Items))
	for _, c := range res.Items {
		rows = append(rows, listRow{
			ID:            c.ID,
			Channel:       c.Channel,
			Snippet:       "", // hydrated in PR10 (last-message snapshot)
			LastMessageAt: c.LastMessageAt,
		})
	}
	token := h.deps.CSRFToken(r)
	if token == "" {
		h.fail(w, http.StatusInternalServerError, "csrf token missing", errors.New("empty csrf token"))
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := inboxLayoutTmpl.Execute(w, layoutData{
		CSRFMeta:  csrf.MetaTag(token),
		HXHeaders: csrf.HXHeadersAttr(token),
		List:      listData{Items: rows},
	}); err != nil {
		h.deps.Logger.Error("web/inbox: render layout", "err", err)
	}
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

	// Pre-render the assist button when the feature is wired. The
	// channel/team scope is currently empty (filled by a future read
	// use case, see PR10) — the policy resolver falls through to the
	// tenant-scope row, which is the correct default.
	var assistHTML template.HTML
	if h.deps.AIAssist.Summarizer != nil {
		var buf strings.Builder
		if err := h.renderAssistButton(r.Context(), &buf, tenant.ID, conversationID, "", "", token); err != nil {
			h.deps.Logger.Error("web/inbox: render assist button", "err", err)
		} else {
			assistHTML = template.HTML(buf.String())
		}
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := conversationViewTmpl.Execute(w, viewData{
		ConversationID: conversationID,
		Channel:        "", // filled by a future read use case (PR10)
		Messages:       res.Items,
		CSRFInput:      csrf.FormHidden(token),
		AssistButton:   assistHTML,
	}); err != nil {
		h.deps.Logger.Error("web/inbox: render view", "err", err)
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

// fail centralises the error reporting + log path. The response body
// never carries the underlying error text — error detail goes to logs.
func (h *Handler) fail(w http.ResponseWriter, status int, msg string, err error) {
	h.deps.Logger.Error("web/inbox: "+msg, "err", err)
	http.Error(w, http.StatusText(status), status)
}

// listRow is the row shape consumed by conversation_list.templ. The
// template references .ID / .Channel / .Snippet / .LastMessageAt.
type listRow struct {
	ID            uuid.UUID
	Channel       string
	Snippet       string
	LastMessageAt time.Time
}

// listData wraps the row slice for the inbox-layout template.
type listData struct {
	Items []listRow
}

// layoutData drives the full-page inbox shell template.
type layoutData struct {
	CSRFMeta  template.HTML
	HXHeaders template.HTMLAttr
	List      listData
}

// viewData drives the right-pane conversation view template. AssistButton
// is the pre-rendered HTMX fragment for the SIN-62908 assist button —
// empty when the assist feature is not wired (default).
type viewData struct {
	ConversationID uuid.UUID
	Channel        string
	Messages       []inboxusecase.MessageView
	CSRFInput      template.HTML
	AssistButton   template.HTML
}
