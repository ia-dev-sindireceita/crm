package email_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/pericles-luz/crm/internal/notify/email"
)

func TestAddress_String(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   email.Address
		want string
	}{
		{"bare email", email.Address{Email: "alice@example.com"}, "alice@example.com"},
		{"display name", email.Address{Email: "alice@example.com", Name: "Alice"}, "Alice <alice@example.com>"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := tc.in.String(); got != tc.want {
				t.Fatalf("String() = %q, want %q", got, tc.want)
			}
		})
	}
}

func validMessage() email.Message {
	return email.Message{
		From:    email.Address{Email: "noreply@acme.com"},
		To:      []email.Address{{Email: "alice@example.com"}},
		Subject: "hello",
		Text:    "world",
	}
}

func TestMessage_Validate(t *testing.T) {
	t.Parallel()

	type mut func(*email.Message)
	cases := []struct {
		name      string
		mutate    mut
		wantValid bool
		wantSub   string
	}{
		{"baseline ok", nil, true, ""},
		{"missing from", func(m *email.Message) { m.From = email.Address{} }, false, "from address"},
		{"missing to", func(m *email.Message) { m.To = nil }, false, "at least one To"},
		{"empty to email", func(m *email.Message) { m.To = []email.Address{{Name: "Alice"}} }, false, "empty email"},
		{"missing subject", func(m *email.Message) { m.Subject = "" }, false, "subject is required"},
		{"missing both bodies", func(m *email.Message) { m.Text = "" }, false, "Text or HTML"},
		{"html only is ok", func(m *email.Message) { m.Text = ""; m.HTML = "<p>hi</p>" }, true, ""},
		{"crlf in subject rejected", func(m *email.Message) { m.Subject = "x\r\ninjected" }, false, "CR/LF"},
		{"reserved header rejected", func(m *email.Message) {
			m.Headers = map[string]string{"From": "evil@x"}
		}, false, "reserved"},
		{"empty header key rejected", func(m *email.Message) {
			m.Headers = map[string]string{"": "x"}
		}, false, "empty key"},
		{"crlf in header value rejected", func(m *email.Message) {
			m.Headers = map[string]string{"X-Tag": "ok\r\nBcc: evil@x"}
		}, false, "CR/LF"},
		{"crlf in header key rejected", func(m *email.Message) {
			m.Headers = map[string]string{"X-Tag\r\nInject": "v"}
		}, false, "CR/LF"},
		{"clean custom header allowed", func(m *email.Message) {
			m.Headers = map[string]string{"X-Tag": "billing"}
		}, true, ""},
		{"crlf in from name rejected", func(m *email.Message) {
			m.From.Name = "Acme\r\nBcc: evil@x"
		}, false, "forbidden control character"},
		{"crlf in from email rejected", func(m *email.Message) {
			m.From.Email = "noreply@acme.com\r\nBcc: evil@x"
		}, false, "forbidden control character"},
		{"nul in from email rejected", func(m *email.Message) {
			m.From.Email = "noreply@acme.com\x00evil"
		}, false, "forbidden control character"},
		{"crlf in to name rejected", func(m *email.Message) {
			m.To = []email.Address{{Email: "alice@example.com", Name: "Alice\r\nBcc: evil@x"}}
		}, false, "forbidden control character"},
		{"crlf in to email rejected", func(m *email.Message) {
			m.To = []email.Address{{Email: "alice@example.com\r\nBcc: evil@x"}}
		}, false, "forbidden control character"},
		{"crlf in reply-to email rejected", func(m *email.Message) {
			m.ReplyTo = &email.Address{Email: "reply@example.com\r\nBcc: evil@x"}
		}, false, "forbidden control character"},
		{"crlf in cc rejected", func(m *email.Message) {
			m.Cc = []email.Address{{Email: "cc@example.com\r\nBcc: evil@x"}}
		}, false, "forbidden control character"},
		{"crlf in bcc rejected", func(m *email.Message) {
			m.Bcc = []email.Address{{Email: "bcc@example.com\r\nBcc: evil@x"}}
		}, false, "forbidden control character"},
		{"empty cc email rejected", func(m *email.Message) {
			m.Cc = []email.Address{{Name: "Cc"}}
		}, false, "empty email"},
		{"empty bcc email rejected", func(m *email.Message) {
			m.Bcc = []email.Address{{Name: "Bcc"}}
		}, false, "empty email"},
		{"empty reply-to email rejected", func(m *email.Message) {
			m.ReplyTo = &email.Address{Name: "Replies"}
		}, false, "empty email"},
		{"clean reply-to allowed", func(m *email.Message) {
			m.ReplyTo = &email.Address{Email: "reply@example.com", Name: "Replies"}
		}, true, ""},
		{"clean cc and bcc allowed", func(m *email.Message) {
			m.Cc = []email.Address{{Email: "cc@example.com"}}
			m.Bcc = []email.Address{{Email: "bcc@example.com"}}
		}, true, ""},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			m := validMessage()
			if tc.mutate != nil {
				tc.mutate(&m)
			}
			err := m.Validate()
			if tc.wantValid {
				if err != nil {
					t.Fatalf("Validate() = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatal("Validate() = nil, want error")
			}
			if !errors.Is(err, email.ErrInvalidMessage) {
				t.Fatalf("Validate() error %v does not wrap ErrInvalidMessage", err)
			}
			if tc.wantSub != "" && !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("Validate() error %q missing substring %q", err.Error(), tc.wantSub)
			}
		})
	}
}

func TestSentinels_AreDistinct(t *testing.T) {
	t.Parallel()
	if errors.Is(email.ErrTransient, email.ErrPermanent) {
		t.Fatal("ErrTransient is.Is ErrPermanent — sentinels collide")
	}
	if errors.Is(email.ErrPermanent, email.ErrInvalidMessage) {
		t.Fatal("ErrPermanent is.Is ErrInvalidMessage — sentinels collide")
	}
	if errors.Is(email.ErrTransient, email.ErrInvalidMessage) {
		t.Fatal("ErrTransient is.Is ErrInvalidMessage — sentinels collide")
	}
}
