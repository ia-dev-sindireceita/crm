package inbox

import "testing"

func TestUserLabelFromEmail(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		email string
		want  string
	}{
		{"local part", "maria.silva@sindi.com.br", "maria.silva"},
		{"trims whitespace", "  joao@x.io  ", "joao"},
		{"no at sign falls back", "operator", "operator"},
		{"leading at falls back to whole", "@weird", "@weird"},
		{"empty", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := userLabelFromEmail(tt.email); got != tt.want {
				t.Fatalf("userLabelFromEmail(%q) = %q, want %q", tt.email, got, tt.want)
			}
		})
	}
}
