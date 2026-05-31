// Package canned is the deterministic PersonaLLM implementation used
// in unit tests for the llmcustomer adapter and as the safe fallback
// when no real LLM credentials are configured. It cycles through a
// short Portuguese-language script in lock-step with the number of
// customer turns already in the history, so the same input always
// produces the same output — no model state, no I/O, no clock.
package canned

import (
	"context"
	"errors"

	"github.com/pericles-luz/crm/internal/adapter/channels/llmcustomer"
)

// DefaultScript is the canned Portuguese-language script used when no
// custom script is provided. The lines play the role of "Mariana", the
// PersonaV1 customer in the parent package (a billing-question
// customer).
var DefaultScript = []string{
	"Oi, boa tarde! Recebi a fatura deste mês e o valor veio mais alto do que o normal. Vocês podem me ajudar a entender?",
	"O aumento foi de uns 18% e eu não recebi nenhum aviso antes. É possível rever?",
	"Entendi. Vocês conseguem me mandar o detalhamento por aqui mesmo?",
	"Tudo bem, fico no aguardo. Obrigada pela atenção.",
}

// LLM is a deterministic PersonaLLM that returns the next entry in a
// fixed script, indexed by how many customer turns are already in the
// supplied history. It is safe for concurrent use and carries no
// mutable state — the same (history, script) always produces the same
// reply, which is what makes it useful for tests and for the
// "no credentials" fallback in dev.
type LLM struct {
	script []string
}

// New returns a canned LLM bound to script. Passing zero scripts yields
// an error rather than a runtime panic — the empty-script case is
// unrecoverable and we surface it at construction so wiring bugs crash
// the process before the first reply.
func New(script ...string) (*LLM, error) {
	if len(script) == 0 {
		return nil, errors.New("canned: script must have at least one line")
	}
	out := make([]string, len(script))
	copy(out, script)
	return &LLM{script: out}, nil
}

// NewDefault returns a canned LLM pre-loaded with DefaultScript.
func NewDefault() *LLM {
	l, _ := New(DefaultScript...)
	return l
}

// NextCustomerMessage implements llmcustomer.PersonaLLM. The reply is
// script[customerTurns % len(script)] where customerTurns is the count
// of llmcustomer.TurnRoleCustomer entries already in history. The
// implementation is pure: persona is accepted to satisfy the contract
// but not consulted, history is read but not mutated, no goroutines are
// spawned, and ctx is only consulted for cancellation.
func (l *LLM) NextCustomerMessage(ctx context.Context, persona string, history []llmcustomer.Turn) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	_ = persona // Persona is fixed for the canned path; accepted for port-symmetry.
	customerTurns := 0
	for _, t := range history {
		if t.Role == llmcustomer.TurnRoleCustomer {
			customerTurns++
		}
	}
	return l.script[customerTurns%len(l.script)], nil
}
