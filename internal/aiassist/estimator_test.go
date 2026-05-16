package aiassist_test

import (
	"strings"
	"testing"

	"github.com/pericles-luz/crm/internal/aiassist"
)

func TestEstimateTokens_TableDriven(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		text  string
		model string
		want  int64
	}{
		{
			name:  "empty input clamps to minimum",
			text:  "",
			model: "",
			want:  1,
		},
		{
			name:  "single byte clamps to minimum",
			text:  "x",
			model: "",
			want:  1,
		},
		{
			name:  "default divisor of 4 on unknown model",
			text:  strings.Repeat("a", 400),
			model: "unknown/x-model",
			want:  100,
		},
		{
			name:  "default divisor of 4 on empty model",
			text:  strings.Repeat("a", 400),
			model: "",
			want:  100,
		},
		{
			name:  "claude divisor of 3",
			text:  strings.Repeat("a", 300),
			model: "anthropic/claude-haiku-4.5",
			want:  100,
		},
		{
			name:  "claude divisor of 3 case insensitive",
			text:  strings.Repeat("a", 300),
			model: "ANTHROPIC/Claude-Sonnet",
			want:  100,
		},
		{
			name:  "gemini falls back to default divisor",
			text:  strings.Repeat("a", 400),
			model: "google/gemini-2.0-flash",
			want:  100,
		},
		{
			name:  "openai gpt falls back to default divisor",
			text:  strings.Repeat("a", 400),
			model: "openai/gpt-4o-mini",
			want:  100,
		},
		{
			name:  "claude pessimistic vs default",
			text:  strings.Repeat("a", 12),
			model: "anthropic/claude",
			want:  4, // 12/3 = 4 (claude pessimistic)
		},
		{
			name:  "default at 12 bytes",
			text:  strings.Repeat("a", 12),
			model: "openai/gpt-4o",
			want:  3, // 12/4 = 3
		},
		{
			name:  "model with whitespace trims",
			text:  strings.Repeat("a", 300),
			model: "  anthropic/claude  ",
			want:  100, // claude divisor applies
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := aiassist.EstimateTokens(tc.text, tc.model)
			if got != tc.want {
				t.Fatalf("EstimateTokens(len=%d, %q) = %d, want %d", len(tc.text), tc.model, got, tc.want)
			}
		})
	}
}

func TestEstimateTokens_DeterministicSameInput(t *testing.T) {
	t.Parallel()
	text := strings.Repeat("Olá mundo ", 1000)
	a := aiassist.EstimateTokens(text, "google/gemini-2.0-flash")
	b := aiassist.EstimateTokens(text, "google/gemini-2.0-flash")
	if a != b {
		t.Fatalf("estimator non-deterministic: %d vs %d", a, b)
	}
}

func TestEstimateReservation(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		text      string
		model     string
		maxOutput int64
		want      int64
	}{
		{
			name:      "prompt plus output",
			text:      strings.Repeat("a", 400),
			model:     "openai/gpt-4o",
			maxOutput: 200,
			want:      300, // 100 in + 200 out
		},
		{
			name:      "negative output treated as zero",
			text:      strings.Repeat("a", 400),
			model:     "openai/gpt-4o",
			maxOutput: -5,
			want:      100,
		},
		{
			name:      "zero output uses prompt only",
			text:      strings.Repeat("a", 400),
			model:     "openai/gpt-4o",
			maxOutput: 0,
			want:      100,
		},
		{
			name:      "tiny prompt still floored",
			text:      "x",
			model:     "openai/gpt-4o",
			maxOutput: 100,
			want:      101, // 1 floor + 100
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := aiassist.EstimateReservation(tc.text, tc.model, tc.maxOutput)
			if got != tc.want {
				t.Fatalf("EstimateReservation = %d, want %d", got, tc.want)
			}
		})
	}
}
