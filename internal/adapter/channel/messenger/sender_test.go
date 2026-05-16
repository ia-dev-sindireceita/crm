package messenger_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/pericles-luz/crm/internal/adapter/channel/messenger"
	"github.com/pericles-luz/crm/internal/inbox"
)

// TestSender_ImplementsOutboundChannel pins the interface contract at compile time.
func TestSender_ImplementsOutboundChannel(t *testing.T) {
	t.Parallel()
	var _ inbox.OutboundChannel = (*messenger.Sender)(nil)
}

func TestNew_Validation(t *testing.T) {
	t.Parallel()

	lookup := stubLookup(messenger.TenantConfig{PageID: "page123", Enabled: true})

	cases := []struct {
		name    string
		token   string
		lookup  messenger.TenantConfigLookup
		reg     prometheus.Registerer
		wantErr string
	}{
		{"empty token", "", lookup, prometheus.NewRegistry(), "META_GRAPH_TOKEN"},
		{"nil lookup", "tok", nil, prometheus.NewRegistry(), "tenant config lookup"},
		{"nil registerer", "tok", lookup, nil, "prometheus registerer"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := messenger.New(tc.token, tc.lookup, tc.reg)
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("expected error containing %q, got %q", tc.wantErr, err.Error())
			}
		})
	}
}

func TestSendMessage_FeatureFlagOff_NoHTTPCall(t *testing.T) {
	t.Parallel()

	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	s := mustSender(t, "tok", stubLookup(messenger.TenantConfig{PageID: "p", Enabled: false}),
		messenger.WithBaseURL(srv.URL))

	_, err := s.SendMessage(context.Background(), outMsg("hi"))
	if !errors.Is(err, inbox.ErrChannelDisabled) {
		t.Fatalf("expected ErrChannelDisabled, got %v", err)
	}
	if h := atomic.LoadInt32(&hits); h != 0 {
		t.Fatalf("expected no HTTP calls when disabled, got %d", h)
	}
}

func TestSendMessage_MissingPageID_AuthFailed(t *testing.T) {
	t.Parallel()

	s := mustSender(t, "tok", stubLookup(messenger.TenantConfig{Enabled: true}))
	_, err := s.SendMessage(context.Background(), outMsg("hi"))
	if !errors.Is(err, inbox.ErrChannelAuthFailed) {
		t.Fatalf("expected ErrChannelAuthFailed, got %v", err)
	}
}

func TestSendMessage_LookupError_Transient(t *testing.T) {
	t.Parallel()

	boom := errors.New("db is down")
	s := mustSender(t, "tok", func(_ context.Context, _ uuid.UUID) (messenger.TenantConfig, error) {
		return messenger.TenantConfig{}, boom
	})
	_, err := s.SendMessage(context.Background(), outMsg("hi"))
	if !errors.Is(err, inbox.ErrChannelTransient) {
		t.Fatalf("expected ErrChannelTransient, got %v", err)
	}
}

func TestSendMessage_EmptyRecipient_Rejected(t *testing.T) {
	t.Parallel()

	s := mustSender(t, "tok", stubLookup(messenger.TenantConfig{PageID: "p", Enabled: true}))
	m := inbox.OutboundMessage{
		TenantID:     uuid.New(),
		ToExternalID: "",
		Body:         "hello",
	}
	_, err := s.SendMessage(context.Background(), m)
	if !errors.Is(err, inbox.ErrChannelRejected) {
		t.Fatalf("expected ErrChannelRejected, got %v", err)
	}
}

func TestSendMessage_TextSuccess(t *testing.T) {
	t.Parallel()

	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Errorf("decode request body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"message_id":"m_abc123","recipient_id":"psid"}`))
	}))
	defer srv.Close()

	s := mustSender(t, "test-token", stubLookup(messenger.TenantConfig{PageID: "page42", Enabled: true}),
		messenger.WithBaseURL(srv.URL))

	mid, err := s.SendMessage(context.Background(), outMsg("Hello Messenger!"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if mid != "m_abc123" {
		t.Errorf("expected mid m_abc123, got %q", mid)
	}

	// Verify request shape.
	rec, _ := gotBody["recipient"].(map[string]any)
	if rec == nil || rec["id"] == nil {
		t.Fatalf("recipient.id missing from request body: %v", gotBody)
	}
	msg, _ := gotBody["message"].(map[string]any)
	if msg == nil || msg["text"] == nil {
		t.Fatalf("message.text missing from request body: %v", gotBody)
	}
}

func TestSendMessage_ImageAttachment(t *testing.T) {
	t.Parallel()

	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"message_id":"m_img1"}`))
	}))
	defer srv.Close()

	s := mustSender(t, "tok", stubLookup(messenger.TenantConfig{PageID: "p", Enabled: true}),
		messenger.WithBaseURL(srv.URL))

	body := messenger.ImagePrefix + "https://example.com/image.jpg"
	mid, err := s.SendMessage(context.Background(), outMsg(body))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if mid != "m_img1" {
		t.Errorf("expected m_img1, got %q", mid)
	}

	msg, _ := gotBody["message"].(map[string]any)
	att, _ := msg["attachment"].(map[string]any)
	if att["type"] != "image" {
		t.Errorf("expected attachment type image, got %v", att["type"])
	}
	payload, _ := att["payload"].(map[string]any)
	if payload["is_reusable"] != true {
		t.Errorf("is_reusable: expected bool true, got %T(%v)", payload["is_reusable"], payload["is_reusable"])
	}
}

func TestSendMessage_VideoAttachment(t *testing.T) {
	t.Parallel()

	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_, _ = w.Write([]byte(`{"message_id":"m_vid1"}`))
	}))
	defer srv.Close()

	s := mustSender(t, "tok", stubLookup(messenger.TenantConfig{PageID: "p", Enabled: true}),
		messenger.WithBaseURL(srv.URL))

	body := messenger.VideoPrefix + "https://example.com/video.mp4"
	if _, err := s.SendMessage(context.Background(), outMsg(body)); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	msg, _ := gotBody["message"].(map[string]any)
	att, _ := msg["attachment"].(map[string]any)
	if att["type"] != "video" {
		t.Errorf("expected video, got %v", att["type"])
	}
}

func TestSendMessage_FileAttachment(t *testing.T) {
	t.Parallel()

	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_, _ = w.Write([]byte(`{"message_id":"m_file1"}`))
	}))
	defer srv.Close()

	s := mustSender(t, "tok", stubLookup(messenger.TenantConfig{PageID: "p", Enabled: true}),
		messenger.WithBaseURL(srv.URL))

	body := messenger.FilePrefix + "https://example.com/doc.pdf"
	if _, err := s.SendMessage(context.Background(), outMsg(body)); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	msg, _ := gotBody["message"].(map[string]any)
	att, _ := msg["attachment"].(map[string]any)
	if att["type"] != "file" {
		t.Errorf("expected file, got %v", att["type"])
	}
}

func TestSendMessage_Unauthorized_AuthFailed(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"Invalid OAuth access token"}}`))
	}))
	defer srv.Close()

	s := mustSender(t, "bad-tok", stubLookup(messenger.TenantConfig{PageID: "p", Enabled: true}),
		messenger.WithBaseURL(srv.URL))
	_, err := s.SendMessage(context.Background(), outMsg("hi"))
	if !errors.Is(err, inbox.ErrChannelAuthFailed) {
		t.Fatalf("expected ErrChannelAuthFailed, got %v", err)
	}
}

func TestSendMessage_ClientError_Rejected(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"outside conversation window"}}`))
	}))
	defer srv.Close()

	s := mustSender(t, "tok", stubLookup(messenger.TenantConfig{PageID: "p", Enabled: true}),
		messenger.WithBaseURL(srv.URL))
	_, err := s.SendMessage(context.Background(), outMsg("late reply"))
	if !errors.Is(err, inbox.ErrChannelRejected) {
		t.Fatalf("expected ErrChannelRejected, got %v", err)
	}
}

func TestSendMessage_ServerError_Transient_Retries(t *testing.T) {
	t.Parallel()

	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	s := mustSender(t, "tok", stubLookup(messenger.TenantConfig{PageID: "p", Enabled: true}),
		messenger.WithBaseURL(srv.URL),
		messenger.WithMaxAttempts(3),
		messenger.WithBackoffBase(0))
	_, err := s.SendMessage(context.Background(), outMsg("hi"))
	if !errors.Is(err, inbox.ErrChannelTransient) {
		t.Fatalf("expected ErrChannelTransient, got %v", err)
	}
	if h := atomic.LoadInt32(&hits); h != 3 {
		t.Errorf("expected 3 attempts, got %d", h)
	}
}

func TestSendMessage_SuccessAfterTransient(t *testing.T) {
	t.Parallel()

	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := atomic.AddInt32(&hits, 1)
		if n < 2 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"message_id":"m_retry_ok"}`))
	}))
	defer srv.Close()

	s := mustSender(t, "tok", stubLookup(messenger.TenantConfig{PageID: "p", Enabled: true}),
		messenger.WithBaseURL(srv.URL),
		messenger.WithMaxAttempts(3),
		messenger.WithBackoffBase(0))
	mid, err := s.SendMessage(context.Background(), outMsg("hi"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if mid != "m_retry_ok" {
		t.Errorf("expected m_retry_ok, got %q", mid)
	}
}

func TestSendMessage_CtxCancel_Transient(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	s := mustSender(t, "tok", stubLookup(messenger.TenantConfig{PageID: "p", Enabled: true}),
		messenger.WithBaseURL(srv.URL),
		messenger.WithMaxAttempts(5),
		messenger.WithBackoffBase(200*time.Millisecond))
	_, err := s.SendMessage(ctx, outMsg("hi"))
	if !errors.Is(err, inbox.ErrChannelTransient) {
		t.Fatalf("expected ErrChannelTransient on ctx cancel, got %v", err)
	}
}

func TestSendMessage_AuthHeader(t *testing.T) {
	t.Parallel()

	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"message_id":"m_auth"}`))
	}))
	defer srv.Close()

	s := mustSender(t, "secret-token", stubLookup(messenger.TenantConfig{PageID: "p", Enabled: true}),
		messenger.WithBaseURL(srv.URL))
	_, _ = s.SendMessage(context.Background(), outMsg("hi"))
	if gotAuth != "Bearer secret-token" {
		t.Errorf("expected Bearer secret-token, got %q", gotAuth)
	}
}

func TestSendMessage_URLPath(t *testing.T) {
	t.Parallel()

	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_, _ = w.Write([]byte(`{"message_id":"m_path"}`))
	}))
	defer srv.Close()

	s := mustSender(t, "tok", stubLookup(messenger.TenantConfig{PageID: "page99", Enabled: true}),
		messenger.WithBaseURL(srv.URL))
	_, _ = s.SendMessage(context.Background(), outMsg("hi"))
	if !strings.Contains(gotPath, "page99/messages") {
		t.Errorf("expected path to contain page99/messages, got %q", gotPath)
	}
}

// helpers

func stubLookup(cfg messenger.TenantConfig) messenger.TenantConfigLookup {
	return func(_ context.Context, _ uuid.UUID) (messenger.TenantConfig, error) { return cfg, nil }
}

func mustSender(t *testing.T, token string, lookup messenger.TenantConfigLookup, opts ...messenger.Option) *messenger.Sender {
	t.Helper()
	s, err := messenger.New(token, lookup, prometheus.NewRegistry(), opts...)
	if err != nil {
		t.Fatalf("messenger.New: %v", err)
	}
	return s
}

func outMsg(body string) inbox.OutboundMessage {
	return inbox.OutboundMessage{
		TenantID:     uuid.New(),
		ToExternalID: "psid-123",
		Body:         body,
	}
}
