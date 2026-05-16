package instagram_test

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

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/adapter/channel/instagram"
)

type stubTokens struct {
	token string
	err   error
}

func (s stubTokens) AccessToken(context.Context, uuid.UUID) (string, error) {
	return s.token, s.err
}

type fixedClock struct{ now time.Time }

func (c fixedClock) Now() time.Time { return c.now }

func mustSender(t *testing.T, tokens instagram.TokenSource, opts ...func(*instagram.SenderConfig)) *instagram.Sender {
	t.Helper()
	cfg := instagram.SenderConfig{
		Clock:       fixedClock{now: time.Date(2026, 5, 16, 12, 0, 0, 0, time.UTC)},
		BackoffBase: time.Nanosecond,
	}
	for _, opt := range opts {
		opt(&cfg)
	}
	s, err := instagram.NewSender(tokens, cfg)
	if err != nil {
		t.Fatalf("NewSender: %v", err)
	}
	return s
}

func withBaseURL(u string) func(*instagram.SenderConfig) {
	return func(c *instagram.SenderConfig) { c.BaseURL = u }
}

func withMaxAttempts(n int) func(*instagram.SenderConfig) {
	return func(c *instagram.SenderConfig) { c.MaxAttempts = n }
}

func withTimeout(d time.Duration) func(*instagram.SenderConfig) {
	return func(c *instagram.SenderConfig) {
		c.HTTPClient = &http.Client{Timeout: d}
	}
}

func TestNewSender_NilTokens(t *testing.T) {
	t.Parallel()
	if _, err := instagram.NewSender(nil, instagram.SenderConfig{}); err == nil {
		t.Fatalf("expected error for nil TokenSource")
	}
}

func TestNewSender_Defaults(t *testing.T) {
	t.Parallel()
	s, err := instagram.NewSender(stubTokens{token: "tok"}, instagram.SenderConfig{})
	if err != nil {
		t.Fatalf("NewSender: %v", err)
	}
	if s == nil {
		t.Fatalf("expected sender, got nil")
	}
}

func TestSendText_Success(t *testing.T) {
	t.Parallel()

	var gotPath, gotQuery, gotMethod, gotCT string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotQuery = r.URL.Query().Get("access_token")
		gotCT = r.Header.Get("Content-Type")
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"message_id":"m_ok"}`))
	}))
	defer srv.Close()

	s := mustSender(t, stubTokens{token: "graph-tok"}, withBaseURL(srv.URL))
	mid, err := s.SendText(context.Background(), uuid.New(), "igsid-1", "ig-biz-42", "hello",
		time.Date(2026, 5, 16, 11, 30, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("SendText: %v", err)
	}
	if mid != "m_ok" {
		t.Errorf("mid: want m_ok, got %q", mid)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("method: want POST, got %s", gotMethod)
	}
	if !strings.HasSuffix(gotPath, "/ig-biz-42/messages") {
		t.Errorf("path: want suffix /ig-biz-42/messages, got %q", gotPath)
	}
	if gotQuery != "graph-tok" {
		t.Errorf("access_token: want graph-tok, got %q", gotQuery)
	}
	if gotCT != "application/json" {
		t.Errorf("content-type: want application/json, got %q", gotCT)
	}
	rec, _ := gotBody["recipient"].(map[string]any)
	if rec == nil || rec["id"] != "igsid-1" {
		t.Errorf("recipient.id: got %v", gotBody["recipient"])
	}
	msg, _ := gotBody["message"].(map[string]any)
	if msg == nil || msg["text"] != "hello" {
		t.Errorf("message.text: got %v", gotBody["message"])
	}
}

func TestSendMedia_ImageSuccess(t *testing.T) {
	t.Parallel()
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"message_id":"m_img"}`))
	}))
	defer srv.Close()

	s := mustSender(t, stubTokens{token: "tok"}, withBaseURL(srv.URL))
	mid, err := s.SendMedia(context.Background(), uuid.New(), "igsid", "ig-biz",
		instagram.Media{Type: instagram.AttachmentImage, URL: "https://example.com/p.jpg"},
		time.Date(2026, 5, 16, 11, 30, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("SendMedia: %v", err)
	}
	if mid != "m_img" {
		t.Errorf("mid: want m_img, got %q", mid)
	}
	msg, _ := gotBody["message"].(map[string]any)
	att, _ := msg["attachment"].(map[string]any)
	if att["type"] != "image" {
		t.Errorf("attachment.type: want image, got %v", att["type"])
	}
	payload, _ := att["payload"].(map[string]any)
	if payload["url"] != "https://example.com/p.jpg" {
		t.Errorf("payload.url: got %v", payload["url"])
	}
}

func TestSendMedia_VideoSuccess(t *testing.T) {
	t.Parallel()
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)
		_, _ = w.Write([]byte(`{"message_id":"m_vid"}`))
	}))
	defer srv.Close()

	s := mustSender(t, stubTokens{token: "tok"}, withBaseURL(srv.URL))
	_, err := s.SendMedia(context.Background(), uuid.New(), "igsid", "ig-biz",
		instagram.Media{Type: instagram.AttachmentVideo, URL: "https://example.com/v.mp4"},
		time.Date(2026, 5, 16, 11, 30, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("SendMedia: %v", err)
	}
	msg, _ := gotBody["message"].(map[string]any)
	att, _ := msg["attachment"].(map[string]any)
	if att["type"] != "video" {
		t.Errorf("attachment.type: want video, got %v", att["type"])
	}
}

func TestSendText_WindowExpired(t *testing.T) {
	t.Parallel()
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	s := mustSender(t, stubTokens{token: "tok"}, withBaseURL(srv.URL))
	// fixedClock now = 2026-05-16 12:00. Last inbound 25h before → expired.
	_, err := s.SendText(context.Background(), uuid.New(), "igsid", "ig-biz", "hi",
		time.Date(2026, 5, 15, 10, 59, 59, 0, time.UTC))
	if !errors.Is(err, instagram.ErrOutsideWindow) {
		t.Fatalf("want ErrOutsideWindow, got %v", err)
	}
	if h := atomic.LoadInt32(&hits); h != 0 {
		t.Errorf("expected 0 HTTP calls, got %d", h)
	}
}

func TestSendText_TokenUnknown(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		tokens instagram.TokenSource
	}{
		{"explicit ErrTokenUnknown", stubTokens{err: instagram.ErrTokenUnknown}},
		{"empty string token", stubTokens{token: ""}},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := mustSender(t, tc.tokens)
			_, err := s.SendText(context.Background(), uuid.New(), "igsid", "ig-biz", "hi",
				time.Date(2026, 5, 16, 11, 30, 0, 0, time.UTC))
			if !errors.Is(err, instagram.ErrTokenUnknown) {
				t.Fatalf("want ErrTokenUnknown, got %v", err)
			}
		})
	}
}

func TestSendText_TokenSourceError(t *testing.T) {
	t.Parallel()
	boom := errors.New("db down")
	s := mustSender(t, stubTokens{err: boom})
	_, err := s.SendText(context.Background(), uuid.New(), "igsid", "ig-biz", "hi",
		time.Date(2026, 5, 16, 11, 30, 0, 0, time.UTC))
	if errors.Is(err, instagram.ErrTokenUnknown) {
		t.Fatalf("non-Unknown lookup error should not coalesce: %v", err)
	}
	if err == nil || !strings.Contains(err.Error(), "token lookup") {
		t.Fatalf("want wrapped token lookup error, got %v", err)
	}
}

func TestSendText_TransportError(t *testing.T) {
	t.Parallel()
	// httptest server that closes the connection immediately to provoke
	// a transport error every attempt.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hj, ok := w.(http.Hijacker)
		if !ok {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		conn, _, _ := hj.Hijack()
		_ = conn.Close()
	}))
	defer srv.Close()

	s := mustSender(t, stubTokens{token: "tok-shh"},
		withBaseURL(srv.URL),
		withMaxAttempts(2),
		withTimeout(2*time.Second),
	)
	_, err := s.SendText(context.Background(), uuid.New(), "igsid", "ig-biz", "hi",
		time.Date(2026, 5, 16, 11, 30, 0, 0, time.UTC))
	if !errors.Is(err, instagram.ErrTransport) {
		t.Fatalf("want ErrTransport, got %v", err)
	}
	if strings.Contains(err.Error(), "tok-shh") {
		t.Fatalf("transport error must not leak access token: %v", err)
	}
	if strings.Contains(err.Error(), "access_token") {
		t.Fatalf("transport error must not leak access_token query param: %v", err)
	}
}

func TestSendText_Non2xxResponse(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		status int
		want   error
	}{
		{"400 rejected", http.StatusBadRequest, instagram.ErrUpstream},
		{"401 rejected", http.StatusUnauthorized, instagram.ErrUpstream},
		{"500 transient", http.StatusInternalServerError, instagram.ErrTransport},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var hits int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				atomic.AddInt32(&hits, 1)
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(`{"error":{"message":"nope"}}`))
			}))
			defer srv.Close()

			s := mustSender(t, stubTokens{token: "tok"},
				withBaseURL(srv.URL),
				withMaxAttempts(2),
			)
			_, err := s.SendText(context.Background(), uuid.New(), "igsid", "ig-biz", "hi",
				time.Date(2026, 5, 16, 11, 30, 0, 0, time.UTC))
			if !errors.Is(err, tc.want) {
				t.Fatalf("want %v, got %v", tc.want, err)
			}
			h := atomic.LoadInt32(&hits)
			if tc.want == instagram.ErrTransport && h != 2 {
				t.Errorf("expected 2 attempts on transient, got %d", h)
			}
			if tc.want == instagram.ErrUpstream && h != 1 {
				t.Errorf("expected 1 attempt on 4xx (no retry), got %d", h)
			}
		})
	}
}

func TestSendMedia_UnsupportedAttachment(t *testing.T) {
	t.Parallel()
	s := mustSender(t, stubTokens{token: "tok"})
	_, err := s.SendMedia(context.Background(), uuid.New(), "igsid", "ig-biz",
		instagram.Media{Type: "audio", URL: "https://example.com/a.mp3"},
		time.Date(2026, 5, 16, 11, 30, 0, 0, time.UTC))
	if !errors.Is(err, instagram.ErrUnsupportedAttachment) {
		t.Fatalf("want ErrUnsupportedAttachment, got %v", err)
	}
}

func TestSendMedia_EmptyURL(t *testing.T) {
	t.Parallel()
	s := mustSender(t, stubTokens{token: "tok"})
	_, err := s.SendMedia(context.Background(), uuid.New(), "igsid", "ig-biz",
		instagram.Media{Type: instagram.AttachmentImage, URL: "  "},
		time.Date(2026, 5, 16, 11, 30, 0, 0, time.UTC))
	if !errors.Is(err, instagram.ErrEmptyURL) {
		t.Fatalf("want ErrEmptyURL, got %v", err)
	}
}

func TestSendText_EmptyBody(t *testing.T) {
	t.Parallel()
	s := mustSender(t, stubTokens{token: "tok"})
	_, err := s.SendText(context.Background(), uuid.New(), "igsid", "ig-biz", "  ",
		time.Date(2026, 5, 16, 11, 30, 0, 0, time.UTC))
	if !errors.Is(err, instagram.ErrEmptyBody) {
		t.Fatalf("want ErrEmptyBody, got %v", err)
	}
}

func TestSendText_EmptyIGBusinessID(t *testing.T) {
	t.Parallel()
	s := mustSender(t, stubTokens{token: "tok"})
	_, err := s.SendText(context.Background(), uuid.New(), "igsid", "", "hi",
		time.Date(2026, 5, 16, 11, 30, 0, 0, time.UTC))
	if err == nil || !strings.Contains(err.Error(), "ig_business_id") {
		t.Fatalf("want ig_business_id empty error, got %v", err)
	}
}

func TestSendText_MalformedResponse(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`not json`))
	}))
	defer srv.Close()

	s := mustSender(t, stubTokens{token: "tok"}, withBaseURL(srv.URL))
	_, err := s.SendText(context.Background(), uuid.New(), "igsid", "ig-biz", "hi",
		time.Date(2026, 5, 16, 11, 30, 0, 0, time.UTC))
	if !errors.Is(err, instagram.ErrUpstream) {
		t.Fatalf("want ErrUpstream on malformed JSON, got %v", err)
	}
}

func TestSendText_MissingMessageID(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	s := mustSender(t, stubTokens{token: "tok"}, withBaseURL(srv.URL))
	_, err := s.SendText(context.Background(), uuid.New(), "igsid", "ig-biz", "hi",
		time.Date(2026, 5, 16, 11, 30, 0, 0, time.UTC))
	if !errors.Is(err, instagram.ErrUpstream) {
		t.Fatalf("want ErrUpstream on missing message_id, got %v", err)
	}
}

func TestSendText_CtxCancelDuringBackoff(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	s := mustSender(t, stubTokens{token: "tok"},
		withBaseURL(srv.URL),
		withMaxAttempts(5),
		func(c *instagram.SenderConfig) { c.BackoffBase = 200 * time.Millisecond },
	)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := s.SendText(ctx, uuid.New(), "igsid", "ig-biz", "hi",
		time.Date(2026, 5, 16, 11, 30, 0, 0, time.UTC))
	if !errors.Is(err, instagram.ErrTransport) {
		t.Fatalf("want ErrTransport on ctx cancel, got %v", err)
	}
}

func TestSendText_RetryThenSuccess(t *testing.T) {
	t.Parallel()
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := atomic.AddInt32(&hits, 1)
		if n < 2 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_, _ = w.Write([]byte(`{"message_id":"m_after_retry"}`))
	}))
	defer srv.Close()

	s := mustSender(t, stubTokens{token: "tok"},
		withBaseURL(srv.URL),
		withMaxAttempts(3),
	)
	mid, err := s.SendText(context.Background(), uuid.New(), "igsid", "ig-biz", "hi",
		time.Date(2026, 5, 16, 11, 30, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if mid != "m_after_retry" {
		t.Errorf("mid: want m_after_retry, got %q", mid)
	}
	if h := atomic.LoadInt32(&hits); h != 2 {
		t.Errorf("expected 2 attempts, got %d", h)
	}
}
