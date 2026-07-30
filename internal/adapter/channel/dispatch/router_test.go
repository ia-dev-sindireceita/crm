package dispatch

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/inbox"
)

func TestRouter_RoutesToRegisteredChannel(t *testing.T) {
	t.Parallel()
	wa := newRecordingChannel("wamid.wa")
	ms := newRecordingChannel("mid.ms")
	r := NewRouter(map[string]inbox.OutboundChannel{
		"whatsapp":  wa,
		"messenger": ms,
	})
	tenant := uuid.New()

	got, err := r.SendMessage(context.Background(), newOutboundMessage(tenant, "whatsapp", ""))
	if err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	if got != "wamid.wa" {
		t.Errorf("wamid = %q, want wamid.wa", got)
	}
	if wa.callCount() != 1 {
		t.Errorf("whatsapp calls = %d, want 1", wa.callCount())
	}
	if ms.callCount() != 0 {
		t.Errorf("messenger calls = %d, want 0 (not routed)", ms.callCount())
	}
}

func TestRouter_UnknownChannel_DisabledNoSend(t *testing.T) {
	t.Parallel()
	wa := newRecordingChannel("wamid.wa")
	r := NewRouter(map[string]inbox.OutboundChannel{"whatsapp": wa})

	_, err := r.SendMessage(context.Background(), newOutboundMessage(uuid.New(), "telegram", ""))
	if !errors.Is(err, inbox.ErrChannelDisabled) {
		t.Fatalf("err = %v, want ErrChannelDisabled", err)
	}
	if wa.callCount() != 0 {
		t.Errorf("whatsapp calls = %d, want 0 (unrouted must not fall through)", wa.callCount())
	}
}

func TestRouter_EmptyRouter_IsGracefulNoOp(t *testing.T) {
	t.Parallel()
	// The composition-root no-op: no carrier credentials → empty router.
	for _, routes := range []map[string]inbox.OutboundChannel{nil, {}} {
		r := NewRouter(routes)
		_, err := r.SendMessage(context.Background(), newOutboundMessage(uuid.New(), "whatsapp", ""))
		if !errors.Is(err, inbox.ErrChannelDisabled) {
			t.Fatalf("empty router err = %v, want ErrChannelDisabled", err)
		}
	}
}

func TestRouter_DropsNilAndBlankRoutes(t *testing.T) {
	t.Parallel()
	wa := newRecordingChannel("wamid.wa")
	r := NewRouter(map[string]inbox.OutboundChannel{
		"whatsapp": wa,
		"":         wa,  // blank channel key dropped
		"broken":   nil, // nil adapter dropped
	})
	// blank + broken are dropped; only whatsapp survives.
	if _, err := r.SendMessage(context.Background(), newOutboundMessage(uuid.New(), "broken", "")); !errors.Is(err, inbox.ErrChannelDisabled) {
		t.Errorf("nil route err = %v, want ErrChannelDisabled", err)
	}
	if _, err := r.SendMessage(context.Background(), newOutboundMessage(uuid.New(), "", "")); !errors.Is(err, inbox.ErrChannelDisabled) {
		t.Errorf("blank route err = %v, want ErrChannelDisabled", err)
	}
	if _, err := r.SendMessage(context.Background(), newOutboundMessage(uuid.New(), "whatsapp", "")); err != nil {
		t.Errorf("whatsapp route err = %v, want nil", err)
	}
}

// TestRouter_DefensiveCopy proves mutating the caller's map after
// construction cannot re-point a live route.
func TestRouter_DefensiveCopy(t *testing.T) {
	t.Parallel()
	wa := newRecordingChannel("wamid.wa")
	src := map[string]inbox.OutboundChannel{"whatsapp": wa}
	r := NewRouter(src)
	src["whatsapp"] = newRecordingChannel("evil") // must not affect r
	got, err := r.SendMessage(context.Background(), newOutboundMessage(uuid.New(), "whatsapp", ""))
	if err != nil || got != "wamid.wa" {
		t.Fatalf("got (%q,%v), want (wamid.wa,nil) — router held a stale reference", got, err)
	}
}
