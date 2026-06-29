// SIN-66305 (R3 / SIN-66292) — composition-root coverage for the
// auditWAStatus tee: it records ONE audit row per terminal transition
// (banned / disconnected), records nothing for non-terminal transitions, and
// forwards EVERY event downstream so the inbound pump's contract is
// unchanged. Mirrors the observeWAStatus tee tests (SIN-66260 Fase 5).
package main

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/iam/audit"
	"github.com/pericles-luz/crm/internal/wasession"
)

type recordedTransition struct {
	tenantID uuid.UUID
	evt      audit.SecurityEvent
	from     string
	to       string
	reason   string
}

type recordingTransitionAuditor struct {
	mu      sync.Mutex
	records []recordedTransition
	err     error
}

func (r *recordingTransitionAuditor) RecordTransition(_ context.Context, tenantID uuid.UUID, evt audit.SecurityEvent, from, to, reason string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.records = append(r.records, recordedTransition{tenantID, evt, from, to, reason})
	return r.err
}

func (r *recordingTransitionAuditor) snapshot() []recordedTransition {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]recordedTransition, len(r.records))
	copy(out, r.records)
	return out
}

func TestAuditWAStatus_RecordsTerminalTransitionsAndForwardsAll(t *testing.T) {
	t.Parallel()
	tenant := uuid.New()
	auditor := &recordingTransitionAuditor{}
	src := make(chan wasession.Event, 6)
	src <- wasession.Event{Kind: wasession.EventStatus, TenantID: tenant, Status: &wasession.StatusChange{From: wasession.StatusDisconnected, To: wasession.StatusConnected}}
	src <- wasession.Event{Kind: wasession.EventInbound, TenantID: tenant, Inbound: &wasession.InboundMessage{ExternalID: "m1", SenderE164: "5511999999999"}}
	src <- wasession.Event{Kind: wasession.EventStatus, TenantID: tenant, Status: &wasession.StatusChange{From: wasession.StatusConnected, To: wasession.StatusDisconnected, Reason: "socket drop"}}
	src <- wasession.Event{Kind: wasession.EventStatus, TenantID: tenant, Status: &wasession.StatusChange{From: wasession.StatusConnected, To: wasession.StatusBanned, Reason: "logged out"}}
	src <- wasession.Event{Kind: wasession.EventQR, TenantID: tenant, QR: &wasession.QRCode{}}
	close(src)

	out := auditWAStatus(context.Background(), src, auditor, nil)

	var forwarded int
	for range out {
		forwarded++
	}
	if forwarded != 5 {
		t.Fatalf("tee dropped events: forwarded %d of 5", forwarded)
	}

	recs := auditor.snapshot()
	if len(recs) != 2 {
		t.Fatalf("recorded %d transitions, want 2 (disconnected + banned only)", len(recs))
	}
	// Order preserved: disconnected first, then banned.
	if recs[0].evt != audit.SecurityEventWASessionDisconnected || recs[0].to != "disconnected" || recs[0].reason != "socket drop" {
		t.Errorf("first record = %+v, want disconnected/socket drop", recs[0])
	}
	if recs[1].evt != audit.SecurityEventWASessionBanned || recs[1].to != "banned" || recs[1].from != "connected" {
		t.Errorf("second record = %+v, want banned/from=connected", recs[1])
	}
	for _, r := range recs {
		if r.tenantID != tenant {
			t.Errorf("record tenant = %s, want %s", r.tenantID, tenant)
		}
	}
}

func TestAuditWAStatus_NilAuditorIsPassThrough(t *testing.T) {
	t.Parallel()
	src := make(chan wasession.Event)
	out := auditWAStatus(context.Background(), src, nil, nil)
	if out != (<-chan wasession.Event)(src) {
		t.Fatal("nil auditor must return the source channel unwrapped (no goroutine)")
	}
}

// A failing audit write must NOT drop the event — losing the inbound stream
// because the audit DB hiccuped is worse than a trail gap.
func TestAuditWAStatus_AuditErrorStillForwards(t *testing.T) {
	t.Parallel()
	tenant := uuid.New()
	auditor := &recordingTransitionAuditor{err: context.DeadlineExceeded}
	src := make(chan wasession.Event, 1)
	src <- wasession.Event{Kind: wasession.EventStatus, TenantID: tenant, Status: &wasession.StatusChange{To: wasession.StatusBanned}}
	close(src)

	out := auditWAStatus(context.Background(), src, auditor, nil)
	var forwarded int
	for range out {
		forwarded++
	}
	if forwarded != 1 {
		t.Fatalf("event dropped on audit error: forwarded %d of 1", forwarded)
	}
	if len(auditor.snapshot()) != 1 {
		t.Fatalf("expected the failing write to still be attempted once")
	}
}

func TestAuditWAStatus_StopsOnContextCancel(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	src := make(chan wasession.Event) // never closed, mirrors Manager.Events()
	out := auditWAStatus(ctx, src, &recordingTransitionAuditor{}, nil)
	cancel()
	select {
	case _, ok := <-out:
		if ok {
			t.Fatal("expected closed output after context cancel, got a value")
		}
	case <-time.After(time.Second):
		t.Fatal("tee did not stop on context cancel")
	}
}

func TestWATerminalAuditEvent(t *testing.T) {
	t.Parallel()
	cases := []struct {
		to     wasession.Status
		want   audit.SecurityEvent
		wantOK bool
	}{
		{wasession.StatusBanned, audit.SecurityEventWASessionBanned, true},
		{wasession.StatusDisconnected, audit.SecurityEventWASessionDisconnected, true},
		{wasession.StatusConnected, "", false},
		{wasession.StatusPairing, "", false},
		{wasession.StatusUnpaired, "", false},
	}
	for _, c := range cases {
		got, ok := waTerminalAuditEvent(c.to)
		if got != c.want || ok != c.wantOK {
			t.Errorf("waTerminalAuditEvent(%s) = (%q,%v), want (%q,%v)", c.to, got, ok, c.want, c.wantOK)
		}
	}
}
