package inbox_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	inboxusecase "github.com/pericles-luz/crm/internal/inbox/usecase"
	webinbox "github.com/pericles-luz/crm/internal/web/inbox"
)

// stubSince is the test double for the SIN-65419 incremental read side. It
// captures the call args (so tests assert the tenant scope is the request
// tenant, never a client value) and returns a fixed result/error.
type stubSince struct {
	mu     sync.Mutex
	in     inboxusecase.ListMessagesSinceInput
	called bool
	res    inboxusecase.ListMessagesSinceResult
	err    error
}

func (s *stubSince) Execute(_ context.Context, in inboxusecase.ListMessagesSinceInput) (inboxusecase.ListMessagesSinceResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.in = in
	s.called = true
	return s.res, s.err
}

func newLiveHandler(t *testing.T, since webinbox.ListMessagesSinceUseCase, msgs webinbox.ListMessagesUseCase) *webinbox.Handler {
	t.Helper()
	h, err := webinbox.New(webinbox.Deps{
		ListConversations: &stubLister{},
		ListMessages:      msgs,
		ListMessagesSince: since,
		SendOutbound:      &stubSender{},
		GetMessage:        &stubGetMessage{},
		CSRFToken:         func(*http.Request) string { return "tok" },
		UserID:            func(*http.Request) uuid.UUID { return uuid.Nil },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return h
}

// TestSince_AppendsNewBubblesAndAdvancesCursor pins the AC-(a) happy path:
// new inbound messages come back as bubbles OOB-appended to the thread,
// plus a fresh sentinel carrying the advanced cursor (the newest message's
// UnixNano), so the next poll only fetches strictly-newer messages.
func TestSince_AppendsNewBubblesAndAdvancesCursor(t *testing.T) {
	t.Parallel()
	tenant := uuid.New()
	convID := uuid.New()
	newest := time.Now().UTC()
	since := &stubSince{res: inboxusecase.ListMessagesSinceResult{Items: []inboxusecase.MessageView{{
		ID:             uuid.New(),
		ConversationID: convID,
		Direction:      "in",
		Body:           "auto-reply do cliente",
		Status:         "delivered",
		CreatedAt:      newest,
	}}}}
	h := newLiveHandler(t, since, &stubMessages{})
	mux := http.NewServeMux()
	h.Routes(mux)

	target := "/inbox/conversations/" + convID.String() + "/messages/since?after=123"
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, reqWithTenant(http.MethodGet, target, "", tenant))

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200 body=%q", rec.Code, rec.Body.String())
	}
	if !since.called || since.in.TenantID != tenant || since.in.ConversationID != convID {
		t.Fatalf("use case not called with request scope: %+v", since.in)
	}
	if since.in.AfterUnixNano != 123 {
		t.Fatalf("cursor: got %d want 123", since.in.AfterUnixNano)
	}
	body := rec.Body.String()
	// The new inbound bubble must be present.
	if !strings.Contains(body, `class="message-bubble msg-in"`) {
		t.Errorf("missing inbound bubble: %q", body)
	}
	if !strings.Contains(body, "auto-reply do cliente") {
		t.Errorf("missing reply text: %q", body)
	}
	// Bubbles must be appended to the END of the thread via an OOB swap.
	if !strings.Contains(body, `hx-swap-oob="beforeend:#conversation-thread"`) {
		t.Errorf("missing OOB beforeend wrapper: %q", body)
	}
	// A fresh sentinel must carry the advanced cursor (newest UnixNano).
	wantCursor := "after=" + strconv.FormatInt(newest.UnixNano(), 10)
	if !strings.Contains(body, `id="thread-live-poll"`) || !strings.Contains(body, wantCursor) {
		t.Errorf("missing advanced sentinel cursor %q in: %q", wantCursor, body)
	}
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("Cache-Control: got %q want no-store", rec.Header().Get("Cache-Control"))
	}
}

// TestSince_NoChangeReturns204 pins AC-(b): an unchanged poll returns 204
// (never 304, which htmx would swap and wipe the thread — SIN-65393) and
// no body, so the sentinel is left polling.
func TestSince_NoChangeReturns204(t *testing.T) {
	t.Parallel()
	tenant := uuid.New()
	convID := uuid.New()
	since := &stubSince{res: inboxusecase.ListMessagesSinceResult{Items: nil}}
	h := newLiveHandler(t, since, &stubMessages{})
	mux := http.NewServeMux()
	h.Routes(mux)

	target := "/inbox/conversations/" + convID.String() + "/messages/since?after=999"
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, reqWithTenant(http.MethodGet, target, "", tenant))

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status: got %d want 204 body=%q", rec.Code, rec.Body.String())
	}
	if rec.Code == http.StatusNotModified {
		t.Fatalf("no-change MUST be 204, never 304 (SIN-65393)")
	}
	if rec.Body.Len() != 0 {
		t.Errorf("204 must carry no body, got %q", rec.Body.String())
	}
}

// TestSince_EmptyCursorMeansZero verifies an absent ?after= is forwarded as
// the zero cursor (the use case then returns all — the correct first fill
// for a thread that rendered empty).
func TestSince_EmptyCursorMeansZero(t *testing.T) {
	t.Parallel()
	tenant := uuid.New()
	convID := uuid.New()
	since := &stubSince{res: inboxusecase.ListMessagesSinceResult{Items: nil}}
	h := newLiveHandler(t, since, &stubMessages{})
	mux := http.NewServeMux()
	h.Routes(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, reqWithTenant(http.MethodGet, "/inbox/conversations/"+convID.String()+"/messages/since", "", tenant))

	if !since.called || since.in.AfterUnixNano != 0 {
		t.Fatalf("absent cursor must forward 0, got %d called=%v", since.in.AfterUnixNano, since.called)
	}
}

func TestSince_MalformedCursorReturns400(t *testing.T) {
	t.Parallel()
	tenant := uuid.New()
	convID := uuid.New()
	since := &stubSince{}
	h := newLiveHandler(t, since, &stubMessages{})
	mux := http.NewServeMux()
	h.Routes(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, reqWithTenant(http.MethodGet, "/inbox/conversations/"+convID.String()+"/messages/since?after=not-a-number", "", tenant))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d want 400", rec.Code)
	}
	if since.called {
		t.Errorf("use case must not run on a malformed cursor")
	}
}

func TestSince_BadConversationIDReturns400(t *testing.T) {
	t.Parallel()
	since := &stubSince{}
	h := newLiveHandler(t, since, &stubMessages{})
	mux := http.NewServeMux()
	h.Routes(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, reqWithTenant(http.MethodGet, "/inbox/conversations/not-a-uuid/messages/since?after=1", "", uuid.New()))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d want 400", rec.Code)
	}
}

func TestSince_MapsErrNotFoundTo404(t *testing.T) {
	t.Parallel()
	since := &stubSince{err: inboxusecase.ErrNotFound}
	h := newLiveHandler(t, since, &stubMessages{})
	mux := http.NewServeMux()
	h.Routes(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, reqWithTenant(http.MethodGet, "/inbox/conversations/"+uuid.New().String()+"/messages/since?after=1", "", uuid.New()))

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status: got %d want 404", rec.Code)
	}
}

func TestSince_MapsGenericErrorTo500(t *testing.T) {
	t.Parallel()
	since := &stubSince{err: errors.New("boom")}
	h := newLiveHandler(t, since, &stubMessages{})
	mux := http.NewServeMux()
	h.Routes(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, reqWithTenant(http.MethodGet, "/inbox/conversations/"+uuid.New().String()+"/messages/since?after=1", "", uuid.New()))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status: got %d want 500", rec.Code)
	}
}

func TestSince_FailsWhenTenantMissing(t *testing.T) {
	t.Parallel()
	since := &stubSince{}
	h := newLiveHandler(t, since, &stubMessages{})
	mux := http.NewServeMux()
	h.Routes(mux)

	// No tenant in context → 500 (the chi stack injects it in prod).
	req := httptest.NewRequest(http.MethodGet, "/inbox/conversations/"+uuid.New().String()+"/messages/since?after=1", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status: got %d want 500", rec.Code)
	}
	if since.called {
		t.Errorf("use case must not run without a tenant scope")
	}
}

// TestSince_RouteAbsentWhenUnwired verifies the live-poll route is not
// registered when the dep is nil — deployments without it keep the static
// thread and a probe of the endpoint 404s at the mux.
func TestSince_RouteAbsentWhenUnwired(t *testing.T) {
	t.Parallel()
	h := newHandler(t, &stubLister{}, &stubMessages{}, &stubSender{}) // no ListMessagesSince
	mux := http.NewServeMux()
	h.Routes(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, reqWithTenant(http.MethodGet, "/inbox/conversations/"+uuid.New().String()+"/messages/since?after=1", "", uuid.New()))

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status: got %d want 404 (route must be unregistered)", rec.Code)
	}
}

// TestView_RendersLivePollSentinelWhenWired checks the conversation view
// embeds the hidden poll sentinel (seeded with the newest message's cursor)
// only when the live read side is wired.
func TestView_RendersLivePollSentinelWhenWired(t *testing.T) {
	t.Parallel()
	tenant := uuid.New()
	convID := uuid.New()
	newest := time.Now().UTC()
	msgs := &stubMessages{res: inboxusecase.ListMessagesResult{Items: []inboxusecase.MessageView{{
		ID:             uuid.New(),
		ConversationID: convID,
		Direction:      "in",
		Body:           "oi",
		Status:         "delivered",
		CreatedAt:      newest,
	}}}}

	// Wired: sentinel present with the newest cursor.
	h := newLiveHandler(t, &stubSince{}, msgs)
	mux := http.NewServeMux()
	h.Routes(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, reqWithTenant(http.MethodGet, "/inbox/conversations/"+convID.String(), "", tenant))
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200 body=%q", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, `id="thread-live-poll"`) {
		t.Errorf("wired view missing live-poll sentinel: %q", body)
	}
	if !strings.Contains(body, "after="+strconv.FormatInt(newest.UnixNano(), 10)) {
		t.Errorf("sentinel missing seeded cursor: %q", body)
	}

	// Unwired: no sentinel.
	h2 := newHandler(t, &stubLister{}, msgs, &stubSender{})
	mux2 := http.NewServeMux()
	h2.Routes(mux2)
	rec2 := httptest.NewRecorder()
	mux2.ServeHTTP(rec2, reqWithTenant(http.MethodGet, "/inbox/conversations/"+convID.String(), "", tenant))
	if strings.Contains(rec2.Body.String(), `id="thread-live-poll"`) {
		t.Errorf("unwired view must not render the sentinel: %q", rec2.Body.String())
	}
}

// TestSend_AdvancesLivePollCursorWhenWired pins the no-duplicate-bubble
// guard: a successful send emits an OOB sentinel that moves the cursor past
// the just-sent message so the next poll never re-fetches it.
func TestSend_AdvancesLivePollCursorWhenWired(t *testing.T) {
	t.Parallel()
	tenant := uuid.New()
	convID := uuid.New()
	sentAt := time.Now().UTC()
	sender := &stubSender{response: inboxusecase.MessageView{
		ID:             uuid.New(),
		ConversationID: convID,
		Direction:      "out",
		Body:           "resposta do operador",
		Status:         "sent",
		CreatedAt:      sentAt,
	}}
	h, err := webinbox.New(webinbox.Deps{
		ListConversations: &stubLister{},
		ListMessages:      &stubMessages{},
		ListMessagesSince: &stubSince{},
		SendOutbound:      sender,
		GetMessage:        &stubGetMessage{},
		CSRFToken:         func(*http.Request) string { return "tok" },
		UserID:            func(*http.Request) uuid.UUID { return uuid.Nil },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	mux := http.NewServeMux()
	h.Routes(mux)

	form := "body=" + "ol%C3%A1"
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, reqWithTenant(http.MethodPost, "/inbox/conversations/"+convID.String()+"/messages", form, tenant))

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200 body=%q", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	// OOB sentinel advancing the cursor to the just-sent message.
	if !strings.Contains(body, `id="thread-live-poll"`) || !strings.Contains(body, `hx-swap-oob="true"`) {
		t.Errorf("send missing OOB live-poll cursor advance: %q", body)
	}
	if !strings.Contains(body, "after="+strconv.FormatInt(sentAt.UnixNano(), 10)) {
		t.Errorf("send sentinel missing advanced cursor: %q", body)
	}
}
