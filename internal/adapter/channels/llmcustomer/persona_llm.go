package llmcustomer

import "context"

// ChannelName is the carrier-channel identifier all messages produced
// by this adapter carry. The "fakellm" prefix is deliberately
// recognisable so any inbox row, metric, or audit log line that leaks
// from a misconfigured environment is obviously synthetic at a glance.
const ChannelName = "fakellm"

// SyntheticContactDisplayName is the operator-visible name of the
// synthetic contact every tenant gets seeded with on Bootstrap. The
// "(LLM)" suffix marks the contact as a simulator so an operator
// staring at the inbox cannot mistake it for a real customer.
const SyntheticContactDisplayName = "Cliente Fake (LLM)"

// SyntheticContactExternalID is the channel-side identifier of the v1
// persona. There is exactly one persona per tenant in v1 — multi-persona
// and per-tenant configuration are explicitly v2 and out of scope.
const SyntheticContactExternalID = "fakellm:demo-1"

// TurnRoleCustomer / TurnRoleOperator are the two well-known role
// labels the adapter and the canned implementation use when populating
// Turn.Role. PersonaLLM implementations MUST treat Role as opaque text
// to keep the contract free of role-enum coupling.
const (
	TurnRoleCustomer = "customer"
	TurnRoleOperator = "operator"
)

// PersonaV1 is the hard-coded persona prompt every fake conversation
// runs under in v1. It is a generic Portuguese-speaking billing
// customer; multi-persona / per-tenant configuration is explicitly v2.
const PersonaV1 = `Você é Mariana, uma cliente brasileira do plano corporativo da Sindireceita. ` +
	`Você acabou de receber a fatura do mês e o valor cobrado está cerca de 18% maior do que ` +
	`o esperado, sem nenhuma comunicação prévia. Você quer entender por que o valor mudou ` +
	`e ver se há como rever a cobrança. Fale em português brasileiro, em tom educado mas ` +
	`firme, mensagens curtas (uma a três frases), evite jargão. Não invente dados sobre ` +
	`seu próprio contrato — descreva apenas o que você percebeu na fatura.`

// Turn is one entry in the conversation history handed to PersonaLLM.
// Role is opaque text (in practice "customer" or "operator" — see the
// TurnRole* constants) and Body is the message text exactly as it was
// shown to the other side. The struct is deliberately field-positional
// because the contract is fixed by SIN-63793 W2.
type Turn struct {
	Role string
	Body string
}

// PersonaLLM is the adapter-land port for "given a persona prompt and
// the recent turn history, give me the customer's next line". It is
// intentionally a sibling of inbox / contacts ports and lives next to
// the adapter that drives it — not in internal/aiassist — because the
// aiassist port carries token-budget, idempotency, and consent
// semantics that do not apply to a fake-customer simulator (no real
// cost, no consent gate, no anonymisation) and a hard dependency
// direction rule keeps internal/adapter/channels/* free of
// internal/aiassist imports.
//
// Implementations MUST honour ctx for cancellation and SHOULD return
// promptly (sub-second for the canned impl; bounded by a small timeout
// for the future HTTP-backed impl in SIN-63793 W3).
type PersonaLLM interface {
	NextCustomerMessage(ctx context.Context, persona string, history []Turn) (string, error)
}
