package recorder_test

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/pericles-luz/crm/internal/adapter/notify/email/recorder"
	"github.com/pericles-luz/crm/internal/notify/email"
)

func TestRecorder_SatisfiesPort(t *testing.T) {
	t.Parallel()
	var _ email.EmailSender = recorder.New()
}

func TestRecorder_CapturesValidMessage(t *testing.T) {
	t.Parallel()
	rec := recorder.New()
	msg := email.Message{
		From:    email.Address{Email: "noreply@acme.com", Name: "Acme"},
		To:      []email.Address{{Email: "alice@example.com"}},
		Cc:      []email.Address{{Email: "carl@example.com"}},
		Bcc:     []email.Address{{Email: "bob@example.com"}},
		ReplyTo: &email.Address{Email: "reply@acme.com"},
		Subject: "hello",
		Text:    "world",
		HTML:    "<p>world</p>",
		Headers: map[string]string{"X-Tag": "billing"},
	}
	if err := rec.Send(context.Background(), msg); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if got := rec.Len(); got != 1 {
		t.Fatalf("Len = %d, want 1", got)
	}
	sent := rec.Sent()
	if len(sent) != 1 {
		t.Fatalf("Sent len = %d, want 1", len(sent))
	}
	got := sent[0]
	if got.Subject != "hello" {
		t.Errorf("Subject = %q", got.Subject)
	}
	if got.From != msg.From {
		t.Errorf("From = %+v, want %+v", got.From, msg.From)
	}
	if got.ReplyTo == nil || got.ReplyTo.Email != "reply@acme.com" {
		t.Errorf("ReplyTo = %+v", got.ReplyTo)
	}
	if got.Headers["X-Tag"] != "billing" {
		t.Errorf("Headers[X-Tag] = %q", got.Headers["X-Tag"])
	}
}

func TestRecorder_DeepCopiesAddressesAndHeaders(t *testing.T) {
	t.Parallel()
	rec := recorder.New()
	to := []email.Address{{Email: "alice@example.com"}}
	hdr := map[string]string{"X-Tag": "v1"}
	msg := email.Message{
		From:    email.Address{Email: "noreply@acme.com"},
		To:      to,
		Subject: "x",
		Text:    "y",
		Headers: hdr,
	}
	if err := rec.Send(context.Background(), msg); err != nil {
		t.Fatalf("Send: %v", err)
	}

	to[0].Email = "mutated@example.com"
	hdr["X-Tag"] = "v2"

	sent := rec.Sent()[0]
	if sent.To[0].Email != "alice@example.com" {
		t.Fatalf("recorder did not deep-copy To: got %q", sent.To[0].Email)
	}
	if sent.Headers["X-Tag"] != "v1" {
		t.Fatalf("recorder did not deep-copy Headers: got %q", sent.Headers["X-Tag"])
	}
}

func TestRecorder_MaterialisesAttachments(t *testing.T) {
	t.Parallel()
	rec := recorder.New()
	msg := email.Message{
		From:    email.Address{Email: "noreply@acme.com"},
		To:      []email.Address{{Email: "alice@example.com"}},
		Subject: "x",
		Text:    "y",
		Attachments: []email.Attachment{
			{Filename: "report.csv", ContentType: "text/csv", Content: bytes.NewReader([]byte("col1,col2\n1,2\n"))},
		},
	}
	if err := rec.Send(context.Background(), msg); err != nil {
		t.Fatalf("Send: %v", err)
	}
	got := rec.Sent()[0].Attachments
	if len(got) != 1 {
		t.Fatalf("Attachments len = %d", len(got))
	}
	if got[0].Filename != "report.csv" || got[0].ContentType != "text/csv" {
		t.Errorf("attachment metadata = %+v", got[0])
	}
	if string(got[0].Content) != "col1,col2\n1,2\n" {
		t.Errorf("attachment content = %q", got[0].Content)
	}
}

type errReader struct{}

func (errReader) Read(_ []byte) (int, error) { return 0, errors.New("boom") }

func TestRecorder_AttachmentReadFailureSurfaces(t *testing.T) {
	t.Parallel()
	rec := recorder.New()
	msg := email.Message{
		From:        email.Address{Email: "noreply@acme.com"},
		To:          []email.Address{{Email: "alice@example.com"}},
		Subject:     "x",
		Text:        "y",
		Attachments: []email.Attachment{{Filename: "x", Content: errReader{}}},
	}
	err := rec.Send(context.Background(), msg)
	if err == nil {
		t.Fatal("Send returned nil for failed attachment read")
	}
}

func TestRecorder_RejectsInvalidMessage(t *testing.T) {
	t.Parallel()
	rec := recorder.New()
	err := rec.Send(context.Background(), email.Message{})
	if !errors.Is(err, email.ErrInvalidMessage) {
		t.Fatalf("Send err = %v, want wrap ErrInvalidMessage", err)
	}
	if rec.Len() != 0 {
		t.Fatalf("Len = %d after invalid send, want 0", rec.Len())
	}
}

func TestRecorder_Reset(t *testing.T) {
	t.Parallel()
	rec := recorder.New()
	for i := 0; i < 3; i++ {
		_ = rec.Send(context.Background(), email.Message{
			From:    email.Address{Email: "n@a"},
			To:      []email.Address{{Email: "a@a"}},
			Subject: "s", Text: "t",
		})
	}
	if rec.Len() != 3 {
		t.Fatalf("Len = %d, want 3", rec.Len())
	}
	rec.Reset()
	if rec.Len() != 0 {
		t.Fatalf("Len after Reset = %d, want 0", rec.Len())
	}
}

func TestRecorder_ConcurrentSendsAreSafe(t *testing.T) {
	t.Parallel()
	rec := recorder.New()
	var wg sync.WaitGroup
	const n = 50
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = rec.Send(context.Background(), email.Message{
				From:    email.Address{Email: "n@a"},
				To:      []email.Address{{Email: "a@a"}},
				Subject: "s", Text: "t",
			})
		}()
	}
	wg.Wait()
	if rec.Len() != n {
		t.Fatalf("Len = %d, want %d", rec.Len(), n)
	}
}
