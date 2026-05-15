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

	"github.com/google/uuid"

	slackadapter "github.com/pericles-luz/crm/internal/adapter/alert/slack"
	"github.com/pericles-luz/crm/internal/media/alert"
)

func newServer(t *testing.T, status int, capture *[]byte, calls *atomic.Int32) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if got := r.Header.Get("Content-Type"); !strings.HasPrefix(got, "application/json") {
			t.Errorf("Content-Type = %q", got)
		}
		body, _ := io.ReadAll(r.Body)
		*capture = append((*capture)[:0], body...)
		w.WriteHeader(status)
	}))
}

func TestNew_RejectsBadConfig(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		cfg  slackadapter.Config
	}{
		{"empty url", slackadapter.Config{}},
		{"relative url", slackadapter.Config{WebhookURL: "/hooks/x"}},
		{"ftp scheme", slackadapter.Config{WebhookURL: "ftp://hooks/x"}},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, err := slackadapter.NewMediaAlerter(tc.cfg); err == nil {
				t.Fatalf("expected error for %s", tc.name)
			}
		})
	}
}

func TestNotify_PostsExpectedJSON(t *testing.T) {
	t.Parallel()
	var captured []byte
	var calls atomic.Int32
	srv := newServer(t, http.StatusOK, &captured, &calls)
	defer srv.Close()

	a, err := slackadapter.NewMediaAlerter(slackadapter.Config{WebhookURL: srv.URL, HTTPClient: srv.Client()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ev := alert.Event{
		TenantID:  uuid.MustParse("11111111-1111-1111-1111-111111111111"),
		MessageID: uuid.MustParse("22222222-2222-2222-2222-222222222222"),
		Key:       "tenant/2026-05/eicar.bin",
		EngineID:  "clamav-1.4.2",
		Signature: "Win.Test.EICAR_HDB-1",
	}
	if err := a.Notify(context.Background(), ev); err != nil {
		t.Fatalf("Notify: %v", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("expected one POST, got %d", calls.Load())
	}

	var got map[string]any
	if err := json.Unmarshal(captured, &got); err != nil {
		t.Fatalf("payload not JSON: %v (body=%s)", err, captured)
	}
	if !strings.Contains(got["text"].(string), "Infected media") {
		t.Errorf("text field unexpected: %v", got["text"])
	}
	if !strings.Contains(string(captured), "11111111-1111-1111-1111-111111111111") {
		t.Error("tenant id should appear in payload")
	}
	if !strings.Contains(string(captured), "clamav-1.4.2") {
		t.Error("engine id should appear in payload")
	}
	if !strings.Contains(string(captured), "Win.Test.EICAR_HDB-1") {
		t.Error("signature should appear in payload")
	}
}

func TestNotify_EmptySignatureRendersPlaceholder(t *testing.T) {
	t.Parallel()
	var captured []byte
	var calls atomic.Int32
	srv := newServer(t, http.StatusOK, &captured, &calls)
	defer srv.Close()

	a, err := slackadapter.NewMediaAlerter(slackadapter.Config{WebhookURL: srv.URL, HTTPClient: srv.Client()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ev := alert.Event{
		TenantID:  uuid.New(),
		MessageID: uuid.New(),
		EngineID:  "clamav-x",
	}
	if err := a.Notify(context.Background(), ev); err != nil {
		t.Fatalf("Notify: %v", err)
	}
	if !strings.Contains(string(captured), "unknown signature") {
		t.Errorf("missing signature placeholder in payload: %s", captured)
	}
}

func TestNotify_NonZeroEventRequired(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
	}))
	defer srv.Close()
	a, err := slackadapter.NewMediaAlerter(slackadapter.Config{WebhookURL: srv.URL, HTTPClient: srv.Client()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := a.Notify(context.Background(), alert.Event{}); !errors.Is(err, alert.ErrEmptyEvent) {
		t.Fatalf("expected ErrEmptyEvent, got %v", err)
	}
	if calls.Load() != 0 {
		t.Fatalf("empty event must not hit Slack, got %d calls", calls.Load())
	}
}

func TestNotify_Non2xxSurfacesBody(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, "invalid_blocks_format")
	}))
	defer srv.Close()
	a, err := slackadapter.NewMediaAlerter(slackadapter.Config{WebhookURL: srv.URL, HTTPClient: srv.Client()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	err = a.Notify(context.Background(), alert.Event{TenantID: uuid.New(), MessageID: uuid.New()})
	if err == nil {
		t.Fatal("expected error for non-2xx")
	}
	if !strings.Contains(err.Error(), "invalid_blocks_format") {
		t.Errorf("error should include body, got: %v", err)
	}
}

func TestNotify_ContextCancelSurfaces(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {}))
	defer srv.Close()
	a, err := slackadapter.NewMediaAlerter(slackadapter.Config{WebhookURL: srv.URL, HTTPClient: srv.Client()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := a.Notify(ctx, alert.Event{TenantID: uuid.New(), MessageID: uuid.New()}); err == nil {
		t.Fatal("expected cancellation error")
	}
}
