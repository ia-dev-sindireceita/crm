package main

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/iam/audit"
)

// recordingChannelSplit captures every WriteSecurity call so the adapter
// mapping (port method → SecurityEvent + Target) can be asserted without a
// database. writeErr lets a case exercise the best-effort failure path.
type recordingChannelSplit struct {
	events   []audit.SecurityAuditEvent
	writeErr error
}

func (r *recordingChannelSplit) WriteSecurity(_ context.Context, e audit.SecurityAuditEvent) error {
	r.events = append(r.events, e)
	return r.writeErr
}

func (r *recordingChannelSplit) WriteData(context.Context, audit.DataAuditEvent) error { return nil }

// TestChannelAccessAuditor_MapsEvents is the adapter table: each port
// method lands one audit_log_security row with the right event type, actor,
// tenant and Target payload.
func TestChannelAccessAuditor_MapsEvents(t *testing.T) {
	actor, tenant, channelID, user := uuid.New(), uuid.New(), uuid.New(), uuid.New()

	cases := []struct {
		name      string
		call      func(a *channelAccessAuditor)
		wantEvent audit.SecurityEvent
		wantKeys  map[string]any
	}{
		{
			name: "granted",
			call: func(a *channelAccessAuditor) {
				a.ChannelAccessGranted(context.Background(), actor, tenant, channelID, user)
			},
			wantEvent: audit.SecurityEventChannelAccessGranted,
			wantKeys:  map[string]any{"channel_id": channelID.String(), "user_id": user.String()},
		},
		{
			name: "revoked",
			call: func(a *channelAccessAuditor) {
				a.ChannelAccessRevoked(context.Background(), actor, tenant, channelID, user)
			},
			wantEvent: audit.SecurityEventChannelAccessRevoked,
			wantKeys:  map[string]any{"channel_id": channelID.String(), "user_id": user.String()},
		},
		{
			name: "restricted changed",
			call: func(a *channelAccessAuditor) {
				a.ChannelRestrictedChanged(context.Background(), actor, tenant, channelID, false, true)
			},
			wantEvent: audit.SecurityEventChannelRestrictedChanged,
			wantKeys:  map[string]any{"channel_id": channelID.String(), "from": false, "to": true},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sink := &recordingChannelSplit{}
			a := newChannelAccessAuditor(sink, nil)
			tc.call(a)

			if len(sink.events) != 1 {
				t.Fatalf("want 1 event, got %d", len(sink.events))
			}
			ev := sink.events[0]
			if ev.Event != tc.wantEvent {
				t.Errorf("event = %q, want %q", ev.Event, tc.wantEvent)
			}
			if !ev.Event.IsKnown() {
				t.Errorf("event %q is not in the controlled vocabulary", ev.Event)
			}
			if ev.ActorUserID != actor {
				t.Errorf("actor = %s, want %s", ev.ActorUserID, actor)
			}
			if ev.TenantID == nil || *ev.TenantID != tenant {
				t.Errorf("tenant = %v, want %s", ev.TenantID, tenant)
			}
			if ev.OccurredAt.IsZero() {
				t.Error("OccurredAt not stamped")
			}
			for k, want := range tc.wantKeys {
				if got := ev.Target[k]; got != want {
					t.Errorf("target[%q] = %v, want %v", k, got, want)
				}
			}
		})
	}
}

// TestChannelAccessAuditor_WriteErrorIsSwallowed pins the best-effort
// contract: a sink error is logged, never propagated (there is no return
// value), so a failed trail can never break the operator's request.
func TestChannelAccessAuditor_WriteErrorIsSwallowed(t *testing.T) {
	sink := &recordingChannelSplit{writeErr: errors.New("boom")}
	a := newChannelAccessAuditor(sink, nil)
	// Must not panic and must still attempt the write.
	a.ChannelAccessGranted(context.Background(), uuid.New(), uuid.New(), uuid.New(), uuid.New())
	if len(sink.events) != 1 {
		t.Fatalf("expected the failing write to be attempted once, got %d", len(sink.events))
	}
}

// TestChannelAccessAuditor_NilWriterPanics guards the required-dependency
// contract at construction.
func TestChannelAccessAuditor_NilWriterPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("newChannelAccessAuditor(nil) did not panic")
		}
	}()
	newChannelAccessAuditor(nil, nil)
}
