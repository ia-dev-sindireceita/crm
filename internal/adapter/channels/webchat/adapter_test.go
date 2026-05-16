package webchat_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/pericles-luz/crm/internal/adapter/channels/webchat"
)

func TestNew_NilChecks(t *testing.T) {
	broker := webchat.NewBroker()
	store := webchat.NewInMemorySessionStore()
	flag := &fakeFlag{on: true}
	origins := &fakeOrigins{allowAll: true}
	rl := webchat.NewInMemoryRateLimiter(0)
	inbox := &fakeInbox{}

	cases := []struct {
		name string
		fn   func() error
	}{
		{"nil inbox", func() error {
			_, err := webchat.New(nil, store, origins, flag, rl, broker, nil)
			return err
		}},
		{"nil sessions", func() error {
			_, err := webchat.New(inbox, nil, origins, flag, rl, broker, nil)
			return err
		}},
		{"nil origins", func() error {
			_, err := webchat.New(inbox, store, nil, flag, rl, broker, nil)
			return err
		}},
		{"nil flag", func() error {
			_, err := webchat.New(inbox, store, origins, nil, rl, broker, nil)
			return err
		}},
		{"nil rl", func() error {
			_, err := webchat.New(inbox, store, origins, flag, nil, broker, nil)
			return err
		}},
		{"nil broker", func() error {
			_, err := webchat.New(inbox, store, origins, flag, rl, nil, nil)
			return err
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.fn(); err == nil {
				t.Errorf("want error for %s, got nil", tc.name)
			}
		})
	}
}

func TestHandleMessage_RateLimit_Returns429(t *testing.T) {
	fi := &fakeInbox{}
	a, _ := newAdapter(t, &fakeFlag{on: true}, webchat.NewInMemoryRateLimiter(0), &fakeOrigins{allowAll: true, sig: "s"}, fi)

	// Build a rate limiter that denies message bucket but not session bucket.
	// Easiest: use a custom rl that allows 1 per key (first = session create, second = message → denied).
	// Since we need to differentiate, use a very low limit that triggers on the "wc.msg." key.
	// Simpler approach: build adapter with rl max=1; first allow is for session, second message gets denied.
	rl2 := webchat.NewInMemoryRateLimiter(1)
	a2, _ := newAdapter(t, &fakeFlag{on: true}, rl2, &fakeOrigins{allowAll: true, sig: "s"}, fi)
	mux := http.NewServeMux()
	a2.Register(mux)
	tenantID := uuid.New()
	sessID, csrf := createSession(t, mux, tenantID)

	// First message should work (rl max=1 for the msg key, not yet hit).
	// But the rate limiter is shared per-key; session create used key "wc.sess.*",
	// message uses key "wc.msg.*sessID". These are distinct keys.
	// Use rl with max=0 for message bucket by using a per-key approach:
	// instead, just exhaust the message bucket with two calls.
	send := func() int {
		body := `{"body":"x","client_msg_id":"` + uuid.New().String() + `"}`
		req := httptest.NewRequest(http.MethodPost, "/widget/v1/message", strings.NewReader(body))
		req.Header.Set(webchat.HeaderSession, sessID)
		req.Header.Set(webchat.HeaderCSRF, csrf)
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		return w.Code
	}

	// max=1 per key; first call to wc.msg.* key allowed, second denied.
	first := send()
	second := send()
	if first != http.StatusNoContent {
		t.Errorf("first message: want 204, got %d", first)
	}
	if second != http.StatusTooManyRequests {
		t.Errorf("second message: want 429, got %d", second)
	}
	_ = a
}

func TestHandleMessage_BadJSON_Returns400(t *testing.T) {
	fi := &fakeInbox{}
	a, _ := newAdapter(t, &fakeFlag{on: true}, webchat.NewInMemoryRateLimiter(0), &fakeOrigins{allowAll: true, sig: "s"}, fi)
	mux := http.NewServeMux()
	a.Register(mux)
	tenantID := uuid.New()
	sessID, csrf := createSession(t, mux, tenantID)

	req := httptest.NewRequest(http.MethodPost, "/widget/v1/message", strings.NewReader(`not json`))
	req.Header.Set(webchat.HeaderSession, sessID)
	req.Header.Set(webchat.HeaderCSRF, csrf)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("want 400, got %d", w.Code)
	}
}

func TestHandleStream_MissingSession_Returns401(t *testing.T) {
	a, _ := newAdapter(t, &fakeFlag{on: true}, webchat.NewInMemoryRateLimiter(0), &fakeOrigins{allowAll: true, sig: "s"}, &fakeInbox{})
	mux := http.NewServeMux()
	a.Register(mux)

	req := httptest.NewRequest(http.MethodGet, "/widget/v1/stream", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("want 401, got %d", w.Code)
	}
}

func TestHandleStream_FlagOff_Returns404(t *testing.T) {
	// Create session with flag on, then disable flag for stream.
	fi := &fakeInbox{}
	flag := &fakeFlag{on: true}
	a, _ := newAdapter(t, flag, webchat.NewInMemoryRateLimiter(0), &fakeOrigins{allowAll: true, sig: "s"}, fi)
	mux := http.NewServeMux()
	a.Register(mux)
	tenantID := uuid.New()
	sessID, _ := createSession(t, mux, tenantID)

	flag.on = false
	req := httptest.NewRequest(http.MethodGet, "/widget/v1/stream", nil)
	req.Header.Set(webchat.HeaderSession, sessID)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Errorf("want 404, got %d", w.Code)
	}
}
