package wa_session

import (
	"testing"

	"github.com/pericles-luz/crm/internal/contacts"
	"github.com/pericles-luz/crm/internal/inbox"
)

// TestChannelMatchesOfficial pins the invariant from ADR 0107 D4: the
// session transport registers under the SAME channel string as the
// official Meta adapter so a contact's WhatsApp thread is unified across
// providers. If this drifts, the (channel, external_id) dedup ledger and
// the E.164 identity rule silently split.
func TestChannelMatchesOfficial(t *testing.T) {
	if Channel != contacts.ChannelWhatsApp {
		t.Fatalf("Channel = %q, want %q (contacts.ChannelWhatsApp)", Channel, contacts.ChannelWhatsApp)
	}
	if Channel != "whatsapp" {
		t.Fatalf("Channel = %q, want %q", Channel, "whatsapp")
	}
}

// TestProviderIsSession pins the provider discriminator (ADR 0107 D4).
func TestProviderIsSession(t *testing.T) {
	if Provider != "session" {
		t.Fatalf("Provider = %q, want %q", Provider, "session")
	}
}

// TestAdapterSatisfiesOutboundPort is a compile-time-as-runtime guard
// that the adapter implements the domain outbound port Fase 3 wires.
func TestAdapterSatisfiesOutboundPort(t *testing.T) {
	var _ inbox.OutboundChannel = (*Adapter)(nil)
}
