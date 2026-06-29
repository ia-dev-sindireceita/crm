package main

// SIN-66258 — unit coverage for the WhatsApp session (whatsmeow) Fase 3
// wireup. buildWASessionWiring's happy path dials Postgres + opens a
// whatsmeow sqlstore, so (like whatsapp_wire_test) we assert the env
// gating contract there and unit-test every pure seam — the inbound
// pump, the outbound bridge, the provider router, the tenant parser and
// the assembler — with table-driven fakes and no DB.

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	wasessionchan "github.com/pericles-luz/crm/internal/adapter/channels/wa_session"
	"github.com/pericles-luz/crm/internal/inbox"
	"github.com/pericles-luz/crm/internal/wasession"
)

// --- fakes ------------------------------------------------------------

type fakeInboundChannel struct {
	called int
	last   inbox.InboundEvent
	err    error
}

func (f *fakeInboundChannel) HandleInbound(_ context.Context, ev inbox.InboundEvent) error {
	f.called++
	f.last = ev
	return f.err
}

type recordingReceiver struct {
	msgs []wasessionchan.SessionMessage
	err  error
}

func (r *recordingReceiver) Receive(_ context.Context, m wasessionchan.SessionMessage) error {
	r.msgs = append(r.msgs, m)
	return r.err
}

type fakeDispatcher struct {
	tenant uuid.UUID
	to     string
	body   string
	id     string
	err    error
}

func (f *fakeDispatcher) Send(_ context.Context, tenantID uuid.UUID, toE164, body string) (string, error) {
	f.tenant = tenantID
	f.to = toE164
	f.body = body
	return f.id, f.err
}

type fakeSessionSender struct{}

func (fakeSessionSender) SendText(_ context.Context, _ uuid.UUID, _, _ string) (string, error) {
	return "wamid.x", nil
}

type fakeFlag struct {
	on  bool
	err error
}

func (f fakeFlag) Enabled(_ context.Context, _ uuid.UUID) (bool, error) { return f.on, f.err }

type fakeRate struct{}

func (fakeRate) Allow(_ context.Context, _ string, _ time.Duration, _ int) (bool, time.Duration, error) {
	return true, 0, nil
}

type recordingOutbound struct {
	tag    string
	called int
	last   inbox.OutboundMessage
	id     string
	err    error
}

func (r *recordingOutbound) SendMessage(_ context.Context, m inbox.OutboundMessage) (string, error) {
	r.called++
	r.last = m
	if r.id == "" {
		return r.tag, r.err
	}
	return r.id, r.err
}

// --- waSessionGloballyEnabled ----------------------------------------

func TestWASessionGloballyEnabled(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		getenv func(string) string
		want   bool
	}{
		{"nil getenv", nil, false},
		{"unset", func(string) string { return "" }, false},
		{"zero", func(k string) string { return "0" }, false},
		{"enabled", func(k string) string {
			if k == wasessionchan.EnvSessionEnabled {
				return "1"
			}
			return ""
		}, true},
		{"enabled with whitespace", func(k string) string {
			if k == wasessionchan.EnvSessionEnabled {
				return "  1  "
			}
			return ""
		}, true},
		{"truthy-but-not-1", func(k string) string {
			if k == wasessionchan.EnvSessionEnabled {
				return "true"
			}
			return ""
		}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := waSessionGloballyEnabled(tc.getenv); got != tc.want {
				t.Fatalf("waSessionGloballyEnabled = %v, want %v", got, tc.want)
			}
		})
	}
}

// --- parseWASessionTenants -------------------------------------------

func TestParseWASessionTenants(t *testing.T) {
	t.Parallel()
	a := uuid.New()
	b := uuid.New()
	getenvFor := func(v string) func(string) string {
		return func(k string) string {
			if k == wasessionchan.EnvSessionTenantAllow {
				return v
			}
			return ""
		}
	}

	t.Run("nil getenv", func(t *testing.T) {
		if got := parseWASessionTenants(nil); got != nil {
			t.Fatalf("want nil, got %v", got)
		}
	})
	t.Run("empty", func(t *testing.T) {
		if got := parseWASessionTenants(getenvFor("")); len(got) != 0 {
			t.Fatalf("want 0, got %v", got)
		}
	})
	t.Run("valid multiple", func(t *testing.T) {
		got := parseWASessionTenants(getenvFor(a.String() + "," + b.String()))
		if len(got) != 2 || got[0] != a || got[1] != b {
			t.Fatalf("want [%s %s], got %v", a, b, got)
		}
	})
	t.Run("blanks and invalid skipped", func(t *testing.T) {
		got := parseWASessionTenants(getenvFor(" , not-a-uuid ," + a.String() + ", "))
		if len(got) != 1 || got[0] != a {
			t.Fatalf("want [%s], got %v", a, got)
		}
	})
	t.Run("duplicates collapsed", func(t *testing.T) {
		got := parseWASessionTenants(getenvFor(a.String() + "," + a.String()))
		if len(got) != 1 || got[0] != a {
			t.Fatalf("want single %s, got %v", a, got)
		}
	})
}

// --- managerSessionSender bridge -------------------------------------

func TestManagerSessionSender_StripsPlusPrefix(t *testing.T) {
	t.Parallel()
	disp := &fakeDispatcher{id: "wamid.42"}
	bridge := managerSessionSender{dispatch: disp}
	tenant := uuid.New()

	id, err := bridge.SendText(context.Background(), tenant, "+5511999990001", "oi")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if id != "wamid.42" {
		t.Fatalf("id = %q, want wamid.42", id)
	}
	if disp.to != "5511999990001" {
		t.Fatalf("dispatched to %q, want bare E.164 without '+'", disp.to)
	}
	if disp.tenant != tenant || disp.body != "oi" {
		t.Fatalf("tenant/body not threaded: %v %q", disp.tenant, disp.body)
	}
}

func TestManagerSessionSender_PropagatesError(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("boom")
	bridge := managerSessionSender{dispatch: &fakeDispatcher{err: sentinel}}
	if _, err := bridge.SendText(context.Background(), uuid.New(), "+551199", "x"); !errors.Is(err, sentinel) {
		t.Fatalf("want sentinel, got %v", err)
	}
}

func TestManagerSessionSender_SatisfiesPort(t *testing.T) {
	t.Parallel()
	var _ wasessionchan.SessionSender = managerSessionSender{}
}

// --- assembleWASessionAdapter ----------------------------------------

func TestAssembleWASessionAdapter_Success(t *testing.T) {
	t.Parallel()
	adapter, err := assembleWASessionAdapter(
		&fakeInboundChannel{},
		fakeSessionSender{},
		fakeFlag{on: true},
		fakeRate{},
		wasessionchan.DefaultConfig(),
		nil,
	)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if adapter == nil {
		t.Fatal("expected non-nil adapter")
	}
}

func TestAssembleWASessionAdapter_NilDepErrors(t *testing.T) {
	t.Parallel()
	_, err := assembleWASessionAdapter(
		&fakeInboundChannel{},
		nil, // missing sender
		fakeFlag{},
		fakeRate{},
		wasessionchan.DefaultConfig(),
		nil,
	)
	if !errors.Is(err, wasessionchan.ErrNilSender) {
		t.Fatalf("want ErrNilSender, got %v", err)
	}
}

// --- pumpWASessionInbound --------------------------------------------

func TestPumpWASessionInbound_DeliversInbound(t *testing.T) {
	t.Parallel()
	tenant := uuid.New()
	ts := time.Unix(1700000000, 0).UTC()
	ch := make(chan wasession.Event, 4)
	ch <- wasession.Event{
		Kind:     wasession.EventInbound,
		TenantID: tenant,
		Inbound: &wasession.InboundMessage{
			ExternalID: "wamid.1",
			SenderE164: "5511999990001",
			SenderName: "Ana",
			Body:       "olá",
			OccurredAt: ts,
			HasMedia:   true,
			FromMe:     false,
		},
	}
	// nil-Inbound inbound event must be skipped, not panic.
	ch <- wasession.Event{Kind: wasession.EventInbound, TenantID: tenant, Inbound: nil}
	// status + qr events must NOT reach the receiver.
	ch <- wasession.Event{Kind: wasession.EventStatus, TenantID: tenant, Status: &wasession.StatusChange{To: wasession.StatusConnected}}
	ch <- wasession.Event{Kind: wasession.EventQR, TenantID: tenant, QR: &wasession.QRCode{}}
	close(ch)

	rcv := &recordingReceiver{}
	pumpWASessionInbound(context.Background(), ch, rcv, nil)

	if len(rcv.msgs) != 1 {
		t.Fatalf("want exactly 1 delivered message, got %d", len(rcv.msgs))
	}
	got := rcv.msgs[0]
	if got.TenantID != tenant || got.MessageID != "wamid.1" || got.SenderPhone != "5511999990001" ||
		got.SenderName != "Ana" || got.Body != "olá" || !got.HasMedia || got.FromMe || !got.Timestamp.Equal(ts) {
		t.Fatalf("mapped message mismatch: %+v", got)
	}
}

func TestPumpWASessionInbound_StopsOnContextCancel(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	ch := make(chan wasession.Event) // empty + never closed
	done := make(chan struct{})
	go func() {
		pumpWASessionInbound(ctx, ch, &recordingReceiver{}, nil)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("pump did not return on context cancel")
	}
}

func TestPumpWASessionInbound_ReceiverErrorDoesNotStop(t *testing.T) {
	t.Parallel()
	tenant := uuid.New()
	ch := make(chan wasession.Event, 2)
	ch <- wasession.Event{Kind: wasession.EventInbound, TenantID: tenant, Inbound: &wasession.InboundMessage{ExternalID: "a"}}
	ch <- wasession.Event{Kind: wasession.EventInbound, TenantID: tenant, Inbound: &wasession.InboundMessage{ExternalID: "b"}}
	close(ch)

	rcv := &recordingReceiver{err: errors.New("downstream")}
	pumpWASessionInbound(context.Background(), ch, rcv, nil)
	if len(rcv.msgs) != 2 {
		t.Fatalf("pump should keep draining after a receiver error, got %d", len(rcv.msgs))
	}
}

// --- observeWAStatus (SIN-66260 Fase 5 ban observability) ------------

type countingStatusObserver struct {
	mu     sync.Mutex
	counts map[string]int
}

func (c *countingStatusObserver) WASessionStatusTransition(to string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.counts == nil {
		c.counts = map[string]int{}
	}
	c.counts[to]++
}

func (c *countingStatusObserver) get(to string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.counts[to]
}

// observeWAStatus must increment the metric on a terminal banned
// transition AND forward every event downstream unchanged. This is the
// regression test for the AC ban signal at the wiring seam: it fails
// against pre-Fase-5 code where the pump consumed manager.Events()
// directly with no metric tee.
func TestObserveWAStatus_RecordsBanAndForwardsAllEvents(t *testing.T) {
	t.Parallel()
	tenant := uuid.New()
	counter := &countingStatusObserver{}
	src := make(chan wasession.Event, 4)
	src <- wasession.Event{Kind: wasession.EventStatus, TenantID: tenant, Status: &wasession.StatusChange{To: wasession.StatusConnected}}
	src <- wasession.Event{Kind: wasession.EventInbound, TenantID: tenant, Inbound: &wasession.InboundMessage{ExternalID: "a"}}
	src <- wasession.Event{Kind: wasession.EventStatus, TenantID: tenant, Status: &wasession.StatusChange{To: wasession.StatusBanned}}
	src <- wasession.Event{Kind: wasession.EventQR, TenantID: tenant, QR: &wasession.QRCode{}}
	close(src)

	out := observeWAStatus(context.Background(), src, counter)

	var got []wasession.EventKind
	for ev := range out {
		got = append(got, ev.Kind)
	}

	if len(got) != 4 {
		t.Fatalf("tee dropped events: forwarded %d of 4 (%v)", len(got), got)
	}
	if counter.get("banned") != 1 {
		t.Errorf("banned transition not recorded: got %d, want 1", counter.get("banned"))
	}
	if counter.get("connected") != 1 {
		t.Errorf("connected transition not recorded: got %d, want 1", counter.get("connected"))
	}
	// QR and inbound carry no status — must not be miscounted.
	if counter.get("") != 0 {
		t.Errorf("non-status event miscounted as a transition: got %d", counter.get(""))
	}
}

// A nil observer makes observeWAStatus a transparent pass-through: it
// returns the SAME channel (no extra goroutine), so flag-off / metric-less
// boots are inert.
func TestObserveWAStatus_NilObserverIsPassThrough(t *testing.T) {
	t.Parallel()
	src := make(chan wasession.Event)
	out := observeWAStatus(context.Background(), src, nil)
	if out != (<-chan wasession.Event)(src) {
		t.Fatal("nil observer must return the source channel unwrapped (no goroutine)")
	}
}

// The relay goroutine must exit when the pump context is cancelled even
// though Manager.Events() is never closed, so shutdown does not leak it.
func TestObserveWAStatus_StopsOnContextCancel(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	src := make(chan wasession.Event) // never closed, mirrors Manager.Events()
	out := observeWAStatus(ctx, src, &countingStatusObserver{})
	cancel()
	select {
	case _, ok := <-out:
		if ok {
			t.Fatal("expected closed output, got a value")
		}
	case <-time.After(time.Second):
		t.Fatal("tee did not stop on context cancel")
	}
}

// --- providerRoutingOutbound -----------------------------------------

func TestProviderRoutingOutbound(t *testing.T) {
	t.Parallel()
	tenant := uuid.New()
	waMsg := inbox.OutboundMessage{TenantID: tenant, Channel: wasessionchan.Channel, ToExternalID: "+55", Body: "x"}

	t.Run("routes to session when flag on and channel whatsapp", func(t *testing.T) {
		primary := &recordingOutbound{tag: "primary"}
		session := &recordingOutbound{tag: "session"}
		r := providerRoutingOutbound{primary: primary, session: session, flag: fakeFlag{on: true}}
		id, err := r.SendMessage(context.Background(), waMsg)
		if err != nil || id != "session" {
			t.Fatalf("want session id, got id=%q err=%v", id, err)
		}
		if session.called != 1 || primary.called != 0 {
			t.Fatalf("routing wrong: session=%d primary=%d", session.called, primary.called)
		}
	})

	t.Run("delegates to primary when flag off", func(t *testing.T) {
		primary := &recordingOutbound{tag: "primary"}
		session := &recordingOutbound{tag: "session"}
		r := providerRoutingOutbound{primary: primary, session: session, flag: fakeFlag{on: false}}
		id, _ := r.SendMessage(context.Background(), waMsg)
		if id != "primary" || primary.called != 1 || session.called != 0 {
			t.Fatalf("want primary, got id=%q primary=%d session=%d", id, primary.called, session.called)
		}
	})

	t.Run("delegates to primary on flag error", func(t *testing.T) {
		primary := &recordingOutbound{tag: "primary"}
		session := &recordingOutbound{tag: "session"}
		r := providerRoutingOutbound{primary: primary, session: session, flag: fakeFlag{on: true, err: errors.New("flag")}}
		id, _ := r.SendMessage(context.Background(), waMsg)
		if id != "primary" || session.called != 0 {
			t.Fatalf("flag error must fall through to primary, got id=%q session=%d", id, session.called)
		}
	})

	t.Run("delegates to primary for non-whatsapp channel", func(t *testing.T) {
		primary := &recordingOutbound{tag: "primary"}
		session := &recordingOutbound{tag: "session"}
		r := providerRoutingOutbound{primary: primary, session: session, flag: fakeFlag{on: true}}
		other := inbox.OutboundMessage{TenantID: tenant, Channel: "instagram", Body: "x"}
		id, _ := r.SendMessage(context.Background(), other)
		if id != "primary" || session.called != 0 {
			t.Fatalf("non-whatsapp must use primary, got id=%q session=%d", id, session.called)
		}
	})

	t.Run("nil session falls through to primary", func(t *testing.T) {
		primary := &recordingOutbound{tag: "primary"}
		r := providerRoutingOutbound{primary: primary, session: nil, flag: fakeFlag{on: true}}
		id, _ := r.SendMessage(context.Background(), waMsg)
		if id != "primary" || primary.called != 1 {
			t.Fatalf("nil session must use primary, got id=%q", id)
		}
	})
}

// TestProviderRoutingOutbound_EmptyAllowlistFailsClosed wires the REAL
// EnvFeatureFlag (not a fake) with the global flag on but no tenant
// allowlist, and proves the outbound fan-out is closed (SIN-66276): an
// accidental empty FEATURE_WA_SESSION_TENANTS must NOT route the whole
// fleet's WhatsApp onto the unofficial session — every tenant falls
// through to the primary (official) channel.
func TestProviderRoutingOutbound_EmptyAllowlistFailsClosed(t *testing.T) {
	t.Parallel()
	flag := wasessionchan.NewEnvFeatureFlag(func(k string) string {
		if k == wasessionchan.EnvSessionEnabled {
			return "1" // globally on, but FEATURE_WA_SESSION_TENANTS unset
		}
		return ""
	})
	primary := &recordingOutbound{tag: "primary"}
	session := &recordingOutbound{tag: "session"}
	r := providerRoutingOutbound{primary: primary, session: session, flag: flag}

	for _, tenant := range []uuid.UUID{uuid.New(), uuid.New()} {
		id, err := r.SendMessage(context.Background(), inbox.OutboundMessage{
			TenantID: tenant, Channel: wasessionchan.Channel, ToExternalID: "+55", Body: "x",
		})
		if err != nil {
			t.Fatalf("SendMessage(%s): %v", tenant, err)
		}
		if id != "primary" {
			t.Fatalf("tenant %s routed to %q, want primary (fan-out must be closed)", tenant, id)
		}
	}
	if session.called != 0 {
		t.Fatalf("session channel invoked %d times on empty allowlist, want 0", session.called)
	}
	if primary.called != 2 {
		t.Fatalf("primary channel invoked %d times, want 2", primary.called)
	}
}

func TestWaSessionWiring_RouteOutbound(t *testing.T) {
	t.Parallel()
	tenant := uuid.New()
	session := &recordingOutbound{tag: "session"}
	primary := &recordingOutbound{tag: "primary"}
	w := &waSessionWiring{Session: session, flag: fakeFlag{on: true}}

	routed := w.RouteOutbound(primary)
	id, err := routed.SendMessage(context.Background(), inbox.OutboundMessage{
		TenantID: tenant, Channel: wasessionchan.Channel, Body: "hi",
	})
	if err != nil || id != "session" {
		t.Fatalf("RouteOutbound should route opted-in whatsapp to session, got id=%q err=%v", id, err)
	}
	if session.called != 1 {
		t.Fatalf("session not invoked: %d", session.called)
	}
}

// --- buildWASessionWiring env gating ---------------------------------

func TestBuildWASessionWiring_DisabledWhenFlagOff(t *testing.T) {
	t.Parallel()
	if got := buildWASessionWiring(context.Background(), func(string) string { return "" }); got != nil {
		t.Fatal("expected nil wiring when FEATURE_WA_SESSION_ENABLED unset")
	}
}

func TestBuildWASessionWiring_DisabledWhenAppDSNMissing(t *testing.T) {
	t.Parallel()
	got := buildWASessionWiring(context.Background(), func(k string) string {
		if k == wasessionchan.EnvSessionEnabled {
			return "1"
		}
		return ""
	})
	if got != nil {
		t.Fatal("expected nil wiring when DATABASE_URL unset")
	}
}

func TestBuildWASessionWiring_DisabledWhenSessionDSNMissing(t *testing.T) {
	t.Parallel()
	got := buildWASessionWiring(context.Background(), func(k string) string {
		switch k {
		case wasessionchan.EnvSessionEnabled:
			return "1"
		case pgDSNEnvForTest():
			return "postgres://x"
		}
		return ""
	})
	if got != nil {
		t.Fatal("expected nil wiring when WA_SESSION_DATABASE_URL unset")
	}
}

// TestBuildWASessionWiring_DisabledWhenTenantsEmpty proves the
// composition-root fail-closed guard (SIN-66276): with the global flag on
// and BOTH DSNs present, an empty FEATURE_WA_SESSION_TENANTS still refuses
// to mount the transport (returns nil) — and does so before dialing the
// pool, so the test needs no DB. This is the wire-level half of the
// defense-in-depth pair with EnvFeatureFlag.Enabled.
func TestBuildWASessionWiring_DisabledWhenTenantsEmpty(t *testing.T) {
	t.Parallel()
	got := buildWASessionWiring(context.Background(), func(k string) string {
		switch k {
		case wasessionchan.EnvSessionEnabled:
			return "1"
		case pgDSNEnvForTest():
			return "postgres://x"
		case envWASessionDSN:
			return "postgres://y"
		}
		return "" // FEATURE_WA_SESSION_TENANTS empty
	})
	if got != nil {
		t.Fatal("expected nil wiring when globally enabled but FEATURE_WA_SESSION_TENANTS empty (fail-closed)")
	}
}

// pgDSNEnvForTest returns the DATABASE_URL env key the wire reads, kept
// in a helper so the test does not duplicate the postgres package's
// constant literal.
func pgDSNEnvForTest() string { return "DATABASE_URL" }
