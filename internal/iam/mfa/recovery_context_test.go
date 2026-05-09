package mfa

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
)

// TestConsumeRecovery_ThreadsRequestContextToAlerter is the SIN-62382
// service-layer assertion: the IP / user-agent / route the HTTP
// boundary captured must reach AlertRecoveryUsed unchanged so the
// Slack body the on-call operator reads is faithful to what the
// caller saw.
func TestConsumeRecovery_ThreadsRequestContextToAlerter(t *testing.T) {
	uid := uuid.New()
	matchID := uuid.New()
	rows := []RecoveryCodeRecord{
		{ID: uuid.New(), Hash: "MATCH:OTHER0CODE"},
		{ID: matchID, Hash: "MATCH:ABCDE23456"},
		{ID: uuid.New(), Hash: "MATCH:THIRD0CODE"},
	}
	codes := &recoveryStoreScripted{listResult: rows}
	alerter := &recordingAlerter{}
	svc := newConsumeService(t, codes, &recordingAudit{}, alerter, hasherForCode{}, &fakeSeedRepository{})

	reqCtx := RequestContext{
		IP:        "203.0.113.5",
		UserAgent: "Mozilla/5.0 (X11; Linux)",
		Route:     "/m/2fa/verify",
	}
	if err := svc.ConsumeRecovery(context.Background(), uid, "ABCDE23456", reqCtx); err != nil {
		t.Fatalf("ConsumeRecovery: %v", err)
	}

	if alerter.calls != 1 {
		t.Fatalf("AlertRecoveryUsed calls: got %d want 1", alerter.calls)
	}
	got := alerter.lastUsed
	if got.UserID != uid {
		t.Errorf("UserID: got %v want %v", got.UserID, uid)
	}
	if got.CodeIndex != 1 {
		t.Errorf("CodeIndex: got %d want 1 (matched row was at position 1)", got.CodeIndex)
	}
	if got.IP != reqCtx.IP {
		t.Errorf("IP: got %q want %q", got.IP, reqCtx.IP)
	}
	if got.UserAgent != reqCtx.UserAgent {
		t.Errorf("UserAgent: got %q want %q", got.UserAgent, reqCtx.UserAgent)
	}
	if got.Route != reqCtx.Route {
		t.Errorf("Route: got %q want %q", got.Route, reqCtx.Route)
	}
}

// TestConsumeRecovery_RouteFlowsToAlertFailureSoftPath confirms that
// when AlertRecoveryUsed errors, the per-request route (not a
// hard-coded constant) is what reaches alertFailure → audit. This
// pins SIN-62382: the soft-fail audit row must reflect the actual
// route the consume handler served, since the alert and the audit
// row are the only ops-facing breadcrumbs left after the alert
// outage.
func TestConsumeRecovery_RouteFlowsToAlertFailureSoftPath(t *testing.T) {
	rows := []RecoveryCodeRecord{{ID: uuid.New(), Hash: "MATCH:ABCDE23456"}}
	codes := &recoveryStoreScripted{listResult: rows}
	audit := &capturingAudit{}
	alerter := &recordingAlerter{err: errors.New("slack 503")}
	svc := newConsumeService(t, codes, &recordingAudit{}, alerter, hasherForCode{}, &fakeSeedRepository{})
	// Swap audit to capturing one — newConsumeService already wired
	// the recordingAudit, but we need richer capture.
	svc.audit = audit

	reqCtx := RequestContext{Route: "/m/2fa/verify"}
	if err := svc.ConsumeRecovery(context.Background(), uuid.New(), "ABCDE23456", reqCtx); err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
	if audit.lastRoute != "/m/2fa/verify" {
		t.Errorf("LogMFARequired route: got %q want %q (must reflect reqCtx.Route on alert soft-fail)",
			audit.lastRoute, "/m/2fa/verify")
	}
}

// TestRegenerateRecovery_ThreadsRequestContextToAlerter is the
// SIN-62382 regenerate-path mirror of the consume assertion above.
func TestRegenerateRecovery_ThreadsRequestContextToAlerter(t *testing.T) {
	uid := uuid.New()
	store := &regenStore{}
	alerter := &regenAlerter{}
	svc := newRegenService(t, store, &regenAudit{}, alerter)

	reqCtx := RequestContext{
		IP:        "198.51.100.42",
		UserAgent: "curl/8.0.1",
		Route:     "/m/2fa/recovery/regenerate",
	}
	if _, err := svc.RegenerateRecovery(context.Background(), uid, reqCtx); err != nil {
		t.Fatalf("RegenerateRecovery: %v", err)
	}
	if alerter.regenCalls != 1 {
		t.Fatalf("AlertRecoveryRegenerated calls: got %d want 1", alerter.regenCalls)
	}
	got := alerter.lastRegened
	if got.UserID != uid {
		t.Errorf("UserID: got %v want %v", got.UserID, uid)
	}
	if got.IP != reqCtx.IP {
		t.Errorf("IP: got %q want %q", got.IP, reqCtx.IP)
	}
	if got.UserAgent != reqCtx.UserAgent {
		t.Errorf("UserAgent: got %q want %q", got.UserAgent, reqCtx.UserAgent)
	}
	if got.Route != reqCtx.Route {
		t.Errorf("Route: got %q want %q", got.Route, reqCtx.Route)
	}
}

// capturingAudit captures the most recent LogMFARequired call so the
// soft-fail-route assertion can confirm the right route flows
// through. Other audit methods are no-ops.
type capturingAudit struct {
	lastRoute  string
	lastReason string
}

func (a *capturingAudit) LogEnrolled(context.Context, uuid.UUID) error     { return nil }
func (a *capturingAudit) LogVerified(context.Context, uuid.UUID) error     { return nil }
func (a *capturingAudit) LogRecoveryUsed(context.Context, uuid.UUID) error { return nil }
func (a *capturingAudit) LogRecoveryRegenerated(context.Context, uuid.UUID) error {
	return nil
}
func (a *capturingAudit) LogMFARequired(_ context.Context, _ uuid.UUID, route, reason string) error {
	a.lastRoute = route
	a.lastReason = reason
	return nil
}
