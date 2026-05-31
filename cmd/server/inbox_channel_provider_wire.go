package main

// SIN-63823 / SIN-63793 W4 — INBOX_CHANNEL_PROVIDER activation gate.
//
// This wire owns the typed enum the eventual SIN-63793 W5 selector uses
// to choose between {disabled, llmcustomer, real} when assembling the
// /inbox/* subtree. It also owns the secure-by-default boot gate that
// refuses to start when an operator points the binary at the
// fake-customer (llmcustomer) channel in a production-tier deploy.
//
// Two values are wired today; the `real` value is reserved for the
// eventual production carrier selector (out of scope for SIN-63793).
// Keeping the enum closed at parse time means a typo like "llm" or
// "fake" is rejected up-front instead of silently degrading to a soft
// default in the selector.
//
// Defense-in-depth around the fake-customer adapter:
//   - default value is `disabled`, so a bare deploy never wires the
//     LLM-driven customer simulator;
//   - production-tier (APP_ENV ∈ {production, staging-prod}) is hard-
//     refused at parse time, before any HTTP listener binds — see
//     InboxChannelProviderRefusedInProd below;
//   - the llmcustomer adapter (W2) ships without a real-carrier
//     client, so even if the gate were bypassed the package cannot
//     send to a real phone number / chat platform.

import (
	"errors"
	"fmt"
	"log/slog"
	"strings"
)

const (
	// envInboxChannelProvider names the canonical knob the cmd/server
	// W5 selector reads to decide which inbox channel-adapter family
	// is wired. Defaults to disabled when unset so a misconfigured
	// deploy 404s on /inbox/* rather than serving the simulator.
	envInboxChannelProvider = "INBOX_CHANNEL_PROVIDER"
)

// InboxChannelProvider names the channel-adapter family the
// /inbox/* subtree binds to at boot. Only the three constants below
// are valid; UnmarshalText rejects any other string with a wrapped
// error that includes the offending value so misconfiguration is
// obvious from the boot log.
type InboxChannelProvider string

const (
	// InboxChannelProviderDisabled (default) leaves /inbox/* unmounted.
	// Production builds ship with this value baked in until SIN-63793
	// W5 lands the selector that flips it on per-environment.
	InboxChannelProviderDisabled InboxChannelProvider = "disabled"

	// InboxChannelProviderLLMCustomer wires the SIN-63793 W2 fake
	// channel: an LLM plays the customer side of a synthetic
	// conversation so the operator can exercise the full inbox loop
	// without a real carrier. Refused at boot when APP_ENV is a
	// production-tier value (see InboxChannelProviderRefusedInProd).
	InboxChannelProviderLLMCustomer InboxChannelProvider = "llmcustomer"

	// InboxChannelProviderReal reserves the slot for the eventual
	// production selector that wires WhatsApp / Messenger / etc.
	// against real carriers. SIN-63793 ships only the disabled and
	// llmcustomer values; cmd/server treats `real` as accepted-but-
	// not-wired for now, which keeps the enum closed at parse time
	// without forcing a config-time error when operators stage the
	// production value ahead of W5.
	InboxChannelProviderReal InboxChannelProvider = "real"
)

// ErrInboxChannelProviderUnknown is returned by UnmarshalText / validate
// when the env value does not match one of the three documented
// constants. Wrapped with the offending raw input so the boot log line
// is self-explanatory.
var ErrInboxChannelProviderUnknown = errors.New("inbox: INBOX_CHANNEL_PROVIDER must be one of disabled, llmcustomer, real")

// ErrInboxChannelProviderRefusedInProd is returned by
// InboxChannelProviderRefusedInProd when an operator selects the
// fake-customer adapter (llmcustomer) on a production-tier deploy
// (APP_ENV ∈ {production, staging-prod}). The boot gate runs before
// the HTTP listener binds so a misconfigured prod deploy aborts on
// startup with a clear error rather than serving synthetic
// conversations at customer-facing scale.
var ErrInboxChannelProviderRefusedInProd = errors.New("inbox: INBOX_CHANNEL_PROVIDER=llmcustomer is refused in production-tier APP_ENV (production, staging-prod)")

// appEnvStagingProd names the optional production-tier staging deploy
// (a staging that mirrors production data / scale). The two literals
// below — production and staging-prod — are the only values that flip
// InboxChannelProviderRefusedInProd into fail-closed mode. Dev/CI/
// regular staging keep the permissive path so unit tests, docker
// compose, and the smoke deploy on https://acme.crm.crm.someu.com.br
// can still boot the fake adapter when operators are dog-fooding.
const appEnvStagingProd = "staging-prod"

// UnmarshalText decodes a textual env value into the typed enum.
// Implementing encoding.TextUnmarshaler keeps the option open for
// future generic config decoders (envconfig / kong / koanf), but the
// production call path is ReadInboxChannelProvider which dispatches
// to UnmarshalText directly. Empty input is treated as
// InboxChannelProviderDisabled (the documented default) so cmd/server
// does not need to special-case "" at every call site.
func (p *InboxChannelProvider) UnmarshalText(b []byte) error {
	raw := strings.TrimSpace(string(b))
	if raw == "" {
		*p = InboxChannelProviderDisabled
		return nil
	}
	candidate := InboxChannelProvider(raw)
	if err := candidate.validate(); err != nil {
		return err
	}
	*p = candidate
	return nil
}

// validate confirms the receiver names one of the three documented
// values. Lives as a method on the value type so call sites that
// build an enum from a non-text source (a Go literal, a future YAML
// decoder that bypasses TextUnmarshaler) can still gate it through
// the same closed-set check.
func (p InboxChannelProvider) validate() error {
	switch p {
	case InboxChannelProviderDisabled,
		InboxChannelProviderLLMCustomer,
		InboxChannelProviderReal:
		return nil
	}
	return fmt.Errorf("%w (got %q)", ErrInboxChannelProviderUnknown, string(p))
}

// String returns the lowercase token form (the same string the env
// var carries) so the boot-log audit line and any future metric label
// stay stable across refactors.
func (p InboxChannelProvider) String() string { return string(p) }

// ReadInboxChannelProvider parses INBOX_CHANNEL_PROVIDER from the
// supplied getenv. A nil getenv (test ergonomics) and an empty value
// both resolve to InboxChannelProviderDisabled. Any other unknown
// value is rejected via UnmarshalText so a typo cannot silently
// degrade to the default.
func ReadInboxChannelProvider(getenv func(string) string) (InboxChannelProvider, error) {
	if getenv == nil {
		return InboxChannelProviderDisabled, nil
	}
	var provider InboxChannelProvider
	if err := provider.UnmarshalText([]byte(getenv(envInboxChannelProvider))); err != nil {
		return InboxChannelProviderDisabled, err
	}
	return provider, nil
}

// LogInboxChannelProviderBoot emits the structured boot-log audit
// line cmd/server uses to surface the selected channel provider on
// every start. The attribute key (inbox.channel.provider) is the
// contract the operator dashboards / log alerts watch — keep it stable.
//
// Lives in its own helper so a unit test can drive it against a
// JSON-capturing slog handler without spinning up the HTTP listener.
func LogInboxChannelProviderBoot(logger *slog.Logger, provider InboxChannelProvider) {
	if logger == nil {
		logger = slog.Default()
	}
	logger.Info(
		"crm: inbox channel provider selected",
		slog.String("inbox.channel.provider", provider.String()),
	)
}

// InboxChannelProviderRefusedInProd enforces the SIN-63793 W4
// secure-by-default gate: when APP_ENV names a production-tier deploy
// (production or staging-prod) AND INBOX_CHANNEL_PROVIDER selects the
// fake-customer adapter, boot fails closed with
// ErrInboxChannelProviderRefusedInProd. Dev/CI/regular staging stay
// permissive so the smoke deploy can dog-food the fake channel.
//
// Call this from cmd/server BEFORE the HTTP listener binds so a
// misconfigured prod deploy aborts on startup with a clear error
// rather than serving synthetic conversations at customer-facing
// scale.
func InboxChannelProviderRefusedInProd(getenv func(string) string) error {
	if getenv == nil {
		return nil
	}
	provider, err := ReadInboxChannelProvider(getenv)
	if err != nil {
		return err
	}
	if provider != InboxChannelProviderLLMCustomer {
		return nil
	}
	switch strings.TrimSpace(getenv(envAppEnv)) {
	case appEnvProduction, appEnvStagingProd:
		return fmt.Errorf("%w (APP_ENV=%q)", ErrInboxChannelProviderRefusedInProd, getenv(envAppEnv))
	}
	return nil
}
