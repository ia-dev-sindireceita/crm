package inbox_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/google/uuid"

	inboxusecase "github.com/pericles-luz/crm/internal/inbox/usecase"
	webinbox "github.com/pericles-luz/crm/internal/web/inbox"
)

// stubClose / stubReopen capture the close/reopen call args and return a
// configurable result/error so the handler tests can assert routing, status
// mapping, and the rendered partials without a real adapter (SIN-65473).
type stubClose struct {
	in     inboxusecase.CloseConversationInput
	called bool
	res    inboxusecase.CloseConversationResult
	err    error
}

func (s *stubClose) Execute(_ context.Context, in inboxusecase.CloseConversationInput) (inboxusecase.CloseConversationResult, error) {
	s.called = true
	s.in = in
	return s.res, s.err
}

type stubReopen struct {
	in     inboxusecase.ReopenConversationInput
	called bool
	res    inboxusecase.ReopenConversationResult
	err    error
}

func (s *stubReopen) Execute(_ context.Context, in inboxusecase.ReopenConversationInput) (inboxusecase.ReopenConversationResult, error) {
	s.called = true
	s.in = in
	return s.res, s.err
}

// stubConvCtxState returns a context view with a configurable Closed flag so
// the view-rendering tests can exercise both sides of the Encerrar / Reabrir
// toggle and the compose region.
type stubConvCtxState struct {
	channel string
	closed  bool
}

func (s *stubConvCtxState) Execute(_ context.Context, in inboxusecase.GetConversationContextInput) (inboxusecase.GetConversationContextResult, error) {
	return inboxusecase.GetConversationContextResult{
		Context: inboxusecase.ConversationContextView{
			ConversationID: in.ConversationID,
			Channel:        s.channel,
			Closed:         s.closed,
		},
	}, nil
}

// newCloseHandler wires a Handler with the close/reopen use cases plus the
// assign + list-assignable deps (so the transfer form renders) and a
// configurable conversation-context stub for the view path.
func newCloseHandler(t *testing.T, closer webinbox.CloseConversationUseCase, reopener webinbox.ReopenConversationUseCase, ctxUC webinbox.GetConversationContextUseCase) (*webinbox.Handler, *http.ServeMux) {
	t.Helper()
	h, err := webinbox.New(webinbox.Deps{
		ListConversations:   &stubLister{},
		ListMessages:        &stubMessages{},
		SendOutbound:        &stubSender{},
		GetMessage:          &stubGetMessage{},
		CSRFToken:           func(*http.Request) string { return "csrf-test-token" },
		UserID:              func(*http.Request) uuid.UUID { return uuid.Nil },
		AssignConversation:  &stubAssigner{},
		ListAssignable:      &stubListAssignable{rows: []webinbox.AssignableRow{{UserID: uuid.New(), DisplayName: "Ana"}}},
		ConversationContext: ctxUC,
		CloseConversation:   closer,
		ReopenConversation:  reopener,
	})
	if err != nil {
		t.Fatalf("webinbox.New: %v", err)
	}
	mux := http.NewServeMux()
	h.Routes(mux)
	return h, mux
}

func TestClose_RoutesRegisteredOnlyWhenWired(t *testing.T) {
	t.Parallel()
	// Without the close dep, the close/reopen routes are absent → 404.
	h := newHandler(t, &stubLister{}, &stubMessages{}, &stubSender{})
	mux := http.NewServeMux()
	h.Routes(mux)
	for _, path := range []string{"/close", "/reopen"} {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, reqWithTenant(http.MethodPost, "/inbox/conversations/"+uuid.New().String()+path, "", uuid.New()))
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status=%d for %s, want 404 (route unregistered)", rec.Code, path)
		}
	}
}

func TestClose_ClosesAndRendersReopenToggle(t *testing.T) {
	t.Parallel()
	closer := &stubClose{}
	h, mux := newCloseHandler(t, closer, &stubReopen{}, nil)
	_ = h
	tenant := uuid.New()
	conv := uuid.New()
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, reqWithTenant(http.MethodPost, "/inbox/conversations/"+conv.String()+"/close", "", tenant))

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200; body=%q", rec.Code, rec.Body.String())
	}
	if !closer.called || closer.in.TenantID != tenant || closer.in.ConversationID != conv {
		t.Fatalf("close args=%+v, want tenant=%v conv=%v", closer.in, tenant, conv)
	}
	body := rec.Body.String()
	// Primary swap: the closed badge + Reabrir button replace the toggle.
	if !strings.Contains(body, `data-testid="conversation-closed-badge"`) {
		t.Fatalf("response missing closed badge; got %q", body)
	}
	if !strings.Contains(body, `data-testid="conversation-reopen"`) {
		t.Fatalf("response missing reopen button; got %q", body)
	}
	if !strings.Contains(body, `/reopen"`) {
		t.Fatalf("reopen form missing hx-post target; got %q", body)
	}
	// OOB compose swap: the closed notice replaces the outbound form.
	if !strings.Contains(body, `id="conversation-compose"`) || !strings.Contains(body, `hx-swap-oob="outerHTML"`) {
		t.Fatalf("response missing OOB compose region; got %q", body)
	}
	if !strings.Contains(body, `data-testid="conversation-closed-notice"`) {
		t.Fatalf("closed compose region missing the closed notice; got %q", body)
	}
}

func TestReopen_ReopensAndRendersCloseToggle(t *testing.T) {
	t.Parallel()
	reopener := &stubReopen{}
	h, mux := newCloseHandler(t, &stubClose{}, reopener, nil)
	_ = h
	tenant := uuid.New()
	conv := uuid.New()
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, reqWithTenant(http.MethodPost, "/inbox/conversations/"+conv.String()+"/reopen", "", tenant))

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200; body=%q", rec.Code, rec.Body.String())
	}
	if !reopener.called || reopener.in.ConversationID != conv {
		t.Fatalf("reopen args=%+v, want conv=%v", reopener.in, conv)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `data-testid="conversation-close"`) {
		t.Fatalf("response missing close button; got %q", body)
	}
	// OOB compose swap restores the live outbound form.
	if !strings.Contains(body, `id="conversation-compose"`) || !strings.Contains(body, `/messages"`) {
		t.Fatalf("reopened compose region missing the outbound form; got %q", body)
	}
	if strings.Contains(body, `data-testid="conversation-closed-notice"`) {
		t.Fatalf("reopened compose region must not show the closed notice; got %q", body)
	}
}

func TestClose_NotFoundMapsTo404(t *testing.T) {
	t.Parallel()
	closer := &stubClose{err: inboxusecase.ErrNotFound}
	_, mux := newCloseHandler(t, closer, &stubReopen{}, nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, reqWithTenant(http.MethodPost, "/inbox/conversations/"+uuid.New().String()+"/close", "", uuid.New()))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d, want 404 for unknown conversation", rec.Code)
	}
}

func TestClose_InvalidConversationID(t *testing.T) {
	t.Parallel()
	closer := &stubClose{}
	_, mux := newCloseHandler(t, closer, &stubReopen{}, nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, reqWithTenant(http.MethodPost, "/inbox/conversations/not-a-uuid/close", "", uuid.New()))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d, want 400 for invalid id", rec.Code)
	}
	if closer.called {
		t.Fatal("use case must not run for an invalid conversation id")
	}
}

func TestClose_FailsWhenTenantMissing(t *testing.T) {
	t.Parallel()
	closer := &stubClose{}
	_, mux := newCloseHandler(t, closer, &stubReopen{}, nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/inbox/conversations/"+uuid.New().String()+"/close", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d, want 500 when tenant missing", rec.Code)
	}
	if closer.called {
		t.Fatal("use case must not run without a tenant")
	}
}

func TestReopen_NotFoundMapsTo404(t *testing.T) {
	t.Parallel()
	reopener := &stubReopen{err: inboxusecase.ErrNotFound}
	_, mux := newCloseHandler(t, &stubClose{}, reopener, nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, reqWithTenant(http.MethodPost, "/inbox/conversations/"+uuid.New().String()+"/reopen", "", uuid.New()))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d, want 404 for unknown conversation", rec.Code)
	}
}

func TestReopen_InvalidConversationIDAndTenantMissing(t *testing.T) {
	t.Parallel()
	reopener := &stubReopen{}
	_, mux := newCloseHandler(t, &stubClose{}, reopener, nil)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, reqWithTenant(http.MethodPost, "/inbox/conversations/not-a-uuid/reopen", "", uuid.New()))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d, want 400 for invalid id", rec.Code)
	}

	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/inbox/conversations/"+uuid.New().String()+"/reopen", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d, want 500 when tenant missing", rec.Code)
	}
	if reopener.called {
		t.Fatal("use case must not run without a tenant / valid id")
	}
}

// TestReopen_RouteRequiresReopenDep proves the reopen route is gated on the
// ReopenConversation dep even when CloseConversation is wired — a closed
// conversation without a reopen path is the trap this guards against.
func TestReopen_RouteRequiresReopenDep(t *testing.T) {
	t.Parallel()
	h, err := webinbox.New(webinbox.Deps{
		ListConversations: &stubLister{},
		ListMessages:      &stubMessages{},
		SendOutbound:      &stubSender{},
		GetMessage:        &stubGetMessage{},
		CSRFToken:         func(*http.Request) string { return "t" },
		UserID:            func(*http.Request) uuid.UUID { return uuid.Nil },
		CloseConversation: &stubClose{},
		// ReopenConversation intentionally nil.
	})
	if err != nil {
		t.Fatalf("webinbox.New: %v", err)
	}
	mux := http.NewServeMux()
	h.Routes(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, reqWithTenant(http.MethodPost, "/inbox/conversations/"+uuid.New().String()+"/reopen", "", uuid.New()))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d, want 404 (reopen route absent without dep)", rec.Code)
	}
}

// TestView_RendersActionsAndIsCSPSafe pins the customer-actions panel: the
// Transferir form, the Encerrar toggle, and (when closed) the Reabrir button
// — all driven by plain hx-* attributes with no inline on*/hx-on handlers
// that the strict CSP would silently no-op or reject as EvalError.
func TestView_RendersActionsAndIsCSPSafe(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		closed bool
	}{
		{"open conversation", false},
		{"closed conversation", true},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, mux := newCloseHandler(t, &stubClose{}, &stubReopen{}, &stubConvCtxState{channel: "whatsapp", closed: tc.closed})
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, reqWithTenant(http.MethodGet, "/inbox/conversations/"+uuid.New().String(), "", uuid.New()))
			if rec.Code != http.StatusOK {
				t.Fatalf("status=%d, want 200; body=%q", rec.Code, rec.Body.String())
			}
			body := rec.Body.String()
			// Transfer affordance is enabled (real form, not the disabled stub).
			if !strings.Contains(body, `data-testid="conversation-transfer"`) {
				t.Fatalf("missing transfer button; got %q", body)
			}
			if !strings.Contains(body, `/transfer"`) {
				t.Fatalf("transfer form missing hx-post to /transfer; got %q", body)
			}
			// State toggle reflects the lifecycle.
			if tc.closed {
				if !strings.Contains(body, `data-testid="conversation-reopen"`) {
					t.Fatalf("closed conversation must show Reabrir; got %q", body)
				}
				if !strings.Contains(body, `data-testid="conversation-closed-notice"`) {
					t.Fatalf("closed conversation must show the compose closed notice; got %q", body)
				}
			} else {
				if !strings.Contains(body, `data-testid="conversation-close"`) {
					t.Fatalf("open conversation must show Encerrar; got %q", body)
				}
			}
			// CSP: no inline handlers anywhere in the actions markup.
			if strings.Contains(body, "onclick") || strings.Contains(body, "hx-on") {
				t.Fatalf("actions markup must not use inline on*/hx-on handlers (CSP); got %q", body)
			}
		})
	}
}

// TestTransfer_ReusesAssignHandler proves POST .../transfer reaches the same
// reassignment pipeline as .../assign: it parses targetUserID and delegates
// to AssignConversation, returning the assignment-section partial that the
// transfer form targets for an OOB-style swap of the context chip.
func TestTransfer_ReusesAssignHandler(t *testing.T) {
	t.Parallel()
	assigner := &stubAssigner{}
	_, mux := newAssignHandler(t, assigner, &stubListAssignable{rows: []webinbox.AssignableRow{{UserID: uuid.New(), DisplayName: "Bia"}}})
	tenant := uuid.New()
	conv := uuid.New()
	target := uuid.New()
	form := url.Values{"targetUserID": {target.String()}}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, reqWithTenant(http.MethodPost, "/inbox/conversations/"+conv.String()+"/transfer", form.Encode(), tenant))

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200; body=%q", rec.Code, rec.Body.String())
	}
	if assigner.in.TargetUserID != target || assigner.in.ConversationID != conv || assigner.in.TenantID != tenant {
		t.Fatalf("assign args=%+v, want tenant=%v conv=%v target=%v", assigner.in, tenant, conv, target)
	}
	if !strings.Contains(rec.Body.String(), `id="conversation-context-assignment"`) {
		t.Fatalf("transfer response missing the assignment section partial; got %q", rec.Body.String())
	}
}
