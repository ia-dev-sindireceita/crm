// Package llmcustomer is the customer-side fake channel adapter used in
// dev/staging so the operator (`agent` user) can exercise the /inbox
// loop end-to-end without a real carrier. Reception path: when the
// operator replies to a synthetic conversation, the adapter records
// the operator's turn, asks a PersonaLLM for the customer's next line
// (asynchronously, on a goroutine bounded by the adapter's lifetime
// context), and pushes that line back into the inbox via the inbox's
// own InboundChannel use-case. Defence-in-depth: the carrier channel
// name is "fakellm" and the synthetic contact's display name is
// "Cliente Fake (LLM)", so any metric, audit row, or log line that
// leaks from a misconfigured dev box is obviously synthetic; the
// package contains no carrier client and therefore structurally cannot
// reach a real phone number, Messenger account, or e-mail address.
//
// Real LLM HTTP impl, config selector, router wireup, and the
// noop wallet adapter ship as separate workstreams (SIN-63793 W3-W5).
//
// Activation policy (SIN-63823): cmd/server's INBOX_CHANNEL_PROVIDER
// gate refuses to start when the fake adapter is selected on a
// production-tier APP_ENV (see cmd/server/inbox_channel_provider_wire.go),
// so the adapter never reaches a customer-facing deploy. Combined with
// the structural absence of a real-carrier client above, a config
// bypass still cannot send to a real phone number / chat platform.
package llmcustomer
