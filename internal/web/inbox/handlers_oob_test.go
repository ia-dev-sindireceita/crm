package inbox_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	inboxusecase "github.com/pericles-luz/crm/internal/inbox/usecase"
	webinbox "github.com/pericles-luz/crm/internal/web/inbox"
)

// TestSend_EmitsOOBTextareaReset pins SIN-65068: a successful POST to
// /inbox/conversations/:id/messages returns both the new message bubble
// AND an out-of-band textarea fragment so htmx clears the compose field
// without any hx-on eval (which would throw EvalError under the prod
// strict CSP). The OOB fragment is keyed by id="compose-body" and carries
// hx-swap-oob="true".
func TestSend_EmitsOOBTextareaReset(t *testing.T) {
	t.Parallel()
	tenant := uuid.New()
	convID := uuid.New()
	sender := &stubSender{response: inboxusecase.MessageView{
		ID:             uuid.New(),
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
		GetMessage:        &stubGetMessage{},
		CSRFToken:         func(*http.Request) string { return "tok" },
		UserID:            func(*http.Request) uuid.UUID { return uuid.Nil },
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
	body := rec.Body.String()
	// The message bubble (main swap) must still be present.
	if !strings.Contains(body, `class="message-bubble msg-out"`) {
		t.Errorf("response missing message bubble: %q", body)
	}
	// The OOB reset fragment must be present and keyed by id=compose-body.
	for _, want := range []string{
		`hx-swap-oob="true"`,
		`id="compose-body"`,
		`name="body"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("response missing OOB textarea reset fragment %q in:\n%s", want, body)
		}
	}
	// No hx-on eval may leak into the response.
	if strings.Contains(body, "hx-on") {
		t.Errorf("response leaked an hx-on attribute (eval under strict CSP): %q", body)
	}
}
