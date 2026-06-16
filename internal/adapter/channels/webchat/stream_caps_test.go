package webchat_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/pericles-luz/crm/internal/adapter/channels/webchat"
)

// denyPrefixRL is a RateLimiter that denies any key with the configured
// prefix and allows everything else. It lets a test drive one specific
// D5 bucket (session-create, /24, or stream-entry) to its limit without
// issuing the real (hundreds of) requests.
type denyPrefixRL struct{ prefix string }

func (d denyPrefixRL) Allow(_ context.Context, key string) (bool, time.Duration, error) {
	if strings.HasPrefix(key, d.prefix) {
		return false, 5 * time.Second, nil
	}
	return true, 0, nil
}

// openStream issues a GET /widget/v1/stream and returns the response.
// The caller owns Body.Close and the cancel func (cancel ends the
// server-side stream so its subscriber slot is released).
func openStream(t *testing.T, baseURL, sessID string) (*http.Response, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/widget/v1/stream?session_id="+sessID, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		cancel()
		t.Fatalf("open stream: %v", err)
	}
	return resp, cancel
}

// Required regression test (SIN-64989): opening more concurrent streams
// for one session than the per-session cap (1) must reject the excess
// with 429 — not silently accept an unbounded number of goroutines.
func TestHandleStream_PerSessionConcurrencyCap_Rejects429(t *testing.T) {
	a, _ := newAdapter(t, &fakeFlag{on: true}, webchat.NewInMemoryRateLimiter(0), &fakeOrigins{allowAll: true, sig: "s"}, &fakeInbox{})
	mux := http.NewServeMux()
	a.Register(mux)
	tenantID := uuid.New()
	sessID, _ := createSession(t, mux, tenantID)

	srv := httptest.NewServer(mux)
	defer srv.Close()

	// First stream stays open for the duration of the test.
	resp1, cancel1 := openStream(t, srv.URL, sessID)
	defer resp1.Body.Close()
	defer cancel1()
	if resp1.StatusCode != http.StatusOK {
		t.Fatalf("first stream: want 200, got %d", resp1.StatusCode)
	}

	// Second concurrent stream for the same session is the (cap+1)th and
	// must be rejected.
	resp2, cancel2 := openStream(t, srv.URL, sessID)
	defer resp2.Body.Close()
	defer cancel2()
	if resp2.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("second concurrent stream: want 429, got %d", resp2.StatusCode)
	}
	if resp2.Header.Get("Retry-After") == "" {
		t.Errorf("429 response missing Retry-After header")
	}
}

// Per-(tenant × IP) cap (5): six distinct sessions behind the same IP
// (same ip_hash) — the sixth concurrent stream must be rejected even
// though each session is under its own per-session cap.
func TestHandleStream_PerTenantIPConcurrencyCap_Rejects429(t *testing.T) {
	a, _ := newAdapter(t, &fakeFlag{on: true}, webchat.NewInMemoryRateLimiter(0), &fakeOrigins{allowAll: true, sig: "s"}, &fakeInbox{})
	mux := http.NewServeMux()
	a.Register(mux)
	tenantID := uuid.New()

	srv := httptest.NewServer(mux)
	defer srv.Close()

	// httptest recorder requests share one RemoteAddr, so all sessions
	// for this tenant hash to the same ip_hash → one IP bucket.
	const ipCap = 5
	for i := 0; i < ipCap; i++ {
		sessID, _ := createSession(t, mux, tenantID)
		resp, cancel := openStream(t, srv.URL, sessID)
		defer resp.Body.Close()
		defer cancel()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("stream %d: want 200, got %d", i+1, resp.StatusCode)
		}
	}

	overflowSess, _ := createSession(t, mux, tenantID)
	resp, cancel := openStream(t, srv.URL, overflowSess)
	defer resp.Body.Close()
	defer cancel()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("6th stream (over per-IP cap): want 429, got %d", resp.StatusCode)
	}
}

// The stream-entry rate limiter gates connects before the concurrency
// caps, so an open→close reconnect loop is throttled.
func TestHandleStream_RateLimited_Rejects429(t *testing.T) {
	broker := webchat.NewBroker()
	a, err := webchat.New(&fakeInbox{}, webchat.NewInMemorySessionStore(), &fakeOrigins{allowAll: true, sig: "s"}, &fakeFlag{on: true}, denyPrefixRL{prefix: "wc.stream."}, broker, nil)
	if err != nil {
		t.Fatalf("webchat.New: %v", err)
	}
	mux := http.NewServeMux()
	a.Register(mux)
	tenantID := uuid.New()
	sessID, _ := createSession(t, mux, tenantID)

	srv := httptest.NewServer(mux)
	defer srv.Close()

	resp, cancel := openStream(t, srv.URL, sessID)
	defer resp.Body.Close()
	defer cancel()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("rate-limited stream: want 429, got %d", resp.StatusCode)
	}
	if resp.Header.Get("Retry-After") == "" {
		t.Errorf("429 response missing Retry-After header")
	}
}

// The /24 anti-sybil bucket gates POST /widget/v1/session.
func TestHandleSession_Sybil24RateLimited_Rejects429(t *testing.T) {
	broker := webchat.NewBroker()
	a, err := webchat.New(&fakeInbox{}, webchat.NewInMemorySessionStore(), &fakeOrigins{allowAll: true, sig: "s"}, &fakeFlag{on: true}, denyPrefixRL{prefix: "wc.s24."}, broker, nil)
	if err != nil {
		t.Fatalf("webchat.New: %v", err)
	}
	mux := http.NewServeMux()
	a.Register(mux)

	req := httptest.NewRequest(http.MethodPost, "/widget/v1/session", nil)
	req = withTenant(req, uuid.New())
	req.Header.Set("Origin", "https://example.com")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("/24-limited session create: want 429, got %d", w.Code)
	}
	if w.Header().Get("Retry-After") == "" {
		t.Errorf("429 response missing Retry-After header")
	}
}
