package wa_session

import (
	"log/slog"
	"testing"
	"time"
)

func TestNormalizeWhatsAppPhone(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    string
		wantErr bool
	}{
		{"bare digits", "5511999990001", "+5511999990001", false},
		{"plus prefixed", "+5511999990001", "+5511999990001", false},
		{"jid server suffix", "5511999990001@s.whatsapp.net", "+5511999990001", false},
		{"jid device + server suffix", "5511999990001:12@s.whatsapp.net", "+5511999990001", false},
		{"surrounding whitespace", "  5511999990001 ", "+5511999990001", false},
		{"empty", "", "", true},
		{"only plus", "+", "", true},
		{"only jid marker", "@s.whatsapp.net", "", true},
		{"letters", "not-a-phone", "", true},
		{"leading zero country code", "0511999990001", "", true},
		{"too long", "5511999990001234567", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := normalizeWhatsAppPhone(tt.in)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr = %v", err, tt.wantErr)
			}
			if got != tt.want {
				t.Errorf("got = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestWithConfig_DeliverTimeoutAndLogger(t *testing.T) {
	a, err := New(&fakeInbound{}, &fakeSender{}, enabledFlag(), allowAllRate(),
		WithConfig(Config{DeliverTimeout: 250 * time.Millisecond}),
		WithLogger(slog.New(slog.NewTextHandler(discard{}, nil))),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if a.cfg.DeliverTimeout != 250*time.Millisecond {
		t.Errorf("DeliverTimeout = %v, want 250ms", a.cfg.DeliverTimeout)
	}
	// RateMaxPerMin not set in the override → keeps default.
	if a.cfg.RateMaxPerMin != defaultRateMaxPerMinute {
		t.Errorf("RateMaxPerMin = %d, want %d", a.cfg.RateMaxPerMin, defaultRateMaxPerMinute)
	}
}

type discard struct{}

func (discard) Write(p []byte) (int, error) { return len(p), nil }
