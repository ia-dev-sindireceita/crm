package inbox_test

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/pericles-luz/crm/internal/inbox"
)

func TestValidateListChannel(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		channel string
		wantErr bool
	}{
		{"empty = no filter", "", false},
		{"whatsapp", "whatsapp", false},
		{"instagram", "instagram", false},
		{"messenger", "messenger", false},
		{"webchat", "webchat", false},
		{"unknown", "telegram", true},
		{"casing not normalised here", "WhatsApp", true},
		{"whitespace not normalised here", " whatsapp ", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := inbox.ValidateListChannel(tt.channel)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("ValidateListChannel(%q) = nil, want ErrInvalidChannel", tt.channel)
				}
				if err != inbox.ErrInvalidChannel {
					t.Fatalf("err = %v, want ErrInvalidChannel", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("ValidateListChannel(%q) = %v, want nil", tt.channel, err)
			}
		})
	}
}

func TestSnippet(t *testing.T) {
	t.Parallel()

	t.Run("empty", func(t *testing.T) {
		t.Parallel()
		if got := inbox.Snippet(""); got != "" {
			t.Fatalf("got %q want empty", got)
		}
		if got := inbox.Snippet("   \n\t "); got != "" {
			t.Fatalf("whitespace-only got %q want empty", got)
		}
	})

	t.Run("short passes through", func(t *testing.T) {
		t.Parallel()
		if got := inbox.Snippet("oi tudo bem?"); got != "oi tudo bem?" {
			t.Fatalf("got %q", got)
		}
	})

	t.Run("collapses internal whitespace", func(t *testing.T) {
		t.Parallel()
		if got := inbox.Snippet("linha um\n\nlinha    dois"); got != "linha um linha dois" {
			t.Fatalf("got %q", got)
		}
	})

	t.Run("exactly at limit is not truncated", func(t *testing.T) {
		t.Parallel()
		in := strings.Repeat("a", inbox.SnippetMaxChars)
		got := inbox.Snippet(in)
		if got != in {
			t.Fatalf("limit-length body was altered: got %q", got)
		}
		if strings.Contains(got, "…") {
			t.Fatalf("unexpected ellipsis at exact limit")
		}
	})

	t.Run("over limit truncates with ellipsis", func(t *testing.T) {
		t.Parallel()
		in := strings.Repeat("a", inbox.SnippetMaxChars+50)
		got := inbox.Snippet(in)
		if !strings.HasSuffix(got, "…") {
			t.Fatalf("missing ellipsis: %q", got)
		}
		// SnippetMaxChars runes + the ellipsis rune.
		if n := utf8.RuneCountInString(got); n != inbox.SnippetMaxChars+1 {
			t.Fatalf("rune count = %d, want %d", n, inbox.SnippetMaxChars+1)
		}
	})

	t.Run("does not split multibyte runes", func(t *testing.T) {
		t.Parallel()
		// Each "é" is multi-byte; a byte-based slice would corrupt the
		// boundary. Build a string longer than the rune limit.
		in := strings.Repeat("é", inbox.SnippetMaxChars+10)
		got := inbox.Snippet(in)
		if !utf8.ValidString(got) {
			t.Fatalf("truncation produced invalid UTF-8: %q", got)
		}
		if !strings.HasSuffix(got, "…") {
			t.Fatalf("missing ellipsis: %q", got)
		}
	})
}
