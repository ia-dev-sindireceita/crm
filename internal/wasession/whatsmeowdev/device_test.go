package whatsmeowdev

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"

	"github.com/pericles-luz/crm/internal/wasession"
)

// fakeClient is an in-memory whatsmeow transport (not a DB mock). It
// implements sessionClient so it can stand in for *whatsmeow.Client.
type fakeClient struct {
	loggedIn   bool
	connectErr error
	qrErr      error
	qrCh       chan whatsmeow.QRChannelItem
	sendResp   whatsmeow.SendResponse
	sendErr    error

	mu          sync.Mutex
	connects    int
	disconnects int
	handler     whatsmeow.EventHandler
	sentTo      types.JID
	sentMsg     *waE2E.Message
}

func (c *fakeClient) IsLoggedIn() bool { return c.loggedIn }

func (c *fakeClient) Connect() error {
	c.mu.Lock()
	c.connects++
	c.mu.Unlock()
	return c.connectErr
}

func (c *fakeClient) Disconnect() {
	c.mu.Lock()
	c.disconnects++
	c.mu.Unlock()
}

func (c *fakeClient) GetQRChannel(ctx context.Context) (<-chan whatsmeow.QRChannelItem, error) {
	if c.qrErr != nil {
		return nil, c.qrErr
	}
	return c.qrCh, nil
}

func (c *fakeClient) SendMessage(ctx context.Context, to types.JID, msg *waE2E.Message, extra ...whatsmeow.SendRequestExtra) (whatsmeow.SendResponse, error) {
	c.mu.Lock()
	c.sentTo = to
	c.sentMsg = msg
	c.mu.Unlock()
	return c.sendResp, c.sendErr
}

func (c *fakeClient) AddEventHandler(h whatsmeow.EventHandler) uint32 {
	c.handler = h
	return 1
}

func (c *fakeClient) counts() (int, int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.connects, c.disconnects
}

type fakeSink struct {
	mu     sync.Mutex
	events []wasession.Event
}

func (s *fakeSink) Emit(ev wasession.Event) {
	s.mu.Lock()
	s.events = append(s.events, ev)
	s.mu.Unlock()
}

func (s *fakeSink) byKind(k wasession.EventKind) []wasession.Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []wasession.Event
	for _, ev := range s.events {
		if ev.Kind == k {
			out = append(out, ev)
		}
	}
	return out
}

func (s *fakeSink) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.events)
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("condition not met within deadline")
}

func TestPairedReflectsLogin(t *testing.T) {
	t.Parallel()
	if !newDevice(&fakeClient{loggedIn: true}, uuid.New(), &fakeSink{}, nil).Paired() {
		t.Error("logged-in client should be Paired")
	}
	if newDevice(&fakeClient{loggedIn: false}, uuid.New(), &fakeSink{}, nil).Paired() {
		t.Error("logged-out client should not be Paired")
	}
}

func TestConnectAlreadyPairedBlocksUntilCtx(t *testing.T) {
	t.Parallel()
	cli := &fakeClient{loggedIn: true}
	d := newDevice(cli, uuid.New(), &fakeSink{}, nil)
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() { done <- d.Connect(ctx) }()

	waitFor(t, func() bool { c, _ := cli.counts(); return c == 1 })
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("Connect err = %v, want Canceled", err)
	}
	if _, dc := cli.counts(); dc < 1 {
		t.Error("Connect should Disconnect on ctx cancel")
	}
}

func TestConnectConnectError(t *testing.T) {
	t.Parallel()
	cli := &fakeClient{loggedIn: true, connectErr: errors.New("dial failed")}
	d := newDevice(cli, uuid.New(), &fakeSink{}, nil)
	if err := d.Connect(context.Background()); err == nil || err.Error() != "dial failed" {
		t.Fatalf("Connect err = %v, want dial failed", err)
	}
}

func TestConnectQRChannelError(t *testing.T) {
	t.Parallel()
	cli := &fakeClient{loggedIn: false, qrErr: errors.New("qr failed")}
	d := newDevice(cli, uuid.New(), &fakeSink{}, nil)
	if err := d.Connect(context.Background()); err == nil || err.Error() != "qr failed" {
		t.Fatalf("Connect err = %v, want qr failed", err)
	}
}

func TestConnectPairingEmitsQR(t *testing.T) {
	t.Parallel()
	fixed := time.Unix(1700000000, 0)
	cli := &fakeClient{loggedIn: false, qrCh: make(chan whatsmeow.QRChannelItem, 4)}
	sink := &fakeSink{}
	tenant := uuid.New()
	d := newDevice(cli, tenant, sink, func() time.Time { return fixed })

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- d.Connect(ctx) }()

	cli.qrCh <- whatsmeow.QRChannelItem{Event: whatsmeow.QRChannelEventCode, Code: "PAIRCODE123", Timeout: 60 * time.Second}
	waitFor(t, func() bool { return len(sink.byKind(wasession.EventQR)) >= 1 })

	qr := sink.byKind(wasession.EventQR)[0]
	if qr.TenantID != tenant {
		t.Errorf("qr tenant = %v", qr.TenantID)
	}
	if qr.QR.Code.Reveal() != "PAIRCODE123" {
		t.Errorf("qr code = %q", qr.QR.Code.Reveal())
	}
	if !qr.QR.ExpiresAt.Equal(fixed.Add(60 * time.Second)) {
		t.Errorf("qr expiry = %v", qr.QR.ExpiresAt)
	}
	// A status->pairing must precede the code.
	if sts := sink.byKind(wasession.EventStatus); len(sts) == 0 || sts[0].Status.To != wasession.StatusPairing {
		t.Errorf("expected pairing status, got %+v", sts)
	}

	// success closes the pairing pump; ctx cancel ends Connect.
	cli.qrCh <- whatsmeow.QRChannelItem{Event: "success"}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("Connect err = %v", err)
	}
}

func TestPumpQRErrorEndsPairing(t *testing.T) {
	t.Parallel()
	cli := &fakeClient{loggedIn: false, qrCh: make(chan whatsmeow.QRChannelItem, 1)}
	sink := &fakeSink{}
	d := newDevice(cli, uuid.New(), sink, nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- d.Connect(ctx) }()

	cli.qrCh <- whatsmeow.QRChannelItem{Event: whatsmeow.QRChannelEventError}
	waitFor(t, func() bool {
		for _, ev := range sink.byKind(wasession.EventStatus) {
			if ev.Status.To == wasession.StatusDisconnected {
				return true
			}
		}
		return false
	})
	cancel()
	<-done
}

func TestConnectReturnsOnFatalBan(t *testing.T) {
	t.Parallel()
	cli := &fakeClient{loggedIn: true}
	sink := &fakeSink{}
	d := newDevice(cli, uuid.New(), sink, nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- d.Connect(ctx) }()

	waitFor(t, func() bool { c, _ := cli.counts(); return c == 1 })
	// Simulate a logout event arriving through the handler.
	d.handleEvent(&events.LoggedOut{})
	if err := <-done; err != nil {
		t.Fatalf("Connect on ban err = %v, want nil", err)
	}
	if got := sink.byKind(wasession.EventStatus); len(got) == 0 || got[len(got)-1].Status.To != wasession.StatusBanned {
		t.Errorf("expected banned status, got %+v", got)
	}
}

func TestHandleEventDispatch(t *testing.T) {
	t.Parallel()
	cli := &fakeClient{loggedIn: true}
	sink := &fakeSink{}
	tenant := uuid.New()
	d := newDevice(cli, tenant, sink, nil)

	d.handleEvent(&events.Connected{})
	d.handleEvent(&events.Disconnected{})
	d.handleEvent(&events.Message{
		Info:    types.MessageInfo{ID: "wamid.A", MessageSource: types.MessageSource{Sender: types.NewJID("5511", types.DefaultUserServer)}},
		Message: &waE2E.Message{Conversation: sptr("hello")},
	})
	d.handleEvent(&events.Receipt{}) // unmapped -> ignored

	st := sink.byKind(wasession.EventStatus)
	if len(st) != 2 {
		t.Fatalf("status events = %d, want 2", len(st))
	}
	if st[0].Status.To != wasession.StatusConnected || st[0].Status.From != "" {
		t.Errorf("first status = %+v", st[0].Status)
	}
	if st[1].Status.To != wasession.StatusDisconnected || st[1].Status.From != wasession.StatusConnected {
		t.Errorf("second status From should chain: %+v", st[1].Status)
	}
	inb := sink.byKind(wasession.EventInbound)
	if len(inb) != 1 || inb[0].Inbound.Body != "hello" || inb[0].TenantID != tenant {
		t.Errorf("inbound = %+v", inb)
	}
	if sink.count() != 3 {
		t.Errorf("total events = %d, want 3 (receipt ignored)", sink.count())
	}
}

func TestSendText(t *testing.T) {
	t.Parallel()
	cli := &fakeClient{sendResp: whatsmeow.SendResponse{ID: "wamid.OUT"}}
	d := newDevice(cli, uuid.New(), &fakeSink{}, nil)

	id, err := d.SendText(context.Background(), "+5511988887777", "oi")
	if err != nil {
		t.Fatalf("SendText: %v", err)
	}
	if id != "wamid.OUT" {
		t.Errorf("id = %q", id)
	}
	cli.mu.Lock()
	to, msg := cli.sentTo, cli.sentMsg
	cli.mu.Unlock()
	if to.User != "5511988887777" || to.Server != types.DefaultUserServer {
		t.Errorf("sent to = %+v", to)
	}
	if msg.GetConversation() != "oi" {
		t.Errorf("sent body = %q", msg.GetConversation())
	}
}

func TestSendTextInvalidRecipient(t *testing.T) {
	t.Parallel()
	cli := &fakeClient{}
	d := newDevice(cli, uuid.New(), &fakeSink{}, nil)
	if _, err := d.SendText(context.Background(), "not-a-phone", "x"); err == nil {
		t.Fatal("expected error for invalid recipient")
	}
	cli.mu.Lock()
	sent := cli.sentMsg
	cli.mu.Unlock()
	if sent != nil {
		t.Error("SendMessage must not be called for invalid recipient")
	}
}

func TestSendTextCarrierError(t *testing.T) {
	t.Parallel()
	cli := &fakeClient{sendErr: errors.New("carrier down")}
	d := newDevice(cli, uuid.New(), &fakeSink{}, nil)
	if _, err := d.SendText(context.Background(), "5511", "x"); err == nil || err.Error() != "carrier down" {
		t.Fatalf("SendText err = %v, want carrier down", err)
	}
}

func TestDisconnectDelegates(t *testing.T) {
	t.Parallel()
	cli := &fakeClient{}
	d := newDevice(cli, uuid.New(), &fakeSink{}, nil)
	d.Disconnect()
	if _, dc := cli.counts(); dc != 1 {
		t.Errorf("disconnect count = %d, want 1", dc)
	}
}
