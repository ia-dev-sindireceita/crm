package inbox_test

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

	inboxusecase "github.com/pericles-luz/crm/internal/inbox/usecase"
	"github.com/pericles-luz/crm/internal/tenancy"
	webinbox "github.com/pericles-luz/crm/internal/web/inbox"
)

// stubLister captures the ListConversations call args and returns a
// preconfigured result/error.
type stubLister struct {
	mu     sync.Mutex
	in     inboxusecase.ListConversationsInput
	called bool
	res    inboxusecase.ListConversationsResult
	err    error
}

func (s *stubLister) Execute(_ context.Context, in inboxusecase.ListConversationsInput) (inboxusecase.ListConversationsResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.in = in
	s.called = true
	return s.res, s.err
}

// stubMessages captures the ListMessages call args.
type stubMessages struct {
	mu  sync.Mutex
	in  inboxusecase.ListMessagesInput
	res inboxusecase.ListMessagesResult
	err error
}

func (s *stubMessages) Execute(_ context.Context, in inboxusecase.ListMessagesInput) (inboxusecase.ListMessagesResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.in = in
	return s.res, s.err
}

// stubSender captures the SendOutbound call args.
type stubSender struct {
	mu       sync.Mutex
	in       inboxusecase.SendOutboundInput
	called   bool
	response inboxusecase.MessageView
	err      error
}

func (s *stubSender) SendForView(_ context.Context, in inboxusecase.SendOutboundInput) (inboxusecase.MessageView, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.in = in
	s.called = true
	return s.response, s.err
}

func newHandler(t *testing.T, lister webinbox.ListConversationsUseCase, msgs webinbox.ListMessagesUseCase, sender webinbox.SendOutboundUseCase) *webinbox.Handler {
	t.Helper()
	h, err := webinbox.New(webinbox.Deps{
		ListConversations: lister,
		ListMessages:      msgs,
		SendOutbound:      sender,
		CSRFToken:         func(*http.Request) string { return "csrf-test-token" },
		UserID:            func(*http.Request) uuid.UUID { return uuid.Nil },
	})
	if err != nil {
		t.Fatalf("webinbox.New: %v", err)
	}
	return h
}

// reqWithTenant attaches a tenant to the request context so the handler
// reads tenancy.FromContext like the production middleware injected
// one.
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
	full := webinbox.Deps{
		ListConversations: &stubLister{},
		ListMessages:      &stubMessages{},
		SendOutbound:      &stubSender{},
		CSRFToken:         func(*http.Request) string { return "tok" },
		UserID:            func(*http.Request) uuid.UUID { return uuid.Nil },
	}
	// Sanity: full deps construct cleanly.
	if _, err := webinbox.New(full); err != nil {
		t.Fatalf("New(full): %v", err)
	}
	cases := map[string]webinbox.Deps{
		"missing ListConversations": {ListMessages: full.ListMessages, SendOutbound: full.SendOutbound, CSRFToken: full.CSRFToken, UserID: full.UserID},
		"missing ListMessages":      {ListConversations: full.ListConversations, SendOutbound: full.SendOutbound, CSRFToken: full.CSRFToken, UserID: full.UserID},
		"missing SendOutbound":      {ListConversations: full.ListConversations, ListMessages: full.ListMessages, CSRFToken: full.CSRFToken, UserID: full.UserID},
		"missing CSRFToken":         {ListConversations: full.ListConversations, ListMessages: full.ListMessages, SendOutbound: full.SendOutbound, UserID: full.UserID},
		"missing UserID":            {ListConversations: full.ListConversations, ListMessages: full.ListMessages, SendOutbound: full.SendOutbound, CSRFToken: full.CSRFToken},
	}
	for name, deps := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := webinbox.New(deps); err == nil {
				t.Fatalf("expected error, got nil")
			}
		})
	}
}

func TestList_RendersLayoutAndAppliesTenantFromContext(t *testing.T) {
	t.Parallel()
	tenant := uuid.New()
	convID := uuid.New()
	lister := &stubLister{
		res: inboxusecase.ListConversationsResult{
			Items: []inboxusecase.ConversationView{{
				ID:            convID,
				Channel:       "whatsapp",
				LastMessageAt: time.Now().Add(-3 * time.Minute),
			}},
		},
	}
	h := newHandler(t, lister, &stubMessages{}, &stubSender{})
	mux := http.NewServeMux()
	h.Routes(mux)

	rec := httptest.NewRecorder()
	r := reqWithTenant(http.MethodGet, "/inbox", "", tenant)
	mux.ServeHTTP(rec, r)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200; body=%q", rec.Code, rec.Body.String())
	}
	if !lister.called {
		t.Fatalf("ListConversations.Execute not called")
	}
	if lister.in.TenantID != tenant {
		t.Fatalf("tenant: got %s want %s", lister.in.TenantID, tenant)
	}
	if lister.in.State != "open" {
		t.Fatalf("state filter: got %q want open", lister.in.State)
	}
	body := rec.Body.String()
	for _, want := range []string{
		`<meta name="csrf-token"`,
		`hx-headers='{"X-CSRF-Token"`,
		`<ul class="conversation-list"`,
		`/inbox/conversations/` + convID.String(),
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q", want)
		}
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("Content-Type: %q", ct)
	}
}

func TestList_FailsWhenTenantMissing(t *testing.T) {
	t.Parallel()
	h := newHandler(t, &stubLister{}, &stubMessages{}, &stubSender{})
	mux := http.NewServeMux()
	h.Routes(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/inbox", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status: got %d want 500", rec.Code)
	}
}

func TestList_FailsWhenUseCaseErrors(t *testing.T) {
	t.Parallel()
	lister := &stubLister{err: errors.New("boom")}
	h := newHandler(t, lister, &stubMessages{}, &stubSender{})
	mux := http.NewServeMux()
	h.Routes(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, reqWithTenant(http.MethodGet, "/inbox", "", uuid.New()))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status: got %d want 500", rec.Code)
	}
}

func TestList_FailsWhenCSRFTokenEmpty(t *testing.T) {
	t.Parallel()
	lister := &stubLister{}
	h, err := webinbox.New(webinbox.Deps{
		ListConversations: lister,
		ListMessages:      &stubMessages{},
		SendOutbound:      &stubSender{},
		CSRFToken:         func(*http.Request) string { return "" },
		UserID:            func(*http.Request) uuid.UUID { return uuid.Nil },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	mux := http.NewServeMux()
	h.Routes(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, reqWithTenant(http.MethodGet, "/inbox", "", uuid.New()))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status: got %d want 500", rec.Code)
	}
}

func TestView_RendersThreadAndComposeForm(t *testing.T) {
	t.Parallel()
	tenant := uuid.New()
	convID := uuid.New()
	msgID := uuid.New()
	created := time.Now().Add(-5 * time.Minute)
	msgs := &stubMessages{
		res: inboxusecase.ListMessagesResult{Items: []inboxusecase.MessageView{{
			ID:             msgID,
			ConversationID: convID,
			Direction:      "in",
			Body:           "olá mundo",
			Status:         "delivered",
			CreatedAt:      created,
		}}},
	}
	h := newHandler(t, &stubLister{}, msgs, &stubSender{})
	mux := http.NewServeMux()
	h.Routes(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, reqWithTenant(http.MethodGet, "/inbox/conversations/"+convID.String(), "", tenant))

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d body=%q", rec.Code, rec.Body.String())
	}
	if msgs.in.ConversationID != convID || msgs.in.TenantID != tenant {
		t.Fatalf("call args: got %+v", msgs.in)
	}
	body := rec.Body.String()
	for _, want := range []string{
		`id="conversation-thread"`,
		`olá mundo`,
		`data-status="delivered"`,
		`name="_csrf"`,
		`hx-post="/inbox/conversations/` + convID.String() + `/messages"`,
		`name="body"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q", want)
		}
	}
}

func TestView_RejectsBadUUID(t *testing.T) {
	t.Parallel()
	h := newHandler(t, &stubLister{}, &stubMessages{}, &stubSender{})
	mux := http.NewServeMux()
	h.Routes(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, reqWithTenant(http.MethodGet, "/inbox/conversations/not-a-uuid", "", uuid.New()))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d want 400", rec.Code)
	}
}

func TestView_MapsErrNotFoundTo404(t *testing.T) {
	t.Parallel()
	msgs := &stubMessages{err: inboxusecase.ErrNotFound}
	h := newHandler(t, &stubLister{}, msgs, &stubSender{})
	mux := http.NewServeMux()
	h.Routes(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, reqWithTenant(http.MethodGet, "/inbox/conversations/"+uuid.New().String(), "", uuid.New()))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status: got %d want 404", rec.Code)
	}
}

func TestView_FailsWhenTenantMissing(t *testing.T) {
	t.Parallel()
	h := newHandler(t, &stubLister{}, &stubMessages{}, &stubSender{})
	mux := http.NewServeMux()
	h.Routes(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/inbox/conversations/"+uuid.New().String(), nil))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status: got %d want 500", rec.Code)
	}
}

func TestView_ListMessagesErrorMapsTo500(t *testing.T) {
	t.Parallel()
	msgs := &stubMessages{err: errors.New("boom")}
	h := newHandler(t, &stubLister{}, msgs, &stubSender{})
	mux := http.NewServeMux()
	h.Routes(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, reqWithTenant(http.MethodGet, "/inbox/conversations/"+uuid.New().String(), "", uuid.New()))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status: got %d want 500", rec.Code)
	}
}

func TestView_FailsWhenCSRFTokenEmpty(t *testing.T) {
	t.Parallel()
	convID := uuid.New()
	msgs := &stubMessages{res: inboxusecase.ListMessagesResult{}}
	h, err := webinbox.New(webinbox.Deps{
		ListConversations: &stubLister{},
		ListMessages:      msgs,
		SendOutbound:      &stubSender{},
		CSRFToken:         func(*http.Request) string { return "" },
		UserID:            func(*http.Request) uuid.UUID { return uuid.Nil },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	mux := http.NewServeMux()
	h.Routes(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, reqWithTenant(http.MethodGet, "/inbox/conversations/"+convID.String(), "", uuid.New()))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status: got %d want 500", rec.Code)
	}
}

func TestSend_HappyPath_RendersBubbleAndCallsUseCase(t *testing.T) {
	t.Parallel()
	tenant := uuid.New()
	convID := uuid.New()
	msgID := uuid.New()
	user := uuid.New()
	sender := &stubSender{response: inboxusecase.MessageView{
		ID:             msgID,
		ConversationID: convID,
		Direction:      "out",
		Body:           "olá!",
		Status:         "sent",
		CreatedAt:      time.Now(),
	}}
	h, err := webinbox.New(webinbox.Deps{
		ListConversations: &stubLister{},
		ListMessages:      &stubMessages{},
		SendOutbound:      sender,
		CSRFToken:         func(*http.Request) string { return "tok" },
		UserID:            func(*http.Request) uuid.UUID { return user },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	mux := http.NewServeMux()
	h.Routes(mux)

	form := "body=" + "ol%C3%A1%21"
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, reqWithTenant(http.MethodPost, "/inbox/conversations/"+convID.String()+"/messages", form, tenant))

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200 body=%q", rec.Code, rec.Body.String())
	}
	if !sender.called {
		t.Fatalf("SendOutbound not called")
	}
	if sender.in.TenantID != tenant || sender.in.ConversationID != convID {
		t.Fatalf("ids: got %+v", sender.in)
	}
	if sender.in.SentByUserID == nil || *sender.in.SentByUserID != user {
		t.Fatalf("sent_by_user_id: got %v want %v", sender.in.SentByUserID, user)
	}
	if sender.in.Body != "olá!" {
		t.Fatalf("body trimmed: got %q", sender.in.Body)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `class="message-bubble msg-out"`) {
		t.Errorf("body missing msg-out class: %q", body)
	}
	if !strings.Contains(body, `data-status="sent"`) {
		t.Errorf("body missing data-status: %q", body)
	}
	if !strings.Contains(body, "olá!") {
		t.Errorf("body missing rendered body text: %q", body)
	}
}

func TestSend_RejectsEmptyBody(t *testing.T) {
	t.Parallel()
	h := newHandler(t, &stubLister{}, &stubMessages{}, &stubSender{})
	mux := http.NewServeMux()
	h.Routes(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, reqWithTenant(http.MethodPost, "/inbox/conversations/"+uuid.New().String()+"/messages", "body=", uuid.New()))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d want 400", rec.Code)
	}
}

func TestSend_RejectsBodyTooLong(t *testing.T) {
	t.Parallel()
	h := newHandler(t, &stubLister{}, &stubMessages{}, &stubSender{})
	mux := http.NewServeMux()
	h.Routes(mux)

	big := strings.Repeat("a", 4097)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, reqWithTenant(http.MethodPost, "/inbox/conversations/"+uuid.New().String()+"/messages", "body="+big, uuid.New()))
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status: got %d want 413", rec.Code)
	}
}

func TestSend_RejectsBadUUID(t *testing.T) {
	t.Parallel()
	h := newHandler(t, &stubLister{}, &stubMessages{}, &stubSender{})
	mux := http.NewServeMux()
	h.Routes(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, reqWithTenant(http.MethodPost, "/inbox/conversations/bad-id/messages", "body=hello", uuid.New()))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d want 400", rec.Code)
	}
}

func TestSend_MapsErrConversationClosedTo409(t *testing.T) {
	t.Parallel()
	sender := &stubSender{err: inboxusecase.ErrConversationClosed}
	h := newHandler(t, &stubLister{}, &stubMessages{}, sender)
	mux := http.NewServeMux()
	h.Routes(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, reqWithTenant(http.MethodPost, "/inbox/conversations/"+uuid.New().String()+"/messages", "body=hi", uuid.New()))
	if rec.Code != http.StatusConflict {
		t.Fatalf("status: got %d want 409", rec.Code)
	}
}

func TestSend_MapsErrNotFoundTo404(t *testing.T) {
	t.Parallel()
	sender := &stubSender{err: inboxusecase.ErrNotFound}
	h := newHandler(t, &stubLister{}, &stubMessages{}, sender)
	mux := http.NewServeMux()
	h.Routes(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, reqWithTenant(http.MethodPost, "/inbox/conversations/"+uuid.New().String()+"/messages", "body=hi", uuid.New()))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status: got %d want 404", rec.Code)
	}
}

func TestSend_MapsGenericErrorTo500(t *testing.T) {
	t.Parallel()
	sender := &stubSender{err: errors.New("boom")}
	h := newHandler(t, &stubLister{}, &stubMessages{}, sender)
	mux := http.NewServeMux()
	h.Routes(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, reqWithTenant(http.MethodPost, "/inbox/conversations/"+uuid.New().String()+"/messages", "body=hi", uuid.New()))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status: got %d want 500", rec.Code)
	}
}

func TestSend_FailsWhenTenantMissing(t *testing.T) {
	t.Parallel()
	h := newHandler(t, &stubLister{}, &stubMessages{}, &stubSender{})
	mux := http.NewServeMux()
	h.Routes(mux)

	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/inbox/conversations/"+uuid.New().String()+"/messages", strings.NewReader("body=hi"))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	mux.ServeHTTP(rec, r)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status: got %d want 500", rec.Code)
	}
}

func TestSend_NoUserIDPropagatesNilSentBy(t *testing.T) {
	t.Parallel()
	tenant := uuid.New()
	convID := uuid.New()
	sender := &stubSender{response: inboxusecase.MessageView{Direction: "out", Body: "x", Status: "sent", CreatedAt: time.Now()}}
	h, err := webinbox.New(webinbox.Deps{
		ListConversations: &stubLister{},
		ListMessages:      &stubMessages{},
		SendOutbound:      sender,
		CSRFToken:         func(*http.Request) string { return "tok" },
		UserID:            func(*http.Request) uuid.UUID { return uuid.Nil },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	mux := http.NewServeMux()
	h.Routes(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, reqWithTenant(http.MethodPost, "/inbox/conversations/"+convID.String()+"/messages", "body=x", tenant))
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d body=%q", rec.Code, rec.Body.String())
	}
	if sender.in.SentByUserID != nil {
		t.Fatalf("sent_by_user_id should be nil when UserID returns Nil; got %v", sender.in.SentByUserID)
	}
}

// Regression: list-conversations propagates tenant id, and the
// template uses the truncate funcmap helper for snippets — verify the
// rendered list renders the channel cell at minimum even when the
// snippet is empty (the default for PR9; snippets land in PR10).
func TestList_RendersWithEmptySnippet(t *testing.T) {
	t.Parallel()
	convID := uuid.New()
	lister := &stubLister{
		res: inboxusecase.ListConversationsResult{
			Items: []inboxusecase.ConversationView{{
				ID:            convID,
				Channel:       "whatsapp",
				LastMessageAt: time.Time{}, // explicitly zero
			}},
		},
	}
	h := newHandler(t, lister, &stubMessages{}, &stubSender{})
	mux := http.NewServeMux()
	h.Routes(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, reqWithTenant(http.MethodGet, "/inbox", "", uuid.New()))
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d body=%q", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `class="conversation-list__channel">whatsapp`) {
		t.Errorf("body missing channel cell: %q", rec.Body.String())
	}
}

func TestList_EmptyStateRendered(t *testing.T) {
	t.Parallel()
	lister := &stubLister{res: inboxusecase.ListConversationsResult{Items: nil}}
	h := newHandler(t, lister, &stubMessages{}, &stubSender{})
	mux := http.NewServeMux()
	h.Routes(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, reqWithTenant(http.MethodGet, "/inbox", "", uuid.New()))
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d body=%q", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "Nenhuma conversa.") {
		t.Errorf("expected empty-state placeholder in body: %q", rec.Body.String())
	}
}
