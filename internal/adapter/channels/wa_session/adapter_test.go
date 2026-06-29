package wa_session

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/inbox"
)

var testTenant = uuid.MustParse("11111111-1111-1111-1111-111111111111")

func TestNew_RequiredDeps(t *testing.T) {
	in := &fakeInbound{}
	snd := &fakeSender{}
	flag := enabledFlag()
	rate := allowAllRate()

	tests := []struct {
		name    string
		in      inbox.InboundChannel
		sender  SessionSender
		flag    FeatureFlag
		rate    RateLimiter
		wantErr error
	}{
		{"ok", in, snd, flag, rate, nil},
		{"nil inbox", nil, snd, flag, rate, ErrNilInbound},
		{"nil sender", in, nil, flag, rate, ErrNilSender},
		{"nil flag", in, snd, nil, rate, ErrNilFlag},
		{"nil rate", in, snd, flag, nil, ErrNilRate},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a, err := New(tt.in, tt.sender, tt.flag, tt.rate)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("err = %v, want %v", err, tt.wantErr)
			}
			if tt.wantErr == nil && a == nil {
				t.Fatal("New returned nil adapter on success")
			}
		})
	}
}

func TestReceive(t *testing.T) {
	occurred := time.Date(2026, 6, 29, 12, 0, 0, 0, time.UTC)

	tests := []struct {
		name        string
		msg         SessionMessage
		flag        fakeFlag
		inErr       error
		wantErr     error
		wantCalls   int    // HandleInbound invocations
		wantBody    string // asserted only when wantCalls == 1
		wantSender  string
		wantAttach  bool
		wantExtitID string
	}{
		{
			name:        "text delivered",
			msg:         SessionMessage{TenantID: testTenant, MessageID: "m1", SenderPhone: "5511999990001", SenderName: "Ana", Body: "oi", Timestamp: occurred},
			flag:        enabledFlag(),
			wantCalls:   1,
			wantBody:    "oi",
			wantSender:  "+5511999990001",
			wantExtitID: "m1",
		},
		{
			name:       "jid sender normalised",
			msg:        SessionMessage{TenantID: testTenant, MessageID: "m2", SenderPhone: "5511999990002@s.whatsapp.net", Body: "hi"},
			flag:       enabledFlag(),
			wantCalls:  1,
			wantBody:   "hi",
			wantSender: "+5511999990002",
		},
		{
			name:       "media only gets placeholder",
			msg:        SessionMessage{TenantID: testTenant, MessageID: "m3", SenderPhone: "+5511999990003", HasMedia: true},
			flag:       enabledFlag(),
			wantCalls:  1,
			wantBody:   mediaPlaceholder,
			wantSender: "+5511999990003",
			wantAttach: true,
		},
		{
			name:      "self echo dropped",
			msg:       SessionMessage{TenantID: testTenant, MessageID: "m4", SenderPhone: "5511999990004", Body: "x", FromMe: true},
			flag:      enabledFlag(),
			wantCalls: 0,
		},
		{
			name:      "group dropped",
			msg:       SessionMessage{TenantID: testTenant, MessageID: "m5", SenderPhone: "5511999990005", Body: "x", IsGroup: true},
			flag:      enabledFlag(),
			wantCalls: 0,
		},
		{
			name:      "missing message id dropped",
			msg:       SessionMessage{TenantID: testTenant, MessageID: "  ", SenderPhone: "5511999990006", Body: "x"},
			flag:      enabledFlag(),
			wantCalls: 0,
		},
		{
			name:      "bad phone dropped",
			msg:       SessionMessage{TenantID: testTenant, MessageID: "m7", SenderPhone: "not-a-phone", Body: "x"},
			flag:      enabledFlag(),
			wantCalls: 0,
		},
		{
			name:      "no tenant dropped",
			msg:       SessionMessage{MessageID: "m8", SenderPhone: "5511999990008", Body: "x"},
			flag:      enabledFlag(),
			wantCalls: 0,
		},
		{
			name:      "flag off dropped",
			msg:       SessionMessage{TenantID: testTenant, MessageID: "m9", SenderPhone: "5511999990009", Body: "x"},
			flag:      disabledFlag(),
			wantCalls: 0,
		},
		{
			name:      "empty content dropped",
			msg:       SessionMessage{TenantID: testTenant, MessageID: "m10", SenderPhone: "5511999990010", Body: "   "},
			flag:      enabledFlag(),
			wantCalls: 0,
		},
		{
			name:      "flag error propagates",
			msg:       SessionMessage{TenantID: testTenant, MessageID: "m11", SenderPhone: "5511999990011", Body: "x"},
			flag:      fakeFlag{enabled: false, err: errBoom},
			wantErr:   errBoom,
			wantCalls: 0,
		},
		{
			name:      "duplicate is success",
			msg:       SessionMessage{TenantID: testTenant, MessageID: "m12", SenderPhone: "5511999990012", Body: "x"},
			flag:      enabledFlag(),
			inErr:     inbox.ErrInboundAlreadyProcessed,
			wantErr:   nil,
			wantCalls: 1,
		},
		{
			name:      "downstream error propagates",
			msg:       SessionMessage{TenantID: testTenant, MessageID: "m13", SenderPhone: "5511999990013", Body: "x"},
			flag:      enabledFlag(),
			inErr:     errBoom,
			wantErr:   errBoom,
			wantCalls: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := &fakeInbound{err: tt.inErr}
			a := mustAdapter(t, in, &fakeSender{}, tt.flag, allowAllRate())

			err := a.Receive(context.Background(), tt.msg)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("err = %v, want %v", err, tt.wantErr)
			}
			if in.calls != tt.wantCalls {
				t.Fatalf("HandleInbound calls = %d, want %d", in.calls, tt.wantCalls)
			}
			if tt.wantCalls == 1 && tt.inErr == nil {
				if in.last.Channel != Channel {
					t.Errorf("Channel = %q, want %q", in.last.Channel, Channel)
				}
				if tt.wantBody != "" && in.last.Body != tt.wantBody {
					t.Errorf("Body = %q, want %q", in.last.Body, tt.wantBody)
				}
				if tt.wantSender != "" && in.last.SenderExternalID != tt.wantSender {
					t.Errorf("SenderExternalID = %q, want %q", in.last.SenderExternalID, tt.wantSender)
				}
				if in.last.HasAttachments != tt.wantAttach {
					t.Errorf("HasAttachments = %v, want %v", in.last.HasAttachments, tt.wantAttach)
				}
				if tt.wantExtitID != "" && in.last.ChannelExternalID != tt.wantExtitID {
					t.Errorf("ChannelExternalID = %q, want %q", in.last.ChannelExternalID, tt.wantExtitID)
				}
			}
		})
	}
}

func TestReceive_PreservesTimestampAndDisplayName(t *testing.T) {
	occurred := time.Date(2026, 6, 29, 8, 30, 0, 0, time.UTC)
	in := &fakeInbound{}
	a := mustAdapter(t, in, &fakeSender{}, enabledFlag(), allowAllRate())

	err := a.Receive(context.Background(), SessionMessage{
		TenantID:    testTenant,
		MessageID:   "mx",
		SenderPhone: "5511999990099",
		SenderName:  "  Bruno  ",
		Body:        "olá",
		Timestamp:   occurred,
	})
	if err != nil {
		t.Fatalf("Receive: %v", err)
	}
	if !in.last.OccurredAt.Equal(occurred) {
		t.Errorf("OccurredAt = %v, want %v", in.last.OccurredAt, occurred)
	}
	if in.last.SenderDisplayName != "Bruno" {
		t.Errorf("SenderDisplayName = %q, want %q", in.last.SenderDisplayName, "Bruno")
	}
	if in.last.TenantID != testTenant {
		t.Errorf("TenantID = %v, want %v", in.last.TenantID, testTenant)
	}
}

func TestSendMessage(t *testing.T) {
	tests := []struct {
		name     string
		msg      inbox.OutboundMessage
		flag     fakeFlag
		rate     *fakeRate
		sender   *fakeSender
		wantID   string
		wantErr  error
		wantSent bool
		wantTo   string // asserted when wantSent
	}{
		{
			name:     "sends and returns id",
			msg:      inbox.OutboundMessage{TenantID: testTenant, ToExternalID: "+5511999990001", Body: "oi"},
			flag:     enabledFlag(),
			rate:     allowAllRate(),
			sender:   &fakeSender{id: "wamid.out.1"},
			wantID:   "wamid.out.1",
			wantSent: true,
			wantTo:   "+5511999990001",
		},
		{
			name:     "bare digits recipient normalised",
			msg:      inbox.OutboundMessage{TenantID: testTenant, ToExternalID: "5511999990002", Body: "oi"},
			flag:     enabledFlag(),
			rate:     allowAllRate(),
			sender:   &fakeSender{id: "wamid.out.2"},
			wantID:   "wamid.out.2",
			wantSent: true,
			wantTo:   "+5511999990002",
		},
		{
			name:    "nil tenant rejected",
			msg:     inbox.OutboundMessage{ToExternalID: "+5511999990003", Body: "oi"},
			flag:    enabledFlag(),
			rate:    allowAllRate(),
			sender:  &fakeSender{},
			wantErr: ErrInvalidTenant,
		},
		{
			name:    "bad recipient rejected",
			msg:     inbox.OutboundMessage{TenantID: testTenant, ToExternalID: "nope", Body: "oi"},
			flag:    enabledFlag(),
			rate:    allowAllRate(),
			sender:  &fakeSender{},
			wantErr: ErrInvalidRecipient,
		},
		{
			name:    "empty body rejected",
			msg:     inbox.OutboundMessage{TenantID: testTenant, ToExternalID: "+5511999990004", Body: "   "},
			flag:    enabledFlag(),
			rate:    allowAllRate(),
			sender:  &fakeSender{},
			wantErr: ErrEmptyBody,
		},
		{
			name:    "disabled tenant rejected",
			msg:     inbox.OutboundMessage{TenantID: testTenant, ToExternalID: "+5511999990005", Body: "oi"},
			flag:    disabledFlag(),
			rate:    allowAllRate(),
			sender:  &fakeSender{},
			wantErr: ErrChannelDisabled,
		},
		{
			name:    "flag error propagates",
			msg:     inbox.OutboundMessage{TenantID: testTenant, ToExternalID: "+5511999990006", Body: "oi"},
			flag:    fakeFlag{enabled: false, err: errBoom},
			rate:    allowAllRate(),
			sender:  &fakeSender{},
			wantErr: errBoom,
		},
		{
			name:    "rate limited rejected",
			msg:     inbox.OutboundMessage{TenantID: testTenant, ToExternalID: "+5511999990007", Body: "oi"},
			flag:    enabledFlag(),
			rate:    &fakeRate{allow: false},
			sender:  &fakeSender{},
			wantErr: ErrRateLimited,
		},
		{
			name:    "rate error propagates",
			msg:     inbox.OutboundMessage{TenantID: testTenant, ToExternalID: "+5511999990008", Body: "oi"},
			flag:    enabledFlag(),
			rate:    &fakeRate{err: errBoom},
			sender:  &fakeSender{},
			wantErr: errBoom,
		},
		{
			name:     "sender error propagates",
			msg:      inbox.OutboundMessage{TenantID: testTenant, ToExternalID: "+5511999990009", Body: "oi"},
			flag:     enabledFlag(),
			rate:     allowAllRate(),
			sender:   &fakeSender{err: errBoom},
			wantErr:  errBoom,
			wantSent: true,
			wantTo:   "+5511999990009",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := mustAdapter(t, &fakeInbound{}, tt.sender, tt.flag, tt.rate)
			id, err := a.SendMessage(context.Background(), tt.msg)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("err = %v, want %v", err, tt.wantErr)
			}
			if id != tt.wantID {
				t.Errorf("id = %q, want %q", id, tt.wantID)
			}
			sent := tt.sender.calls == 1
			if sent != tt.wantSent {
				t.Fatalf("sender called = %v, want %v", sent, tt.wantSent)
			}
			if tt.wantSent {
				if tt.sender.to != tt.wantTo {
					t.Errorf("sender.to = %q, want %q", tt.sender.to, tt.wantTo)
				}
				if tt.sender.tenantID != testTenant {
					t.Errorf("sender.tenantID = %v, want %v", tt.sender.tenantID, testTenant)
				}
				if tt.sender.body != "oi" {
					t.Errorf("sender.body = %q, want %q", tt.sender.body, "oi")
				}
			}
		})
	}
}

func TestSendMessage_RateKeyIsPerTenant(t *testing.T) {
	rate := allowAllRate()
	a := mustAdapter(t, &fakeInbound{}, &fakeSender{id: "x"}, enabledFlag(), rate)
	if _, err := a.SendMessage(context.Background(), inbox.OutboundMessage{
		TenantID: testTenant, ToExternalID: "+5511999990001", Body: "oi",
	}); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	want := "wa_session:out:" + testTenant.String()
	if rate.lastKey != want {
		t.Errorf("rate key = %q, want %q", rate.lastKey, want)
	}
}

func TestWithConfig_OverridesAndKeepsDefaults(t *testing.T) {
	a := mustAdapter(t, &fakeInbound{}, &fakeSender{}, enabledFlag(), allowAllRate())
	// default
	if a.cfg.RateMaxPerMin != defaultRateMaxPerMinute {
		t.Fatalf("default RateMaxPerMin = %d, want %d", a.cfg.RateMaxPerMin, defaultRateMaxPerMinute)
	}
	b, err := New(&fakeInbound{}, &fakeSender{}, enabledFlag(), allowAllRate(),
		WithConfig(Config{RateMaxPerMin: 5}), WithLogger(nil))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if b.cfg.RateMaxPerMin != 5 {
		t.Errorf("override RateMaxPerMin = %d, want 5", b.cfg.RateMaxPerMin)
	}
	// zero-valued fields in the override must not clobber defaults
	if b.cfg.DeliverTimeout != defaultDeliverTimeout {
		t.Errorf("DeliverTimeout = %v, want %v", b.cfg.DeliverTimeout, defaultDeliverTimeout)
	}
}

var errBoom = errors.New("boom")

func mustAdapter(t *testing.T, in inbox.InboundChannel, snd SessionSender, flag FeatureFlag, rate RateLimiter) *Adapter {
	t.Helper()
	a, err := New(in, snd, flag, rate)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return a
}
