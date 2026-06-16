package inbox_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	inboxusecase "github.com/pericles-luz/crm/internal/inbox/usecase"
	webinbox "github.com/pericles-luz/crm/internal/web/inbox"
)

// stubAssigner stubs AssignConversationUseCase.
type stubAssigner struct {
	in  inboxusecase.AssignConversationInput
	res inboxusecase.AssignConversationResult
	err error
}

func (s *stubAssigner) Execute(_ context.Context, in inboxusecase.AssignConversationInput) (inboxusecase.AssignConversationResult, error) {
	s.in = in
	return s.res, s.err
}

// stubListAssignable stubs ListAssignableUseCase.
type stubListAssignable struct {
	rows []webinbox.AssignableRow
	err  error
}

func (s *stubListAssignable) Execute(_ context.Context, _ uuid.UUID) ([]webinbox.AssignableRow, error) {
	return s.rows, s.err
}

// newAssignHandler builds a Handler wired with the assign deps.
func newAssignHandler(t *testing.T, assigner webinbox.AssignConversationUseCase, listAssignable webinbox.ListAssignableUseCase) (*webinbox.Handler, *http.ServeMux) {
	t.Helper()
	h, err := webinbox.New(webinbox.Deps{
		ListConversations:  &stubLister{},
		ListMessages:       &stubMessages{},
		SendOutbound:       &stubSender{},
		GetMessage:         &stubGetMessage{},
		CSRFToken:          func(*http.Request) string { return "csrf-test-token" },
		UserID:             func(*http.Request) uuid.UUID { return uuid.Nil },
		AssignConversation: assigner,
		ListAssignable:     listAssignable,
	})
	if err != nil {
		t.Fatalf("webinbox.New: %v", err)
	}
	mux := http.NewServeMux()
	h.Routes(mux)
	return h, mux
}

func TestAssign_OK(t *testing.T) {
	t.Parallel()
	tenant := uuid.New()
	convID := uuid.New()
	targetID := uuid.New()

	assigner := &stubAssigner{}
	_, mux := newAssignHandler(t, assigner, &stubListAssignable{
		rows: []webinbox.AssignableRow{{UserID: targetID, DisplayName: "Ana Lima"}},
	})

	body := "targetUserID=" + targetID.String()
	r := reqWithTenant(http.MethodPost, "/inbox/conversations/"+convID.String()+"/assign", body, tenant)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, r)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200; body=%q", rec.Code, rec.Body.String())
	}
	if assigner.in.TenantID != tenant {
		t.Errorf("tenant: got %s want %s", assigner.in.TenantID, tenant)
	}
	if assigner.in.ConversationID != convID {
		t.Errorf("conversationID: got %s want %s", assigner.in.ConversationID, convID)
	}
	if assigner.in.TargetUserID != targetID {
		t.Errorf("targetUserID: got %s want %s", assigner.in.TargetUserID, targetID)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("Content-Type: %q", ct)
	}
	// Response must be the assignment partial with the new assignee label.
	html := rec.Body.String()
	if !strings.Contains(html, "conversation-context-assignment") {
		t.Errorf("response missing assignment section id; body=%q", html)
	}
	if !strings.Contains(html, "Ana Lima") {
		t.Errorf("response missing assignee display name; body=%q", html)
	}
}

func TestAssign_Reassign_IsIdempotent(t *testing.T) {
	t.Parallel()
	tenant := uuid.New()
	convID := uuid.New()
	targetID := uuid.New()

	assigner := &stubAssigner{err: inboxusecase.ErrAlreadyAssigned}
	_, mux := newAssignHandler(t, assigner, &stubListAssignable{
		rows: []webinbox.AssignableRow{{UserID: targetID, DisplayName: "João"}},
	})

	body := "targetUserID=" + targetID.String()
	r := reqWithTenant(http.MethodPost, "/inbox/conversations/"+convID.String()+"/assign", body, tenant)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, r)

	// ErrAlreadyAssigned is a no-op — still 200 with the panel re-rendered.
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200 (idempotent); body=%q", rec.Code, rec.Body.String())
	}
}

func TestAssign_MissingTargetUserID_400(t *testing.T) {
	t.Parallel()
	assigner := &stubAssigner{}
	_, mux := newAssignHandler(t, assigner, nil)

	r := reqWithTenant(http.MethodPost, "/inbox/conversations/"+uuid.New().String()+"/assign", "", uuid.New())
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, r)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d want 400", rec.Code)
	}
}

func TestAssign_InvalidTargetUserID_400(t *testing.T) {
	t.Parallel()
	assigner := &stubAssigner{}
	_, mux := newAssignHandler(t, assigner, nil)

	body := "targetUserID=not-a-uuid"
	r := reqWithTenant(http.MethodPost, "/inbox/conversations/"+uuid.New().String()+"/assign", body, uuid.New())
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, r)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d want 400", rec.Code)
	}
}

func TestAssign_InvalidConversationID_400(t *testing.T) {
	t.Parallel()
	assigner := &stubAssigner{}
	_, mux := newAssignHandler(t, assigner, nil)

	body := "targetUserID=" + uuid.New().String()
	r := reqWithTenant(http.MethodPost, "/inbox/conversations/not-a-uuid/assign", body, uuid.New())
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, r)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d want 400", rec.Code)
	}
}

func TestAssign_UserNotAssignable_403(t *testing.T) {
	t.Parallel()
	assigner := &stubAssigner{err: inboxusecase.ErrUserNotAssignable}
	_, mux := newAssignHandler(t, assigner, nil)

	body := "targetUserID=" + uuid.New().String()
	r := reqWithTenant(http.MethodPost, "/inbox/conversations/"+uuid.New().String()+"/assign", body, uuid.New())
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, r)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status: got %d want 403", rec.Code)
	}
}

func TestAssign_ConversationNotFound_404(t *testing.T) {
	t.Parallel()
	assigner := &stubAssigner{err: inboxusecase.ErrNotFound}
	_, mux := newAssignHandler(t, assigner, nil)

	body := "targetUserID=" + uuid.New().String()
	r := reqWithTenant(http.MethodPost, "/inbox/conversations/"+uuid.New().String()+"/assign", body, uuid.New())
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, r)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status: got %d want 404", rec.Code)
	}
}

func TestAssign_TenantMissing_500(t *testing.T) {
	t.Parallel()
	assigner := &stubAssigner{}
	_, mux := newAssignHandler(t, assigner, nil)

	body := "targetUserID=" + uuid.New().String()
	r := httptest.NewRequest(http.MethodPost, "/inbox/conversations/"+uuid.New().String()+"/assign", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, r)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status: got %d want 500", rec.Code)
	}
}

func TestAssign_RouteNotRegisteredWithoutDep(t *testing.T) {
	t.Parallel()
	// When AssignConversation dep is nil the route must not be registered:
	// the mux returns 404 for POST /inbox/conversations/{id}/assign.
	h, err := webinbox.New(webinbox.Deps{
		ListConversations: &stubLister{},
		ListMessages:      &stubMessages{},
		SendOutbound:      &stubSender{},
		GetMessage:        &stubGetMessage{},
		CSRFToken:         func(*http.Request) string { return "tok" },
		UserID:            func(*http.Request) uuid.UUID { return uuid.Nil },
		// AssignConversation intentionally nil
	})
	if err != nil {
		t.Fatalf("webinbox.New: %v", err)
	}
	mux := http.NewServeMux()
	h.Routes(mux)

	body := "targetUserID=" + uuid.New().String()
	r := reqWithTenant(http.MethodPost, "/inbox/conversations/"+uuid.New().String()+"/assign", body, uuid.New())
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, r)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("route should not be registered: got %d want 404", rec.Code)
	}
}

func TestAssign_InternalError_500(t *testing.T) {
	t.Parallel()
	assigner := &stubAssigner{err: errors.New("db dead")}
	_, mux := newAssignHandler(t, assigner, nil)

	body := "targetUserID=" + uuid.New().String()
	r := reqWithTenant(http.MethodPost, "/inbox/conversations/"+uuid.New().String()+"/assign", body, uuid.New())
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, r)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status: got %d want 500", rec.Code)
	}
}
