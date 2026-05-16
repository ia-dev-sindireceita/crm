package mailgun_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pericles-luz/crm/internal/adapter/notify/email/mailgun"
	"github.com/pericles-luz/crm/internal/notify/email"
)

// recordingServer captures the most recent request so tests can
// assert on the exact body Mailgun would have received.
type recordingServer struct {
	*httptest.Server
	mu             atomic.Pointer[recordedRequest]
	statusCode     int
	responseBody   string
	delayBeforeRsp time.Duration
}

type recordedRequest struct {
	Method      string
	Path        string
	ContentType string
	Auth        string
	Body        []byte
}

func newServer(t *testing.T, status int, body string) *recordingServer {
	t.Helper()
	rs := &recordingServer{statusCode: status, responseBody: body}
	rs.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf, _ := io.ReadAll(r.Body)
		rs.mu.Store(&recordedRequest{
			Method:      r.Method,
			Path:        r.URL.Path,
			ContentType: r.Header.Get("Content-Type"),
			Auth:        r.Header.Get("Authorization"),
			Body:        buf,
		})
		if rs.delayBeforeRsp > 0 {
			select {
			case <-r.Context().Done():
				return
			case <-time.After(rs.delayBeforeRsp):
			}
		}
		w.WriteHeader(rs.statusCode)
		_, _ = w.Write([]byte(rs.responseBody))
	}))
	t.Cleanup(rs.Close)
	return rs
}

func (rs *recordingServer) latest() *recordedRequest { return rs.mu.Load() }

// quietLogger discards every record so the test output stays clean.
func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func newSender(t *testing.T, srv *recordingServer) *mailgun.Sender {
	t.Helper()
	s, err := mailgun.New(mailgun.Config{
		APIKey:  "secret-key",
		Domain:  "mg.acme.com",
		BaseURL: srv.URL,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s.WithClient(srv.Client()).WithLogger(quietLogger())
}

func validMessage() email.Message {
	return email.Message{
		From:    email.Address{Email: "noreply@mg.acme.com", Name: "Acme"},
		To:      []email.Address{{Email: "alice@example.com"}},
		Subject: "hello",
		Text:    "world",
	}
}

func TestNew_RegionUSAndEU(t *testing.T) {
	t.Parallel()
	cases := []struct {
		region mailgun.Region
		ok     bool
	}{
		{mailgun.RegionUS, true},
		{mailgun.RegionEU, true},
		{mailgun.Region("xx"), false},
		{mailgun.Region(""), false},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(string(tc.region), func(t *testing.T) {
			t.Parallel()
			_, err := mailgun.New(mailgun.Config{APIKey: "k", Domain: "d", Region: tc.region})
			if tc.ok && err != nil {
				t.Fatalf("New(%q) = %v, want nil", tc.region, err)
			}
			if !tc.ok && !errors.Is(err, mailgun.ErrMissingConfig) {
				t.Fatalf("New(%q) err = %v, want ErrMissingConfig", tc.region, err)
			}
		})
	}
}

func TestNew_RejectsEmptyAPIKeyOrDomain(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		cfg  mailgun.Config
		want string
	}{
		{"no api key", mailgun.Config{Domain: "d", Region: mailgun.RegionUS}, "APIKey"},
		{"no domain", mailgun.Config{APIKey: "k", Region: mailgun.RegionUS}, "Domain"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := mailgun.New(tc.cfg)
			if !errors.Is(err, mailgun.ErrMissingConfig) {
				t.Fatalf("err = %v, want ErrMissingConfig", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err %q missing %q", err.Error(), tc.want)
			}
		})
	}
}

func TestSend_RejectsInvalidMessage(t *testing.T) {
	t.Parallel()
	srv := newServer(t, http.StatusOK, `{"id":"x","message":"queued"}`)
	s := newSender(t, srv)
	err := s.Send(context.Background(), email.Message{})
	if !errors.Is(err, email.ErrInvalidMessage) {
		t.Fatalf("Send err = %v, want wrap ErrInvalidMessage", err)
	}
	if got := srv.latest(); got != nil {
		t.Fatalf("server received a request for an invalid message: %+v", got)
	}
}

func TestSend_PostsMultipartFormWithBasicAuthAndExpectedFields(t *testing.T) {
	t.Parallel()
	srv := newServer(t, http.StatusOK, `{"id":"<msg-id-123>","message":"queued"}`)
	s := newSender(t, srv)

	msg := validMessage()
	msg.Cc = []email.Address{{Email: "carl@example.com"}}
	msg.Bcc = []email.Address{{Email: "bob@example.com"}}
	msg.ReplyTo = &email.Address{Email: "reply@acme.com"}
	msg.HTML = "<p>world</p>"
	msg.Headers = map[string]string{"X-Tag": "billing"}
	msg.Attachments = []email.Attachment{
		{Filename: "report.csv", ContentType: "text/csv", Content: bytes.NewReader([]byte("a,b\n1,2\n"))},
	}

	if err := s.Send(context.Background(), msg); err != nil {
		t.Fatalf("Send: %v", err)
	}

	got := srv.latest()
	if got == nil {
		t.Fatal("server received no request")
	}
	if got.Method != http.MethodPost {
		t.Errorf("method = %q, want POST", got.Method)
	}
	if got.Path != "/v3/mg.acme.com/messages" {
		t.Errorf("path = %q", got.Path)
	}
	if !strings.HasPrefix(got.ContentType, "multipart/form-data") {
		t.Errorf("content-type = %q, want multipart/form-data", got.ContentType)
	}
	if !strings.HasPrefix(got.Auth, "Basic ") {
		t.Errorf("auth = %q, want Basic", got.Auth)
	}

	// Decode multipart and assert the fields shape.
	_, params, err := mime.ParseMediaType(got.ContentType)
	if err != nil {
		t.Fatalf("parse content-type: %v", err)
	}
	mr := multipart.NewReader(bytes.NewReader(got.Body), params["boundary"])
	fields := map[string][]string{}
	var attachmentSeen bool
	var attachmentBody bytes.Buffer
	for {
		part, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("next part: %v", err)
		}
		name := part.FormName()
		if name == "attachment" {
			attachmentSeen = true
			if got, want := part.FileName(), "report.csv"; got != want {
				t.Errorf("attachment filename = %q, want %q", got, want)
			}
			if got, want := part.Header.Get("Content-Type"), "text/csv"; got != want {
				t.Errorf("attachment content-type = %q, want %q", got, want)
			}
			if _, err := io.Copy(&attachmentBody, part); err != nil {
				t.Fatalf("copy attachment: %v", err)
			}
			continue
		}
		buf, _ := io.ReadAll(part)
		fields[name] = append(fields[name], string(buf))
	}

	wantFields := map[string][]string{
		"from":       {"Acme <noreply@mg.acme.com>"},
		"to":         {"alice@example.com"},
		"cc":         {"carl@example.com"},
		"bcc":        {"bob@example.com"},
		"subject":    {"hello"},
		"text":       {"world"},
		"html":       {"<p>world</p>"},
		"h:Reply-To": {"reply@acme.com"},
		"h:X-Tag":    {"billing"},
	}
	for k, want := range wantFields {
		got := fields[k]
		if len(got) != len(want) {
			t.Errorf("field %q got %v, want %v", k, got, want)
			continue
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("field %q[%d] = %q, want %q", k, i, got[i], want[i])
			}
		}
	}
	if !attachmentSeen {
		t.Fatal("expected attachment part to be present")
	}
	if attachmentBody.String() != "a,b\n1,2\n" {
		t.Errorf("attachment body = %q", attachmentBody.String())
	}
}

func TestSend_MultipleToRecipients(t *testing.T) {
	t.Parallel()
	srv := newServer(t, http.StatusOK, `{}`)
	s := newSender(t, srv)
	msg := validMessage()
	msg.To = []email.Address{
		{Email: "alice@example.com"},
		{Email: "bob@example.com", Name: "Bob"},
	}
	if err := s.Send(context.Background(), msg); err != nil {
		t.Fatalf("Send: %v", err)
	}
	got := srv.latest()
	body := string(got.Body)
	if !strings.Contains(body, `name="to"`) {
		t.Fatal("missing to part")
	}
	if !strings.Contains(body, "alice@example.com") || !strings.Contains(body, "Bob <bob@example.com>") {
		t.Fatalf("body missing recipient(s):\n%s", body)
	}
}

func TestSend_DefaultAttachmentContentType(t *testing.T) {
	t.Parallel()
	srv := newServer(t, http.StatusOK, `{}`)
	s := newSender(t, srv)
	msg := validMessage()
	msg.Attachments = []email.Attachment{
		{Filename: "blob.bin", Content: bytes.NewReader([]byte{0x00, 0x01})},
	}
	if err := s.Send(context.Background(), msg); err != nil {
		t.Fatalf("Send: %v", err)
	}
	got := srv.latest()
	if !strings.Contains(string(got.Body), "application/octet-stream") {
		t.Fatalf("expected default attachment content-type in body:\n%s", string(got.Body))
	}
}

func TestSend_StatusClassification(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		status int
		want   error
	}{
		{"403 permanent", http.StatusForbidden, email.ErrPermanent},
		{"400 permanent", http.StatusBadRequest, email.ErrPermanent},
		{"408 transient", http.StatusRequestTimeout, email.ErrTransient},
		{"429 transient", http.StatusTooManyRequests, email.ErrTransient},
		{"500 transient", http.StatusInternalServerError, email.ErrTransient},
		{"502 transient", http.StatusBadGateway, email.ErrTransient},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			srv := newServer(t, tc.status, `{"message":"nope"}`)
			s := newSender(t, srv)
			err := s.Send(context.Background(), validMessage())
			if err == nil {
				t.Fatalf("Send returned nil for status %d", tc.status)
			}
			if !errors.Is(err, tc.want) {
				t.Fatalf("Send err = %v, want wrap %v", err, tc.want)
			}
		})
	}
}

func TestSend_NetworkErrorIsTransient(t *testing.T) {
	t.Parallel()
	srv := newServer(t, http.StatusOK, `{}`)
	s := newSender(t, srv)
	srv.Close()
	err := s.Send(context.Background(), validMessage())
	if !errors.Is(err, email.ErrTransient) {
		t.Fatalf("Send err = %v, want wrap ErrTransient", err)
	}
}

func TestSend_HonoursContextDeadline(t *testing.T) {
	t.Parallel()
	srv := newServer(t, http.StatusOK, `{}`)
	srv.delayBeforeRsp = 2 * time.Second
	s := newSender(t, srv).WithTimeout(50 * time.Millisecond)

	start := time.Now()
	err := s.Send(context.Background(), validMessage())
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if !errors.Is(err, email.ErrTransient) {
		t.Fatalf("err = %v, want wrap ErrTransient", err)
	}
	if elapsed > time.Second {
		t.Fatalf("Send took %v, expected ~50ms", elapsed)
	}
}

func TestSend_BadBaseURLBuildRequestError(t *testing.T) {
	t.Parallel()
	s, err := mailgun.New(mailgun.Config{
		APIKey:  "k",
		Domain:  "mg.acme.com",
		BaseURL: "http://example.com/ has space",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	err = s.WithLogger(quietLogger()).Send(context.Background(), validMessage())
	if err == nil {
		t.Fatal("expected build-request error, got nil")
	}
}

func TestNew_DefaultsTimeoutAndLogger(t *testing.T) {
	t.Parallel()
	s, err := mailgun.New(mailgun.Config{APIKey: "k", Domain: "d", Region: mailgun.RegionUS})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// WithLogger(nil) must keep the previous logger (no panic, no swap).
	s2 := s.WithLogger(nil)
	if s2 == nil {
		t.Fatal("WithLogger(nil) returned nil")
	}
}

func TestSend_SatisfiesPort(t *testing.T) {
	t.Parallel()
	srv := newServer(t, http.StatusOK, `{}`)
	var _ email.EmailSender = newSender(t, srv)
}

type errReader struct{}

func (errReader) Read(_ []byte) (int, error) { return 0, errors.New("disk gone") }

func TestSend_AttachmentReadFailureBubblesAsBuildError(t *testing.T) {
	t.Parallel()
	srv := newServer(t, http.StatusOK, `{}`)
	s := newSender(t, srv)
	msg := validMessage()
	msg.Attachments = []email.Attachment{
		{Filename: "report.csv", ContentType: "text/csv", Content: errReader{}},
	}
	err := s.Send(context.Background(), msg)
	if err == nil {
		t.Fatal("expected build-payload error, got nil")
	}
	if !strings.Contains(err.Error(), "build payload") {
		t.Fatalf("err = %v, want 'build payload' wrap", err)
	}
	if srv.latest() != nil {
		t.Fatal("server should not have received a request when payload build failed")
	}
}

func TestWithTimeout_NegativeFallsBackToDefault(t *testing.T) {
	t.Parallel()
	srv := newServer(t, http.StatusOK, `{}`)
	s := newSender(t, srv).WithTimeout(-1 * time.Second)
	if err := s.Send(context.Background(), validMessage()); err != nil {
		t.Fatalf("Send: %v", err)
	}
}
