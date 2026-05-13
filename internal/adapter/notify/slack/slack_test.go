package slack_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pericles-luz/crm/internal/adapter/notify/slack"
	"github.com/pericles-luz/crm/internal/iam/ratelimit"
)

// recordingServer is an httptest server that captures the most recent
// request body so tests can assert the on-the-wire payload shape.
type recordingServer struct {
	*httptest.Server
	mu     atomic.Pointer[recordedRequest]
	status int
}

type recordedRequest struct {
	Body        []byte
	ContentType string
	Method      string
}

func newRecordingServer(t *testing.T, status int) *recordingServer {
	t.Helper()
	rs := &recordingServer{status: status}
	rs.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read body: %v", err)
		}
		rs.mu.Store(&recordedRequest{
			Body:        body,
			ContentType: r.Header.Get("Content-Type"),
			Method:      r.Method,
		})
		w.WriteHeader(rs.status)
	}))
	t.Cleanup(rs.Close)
	return rs
}

func (rs *recordingServer) latest() *recordedRequest { return rs.mu.Load() }

func TestNotifier_SatisfiesAlerterPort(t *testing.T) {
	t.Parallel()
	var _ ratelimit.Alerter = slack.New("https://hooks.slack.test/services/X/Y/Z")
}

func TestNotifier_NoOpOnEmptyURL(t *testing.T) {
	t.Parallel()
	n := slack.New("")
	if err := n.Notify(context.Background(), "irrelevant"); err != nil {
		t.Fatalf("empty URL Notify: got %v, want nil (no-op)", err)
	}
}

func TestNotifier_PostsJSONPayloadOn2xx(t *testing.T) {
	t.Parallel()
	rs := newRecordingServer(t, http.StatusOK)
	n := slack.New(rs.URL)

	if err := n.Notify(context.Background(), "lockout: master:42"); err != nil {
		t.Fatalf("Notify: %v", err)
	}
	got := rs.latest()
	if got == nil {
		t.Fatal("server received no request")
	}
	if got.Method != http.MethodPost {
		t.Fatalf("method = %q, want POST", got.Method)
	}
	if got.ContentType != "application/json" {
		t.Fatalf("content-type = %q, want application/json", got.ContentType)
	}
	var body struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(got.Body, &body); err != nil {
		t.Fatalf("unmarshal body: %v", err)
	}
	if body.Text != "lockout: master:42" {
		t.Fatalf("body.Text = %q, want %q", body.Text, "lockout: master:42")
	}
}

func TestNotifier_NonSuccessReturnsError(t *testing.T) {
	t.Parallel()
	rs := newRecordingServer(t, http.StatusInternalServerError)
	n := slack.New(rs.URL)
	err := n.Notify(context.Background(), "anything")
	if err == nil {
		t.Fatal("Notify on 500 returned nil error")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Fatalf("error %q does not mention status 500", err.Error())
	}
}

func TestNotifier_HonoursContextDeadline(t *testing.T) {
	t.Parallel()
	// Server sleeps longer than the per-call timeout — the request MUST
	// be cancelled by the context deadline before the response arrives.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(2 * time.Second):
		}
	}))
	t.Cleanup(srv.Close)

	n := slack.New(srv.URL).WithTimeout(50 * time.Millisecond)
	start := time.Now()
	err := n.Notify(context.Background(), "ping")
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected timeout error, got nil")
	}
	if elapsed > time.Second {
		t.Fatalf("Notify took %v, expected timeout near 50ms — deadline not honoured", elapsed)
	}
}

// stubDoer is a Doer that returns a canned response or error without
// touching the network. Used to cover the marshal / build-request /
// non-2xx paths deterministically.
type stubDoer struct {
	resp *http.Response
	err  error
}

func (s stubDoer) Do(_ *http.Request) (*http.Response, error) {
	if s.err != nil {
		return nil, s.err
	}
	return s.resp, nil
}

func TestNotifier_WithClientErrorPropagates(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("dial: connection refused")
	n := slack.New("https://hooks.slack.test/").WithClient(stubDoer{err: sentinel})
	err := n.Notify(context.Background(), "x")
	if err == nil || !errors.Is(err, sentinel) {
		t.Fatalf("Notify err = %v, want wrap of sentinel", err)
	}
}

func TestNotifier_NewRequestErrorOnBadURL(t *testing.T) {
	t.Parallel()
	// http.NewRequestWithContext rejects a URL with control characters.
	n := slack.New("http://invalid-url-with-space /webhook")
	err := n.Notify(context.Background(), "x")
	if err == nil {
		t.Fatal("expected a build-request error, got nil")
	}
}
