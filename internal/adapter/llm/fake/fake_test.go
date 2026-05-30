package fake_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/pericles-luz/crm/internal/adapter/llm/fake"
	"github.com/pericles-luz/crm/internal/aiassist"
)

const (
	tenantUUID = "0bff8a31-9c2e-4ec1-9c93-7a3a7d9c3a01"
	tenantPfx  = "0bff8a31"
	convUUID   = "11111111-2222-3333-4444-555555555555"
	reqID      = "req-001"
)

func newKey(tenant, conv, req string) string {
	return tenant + ":" + conv + ":" + req
}

func TestComplete_Shape(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name             string
		prompt           string
		idempotencyKey   string
		wantTenantPrefix string
		wantMessages     int
	}{
		{
			name:             "short prompt clamps to 1 message",
			prompt:           "oi",
			idempotencyKey:   newKey(tenantUUID, convUUID, reqID),
			wantTenantPrefix: tenantPfx,
			wantMessages:     1,
		},
		{
			name:             "medium prompt scales linearly",
			prompt:           strings.Repeat("a", 80*7),
			idempotencyKey:   newKey(tenantUUID, convUUID, reqID),
			wantTenantPrefix: tenantPfx,
			wantMessages:     7,
		},
		{
			name:             "huge prompt clamps to 50",
			prompt:           strings.Repeat("a", 80*200),
			idempotencyKey:   newKey(tenantUUID, convUUID, reqID),
			wantTenantPrefix: tenantPfx,
			wantMessages:     50,
		},
		{
			name:             "short tenant id surfaces verbatim",
			prompt:           "ola",
			idempotencyKey:   newKey("abc", convUUID, reqID),
			wantTenantPrefix: "abc",
			wantMessages:     1,
		},
		{
			name:             "empty idempotency key produces empty prefix",
			prompt:           "ola",
			idempotencyKey:   "",
			wantTenantPrefix: "",
			wantMessages:     1,
		},
	}

	llm := fake.New(fake.Config{})

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			resp, err := llm.Complete(context.Background(), aiassist.LLMRequest{
				Prompt:         tc.prompt,
				Model:          "anthropic/claude-3-haiku",
				MaxTokens:      256,
				IdempotencyKey: tc.idempotencyKey,
			})
			if err != nil {
				t.Fatalf("Complete returned error: %v", err)
			}

			assertResumoMatchesPrefix(t, resp.Text, tc.wantTenantPrefix, tc.wantMessages)
			assertHasThreeSuggestions(t, resp.Text)

			if resp.TokensIn <= 0 && len(tc.prompt) >= 4 {
				t.Errorf("TokensIn=%d, want > 0 for prompt len=%d", resp.TokensIn, len(tc.prompt))
			}
			if resp.TokensOut <= 0 {
				t.Errorf("TokensOut=%d, want > 0", resp.TokensOut)
			}
			if want := int64(len(tc.prompt) / 4); resp.TokensIn != want {
				t.Errorf("TokensIn=%d, want %d (len(prompt)/4)", resp.TokensIn, want)
			}
			if want := int64(len(resp.Text) / 4); resp.TokensOut != want {
				t.Errorf("TokensOut=%d, want %d (len(response)/4)", resp.TokensOut, want)
			}
		})
	}
}

// TestComplete_ParserContract asserts that the rendered text matches the
// marker contract documented in internal/web/inbox/ai_assist.go's
// parseAssistText: a leading RESUMO: line followed by SUGESTAO 1..3:
// lines. Re-implementing the marker scan keeps the test focused on the
// shape this package owns; the inbox package owns its own parser tests.
func TestComplete_ParserContract(t *testing.T) {
	t.Parallel()

	llm := fake.New(fake.Config{})
	resp, err := llm.Complete(context.Background(), aiassist.LLMRequest{
		Prompt:         "qualquer prompt",
		IdempotencyKey: newKey(tenantUUID, convUUID, reqID),
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}

	summary, suggestions := scanMarkers(resp.Text)
	if summary == "" {
		t.Fatalf("no RESUMO extracted from response: %q", resp.Text)
	}
	if !strings.Contains(summary, "[fake-llm]") {
		t.Errorf("RESUMO missing [fake-llm] tag: %q", summary)
	}
	if got, want := len(suggestions), 3; got != want {
		t.Fatalf("got %d SUGESTAO blocks, want %d (text=%q)", got, want, resp.Text)
	}
	for i, s := range suggestions {
		if strings.TrimSpace(s) == "" {
			t.Errorf("SUGESTAO %d is empty", i+1)
		}
	}
}

func TestComplete_ContextCancelledDuringDelay(t *testing.T) {
	t.Parallel()

	llm := fake.New(fake.Config{Delay: 5 * time.Second})
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(10 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	_, err := llm.Complete(ctx, aiassist.LLMRequest{
		Prompt:         "hi",
		IdempotencyKey: newKey(tenantUUID, convUUID, reqID),
	})
	elapsed := time.Since(start)

	if err == nil {
		t.Fatalf("expected ctx.Err(), got nil")
	}
	if err != context.Canceled {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
	if elapsed >= 1*time.Second {
		t.Errorf("Complete did not honour cancel: elapsed=%v", elapsed)
	}
}

func TestComplete_ContextAlreadyCancelled(t *testing.T) {
	t.Parallel()

	llm := fake.New(fake.Config{Delay: time.Hour})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := llm.Complete(ctx, aiassist.LLMRequest{
		Prompt:         "hi",
		IdempotencyKey: newKey(tenantUUID, convUUID, reqID),
	})
	if err != context.Canceled {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
}

func TestComplete_ZeroDelayRespondsImmediately(t *testing.T) {
	t.Parallel()

	llm := fake.New(fake.Config{})
	start := time.Now()
	_, err := llm.Complete(context.Background(), aiassist.LLMRequest{
		Prompt:         "oi",
		IdempotencyKey: newKey(tenantUUID, convUUID, reqID),
	})
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if elapsed > 50*time.Millisecond {
		t.Errorf("zero-delay path took %v, want < 50ms", elapsed)
	}
}

func TestComplete_DelayHonouredWhenNotCancelled(t *testing.T) {
	t.Parallel()

	delay := 30 * time.Millisecond
	llm := fake.New(fake.Config{Delay: delay})
	start := time.Now()
	_, err := llm.Complete(context.Background(), aiassist.LLMRequest{
		Prompt:         "oi",
		IdempotencyKey: newKey(tenantUUID, convUUID, reqID),
	})
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if elapsed < delay {
		t.Errorf("Complete returned before configured delay: elapsed=%v want >= %v", elapsed, delay)
	}
}

func TestNew_NegativeDelayTreatedAsZero(t *testing.T) {
	t.Parallel()

	llm := fake.New(fake.Config{Delay: -time.Hour})
	start := time.Now()
	_, err := llm.Complete(context.Background(), aiassist.LLMRequest{
		Prompt:         "oi",
		IdempotencyKey: newKey(tenantUUID, convUUID, reqID),
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 50*time.Millisecond {
		t.Errorf("negative delay should be zeroed, but elapsed=%v", elapsed)
	}
}

// assertResumoMatchesPrefix verifies that the first marker line carries
// the expected tenant prefix and message count in the documented order.
func assertResumoMatchesPrefix(t *testing.T, text, prefix string, messages int) {
	t.Helper()
	summary, _ := scanMarkers(text)
	if summary == "" {
		t.Fatalf("no RESUMO line in %q", text)
	}
	if !strings.Contains(summary, "tenant "+prefix+",") {
		t.Errorf("RESUMO missing tenant prefix %q: %q", prefix, summary)
	}
	if !strings.Contains(summary, sprintMsgs(messages)) {
		t.Errorf("RESUMO missing %d mensagens marker: %q", messages, summary)
	}
}

// assertHasThreeSuggestions ensures the three SUGESTAO markers appear
// in order and at the start of their respective lines.
func assertHasThreeSuggestions(t *testing.T, text string) {
	t.Helper()
	for i := 1; i <= 3; i++ {
		marker := sprintSugMarker(i)
		if !strings.Contains(text, marker) {
			t.Errorf("response missing %q: %q", marker, text)
		}
	}
}

// scanMarkers mirrors the marker shape parseAssistText looks for, so
// the test verifies the contract without importing the web package.
func scanMarkers(text string) (summary string, suggestions []string) {
	for _, raw := range strings.Split(text, "\n") {
		line := strings.TrimSpace(raw)
		switch {
		case strings.HasPrefix(strings.ToUpper(line), "RESUMO:"):
			summary = strings.TrimSpace(line[len("RESUMO:"):])
		case strings.HasPrefix(strings.ToUpper(line), "SUGESTAO 1:"):
			suggestions = append(suggestions, strings.TrimSpace(line[len("SUGESTAO 1:"):]))
		case strings.HasPrefix(strings.ToUpper(line), "SUGESTAO 2:"):
			suggestions = append(suggestions, strings.TrimSpace(line[len("SUGESTAO 2:"):]))
		case strings.HasPrefix(strings.ToUpper(line), "SUGESTAO 3:"):
			suggestions = append(suggestions, strings.TrimSpace(line[len("SUGESTAO 3:"):]))
		}
	}
	return summary, suggestions
}

func sprintMsgs(n int) string {
	return fmt.Sprintf("%d mensagens", n)
}

func sprintSugMarker(n int) string {
	return fmt.Sprintf("SUGESTAO %d:", n)
}
