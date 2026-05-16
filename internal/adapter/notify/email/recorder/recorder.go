// Package recorder implements email.EmailSender as an in-memory
// capture buffer. CI uses it (EMAIL_PROVIDER=recorder) so integration
// tests can assert that a flow produced a specific email without
// hitting Mailgun, and dev can opt into it when a developer wants to
// inspect the rendered message without delivery.
//
// The recorder is concurrency-safe: every Send takes a short-held
// mutex so parallel tests do not race when reading Sent.
package recorder

import (
	"context"
	"io"
	"sync"

	"github.com/pericles-luz/crm/internal/notify/email"
)

// MaterialisedAttachment is the post-Send view of an attachment: the
// reader has been drained into a byte slice so tests can assert
// payload content. Filename and ContentType are copied verbatim.
type MaterialisedAttachment struct {
	Filename    string
	ContentType string
	Content     []byte
}

// Recorded is the captured form of one Send call. Headers and
// Attachments are deep-copied so a caller mutating the original
// Message after Send cannot retroactively change a recorded entry.
type Recorded struct {
	From        email.Address
	To          []email.Address
	Cc          []email.Address
	Bcc         []email.Address
	ReplyTo     *email.Address
	Subject     string
	Text        string
	HTML        string
	Headers     map[string]string
	Attachments []MaterialisedAttachment
}

// Sender is the in-memory recorder. The zero value is NOT ready —
// callers must use New so the internal slice is non-nil. Sender is
// safe for concurrent use.
type Sender struct {
	mu   sync.Mutex
	sent []Recorded
}

// New returns an empty Sender ready to capture messages.
func New() *Sender { return &Sender{} }

// Send validates msg, drains every attachment, deep-copies recipient
// slices and headers, and appends a Recorded entry. Returns the same
// error contract as the port (ErrInvalidMessage on Validate failure).
func (s *Sender) Send(_ context.Context, msg email.Message) error {
	if err := msg.Validate(); err != nil {
		return err
	}
	rec := Recorded{
		From:    msg.From,
		To:      copyAddresses(msg.To),
		Cc:      copyAddresses(msg.Cc),
		Bcc:     copyAddresses(msg.Bcc),
		ReplyTo: copyAddressPtr(msg.ReplyTo),
		Subject: msg.Subject,
		Text:    msg.Text,
		HTML:    msg.HTML,
		Headers: copyHeaders(msg.Headers),
	}
	for _, att := range msg.Attachments {
		buf, err := io.ReadAll(att.Content)
		if err != nil {
			return err
		}
		rec.Attachments = append(rec.Attachments, MaterialisedAttachment{
			Filename:    att.Filename,
			ContentType: att.ContentType,
			Content:     buf,
		})
	}
	s.mu.Lock()
	s.sent = append(s.sent, rec)
	s.mu.Unlock()
	return nil
}

// Sent returns a snapshot of every captured message in send order.
// The returned slice is a copy so the caller can iterate or mutate
// without risk of a concurrent Send appending mid-iteration.
func (s *Sender) Sent() []Recorded {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Recorded, len(s.sent))
	copy(out, s.sent)
	return out
}

// Reset drops every captured message. Tests call it between sub-tests
// when they share a single recorder instance.
func (s *Sender) Reset() {
	s.mu.Lock()
	s.sent = nil
	s.mu.Unlock()
}

// Len returns the number of captured messages without copying the
// slice. Cheap for tabletop "how many emails were sent?" assertions.
func (s *Sender) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.sent)
}

// Compile-time port assertion.
var _ email.EmailSender = (*Sender)(nil)

func copyAddresses(in []email.Address) []email.Address {
	if len(in) == 0 {
		return nil
	}
	out := make([]email.Address, len(in))
	copy(out, in)
	return out
}

func copyAddressPtr(in *email.Address) *email.Address {
	if in == nil {
		return nil
	}
	cp := *in
	return &cp
}

func copyHeaders(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}
