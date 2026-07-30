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
	"net/http"
	"net/http/httptest"
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

// TestBuildInboxHandlerReal_Boots pins the SIN-67470 W3 acceptance: the
// real-carrier assembler constructs a non-nil, mountable http.Handler from
// a postgres pool.
//
// assembleInboxHandlerRealFromPool performs no database I/O at assembly
// time — every pg adapter constructor (pginbox.New / pgcontacts.New /
// NewUserDirectory) and every use case constructor only wraps the pool and
// validates non-nil deps; the first query is deferred to a live request.
// So a lazy, never-dialled pool (newUnreachablePool) exercises the exact
// production assembly path and proves the handler is built and mountable
// without standing up Postgres. With META_GRAPH_TOKEN unset the outbound
// dispatcher is the deny-by-default no-op Router, so no carrier I/O either.
func TestBuildInboxHandlerReal_Boots(t *testing.T) {
	t.Parallel()
	pool := newUnreachablePool(t)
	mux, cleanup, err := assembleInboxHandlerRealFromPool(pool, nil, envOnly(map[string]string{}))
	if err != nil {
		t.Fatalf("assembleInboxHandlerRealFromPool: %v", err)
	}
	t.Cleanup(cleanup)
	if mux == nil {
		t.Fatalf("assembleInboxHandlerRealFromPool returned nil handler")
	}

	// Mountable + routed: GET /inbox must not 404 (the route is
	// registered). Without the chi tenancy middleware the handler fails at
	// tenancy.FromContext and returns 500 — a non-404 that proves the route
	// fired before any DB read, mirroring TestLLMCustomerHandler_ServesRoutes.
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	res, err := http.Get(srv.URL + "/inbox")
	if err != nil {
		t.Fatalf("GET /inbox: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode == http.StatusNotFound {
		t.Fatalf("GET /inbox returned 404; real-carrier route not mounted")
	}
}

// TestBuildInboxHandlerReal_UnreachableDSN_DegradesToStubs covers the
// pg-connect-error fallback: provider=real with a DATABASE_URL that parses
// but cannot be dialled makes buildInboxHandlerReal fall back to the
// disabled-mode stub mux (non-nil) rather than downing the listener. The
// package TestMain caps the ping-retry budget at 1ms so this fails fast.
func TestBuildInboxHandlerReal_UnreachableDSN_DegradesToStubs(t *testing.T) {
	t.Parallel()
	h, cleanup := buildInboxHandler(context.Background(), envOnly(map[string]string{
		envInboxChannelProvider: string(InboxChannelProviderReal),
		"DATABASE_URL":          "postgres://nobody:nobody@127.0.0.1:1/nobody?sslmode=disable&connect_timeout=1",
	}))
	t.Cleanup(cleanup)
	if h == nil {
		t.Fatalf("buildInboxHandler returned nil for provider=real w/ unreachable DSN; want stub fallback")
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
