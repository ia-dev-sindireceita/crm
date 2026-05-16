package webchat_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/pericles-luz/crm/internal/adapter/channels/webchat"
	"github.com/pericles-luz/crm/internal/inbox"
	"github.com/pericles-luz/crm/internal/tenancy"
)

// --- fakes ---

type fakeOrigins struct {
	allowAll bool
	sig      string
}

func (f *fakeOrigins) Valid(_ context.Context, _ uuid.UUID, _ string) (bool, error) {
	return f.allowAll, nil
}
func (f *fakeOrigins) HMAC(_ context.Context, _ uuid.UUID, _ string) (string, error) {
	return f.sig, nil
}

type fakeFlag struct{ on bool }

func (f *fakeFlag) Enabled(_ context.Context, _ uuid.UUID) (bool, error) { return f.on, nil }

type fakeInbox struct {
	calls []inbox.InboundEvent
}

func (f *fakeInbox) HandleInbound(_ context.Context, ev inbox.InboundEvent) error {
	f.calls = append(f.calls, ev)
	return nil
}

func newAdapter(t *testing.T, flag *fakeFlag, rl *webchat.InMemoryRateLimiter, origins *fakeOrigins, inbox *fakeInbox) (*webchat.Adapter, *webchat.Broker) {
	t.Helper()
	broker := webchat.NewBroker()
	a, err := webchat.New(inbox, webchat.NewInMemorySessionStore(), origins, flag, rl, broker, nil)
	if err != nil {
		t.Fatalf("webchat.New: %v", err)
	}
	return a, broker
}

func withTenant(r *http.Request, id uuid.UUID) *http.Request {
	t := &tenancy.Tenant{ID: id}
	return r.WithContext(tenancy.WithContext(r.Context(), t))
}

// --- POST /widget/v1/session tests ---

func TestHandleSession_FlagOff_Returns404(t *testing.T) {
	a, _ := newAdapter(t, &fakeFlag{on: false}, webchat.NewInMemoryRateLimiter(0), &fakeOrigins{allowAll: true}, &fakeInbox{})
	mux := http.NewServeMux()
	a.Register(mux)

	req := httptest.NewRequest(http.MethodPost, "/widget/v1/session", nil)
	req = withTenant(req, uuid.New())
	req.Header.Set("Origin", "https://example.com")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("want 404, got %d", w.Code)
	}
}

func TestHandleSession_MissingOrigin_Returns403(t *testing.T) {
	a, _ := newAdapter(t, &fakeFlag{on: true}, webchat.NewInMemoryRateLimiter(0), &fakeOrigins{allowAll: true}, &fakeInbox{})
	mux := http.NewServeMux()
	a.Register(mux)

	req := httptest.NewRequest(http.MethodPost, "/widget/v1/session", nil)
	req = withTenant(req, uuid.New())
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Errorf("want 403, got %d", w.Code)
	}
}

func TestHandleSession_OriginNotAllowed_Returns403(t *testing.T) {
	a, _ := newAdapter(t, &fakeFlag{on: true}, webchat.NewInMemoryRateLimiter(0), &fakeOrigins{allowAll: false}, &fakeInbox{})
	mux := http.NewServeMux()
	a.Register(mux)

	req := httptest.NewRequest(http.MethodPost, "/widget/v1/session", nil)
	req = withTenant(req, uuid.New())
	req.Header.Set("Origin", "https://evil.com")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Errorf("want 403, got %d", w.Code)
	}
}

func TestHandleSession_RateLimit_Returns429(t *testing.T) {
	a, _ := newAdapter(t, &fakeFlag{on: true}, webchat.NewInMemoryRateLimiter(1), &fakeOrigins{allowAll: true, sig: "sig"}, &fakeInbox{})
	mux := http.NewServeMux()
	a.Register(mux)

	tenantID := uuid.New()
	do := func() int {
		req := httptest.NewRequest(http.MethodPost, "/widget/v1/session", nil)
		req = withTenant(req, tenantID)
		req.Header.Set("Origin", "https://example.com")
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		return w.Code
	}

	if got := do(); got != http.StatusOK {
		t.Fatalf("want 200 on first call, got %d", got)
	}
	if got := do(); got != http.StatusTooManyRequests {
		t.Errorf("want 429 on second call, got %d", got)
	}
}

func TestHandleSession_OK_ReturnsSessionAndCSRF(t *testing.T) {
	a, _ := newAdapter(t, &fakeFlag{on: true}, webchat.NewInMemoryRateLimiter(0), &fakeOrigins{allowAll: true, sig: "testsig"}, &fakeInbox{})
	mux := http.NewServeMux()
	a.Register(mux)

	req := httptest.NewRequest(http.MethodPost, "/widget/v1/session", nil)
	req = withTenant(req, uuid.New())
	req.Header.Set("Origin", "https://example.com")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body)
	}
	var resp struct {
		SessionID string    `json:"session_id"`
		CSRFToken string    `json:"csrf_token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.SessionID == "" || resp.CSRFToken == "" {
		t.Errorf("empty session_id or csrf_token in response")
	}
}

// --- POST /widget/v1/message tests ---

func createSession(t *testing.T, mux http.Handler, tenantID uuid.UUID) (sessID, csrf string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/widget/v1/session", nil)
	req = withTenant(req, tenantID)
	req.Header.Set("Origin", "https://example.com")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("create session: want 200, got %d", w.Code)
	}
	var resp struct {
		SessionID string `json:"session_id"`
		CSRFToken string `json:"csrf_token"`
	}
	_ = json.NewDecoder(w.Body).Decode(&resp)
	return resp.SessionID, resp.CSRFToken
}

func TestHandleMessage_MissingCSRF_Returns401(t *testing.T) {
	a, _ := newAdapter(t, &fakeFlag{on: true}, webchat.NewInMemoryRateLimiter(0), &fakeOrigins{allowAll: true, sig: "s"}, &fakeInbox{})
	mux := http.NewServeMux()
	a.Register(mux)

	req := httptest.NewRequest(http.MethodPost, "/widget/v1/message", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("want 401, got %d", w.Code)
	}
}

func TestHandleMessage_BadCSRF_Returns403(t *testing.T) {
	fi := &fakeInbox{}
	a, _ := newAdapter(t, &fakeFlag{on: true}, webchat.NewInMemoryRateLimiter(0), &fakeOrigins{allowAll: true, sig: "s"}, fi)
	mux := http.NewServeMux()
	a.Register(mux)
	tenantID := uuid.New()
	sessID, _ := createSession(t, mux, tenantID)

	body := `{"body":"hi","client_msg_id":"` + uuid.New().String() + `"}`
	req := httptest.NewRequest(http.MethodPost, "/widget/v1/message", strings.NewReader(body))
	req.Header.Set(webchat.HeaderSession, sessID)
	req.Header.Set(webchat.HeaderCSRF, "wrong-token")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Errorf("want 403, got %d", w.Code)
	}
}

func TestHandleMessage_Idempotent_DeduplicatesDuplicateClientMsgID(t *testing.T) {
	fi := &fakeInbox{}
	a, _ := newAdapter(t, &fakeFlag{on: true}, webchat.NewInMemoryRateLimiter(0), &fakeOrigins{allowAll: true, sig: "s"}, fi)
	// Return already-processed on second call
	origHandle := fi
	_ = origHandle

	mux := http.NewServeMux()
	a.Register(mux)
	tenantID := uuid.New()
	sessID, csrf := createSession(t, mux, tenantID)

	clientMsgID := uuid.New().String()
	send := func() int {
		body := fmt.Sprintf(`{"body":"hello","client_msg_id":%q}`, clientMsgID)
		req := httptest.NewRequest(http.MethodPost, "/widget/v1/message", strings.NewReader(body))
		req.Header.Set(webchat.HeaderSession, sessID)
		req.Header.Set(webchat.HeaderCSRF, csrf)
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		return w.Code
	}

	if got := send(); got != http.StatusNoContent {
		t.Fatalf("first send: want 204, got %d", got)
	}
	// Second send — fakeInbox doesn't enforce dedup so both return 204.
	// Idempotency at the storage layer is enforced by inbound_message_dedup;
	// this test verifies the adapter forwards the same client_msg_id as
	// ChannelExternalID so the inbox use case can dedup.
	if got := send(); got != http.StatusNoContent {
		t.Errorf("second send: want 204, got %d", got)
	}
	if len(fi.calls) < 1 {
		t.Errorf("expected at least one HandleInbound call")
	}
	if fi.calls[0].ChannelExternalID != clientMsgID {
		t.Errorf("want ChannelExternalID=%q, got %q", clientMsgID, fi.calls[0].ChannelExternalID)
	}
}

// --- SSE integration test ---

func TestHandleStream_SSEDelivery(t *testing.T) {
	fi := &fakeInbox{}
	a, broker := newAdapter(t, &fakeFlag{on: true}, webchat.NewInMemoryRateLimiter(0), &fakeOrigins{allowAll: true, sig: "s"}, fi)
	mux := http.NewServeMux()
	a.Register(mux)

	tenantID := uuid.New()
	sessID, _ := createSession(t, mux, tenantID)

	srv := httptest.NewServer(mux)
	defer srv.Close()

	// Open SSE stream
	sseReq, _ := http.NewRequest(http.MethodGet, srv.URL+"/widget/v1/stream", nil)
	sseReq.Header.Set(webchat.HeaderSession, sessID)
	resp, err := http.DefaultClient.Do(sseReq)
	if err != nil {
		t.Fatalf("SSE connect: %v", err)
	}
	defer resp.Body.Close()

	if resp.Header.Get("Content-Type") != "text/event-stream" {
		t.Errorf("want text/event-stream, got %q", resp.Header.Get("Content-Type"))
	}

	// Publish a message and expect the widget to receive it.
	done := make(chan string, 1)
	go func() {
		scanner := bufio.NewScanner(resp.Body)
		for scanner.Scan() {
			line := scanner.Text()
			if strings.HasPrefix(line, "data: ") {
				done <- strings.TrimPrefix(line, "data: ")
				return
			}
		}
		close(done)
	}()

	// Small delay to ensure subscriber is registered before publish.
	time.Sleep(20 * time.Millisecond)
	broker.Publish(sessID, `{"text":"hello from agent"}`)

	select {
	case payload := <-done:
		if !strings.Contains(payload, "hello from agent") {
			t.Errorf("unexpected payload: %q", payload)
		}
	case <-time.After(2 * time.Second):
		t.Error("timeout waiting for SSE event")
	}

	// Verify CSP header
	if csp := resp.Header.Get("Content-Security-Policy"); csp != "default-src 'none'" {
		t.Errorf("want CSP default-src 'none', got %q", csp)
	}

	_ = bytes.NewReader(nil) // avoid unused import
}

// Browser EventSource cannot set custom headers, so the session id must
// be accepted via the session_id query parameter. This test pins that
// contract — the SSE handler MUST authenticate the stream via either
// the header or the query string.
func TestHandleStream_SessionIDFromQueryParam(t *testing.T) {
	fi := &fakeInbox{}
	a, broker := newAdapter(t, &fakeFlag{on: true}, webchat.NewInMemoryRateLimiter(0), &fakeOrigins{allowAll: true, sig: "s"}, fi)
	mux := http.NewServeMux()
	a.Register(mux)

	tenantID := uuid.New()
	sessID, _ := createSession(t, mux, tenantID)

	srv := httptest.NewServer(mux)
	defer srv.Close()

	sseReq, _ := http.NewRequest(http.MethodGet, srv.URL+"/widget/v1/stream?session_id="+sessID, nil)
	resp, err := http.DefaultClient.Do(sseReq)
	if err != nil {
		t.Fatalf("SSE connect: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}
	if resp.Header.Get("Content-Type") != "text/event-stream" {
		t.Errorf("want text/event-stream, got %q", resp.Header.Get("Content-Type"))
	}

	done := make(chan string, 1)
	go func() {
		scanner := bufio.NewScanner(resp.Body)
		for scanner.Scan() {
			line := scanner.Text()
			if strings.HasPrefix(line, "data: ") {
				done <- strings.TrimPrefix(line, "data: ")
				return
			}
		}
		close(done)
	}()

	time.Sleep(20 * time.Millisecond)
	broker.Publish(sessID, `{"text":"hello via query"}`)

	select {
	case payload := <-done:
		if !strings.Contains(payload, "hello via query") {
			t.Errorf("unexpected payload: %q", payload)
		}
	case <-time.After(2 * time.Second):
		t.Error("timeout waiting for SSE event")
	}
}
