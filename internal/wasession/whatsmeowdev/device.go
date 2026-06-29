package whatsmeowdev

import (
	"context"
	"sync"
	"time"

	"github.com/google/uuid"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types/events"

	"github.com/pericles-luz/crm/internal/wasession"
)

const (
	qrEventCode    = whatsmeow.QRChannelEventCode // "code"
	qrEventSuccess = "success"
)

// device implements wasession.Device on top of a whatsmeow client.
type device struct {
	cli    waClient
	tenant uuid.UUID
	sink   wasession.Sink
	now    func() time.Time

	mu        sync.Mutex
	last      wasession.Status
	fatalCh   chan struct{}
	fatalOnce sync.Once
}

func newDevice(cli waClient, tenant uuid.UUID, sink wasession.Sink, now func() time.Time) *device {
	if now == nil {
		now = time.Now
	}
	return &device{
		cli:     cli,
		tenant:  tenant,
		sink:    sink,
		now:     now,
		fatalCh: make(chan struct{}),
	}
}

// handleEvent is registered with the whatsmeow client (AddEventHandler). It
// fans status changes and inbound messages out through the Sink and trips the
// fatal latch on a terminal (banned) status so Connect returns and the
// supervisor stops reconnecting.
func (d *device) handleEvent(evt any) {
	if to, reason, ok := mapConnEvent(evt); ok {
		d.emitStatus(to, reason)
		if to.Terminal() {
			d.markFatal()
		}
		return
	}
	if msg, ok := evt.(*events.Message); ok {
		im := messageToInbound(msg)
		d.sink.Emit(wasession.Event{Kind: wasession.EventInbound, TenantID: d.tenant, Inbound: &im})
	}
}

func (d *device) emitStatus(to wasession.Status, reason string) {
	d.mu.Lock()
	from := d.last
	d.last = to
	d.mu.Unlock()
	sc := wasession.StatusChange{From: from, To: to, Reason: reason}
	d.sink.Emit(wasession.Event{Kind: wasession.EventStatus, TenantID: d.tenant, Status: &sc})
}

func (d *device) emitQR(code string, expiresAt time.Time) {
	qr := wasession.QRCode{Code: wasession.NewCredential(code), ExpiresAt: expiresAt}
	d.sink.Emit(wasession.Event{Kind: wasession.EventQR, TenantID: d.tenant, QR: &qr})
}

func (d *device) markFatal() { d.fatalOnce.Do(func() { close(d.fatalCh) }) }

// Connect establishes the session. An unpaired device starts QR pairing (the
// codes are pumped to the Sink), then the call blocks until the context is
// cancelled (clean stop) or the session becomes terminal (banned).
func (d *device) Connect(ctx context.Context) error {
	if !d.cli.IsLoggedIn() {
		qrCh, err := d.cli.GetQRChannel(ctx)
		if err != nil {
			return err
		}
		if err := d.cli.Connect(); err != nil {
			return err
		}
		go d.pumpQR(ctx, qrCh)
	} else if err := d.cli.Connect(); err != nil {
		return err
	}

	select {
	case <-ctx.Done():
		d.cli.Disconnect()
		return ctx.Err()
	case <-d.fatalCh:
		d.cli.Disconnect()
		return nil
	}
}

// pumpQR forwards QR pairing codes from whatsmeow to the Sink until the
// channel closes (pairing succeeds / errors) or the context is cancelled.
func (d *device) pumpQR(ctx context.Context, ch <-chan whatsmeow.QRChannelItem) {
	for {
		select {
		case <-ctx.Done():
			return
		case item, ok := <-ch:
			if !ok {
				return
			}
			switch item.Event {
			case qrEventCode:
				d.emitStatus(wasession.StatusPairing, "qr code issued")
				d.emitQR(item.Code, d.now().Add(item.Timeout))
			case qrEventSuccess:
				return
			default:
				d.emitStatus(wasession.StatusDisconnected, "pairing ended: "+item.Event)
				return
			}
		}
	}
}

// SendText sends a plain-text message and returns the whatsmeow message id.
func (d *device) SendText(ctx context.Context, toE164, body string) (string, error) {
	jid, err := e164ToJID(toE164)
	if err != nil {
		return "", err
	}
	resp, err := d.cli.SendMessage(ctx, jid, &waE2E.Message{Conversation: &body})
	if err != nil {
		return "", err
	}
	return resp.ID, nil
}

// Disconnect tears down the live connection without clearing credentials.
func (d *device) Disconnect() { d.cli.Disconnect() }

// Paired reports whether the whatsmeow store holds credentials for this
// tenant (i.e. it has completed pairing at least once).
func (d *device) Paired() bool { return d.cli.IsLoggedIn() }

var _ wasession.Device = (*device)(nil)
