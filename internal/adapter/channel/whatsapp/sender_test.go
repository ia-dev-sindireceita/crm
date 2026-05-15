package whatsapp_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/pericles-luz/crm/internal/adapter/channel/whatsapp"
	"github.com/pericles-luz/crm/internal/inbox"
)

// TestSender_ImplementsOutboundChannel pins the interface assertion that
// the sender struct compiles against the port. Failure mode is a build
// break, but having a runtime test makes the contract explicit.
func TestSender_ImplementsOutboundChannel(t *testing.T) {
	t.Parallel()
	var _ inbox.OutboundChannel = (*whatsapp.Sender)(nil)
}

func TestNew_Validation(t *testing.T) {
	t.Parallel()

	lookup := stubLookup(whatsapp.TenantConfig{PhoneNumberID: "p", Enabled: true})

	cases := []struct {
		name    string
		token   string
		lookup  whatsapp.TenantConfigLookup
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
			_, err := whatsapp.New(tc.token, tc.lookup, tc.reg)
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

	s := mustSender(t, "tok-disabled", stubLookup(whatsapp.TenantConfig{
		PhoneNumberID: "p123",
		Enabled:       false,
	}), whatsapp.WithBaseURL(srv.URL))

	_, err := s.SendMessage(context.Background(), outMsg("hi"))
	if !errors.Is(err, inbox.ErrChannelDisabled) {
		t.Fatalf("expected ErrChannelDisabled, got %v", err)
	}
	if h := atomic.LoadInt32(&hits); h != 0 {
		t.Fatalf("expected no HTTP calls when disabled, got %d", h)
	}
}

func TestSendMessage_MissingPhoneNumberID_AuthFailed(t *testing.T) {
	t.Parallel()

	s := mustSender(t, "tok-noid", stubLookup(whatsapp.TenantConfig{Enabled: true}))

	_, err := s.SendMessage(context.Background(), outMsg("hi"))
	if !errors.Is(err, inbox.ErrChannelAuthFailed) {
		t.Fatalf("expected ErrChannelAuthFailed, got %v", err)
	}
}

func TestSendMessage_LookupError_Transient(t *testing.T) {
	t.Parallel()

	boom := errors.New("db is down")
	lookup := func(_ context.Context, _ uuid.UUID) (whatsapp.TenantConfig, error) {
		return whatsapp.TenantConfig{}, boom
	}
	s := mustSender(t, "tok-lkupbad", lookup)
	_, err := s.SendMessage(context.Background(), outMsg("hi"))
	if !errors.Is(err, inbox.ErrChannelTransient) {
		t.Fatalf("expected ErrChannelTransient, got %v", err)
	}
	if strings.Contains(err.Error(), "tok-lkupbad") {
		t.Fatalf("error must not echo the bearer token; got %q", err.Error())
	}
}

func TestSendMessage_TextSuccess_ReturnsWAMID(t *testing.T) {
	t.Parallel()

	var captured *http.Request
	var capturedBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured = r
		capturedBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"messaging_product":"whatsapp","messages":[{"id":"wamid.OK_TEXT"}]}`))
	}))
	defer srv.Close()

	s := mustSender(t, "tok-good", stubLookup(whatsapp.TenantConfig{
		PhoneNumberID: "ph42",
		Enabled:       true,
	}), whatsapp.WithBaseURL(srv.URL))

	wamid, err := s.SendMessage(context.Background(), outMsg("hello there"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if wamid != "wamid.OK_TEXT" {
		t.Fatalf("wamid = %q, want wamid.OK_TEXT", wamid)
	}

	if got, want := captured.URL.Path, "/ph42/messages"; got != want {
		t.Errorf("path = %q, want %q", got, want)
	}
	if got := captured.Header.Get("Authorization"); got != "Bearer tok-good" {
		t.Errorf("Authorization header = %q, want %q", got, "Bearer tok-good")
	}
	if got := captured.Header.Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", got)
	}

	var payload map[string]any
	if err := json.Unmarshal(capturedBody, &payload); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if got := payload["messaging_product"]; got != "whatsapp" {
		t.Errorf("messaging_product = %v, want whatsapp", got)
	}
	if got := payload["type"]; got != "text" {
		t.Errorf("type = %v, want text", got)
	}
	if got := payload["to"]; got != "+5511999999999" {
		t.Errorf("to = %v, want +5511999999999", got)
	}
	text, ok := payload["text"].(map[string]any)
	if !ok {
		t.Fatalf("payload.text not an object: %v", payload["text"])
	}
	if text["body"] != "hello there" {
		t.Errorf("text.body = %v, want %q", text["body"], "hello there")
	}
}

func TestSendMessage_TemplatePayload(t *testing.T) {
	t.Parallel()

	var capturedBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedBody, _ = io.ReadAll(r.Body)
		_, _ = w.Write([]byte(`{"messages":[{"id":"wamid.TMPL"}]}`))
	}))
	defer srv.Close()

	s := mustSender(t, "tok", stubLookup(whatsapp.TenantConfig{
		PhoneNumberID: "ph9",
		Enabled:       true,
	}), whatsapp.WithBaseURL(srv.URL))

	wamid, err := s.SendMessage(context.Background(), outMsg("wa:template:welcome:en_US"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if wamid != "wamid.TMPL" {
		t.Fatalf("wamid = %q, want wamid.TMPL", wamid)
	}
	var payload map[string]any
	if err := json.Unmarshal(capturedBody, &payload); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if got := payload["type"]; got != "template" {
		t.Fatalf("type = %v, want template", got)
	}
	tmpl, ok := payload["template"].(map[string]any)
	if !ok {
		t.Fatalf("payload.template not an object: %v", payload["template"])
	}
	if tmpl["name"] != "welcome" {
		t.Errorf("template.name = %v, want welcome", tmpl["name"])
	}
	lang, ok := tmpl["language"].(map[string]any)
	if !ok {
		t.Fatalf("template.language not an object: %v", tmpl["language"])
	}
	if lang["code"] != "en_US" {
		t.Errorf("template.language.code = %v, want en_US", lang["code"])
	}
}

func TestSendMessage_InvalidTemplateBody_Rejected(t *testing.T) {
	t.Parallel()

	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	s := mustSender(t, "tok", stubLookup(whatsapp.TenantConfig{
		PhoneNumberID: "ph",
		Enabled:       true,
	}), whatsapp.WithBaseURL(srv.URL))

	cases := []string{
		"wa:template:",        // missing name and lang
		"wa:template:onlyone", // missing lang
		"wa:template::en_US",  // empty name
		"wa:template:hi:",     // empty lang
	}
	for _, body := range cases {
		body := body
		t.Run(body, func(t *testing.T) {
			t.Parallel()
			_, err := s.SendMessage(context.Background(), outMsg(body))
			if !errors.Is(err, inbox.ErrChannelRejected) {
				t.Fatalf("expected ErrChannelRejected for %q, got %v", body, err)
			}
		})
	}
	if h := atomic.LoadInt32(&hits); h != 0 {
		t.Fatalf("encoding rejection must not reach HTTP; got %d hits", h)
	}
}

func TestSendMessage_EmptyTextBody_Rejected(t *testing.T) {
	t.Parallel()

	s := mustSender(t, "tok", stubLookup(whatsapp.TenantConfig{
		PhoneNumberID: "ph",
		Enabled:       true,
	}))

	msg := outMsg("")
	msg.Body = "   "
	_, err := s.SendMessage(context.Background(), msg)
	if !errors.Is(err, inbox.ErrChannelRejected) {
		t.Fatalf("expected ErrChannelRejected for empty body, got %v", err)
	}
}

func TestSendMessage_EmptyRecipient_Rejected(t *testing.T) {
	t.Parallel()

	s := mustSender(t, "tok", stubLookup(whatsapp.TenantConfig{
		PhoneNumberID: "ph",
		Enabled:       true,
	}))

	msg := outMsg("hello")
	msg.ToExternalID = ""
	_, err := s.SendMessage(context.Background(), msg)
	if !errors.Is(err, inbox.ErrChannelRejected) {
		t.Fatalf("expected ErrChannelRejected for empty recipient, got %v", err)
	}
}

func TestSendMessage_401_AuthFailed_NoRetry(t *testing.T) {
	t.Parallel()

	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"Invalid OAuth access token","type":"OAuthException","code":190}}`))
	}))
	defer srv.Close()

	s := mustSender(t, "tok-revoked", stubLookup(whatsapp.TenantConfig{
		PhoneNumberID: "ph",
		Enabled:       true,
	}),
		whatsapp.WithBaseURL(srv.URL),
		whatsapp.WithBackoffBase(time.Microsecond),
	)

	_, err := s.SendMessage(context.Background(), outMsg("hi"))
	if !errors.Is(err, inbox.ErrChannelAuthFailed) {
		t.Fatalf("expected ErrChannelAuthFailed, got %v", err)
	}
	if h := atomic.LoadInt32(&hits); h != 1 {
		t.Fatalf("expected 1 attempt on 401 (no retry), got %d", h)
	}
	if strings.Contains(err.Error(), "tok-revoked") {
		t.Fatalf("error must not echo the bearer token; got %q", err.Error())
	}
	if !strings.Contains(err.Error(), "Invalid OAuth access token") {
		t.Fatalf("error should surface meta message; got %q", err.Error())
	}
}

func TestSendMessage_403_AuthFailed(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":{"message":"forbidden"}}`))
	}))
	defer srv.Close()

	s := mustSender(t, "tok", stubLookup(whatsapp.TenantConfig{
		PhoneNumberID: "ph",
		Enabled:       true,
	}), whatsapp.WithBaseURL(srv.URL))

	_, err := s.SendMessage(context.Background(), outMsg("hi"))
	if !errors.Is(err, inbox.ErrChannelAuthFailed) {
		t.Fatalf("expected ErrChannelAuthFailed, got %v", err)
	}
}

func TestSendMessage_400_Rejected_NoRetry(t *testing.T) {
	t.Parallel()

	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"Message failed to send because more than 24 hours have passed since the customer last replied to this number","code":131047}}`))
	}))
	defer srv.Close()

	s := mustSender(t, "tok", stubLookup(whatsapp.TenantConfig{
		PhoneNumberID: "ph",
		Enabled:       true,
	}),
		whatsapp.WithBaseURL(srv.URL),
		whatsapp.WithBackoffBase(time.Microsecond),
	)

	_, err := s.SendMessage(context.Background(), outMsg("hi"))
	if !errors.Is(err, inbox.ErrChannelRejected) {
		t.Fatalf("expected ErrChannelRejected, got %v", err)
	}
	if h := atomic.LoadInt32(&hits); h != 1 {
		t.Fatalf("expected 1 attempt on 4xx (no retry), got %d", h)
	}
	if !strings.Contains(err.Error(), "24 hours have passed") {
		t.Fatalf("error should preserve meta message; got %q", err.Error())
	}
}

func TestSendMessage_429_Rejected_NoRetry(t *testing.T) {
	t.Parallel()

	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	s := mustSender(t, "tok", stubLookup(whatsapp.TenantConfig{
		PhoneNumberID: "ph",
		Enabled:       true,
	}),
		whatsapp.WithBaseURL(srv.URL),
		whatsapp.WithBackoffBase(time.Microsecond),
	)

	_, err := s.SendMessage(context.Background(), outMsg("hi"))
	if !errors.Is(err, inbox.ErrChannelRejected) {
		t.Fatalf("expected ErrChannelRejected (no retry on 4xx), got %v", err)
	}
	if h := atomic.LoadInt32(&hits); h != 1 {
		t.Fatalf("expected 1 attempt, got %d", h)
	}
}

func TestSendMessage_500_RetryThenTransient(t *testing.T) {
	t.Parallel()

	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()

	s := mustSender(t, "tok", stubLookup(whatsapp.TenantConfig{
		PhoneNumberID: "ph",
		Enabled:       true,
	}),
		whatsapp.WithBaseURL(srv.URL),
		whatsapp.WithBackoffBase(time.Microsecond),
		whatsapp.WithMaxAttempts(3),
	)

	_, err := s.SendMessage(context.Background(), outMsg("hi"))
	if !errors.Is(err, inbox.ErrChannelTransient) {
		t.Fatalf("expected ErrChannelTransient, got %v", err)
	}
	if h := atomic.LoadInt32(&hits); h != 3 {
		t.Fatalf("expected 3 attempts on persistent 5xx, got %d", h)
	}
}

func TestSendMessage_5xxThenOK_RetrySuccess(t *testing.T) {
	t.Parallel()

	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := atomic.AddInt32(&hits, 1)
		if n < 3 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_, _ = w.Write([]byte(`{"messages":[{"id":"wamid.RECOVERED"}]}`))
	}))
	defer srv.Close()

	s := mustSender(t, "tok", stubLookup(whatsapp.TenantConfig{
		PhoneNumberID: "ph",
		Enabled:       true,
	}),
		whatsapp.WithBaseURL(srv.URL),
		whatsapp.WithBackoffBase(time.Microsecond),
		whatsapp.WithMaxAttempts(5),
	)

	wamid, err := s.SendMessage(context.Background(), outMsg("hi"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if wamid != "wamid.RECOVERED" {
		t.Fatalf("wamid = %q, want wamid.RECOVERED", wamid)
	}
	if h := atomic.LoadInt32(&hits); h != 3 {
		t.Fatalf("expected 3 attempts before success, got %d", h)
	}
}

func TestSendMessage_NetworkError_Retries(t *testing.T) {
	t.Parallel()

	// httptest with immediate close → connection reset / EOF.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hj, ok := w.(http.Hijacker)
		if !ok {
			http.Error(w, "no hijack", http.StatusInternalServerError)
			return
		}
		conn, _, err := hj.Hijack()
		if err == nil {
			_ = conn.Close()
		}
	}))
	defer srv.Close()

	s := mustSender(t, "tok", stubLookup(whatsapp.TenantConfig{
		PhoneNumberID: "ph",
		Enabled:       true,
	}),
		whatsapp.WithBaseURL(srv.URL),
		whatsapp.WithBackoffBase(time.Microsecond),
		whatsapp.WithMaxAttempts(2),
	)

	_, err := s.SendMessage(context.Background(), outMsg("hi"))
	if !errors.Is(err, inbox.ErrChannelTransient) {
		t.Fatalf("expected ErrChannelTransient on network error, got %v", err)
	}
}

func TestSendMessage_ContextCanceledMidBackoff(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	s := mustSender(t, "tok", stubLookup(whatsapp.TenantConfig{
		PhoneNumberID: "ph",
		Enabled:       true,
	}),
		whatsapp.WithBaseURL(srv.URL),
		whatsapp.WithBackoffBase(50*time.Millisecond),
		whatsapp.WithMaxAttempts(5),
	)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()

	_, err := s.SendMessage(ctx, outMsg("hi"))
	if !errors.Is(err, inbox.ErrChannelTransient) {
		t.Fatalf("expected ErrChannelTransient when ctx is cancelled, got %v", err)
	}
}

func TestSendMessage_OKWithoutMessages_Rejected(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"messaging_product":"whatsapp"}`))
	}))
	defer srv.Close()

	s := mustSender(t, "tok", stubLookup(whatsapp.TenantConfig{
		PhoneNumberID: "ph",
		Enabled:       true,
	}), whatsapp.WithBaseURL(srv.URL))

	_, err := s.SendMessage(context.Background(), outMsg("hi"))
	if !errors.Is(err, inbox.ErrChannelRejected) {
		t.Fatalf("expected ErrChannelRejected on missing messages[]; got %v", err)
	}
}

func TestSendMessage_OKWithGarbageBody_Rejected(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`not-json`))
	}))
	defer srv.Close()

	s := mustSender(t, "tok", stubLookup(whatsapp.TenantConfig{
		PhoneNumberID: "ph",
		Enabled:       true,
	}), whatsapp.WithBaseURL(srv.URL))

	_, err := s.SendMessage(context.Background(), outMsg("hi"))
	if !errors.Is(err, inbox.ErrChannelRejected) {
		t.Fatalf("expected ErrChannelRejected on unparseable body, got %v", err)
	}
}

func TestSendMessage_UnexpectedStatus_Transient(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusMultipleChoices) // 3xx is unexpected
	}))
	defer srv.Close()

	s := mustSender(t, "tok", stubLookup(whatsapp.TenantConfig{
		PhoneNumberID: "ph",
		Enabled:       true,
	}),
		whatsapp.WithBaseURL(srv.URL),
		whatsapp.WithBackoffBase(time.Microsecond),
	)

	_, err := s.SendMessage(context.Background(), outMsg("hi"))
	if !errors.Is(err, inbox.ErrChannelTransient) {
		t.Fatalf("expected ErrChannelTransient on unexpected 3xx, got %v", err)
	}
}

// TestSendMessage_TokenNotEchoed_Body verifies the sender never includes
// the bearer token in errors built from large 4xx response bodies (Meta
// can return verbose error envelopes that an unwary implementation might
// glue together with auth metadata).
func TestSendMessage_TokenNotEchoed_Body(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		// Pad with > 256 chars so truncation path is exercised too.
		_, _ = w.Write([]byte(fmt.Sprintf(`{"error":{"message":"%s"}}`, strings.Repeat("E", 1024))))
	}))
	defer srv.Close()

	const token = "supersecret-tok-XYZ"
	s := mustSender(t, token, stubLookup(whatsapp.TenantConfig{
		PhoneNumberID: "ph",
		Enabled:       true,
	}), whatsapp.WithBaseURL(srv.URL))

	_, err := s.SendMessage(context.Background(), outMsg("hi"))
	if err == nil {
		t.Fatalf("expected error, got nil")
	}
	if strings.Contains(err.Error(), token) {
		t.Fatalf("error must not contain the bearer token; got %q", err.Error())
	}
}

// TestMetrics_OutcomesEmitted asserts the whatsapp_send_total counter
// records the correct outcome label per send.
func TestMetrics_OutcomesEmitted(t *testing.T) {
	t.Parallel()

	scenarios := []struct {
		name        string
		cfg         whatsapp.TenantConfig
		respond     http.HandlerFunc
		body        string
		wantOutcome string
	}{
		{
			name:        "success",
			cfg:         whatsapp.TenantConfig{PhoneNumberID: "ph", Enabled: true},
			respond:     func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{"messages":[{"id":"w1"}]}`)) },
			body:        "ok",
			wantOutcome: "success",
		},
		{
			name:        "rejected",
			cfg:         whatsapp.TenantConfig{PhoneNumberID: "ph", Enabled: true},
			respond:     func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusBadRequest) },
			body:        "ok",
			wantOutcome: "rejected",
		},
		{
			name:        "auth_failed",
			cfg:         whatsapp.TenantConfig{PhoneNumberID: "ph", Enabled: true},
			respond:     func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusUnauthorized) },
			body:        "ok",
			wantOutcome: "auth_failed",
		},
		{
			name:        "transient",
			cfg:         whatsapp.TenantConfig{PhoneNumberID: "ph", Enabled: true},
			respond:     func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusInternalServerError) },
			body:        "ok",
			wantOutcome: "transient",
		},
		{
			name:        "disabled",
			cfg:         whatsapp.TenantConfig{Enabled: false},
			respond:     func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) },
			body:        "ok",
			wantOutcome: "disabled",
		},
	}
	for _, sc := range scenarios {
		sc := sc
		t.Run(sc.name, func(t *testing.T) {
			t.Parallel()
			srv := httptest.NewServer(sc.respond)
			defer srv.Close()

			reg := prometheus.NewRegistry()
			s, err := whatsapp.New("tok", stubLookup(sc.cfg), reg,
				whatsapp.WithBaseURL(srv.URL),
				whatsapp.WithBackoffBase(time.Microsecond),
				whatsapp.WithMaxAttempts(1),
			)
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			_, _ = s.SendMessage(context.Background(), outMsg(sc.body))

			got := counterValue(t, reg, "whatsapp_send_total", map[string]string{"outcome": sc.wantOutcome})
			if got != 1 {
				t.Fatalf("expected whatsapp_send_total{outcome=%q} = 1, got %v", sc.wantOutcome, got)
			}
			// duration histogram should have observed exactly one sample.
			h := histogramCount(t, reg, "whatsapp_send_duration_seconds")
			if h != 1 {
				t.Fatalf("expected one whatsapp_send_duration_seconds observation, got %d", h)
			}
		})
	}
}

// TestSendMessage_LatencyUnder5s exercises AC #6: against fake-Meta the
// per-call latency stays well below the 5s p95 budget. We run a small
// batch sequentially to keep the test deterministic.
func TestSendMessage_LatencyUnder5s(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"messages":[{"id":"w"}]}`))
	}))
	defer srv.Close()

	s := mustSender(t, "tok", stubLookup(whatsapp.TenantConfig{
		PhoneNumberID: "ph",
		Enabled:       true,
	}), whatsapp.WithBaseURL(srv.URL))

	for i := 0; i < 5; i++ {
		start := time.Now()
		if _, err := s.SendMessage(context.Background(), outMsg("hi")); err != nil {
			t.Fatalf("send %d: %v", i, err)
		}
		if d := time.Since(start); d > 5*time.Second {
			t.Fatalf("send %d took %v (>5s)", i, d)
		}
	}
}

// ----- helpers -----

func outMsg(body string) inbox.OutboundMessage {
	return inbox.OutboundMessage{
		TenantID:       uuid.New(),
		ConversationID: uuid.New(),
		Channel:        "whatsapp",
		ToExternalID:   "+5511999999999",
		Body:           body,
	}
}

func stubLookup(cfg whatsapp.TenantConfig) whatsapp.TenantConfigLookup {
	return func(_ context.Context, _ uuid.UUID) (whatsapp.TenantConfig, error) {
		return cfg, nil
	}
}

func mustSender(t *testing.T, token string, lookup whatsapp.TenantConfigLookup, opts ...whatsapp.Option) *whatsapp.Sender {
	t.Helper()
	s, err := whatsapp.New(token, lookup, prometheus.NewRegistry(), opts...)
	if err != nil {
		t.Fatalf("whatsapp.New: %v", err)
	}
	return s
}

// counterValue reads a single labelled value from a gathered CounterVec.
// Returns 0 when the label combination is absent so callers can assert
// "was not incremented".
func counterValue(t *testing.T, reg *prometheus.Registry, name string, labels map[string]string) float64 {
	t.Helper()
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, mf := range mfs {
		if mf.GetName() != name {
			continue
		}
		for _, m := range mf.GetMetric() {
			if matchLabels(m.GetLabel(), labels) {
				return m.GetCounter().GetValue()
			}
		}
	}
	return 0
}

func histogramCount(t *testing.T, reg *prometheus.Registry, name string) uint64 {
	t.Helper()
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, mf := range mfs {
		if mf.GetName() != name {
			continue
		}
		var total uint64
		for _, m := range mf.GetMetric() {
			total += m.GetHistogram().GetSampleCount()
		}
		return total
	}
	return 0
}

type labelGetter interface {
	GetName() string
	GetValue() string
}

func matchLabels[T labelGetter](pairs []T, want map[string]string) bool {
	if len(want) == 0 {
		return true
	}
	got := map[string]string{}
	for _, p := range pairs {
		got[p.GetName()] = p.GetValue()
	}
	for k, v := range want {
		if got[k] != v {
			return false
		}
	}
	return true
}
