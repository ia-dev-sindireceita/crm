package main

// SIN-63821 — buildInboxHandler smoke + stub-dep contract tests.
//
// The wire constructs internal/web/inbox.Handler with stub use cases
// (W1 placeholder) and returns the *http.ServeMux it produces. These
// tests pin:
//
//   - The handler is non-nil for any getenv (the wire is independent
//     of DATABASE_URL today because the stubs need nothing).
//   - The stubs return the documented shapes: ListConversations gives
//     an empty page, every other use case yields ErrNotFound (which
//     the handler converts to 404).
//
// Together they form the coverage anchor for the new file so the
// cmd/server package stays above the 85% bar after SIN-63821.

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	inboxusecase "github.com/pericles-luz/crm/internal/inbox/usecase"
)

func TestBuildInboxHandler_ReturnsNonNilMux(t *testing.T) {
	t.Parallel()
	h, cleanup := buildInboxHandler(context.Background(), func(string) string { return "" })
	t.Cleanup(cleanup)
	if h == nil {
		t.Fatalf("buildInboxHandler returned nil handler")
	}
}

func TestBuildInboxHandler_StubListConversations_EmptyPage(t *testing.T) {
	t.Parallel()
	got, err := emptyListConversations{}.Execute(context.Background(), inboxusecase.ListConversationsInput{
		TenantID: uuid.New(),
		State:    "open",
	})
	if err != nil {
		t.Fatalf("Execute err=%v, want nil", err)
	}
	if len(got.Items) != 0 {
		t.Fatalf("len(Items)=%d, want 0 (empty inbox placeholder)", len(got.Items))
	}
}

func TestBuildInboxHandler_StubListMessages_NotFound(t *testing.T) {
	t.Parallel()
	_, err := notFoundListMessages{}.Execute(context.Background(), inboxusecase.ListMessagesInput{
		TenantID:       uuid.New(),
		ConversationID: uuid.New(),
	})
	if !errors.Is(err, inboxusecase.ErrNotFound) {
		t.Fatalf("err=%v, want ErrNotFound", err)
	}
}

func TestBuildInboxHandler_StubSendOutbound_NotFound(t *testing.T) {
	t.Parallel()
	_, err := notFoundSendOutbound{}.SendForView(context.Background(), inboxusecase.SendOutboundInput{
		TenantID:       uuid.New(),
		ConversationID: uuid.New(),
		Body:           "irrelevant",
	})
	if !errors.Is(err, inboxusecase.ErrNotFound) {
		t.Fatalf("err=%v, want ErrNotFound", err)
	}
}

func TestBuildInboxHandler_StubGetMessage_NotFound(t *testing.T) {
	t.Parallel()
	_, err := notFoundGetMessage{}.Execute(context.Background(), inboxusecase.GetMessageInput{
		TenantID:       uuid.New(),
		ConversationID: uuid.New(),
		MessageID:      uuid.New(),
	})
	if !errors.Is(err, inboxusecase.ErrNotFound) {
		t.Fatalf("err=%v, want ErrNotFound", err)
	}
}
