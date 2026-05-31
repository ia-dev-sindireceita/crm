package canned_test

import (
	"context"
	"strings"
	"testing"

	"github.com/pericles-luz/crm/internal/adapter/channels/llmcustomer"
	"github.com/pericles-luz/crm/internal/adapter/channels/llmcustomer/canned"
)

func TestNewRejectsEmptyScript(t *testing.T) {
	t.Parallel()
	if _, err := canned.New(); err == nil {
		t.Fatalf("expected error for empty script, got nil")
	}
}

func TestNewDefaultUsesDefaultScript(t *testing.T) {
	t.Parallel()
	l := canned.NewDefault()
	got, err := l.NextCustomerMessage(context.Background(), llmcustomer.PersonaV1, nil)
	if err != nil {
		t.Fatalf("NextCustomerMessage: %v", err)
	}
	if got != canned.DefaultScript[0] {
		t.Fatalf("first reply = %q, want %q", got, canned.DefaultScript[0])
	}
	if strings.TrimSpace(got) == "" {
		t.Fatalf("default script returned blank first line")
	}
}

func TestNextCustomerMessageDeterministicByCustomerTurns(t *testing.T) {
	t.Parallel()
	script := []string{"linha-um", "linha-dois", "linha-tres"}
	l, err := canned.New(script...)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	cases := []struct {
		name    string
		history []llmcustomer.Turn
		want    string
	}{
		{name: "empty history", history: nil, want: "linha-um"},
		{
			name: "one customer turn",
			history: []llmcustomer.Turn{
				{Role: llmcustomer.TurnRoleCustomer, Body: "x"},
			},
			want: "linha-dois",
		},
		{
			name: "operator-only history ignored",
			history: []llmcustomer.Turn{
				{Role: llmcustomer.TurnRoleOperator, Body: "x"},
				{Role: llmcustomer.TurnRoleOperator, Body: "y"},
			},
			want: "linha-um",
		},
		{
			name: "two customer turns",
			history: []llmcustomer.Turn{
				{Role: llmcustomer.TurnRoleCustomer, Body: "a"},
				{Role: llmcustomer.TurnRoleOperator, Body: "b"},
				{Role: llmcustomer.TurnRoleCustomer, Body: "c"},
			},
			want: "linha-tres",
		},
		{
			name: "wraps after script length",
			history: []llmcustomer.Turn{
				{Role: llmcustomer.TurnRoleCustomer},
				{Role: llmcustomer.TurnRoleCustomer},
				{Role: llmcustomer.TurnRoleCustomer},
			},
			want: "linha-um",
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := l.NextCustomerMessage(context.Background(), llmcustomer.PersonaV1, tc.history)
			if err != nil {
				t.Fatalf("NextCustomerMessage: %v", err)
			}
			if got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestNextCustomerMessageHonoursContextCancellation(t *testing.T) {
	t.Parallel()
	l := canned.NewDefault()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := l.NextCustomerMessage(ctx, llmcustomer.PersonaV1, nil); err == nil {
		t.Fatalf("expected context error, got nil")
	}
}

func TestScriptDefensiveCopy(t *testing.T) {
	t.Parallel()
	script := []string{"alpha", "beta"}
	l, err := canned.New(script...)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	script[0] = "mutated"
	got, err := l.NextCustomerMessage(context.Background(), llmcustomer.PersonaV1, nil)
	if err != nil {
		t.Fatalf("NextCustomerMessage: %v", err)
	}
	if got != "alpha" {
		t.Fatalf("got %q, want %q (constructor must defensively copy)", got, "alpha")
	}
}
