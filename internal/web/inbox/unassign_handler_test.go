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

// stubUnassigner stubs UnassignConversationUseCase and records the call.
type stubUnassigner struct {
	called bool
	in     inboxusecase.UnassignConversationInput
	res    inboxusecase.UnassignConversationResult
	err    error
}

func (s *stubUnassigner) Execute(_ context.Context, in inboxusecase.UnassignConversationInput) (inboxusecase.UnassignConversationResult, error) {
	s.called = true
	s.in = in
	return s.res, s.err
}

// newTransferHandler wires the assign + unassign deps so POST .../transfer
// can route both branches.
func newTransferHandler(t *testing.T, assigner webinbox.AssignConversationUseCase, unassigner webinbox.UnassignConversationUseCase) (*webinbox.Handler, *http.ServeMux) {
	t.Helper()
	h, err := webinbox.New(webinbox.Deps{
		ListConversations:    &stubLister{},
		ListMessages:         &stubMessages{},
		SendOutbound:         &stubSender{},
		GetMessage:           &stubGetMessage{},
		CSRFToken:            func(*http.Request) string { return "csrf-test-token" },
		UserID:               func(*http.Request) uuid.UUID { return uuid.Nil },
		AssignConversation:   assigner,
		UnassignConversation: unassigner,
		ListAssignable:       &stubListAssignable{rows: []webinbox.AssignableRow{{UserID: uuid.New(), DisplayName: "Ana"}}},
	})
	if err != nil {
		t.Fatalf("webinbox.New: %v", err)
	}
	mux := http.NewServeMux()
	h.Routes(mux)
	return h, mux
}

func TestTransfer_UnassignSentinel_RoutesToUnassign(t *testing.T) {
	t.Parallel()
	tenant := uuid.New()
	conv := uuid.New()
	assigner := &stubAssigner{}
	unassigner := &stubUnassigner{}
	_, mux := newTransferHandler(t, assigner, unassigner)

	body := "targetUserID=unassigned"
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, reqWithTenant(http.MethodPost, "/inbox/conversations/"+conv.String()+"/transfer", body, tenant))

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200; body=%q", rec.Code, rec.Body.String())
	}
	if !unassigner.called {
		t.Fatalf("unassign use case was not called")
	}
	if assigner.in.TargetUserID != uuid.Nil {
		t.Errorf("assign use case was invoked (target=%v); sentinel must route only to unassign", assigner.in.TargetUserID)
	}
	if unassigner.in.TenantID != tenant || unassigner.in.ConversationID != conv {
		t.Errorf("unassign args=%+v, want tenant=%v conv=%v", unassigner.in, tenant, conv)
	}
	// The OOB swap re-renders the assignment section in the unassigned state.
	if !strings.Contains(rec.Body.String(), `id="conversation-context-assignment"`) {
		t.Fatalf("response missing assignment partial; got %q", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "Não atribuída") {
		t.Errorf("response missing the unassigned label; got %q", rec.Body.String())
	}
}

func TestTransfer_UUIDValue_DoesNotUnassign(t *testing.T) {
	t.Parallel()
	tenant := uuid.New()
	conv := uuid.New()
	target := uuid.New()
	assigner := &stubAssigner{}
	unassigner := &stubUnassigner{}
	_, mux := newTransferHandler(t, assigner, unassigner)

	body := "targetUserID=" + target.String()
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, reqWithTenant(http.MethodPost, "/inbox/conversations/"+conv.String()+"/transfer", body, tenant))

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200; body=%q", rec.Code, rec.Body.String())
	}
	if unassigner.called {
		t.Errorf("unassign use case was called for a UUID transfer; want assign only")
	}
	if assigner.in.TargetUserID != target {
		t.Errorf("assign target=%v, want %v", assigner.in.TargetUserID, target)
	}
}

func TestTransfer_UnassignNotWired_Returns404(t *testing.T) {
	t.Parallel()
	tenant := uuid.New()
	conv := uuid.New()
	// newAssignHandler wires AssignConversation but NOT UnassignConversation,
	// so the sentinel reaches an unwired path and must 404 (deny-by-default).
	_, mux := newAssignHandler(t, &stubAssigner{}, &stubListAssignable{rows: []webinbox.AssignableRow{{UserID: uuid.New(), DisplayName: "Bia"}}})

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, reqWithTenant(http.MethodPost, "/inbox/conversations/"+conv.String()+"/transfer", "targetUserID=unassigned", tenant))

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d, want 404; body=%q", rec.Code, rec.Body.String())
	}
}

func TestUnassign_ErrorMapping(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		err  error
		want int
	}{
		{"closed conversation", inboxusecase.ErrConversationClosed, http.StatusConflict},
		{"unknown / cross-tenant (IDOR)", inboxusecase.ErrNotFound, http.StatusNotFound},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tenant := uuid.New()
			conv := uuid.New()
			unassigner := &stubUnassigner{err: tc.err}
			_, mux := newTransferHandler(t, &stubAssigner{}, unassigner)

			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, reqWithTenant(http.MethodPost, "/inbox/conversations/"+conv.String()+"/transfer", "targetUserID=unassigned", tenant))

			if rec.Code != tc.want {
				t.Fatalf("status=%d, want %d; body=%q", rec.Code, tc.want, rec.Body.String())
			}
		})
	}
}

func TestTransfer_InvalidConversationID_BadRequest(t *testing.T) {
	t.Parallel()
	tenant := uuid.New()
	unassigner := &stubUnassigner{}
	_, mux := newTransferHandler(t, &stubAssigner{}, unassigner)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, reqWithTenant(http.MethodPost, "/inbox/conversations/not-a-uuid/transfer", "targetUserID=unassigned", tenant))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d, want 400; body=%q", rec.Code, rec.Body.String())
	}
	if unassigner.called {
		t.Errorf("unassign called with an invalid conversation id")
	}
}
