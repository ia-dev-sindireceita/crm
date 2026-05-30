package llmfake_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/pericles-luz/crm/internal/adapter/llmfake"
	"github.com/pericles-luz/crm/internal/aiassist"
)

func TestComplete_HappyPath(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		req  aiassist.LLMRequest
	}{
		{
			name: "small prompt with model",
			req: aiassist.LLMRequest{
				Prompt:         "olá, resuma a conversa",
				Model:          "openai/gpt-4o-mini",
				MaxTokens:      400,
				IdempotencyKey: "tenant-1:conv-1:req-1",
			},
		},
		{
			name: "empty model is allowed (policy default)",
			req: aiassist.LLMRequest{
				Prompt:         "qualquer prompt",
				Model:          "",
				MaxTokens:      120,
				IdempotencyKey: "tenant-1:conv-2:req-1",
			},
		},
		{
			name: "long prompt",
			req: aiassist.LLMRequest{
				Prompt:         strings.Repeat("a", 4096),
				Model:          "anthropic/claude-haiku",
				MaxTokens:      800,
				IdempotencyKey: "tenant-2:conv-99:req-7",
			},
		},
	}

	c := llmfake.New(llmfake.WithDelay(0))
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			resp, err := c.Complete(context.Background(), tc.req)
			if err != nil {
				t.Fatalf("Complete returned error: %v", err)
			}
			if !strings.HasPrefix(resp.Text, "[FAKE LLM]") {
				t.Errorf("text missing [FAKE LLM] prefix: %q", resp.Text)
			}
			if !strings.Contains(resp.Text, "model="+tc.req.Model) {
				t.Errorf("text does not include model=%q: %q", tc.req.Model, resp.Text)
			}
			if wantIn := int64(len(tc.req.Prompt) / 4); resp.TokensIn != wantIn {
				t.Errorf("TokensIn = %d, want %d", resp.TokensIn, wantIn)
			}
			if wantOut := int64(tc.req.MaxTokens / 2); resp.TokensOut != wantOut {
				t.Errorf("TokensOut = %d, want %d", resp.TokensOut, wantOut)
			}
		})
	}
}

func TestComplete_EmptyPromptErr(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		prompt string
	}{
		{name: "empty string", prompt: ""},
		{name: "whitespace only", prompt: "   \n\t  "},
	}
	c := llmfake.New(llmfake.WithDelay(0))
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			resp, err := c.Complete(context.Background(), aiassist.LLMRequest{
				Prompt:         tc.prompt,
				Model:          "openai/gpt-4o-mini",
				MaxTokens:      100,
				IdempotencyKey: "irrelevant",
			})
			if !errors.Is(err, llmfake.ErrEmptyPrompt) {
				t.Fatalf("err = %v, want ErrEmptyPrompt", err)
			}
			if resp.Text != "" || resp.TokensIn != 0 || resp.TokensOut != 0 {
				t.Errorf("non-zero response on error: %+v", resp)
			}
		})
	}
}

func TestComplete_ContextCancelled(t *testing.T) {
	t.Parallel()

	t.Run("cancelled mid-delay returns ctx.Err", func(t *testing.T) {
		t.Parallel()
		c := llmfake.New(llmfake.WithDelay(200 * time.Millisecond))
		ctx, cancel := context.WithCancel(context.Background())
		errCh := make(chan error, 1)
		go func() {
			_, err := c.Complete(ctx, aiassist.LLMRequest{
				Prompt:         "anything",
				Model:          "m",
				MaxTokens:      10,
				IdempotencyKey: "k",
			})
			errCh <- err
		}()
		time.Sleep(20 * time.Millisecond)
		cancel()
		select {
		case err := <-errCh:
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("err = %v, want context.Canceled", err)
			}
		case <-time.After(time.Second):
			t.Fatal("Complete did not return after cancel")
		}
	})

	t.Run("already-cancelled ctx with zero delay returns ctx.Err", func(t *testing.T) {
		t.Parallel()
		c := llmfake.New(llmfake.WithDelay(0))
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, err := c.Complete(ctx, aiassist.LLMRequest{
			Prompt:         "anything",
			Model:          "m",
			MaxTokens:      10,
			IdempotencyKey: "k",
		})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
	})

	t.Run("deadline-exceeded propagates", func(t *testing.T) {
		t.Parallel()
		c := llmfake.New(llmfake.WithDelay(200 * time.Millisecond))
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		defer cancel()
		_, err := c.Complete(ctx, aiassist.LLMRequest{
			Prompt:         "anything",
			Model:          "m",
			MaxTokens:      10,
			IdempotencyKey: "k",
		})
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("err = %v, want context.DeadlineExceeded", err)
		}
	})
}

func TestComplete_DeterministicByIdempotencyKey(t *testing.T) {
	t.Parallel()
	c := llmfake.New(llmfake.WithDelay(0))
	req := aiassist.LLMRequest{
		Prompt:         "prompt A",
		Model:          "openai/gpt-4o-mini",
		MaxTokens:      200,
		IdempotencyKey: "tenant-7:conv-3:req-1",
	}
	a, err := c.Complete(context.Background(), req)
	if err != nil {
		t.Fatalf("first call: %v", err)
	}
	b, err := c.Complete(context.Background(), req)
	if err != nil {
		t.Fatalf("second call: %v", err)
	}
	if a.Text != b.Text {
		t.Errorf("same idempotency key produced different text:\n  a=%q\n  b=%q", a.Text, b.Text)
	}
	if a.TokensIn != b.TokensIn || a.TokensOut != b.TokensOut {
		t.Errorf("tokens not stable: a=%+v b=%+v", a, b)
	}
}

func TestComplete_DifferentKeysSpreadAcrossBodies(t *testing.T) {
	t.Parallel()
	c := llmfake.New(llmfake.WithDelay(0))
	keys := []string{
		"tenant-1:conv-1:req-1",
		"tenant-1:conv-1:req-2",
		"tenant-1:conv-2:req-1",
		"tenant-2:conv-9:req-5",
		"tenant-3:conv-4:req-1",
		"tenant-4:conv-1:req-1",
	}
	seen := make(map[string]struct{}, len(keys))
	for _, k := range keys {
		resp, err := c.Complete(context.Background(), aiassist.LLMRequest{
			Prompt:         "p",
			Model:          "m",
			MaxTokens:      10,
			IdempotencyKey: k,
		})
		if err != nil {
			t.Fatalf("Complete(%q): %v", k, err)
		}
		seen[resp.Text] = struct{}{}
	}
	if len(seen) < 2 {
		t.Errorf("expected >=2 distinct texts across %d keys, got %d", len(keys), len(seen))
	}
}

func TestNew_DefaultsToInjectedDelay(t *testing.T) {
	t.Parallel()
	c := llmfake.New()
	start := time.Now()
	_, err := c.Complete(context.Background(), aiassist.LLMRequest{
		Prompt:         "anything",
		Model:          "m",
		MaxTokens:      10,
		IdempotencyKey: "k",
	})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if elapsed < 30*time.Millisecond {
		t.Errorf("default delay too short: %v (expected ~50ms)", elapsed)
	}
	if elapsed > 500*time.Millisecond {
		t.Errorf("default delay too long: %v (expected ~50ms)", elapsed)
	}
}
