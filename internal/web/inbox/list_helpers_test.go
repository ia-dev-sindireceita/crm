package inbox

import (
	"testing"
	"time"
)

func TestInitials(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		in   string
		want string
	}{
		"two words":    {"Joao Souza", "JS"},
		"single word":  {"Maria", "M"},
		"three words":  {"Ana Paula Lima", "AL"},
		"lowercase":    {"joao souza", "JS"},
		"empty":        {"", ""},
		"whitespace":   {"   ", ""},
		"extra spaces": {"  Joao   Souza  ", "JS"},
	}
	for name, tc := range cases {
		tc := tc
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if got := initials(tc.in); got != tc.want {
				t.Errorf("initials(%q) = %q want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestRelativeTimeLong(t *testing.T) {
	t.Parallel()
	now := time.Now()
	cases := map[string]struct {
		in   time.Time
		want string
	}{
		"zero":     {time.Time{}, ""},
		"seconds":  {now.Add(-30 * time.Second), "agora mesmo"},
		"one min":  {now.Add(-1 * time.Minute), "1 minuto"},
		"minutes":  {now.Add(-5 * time.Minute), "5 minutos"},
		"one hour": {now.Add(-1 * time.Hour), "1 hora"},
		"hours":    {now.Add(-3 * time.Hour), "3 horas"},
		"one day":  {now.Add(-24 * time.Hour), "1 dia"},
		"days":     {now.Add(-72 * time.Hour), "3 dias"},
	}
	for name, tc := range cases {
		tc := tc
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if got := relativeTimeLong(tc.in); got != tc.want {
				t.Errorf("relativeTimeLong = %q want %q", got, tc.want)
			}
		})
	}
}

func TestChannelLabel_AddedChannels(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"messenger": "Messenger",
		"webchat":   "Webchat",
		"WhatsApp":  "WhatsApp", // case-insensitive
	}
	for in, want := range cases {
		if got := channelLabel(in); got != want {
			t.Errorf("channelLabel(%q) = %q want %q", in, got, want)
		}
	}
}
