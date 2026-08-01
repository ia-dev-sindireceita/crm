package main

// INBOX_FAKE_CUSTOMER_ENABLED wire-up.
//
// The fake-customer channel (llmcustomer, key "fakellm") used to be its
// own exclusive INBOX_CHANNEL_PROVIDER value — mutually exclusive with
// `real`, so turning on real WhatsApp outbound (INBOX_CHANNEL_PROVIDER=real)
// silently killed the demo/QA fake customer. This wire lets the two run
// side by side: when INBOX_CHANNEL_PROVIDER=real AND
// INBOX_FAKE_CUSTOMER_ENABLED=1, inbox_wire_real.go additionally builds a
// *llmcustomer.Adapter and registers it under the "fakellm" key in the
// same outbound dispatch.Router the real WhatsApp sender uses.
//
// Mirrors two existing, independently-reviewed patterns so operators
// reading this file recognise the shape immediately:
//   - the two-env-var global+allowlist gate is
//     channelswhatsapp.EnvFeatureFlag (internal/adapter/channels/whatsapp/config.go);
//   - the prod-tier boot refusal is InboxChannelProviderRefusedInProd
//     (inbox_channel_provider_wire.go), reusing the same envAppEnv /
//     appEnvProduction / appEnvStagingProd constants so a stray "1" left
//     over from a staging config can never boot in a production-tier
//     APP_ENV.
import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/adapter/channels/llmcustomer"
)

const (
	// envInboxFakeCustomerEnabled globally gates the fake-customer
	// channel. Unset/anything but "1" ⇒ off — the pre-existing,
	// llmcustomer-only-provider behavior is unaffected either way.
	envInboxFakeCustomerEnabled = "INBOX_FAKE_CUSTOMER_ENABLED"
	// envInboxFakeCustomerTenants is a comma-separated tenant UUID
	// allowlist. Empty ⇒ every tenant, matching
	// channelswhatsapp.EnvFeatureFlag's "empty allowlist" semantics.
	envInboxFakeCustomerTenants = "INBOX_FAKE_CUSTOMER_TENANTS"
)

// inboxFakeCustomerFlag is the llmcustomer.TenantAllowlist implementation
// bound to the two env vars above. Struct shape intentionally mirrors
// channelswhatsapp.EnvFeatureFlag.
type inboxFakeCustomerFlag struct {
	globalOn bool
	allowed  map[uuid.UUID]struct{}
}

// newInboxFakeCustomerFlag parses INBOX_FAKE_CUSTOMER_ENABLED /
// INBOX_FAKE_CUSTOMER_TENANTS. Invalid UUIDs in the allowlist are
// silently dropped, matching NewEnvFeatureFlag's posture.
func newInboxFakeCustomerFlag(getenv func(string) string) *inboxFakeCustomerFlag {
	if getenv == nil {
		return &inboxFakeCustomerFlag{}
	}
	on := strings.TrimSpace(getenv(envInboxFakeCustomerEnabled)) == "1"
	allow := map[uuid.UUID]struct{}{}
	for _, raw := range strings.Split(getenv(envInboxFakeCustomerTenants), ",") {
		s := strings.TrimSpace(raw)
		if s == "" {
			continue
		}
		id, err := uuid.Parse(s)
		if err != nil {
			continue
		}
		allow[id] = struct{}{}
	}
	return &inboxFakeCustomerFlag{globalOn: on, allowed: allow}
}

// Enabled implements llmcustomer.TenantAllowlist. globalOn=false ⇒
// always off. globalOn with an empty allowlist ⇒ always on. globalOn
// with a populated allowlist ⇒ on iff tenantID is listed.
func (f *inboxFakeCustomerFlag) Enabled(_ context.Context, tenantID uuid.UUID) (bool, error) {
	if f == nil || !f.globalOn {
		return false, nil
	}
	if len(f.allowed) == 0 {
		return true, nil
	}
	_, ok := f.allowed[tenantID]
	return ok, nil
}

// ErrInboxFakeCustomerRefusedInProd is returned by
// InboxFakeCustomerRefusedInProd when INBOX_FAKE_CUSTOMER_ENABLED=1 on a
// production-tier deploy (APP_ENV ∈ {production, staging-prod}).
var ErrInboxFakeCustomerRefusedInProd = fmt.Errorf(
	"inbox: %s=1 is refused in production-tier APP_ENV (production, staging-prod)",
	envInboxFakeCustomerEnabled,
)

// InboxFakeCustomerRefusedInProd checks the raw env var directly — not
// gated on INBOX_CHANNEL_PROVIDER's value — so a stray "1" left over
// from a staging config can never boot the LLM-simulated customer in a
// production-tier deploy even if INBOX_CHANNEL_PROVIDER later changes.
// Call this from cmd/server BEFORE the HTTP listener binds, alongside
// InboxChannelProviderRefusedInProd.
func InboxFakeCustomerRefusedInProd(getenv func(string) string) error {
	if getenv == nil {
		return nil
	}
	if strings.TrimSpace(getenv(envInboxFakeCustomerEnabled)) != "1" {
		return nil
	}
	switch strings.TrimSpace(getenv(envAppEnv)) {
	case appEnvProduction, appEnvStagingProd:
		return fmt.Errorf("%w (APP_ENV=%q)", ErrInboxFakeCustomerRefusedInProd, getenv(envAppEnv))
	}
	return nil
}

// compile-time guard: inboxFakeCustomerFlag satisfies the allowlist port
// the llmcustomer adapter consumes.
var _ llmcustomer.TenantAllowlist = (*inboxFakeCustomerFlag)(nil)
