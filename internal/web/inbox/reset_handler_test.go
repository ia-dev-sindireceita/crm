package inbox_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	inboxusecase "github.com/pericles-luz/crm/internal/inbox/usecase"
	webinbox "github.com/pericles-luz/crm/internal/web/inbox"
)

// stubReset captures the ResetConversation call args and returns a
// configurable result/error so the handler tests can assert routing,
// status mapping, and the empty-thread partial without a real adapter.
type stubReset struct {
	in     inboxusecase.ResetConversationInput
	called bool
	result inboxusecase.ResetConversationResult
	err    error
}

func (s *stubReset) Execute(_ context.Context, in inboxusecase.ResetConversationInput) (inboxusecase.ResetConversationResult, error) {
	s.called = true
	s.in = in
	return s.result, s.err
}

// newResetHandler builds a Handler with the reset use case wired plus a
// ListMessages stub (view path) and a ConversationContext stub so the
// conversation view can resolve a channel for the button-visibility
// tests.
func newResetHandler(t *testing.T, reset webinbox.ResetConversationUseCase, msgs webinbox.ListMessagesUseCase, ctxUC webinbox.GetConversationContextUseCase) *webinbox.Handler {
	t.Helper()
	h, err := webinbox.New(webinbox.Deps{
		ListConversations:   &stubLister{},
		ListMessages:        msgs,
		SendOutbound:        &stubSender{},
		GetMessage:          &stubGetMessage{},
		ResetConversation:   reset,
		ConversationContext: ctxUC,
		CSRFToken:           func(*http.Request) string { return "csrf-test-token" },
		UserID:              func(*http.Request) uuid.UUID { return uuid.Nil },
	})
	if err != nil {
		t.Fatalf("webinbox.New: %v", err)
	}
	return h
}

// stubConvContext returns a fixed channel for the view so the button
// visibility logic (channel == "fakellm") can be exercised.
type stubConvContext struct {
	channel string
}

func (s *stubConvContext) Execute(_ context.Context, in inboxusecase.GetConversationContextInput) (inboxusecase.GetConversationContextResult, error) {
	return inboxusecase.GetConversationContextResult{
		Context: inboxusecase.ConversationContextView{
			ConversationID: in.ConversationID,
			Channel:        s.channel,
		},
	}, nil
}

func TestReset_RouteRegisteredOnlyWhenWired(t *testing.T) {
	t.Parallel()
	// Without the reset dep, the POST .../reset route is absent → 404 from
	// the mux (not a handler-level 404).
	h := newHandler(t, &stubLister{}, &stubMessages{}, &stubSender{})
	mux := http.NewServeMux()
	h.Routes(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, reqWithTenant(http.MethodPost, "/inbox/conversations/"+uuid.New().String()+"/reset", "", uuid.New()))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (route unregistered)", rec.Code)
	}
}

func TestReset_DeletesAndReturnsEmptyThread(t *testing.T) {
	t.Parallel()
	reset := &stubReset{result: inboxusecase.ResetConversationResult{Deleted: 3}}
	h := newResetHandler(t, reset, &stubMessages{}, nil)
	mux := http.NewServeMux()
	h.Routes(mux)

	tenant := uuid.New()
	conv := uuid.New()
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, reqWithTenant(http.MethodPost, "/inbox/conversations/"+conv.String()+"/reset", "", tenant))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !reset.called {
		t.Fatal("ResetConversation use case was not called")
	}
	if reset.in.TenantID != tenant || reset.in.ConversationID != conv {
		t.Fatalf("use case args = %+v, want tenant=%v conv=%v", reset.in, tenant, conv)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `id="conversation-thread"`) {
		t.Fatalf("response missing thread container; got %q", body)
	}
	if strings.Contains(body, "message-bubble") {
		t.Fatalf("empty thread must contain no message bubbles; got %q", body)
	}
}

func TestReset_NotResettableMapsTo404(t *testing.T) {
	t.Parallel()
	reset := &stubReset{err: inboxusecase.ErrConversationNotResettable}
	h := newResetHandler(t, reset, &stubMessages{}, nil)
	mux := http.NewServeMux()
	h.Routes(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, reqWithTenant(http.MethodPost, "/inbox/conversations/"+uuid.New().String()+"/reset", "", uuid.New()))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 for non-resettable channel", rec.Code)
	}
}

func TestReset_NotFoundMapsTo404(t *testing.T) {
	t.Parallel()
	reset := &stubReset{err: inboxusecase.ErrNotFound}
	h := newResetHandler(t, reset, &stubMessages{}, nil)
	mux := http.NewServeMux()
	h.Routes(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, reqWithTenant(http.MethodPost, "/inbox/conversations/"+uuid.New().String()+"/reset", "", uuid.New()))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 for unknown conversation", rec.Code)
	}
}

func TestReset_FailsWhenTenantMissing(t *testing.T) {
	t.Parallel()
	reset := &stubReset{}
	h := newResetHandler(t, reset, &stubMessages{}, nil)
	mux := http.NewServeMux()
	h.Routes(mux)
	rec := httptest.NewRecorder()
	// No tenant in context → 500 before the use case is reached.
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/inbox/conversations/"+uuid.New().String()+"/reset", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 when tenant missing", rec.Code)
	}
	if reset.called {
		t.Fatal("use case must not run without a tenant")
	}
}

func TestReset_InvalidConversationID(t *testing.T) {
	t.Parallel()
	reset := &stubReset{}
	h := newResetHandler(t, reset, &stubMessages{}, nil)
	mux := http.NewServeMux()
	h.Routes(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, reqWithTenant(http.MethodPost, "/inbox/conversations/not-a-uuid/reset", "", uuid.New()))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for invalid id", rec.Code)
	}
	if reset.called {
		t.Fatal("use case must not run for an invalid conversation id")
	}
}

// TestView_ShowsResetButtonOnlyForFakellm asserts the destructive
// "Apagar mensagens" button renders for a fakellm conversation (reset
// wired) and is absent for any other channel.
func TestView_ShowsResetButtonOnlyForFakellm(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		channel string
		want    bool
	}{
		{"fakellm shows button", "fakellm", true},
		{"whatsapp hides button", "whatsapp", false},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newResetHandler(t, &stubReset{}, &stubMessages{}, &stubConvContext{channel: tc.channel})
			mux := http.NewServeMux()
			h.Routes(mux)
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, reqWithTenant(http.MethodGet, "/inbox/conversations/"+uuid.New().String(), "", uuid.New()))
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", rec.Code)
			}
			has := strings.Contains(rec.Body.String(), `data-testid="conversation-reset"`)
			if has != tc.want {
				t.Fatalf("reset button present = %v, want %v (channel=%q)", has, tc.want, tc.channel)
			}
			if tc.want {
				// The destructive confirm must be the CSP-safe htmx-core
				// hx-confirm (no hx-on / inline on*).
				body := rec.Body.String()
				if !strings.Contains(body, "hx-confirm=") {
					t.Fatal("reset form missing hx-confirm")
				}
				if strings.Contains(body, "hx-on") || strings.Contains(body, "onclick") {
					t.Fatal("reset form must not use hx-on / inline on* handlers (CSP)")
				}
			}
		})
	}
}
