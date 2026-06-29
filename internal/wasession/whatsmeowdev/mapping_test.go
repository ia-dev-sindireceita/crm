package whatsmeowdev

import (
	"testing"
	"time"

	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"

	"github.com/pericles-luz/crm/internal/wasession"
)

func sptr(s string) *string { return &s }

func TestMapConnEvent(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		evt    any
		want   wasession.Status
		reason string
		ok     bool
	}{
		{"connected", &events.Connected{}, wasession.StatusConnected, "connected", true},
		{"disconnected", &events.Disconnected{}, wasession.StatusDisconnected, "disconnected", true},
		{"loggedout", &events.LoggedOut{}, wasession.StatusBanned, "logged out", true},
		{"streamreplaced", &events.StreamReplaced{}, wasession.StatusBanned, "stream replaced", true},
		{"tempban", &events.TemporaryBan{}, wasession.StatusBanned, "temporary ban", true},
		{"outdated", &events.ClientOutdated{}, wasession.StatusBanned, "client outdated", true},
		{"message-not-status", &events.Message{}, "", "", false},
		{"receipt-not-status", &events.Receipt{}, "", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			to, reason, ok := mapConnEvent(tc.evt)
			if to != tc.want || reason != tc.reason || ok != tc.ok {
				t.Errorf("mapConnEvent = (%q,%q,%v), want (%q,%q,%v)", to, reason, ok, tc.want, tc.reason, tc.ok)
			}
		})
	}
}

func TestMessageToInboundText(t *testing.T) {
	t.Parallel()
	ts := time.Unix(1700000000, 0)
	evt := &events.Message{
		Info: types.MessageInfo{
			MessageSource: types.MessageSource{
				Sender:   types.NewJID("5511988887777", types.DefaultUserServer),
				IsFromMe: false,
			},
			ID:        "wamid.IN1",
			PushName:  "Alice",
			Timestamp: ts,
		},
		Message: &waE2E.Message{Conversation: sptr("olá mundo")},
	}
	got := messageToInbound(evt)
	want := wasession.InboundMessage{
		ExternalID: "wamid.IN1",
		SenderE164: "5511988887777",
		SenderName: "Alice",
		Body:       "olá mundo",
		OccurredAt: ts,
		HasMedia:   false,
		FromMe:     false,
	}
	if got != want {
		t.Errorf("messageToInbound = %+v, want %+v", got, want)
	}
}

func TestMessageToInboundExtendedText(t *testing.T) {
	t.Parallel()
	evt := &events.Message{
		Info: types.MessageInfo{
			MessageSource: types.MessageSource{Sender: types.NewJID("5511", types.DefaultUserServer), IsFromMe: true},
			ID:            "wamid.IN2",
		},
		Message: &waE2E.Message{
			ExtendedTextMessage: &waE2E.ExtendedTextMessage{Text: sptr("link msg")},
		},
	}
	got := messageToInbound(evt)
	if got.Body != "link msg" || !got.FromMe || got.HasMedia {
		t.Errorf("extended-text inbound = %+v", got)
	}
}

func TestMessageToInboundMediaOnly(t *testing.T) {
	t.Parallel()
	evt := &events.Message{
		Info:    types.MessageInfo{ID: "wamid.IN3"},
		Message: &waE2E.Message{ImageMessage: &waE2E.ImageMessage{}},
	}
	got := messageToInbound(evt)
	if got.Body != "" || !got.HasMedia {
		t.Errorf("media-only inbound = %+v, want empty body + HasMedia", got)
	}
}

func TestTextOf(t *testing.T) {
	t.Parallel()
	if _, ok := textOf(nil); ok {
		t.Error("nil message should not have text")
	}
	if got, ok := textOf(&waE2E.Message{Conversation: sptr("hi")}); !ok || got != "hi" {
		t.Errorf("conversation textOf = (%q,%v)", got, ok)
	}
	if got, ok := textOf(&waE2E.Message{ExtendedTextMessage: &waE2E.ExtendedTextMessage{Text: sptr("x")}}); !ok || got != "x" {
		t.Errorf("extended textOf = (%q,%v)", got, ok)
	}
	// Extended text present but empty -> no text.
	if _, ok := textOf(&waE2E.Message{ExtendedTextMessage: &waE2E.ExtendedTextMessage{}}); ok {
		t.Error("empty extended text should not count as text")
	}
	if _, ok := textOf(&waE2E.Message{}); ok {
		t.Error("empty message should not have text")
	}
}

func TestHasMedia(t *testing.T) {
	t.Parallel()
	if hasMedia(nil) {
		t.Error("nil has no media")
	}
	if !hasMedia(&waE2E.Message{ImageMessage: &waE2E.ImageMessage{}}) {
		t.Error("image should count as media")
	}
	if !hasMedia(&waE2E.Message{DocumentMessage: &waE2E.DocumentMessage{}}) {
		t.Error("document should count as media")
	}
	if hasMedia(&waE2E.Message{Conversation: sptr("text")}) {
		t.Error("plain text is not media")
	}
}

func TestJidToE164(t *testing.T) {
	t.Parallel()
	if got := jidToE164(types.NewJID("5511999", types.DefaultUserServer)); got != "5511999" {
		t.Errorf("jidToE164 = %q", got)
	}
}

func TestE164ToJID(t *testing.T) {
	t.Parallel()
	jid, err := e164ToJID("+5511988887777")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if jid.User != "5511988887777" || jid.Server != types.DefaultUserServer {
		t.Errorf("jid = %+v", jid)
	}
	if jid2, err := e164ToJID("5511"); err != nil || jid2.User != "5511" {
		t.Errorf("plain digits: jid=%+v err=%v", jid2, err)
	}
	for _, bad := range []string{"", "  ", "+", "55a11", "55 11", "55-11"} {
		if _, err := e164ToJID(bad); err == nil {
			t.Errorf("e164ToJID(%q) expected error", bad)
		}
	}
}
