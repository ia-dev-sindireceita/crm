package slack

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/iam/mfa"
)

// Capturing test server: records every JSON body so assertions can
// confirm both the event tag and the user_id are in the payload.
type captureServer struct {
	mu     sync.Mutex
	bodies []string
	srv    *httptest.Server
}

func newCaptureServer(t *testing.T) *captureServer {
	t.Helper()
	c := &captureServer{}
	c.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		c.mu.Lock()
		c.bodies = append(c.bodies, string(body))
		c.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(c.srv.Close)
	return c
}

func (c *captureServer) Bodies() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, len(c.bodies))
	copy(out, c.bodies)
	return out
}

func TestNewMFAAlerter_PanicsOnNilNotifier(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic on nil notifier")
		}
	}()
	NewMFAAlerter(nil)
}

func TestMFAAlerter_AlertRecoveryUsed(t *testing.T) {
	cap := newCaptureServer(t)
	notifier := New(cap.srv.URL)
	alerter := NewMFAAlerter(notifier)
	uid := uuid.New()
	details := mfa.RecoveryUsedDetails{
		UserID:    uid,
		CodeIndex: 3,
		IP:        "203.0.113.5",
		UserAgent: "Mozilla/5.0 (X11; Linux x86_64)",
		Route:     "/m/2fa/verify",
	}
	if err := alerter.AlertRecoveryUsed(context.Background(), details); err != nil {
		t.Fatalf("AlertRecoveryUsed: %v", err)
	}
	bodies := cap.Bodies()
	if len(bodies) != 1 {
		t.Fatalf("bodies: got %d want 1", len(bodies))
	}
	body := bodies[0]
	if !strings.Contains(body, "master_recovery_used") {
		t.Errorf("body missing event tag: %q", body)
	}
	if !strings.Contains(body, uid.String()) {
		t.Errorf("body missing user_id: %q", body)
	}
	// Severity emoji distinguishes from regen alerts in Slack.
	if !strings.Contains(body, ":rotating_light:") {
		t.Errorf("body missing rotating_light emoji: %q", body)
	}
}

func TestMFAAlerter_AlertRecoveryUsed_IncludesContextFields(t *testing.T) {
	// CAVEAT-5 / SIN-62382: the on-call operator needs code_index +
	// ip + user_agent + route in the alert body so they don't have
	// to round-trip to the audit log.
	cap := newCaptureServer(t)
	notifier := New(cap.srv.URL)
	alerter := NewMFAAlerter(notifier)
	uid := uuid.New()
	details := mfa.RecoveryUsedDetails{
		UserID:    uid,
		CodeIndex: 7,
		IP:        "198.51.100.42",
		UserAgent: "curl/8.0.1",
		Route:     "/m/2fa/verify",
	}
	if err := alerter.AlertRecoveryUsed(context.Background(), details); err != nil {
		t.Fatalf("AlertRecoveryUsed: %v", err)
	}
	body := cap.Bodies()[0]
	for _, want := range []string{
		"code_index=7",
		`ip=[198.51.100.42]`,
		`user_agent=[curl/8.0.1]`,
		`route=[/m/2fa/verify]`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q: %q", want, body)
		}
	}
}

func TestMFAAlerter_AlertRecoveryUsed_EmptyFieldsRenderAsBlank(t *testing.T) {
	// A missing User-Agent header (or unparseable RemoteAddr) should
	// render as an obvious blank rather than disappear. Operators
	// reading the alert can then attribute the gap to header absence
	// rather than wondering whether the alert formatter dropped it.
	cap := newCaptureServer(t)
	notifier := New(cap.srv.URL)
	alerter := NewMFAAlerter(notifier)
	details := mfa.RecoveryUsedDetails{
		UserID:    uuid.New(),
		CodeIndex: 0,
		IP:        "",
		UserAgent: "",
		Route:     "",
	}
	if err := alerter.AlertRecoveryUsed(context.Background(), details); err != nil {
		t.Fatalf("AlertRecoveryUsed: %v", err)
	}
	body := cap.Bodies()[0]
	for _, want := range []string{
		`ip=[]`,
		`user_agent=[]`,
		`route=[]`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q: %q", want, body)
		}
	}
}

func TestMFAAlerter_AlertRecoveryUsed_QuotesPreserveSpacesInUserAgent(t *testing.T) {
	// Real user-agents contain spaces; Slack readers need to see
	// field boundaries.
	cap := newCaptureServer(t)
	notifier := New(cap.srv.URL)
	alerter := NewMFAAlerter(notifier)
	details := mfa.RecoveryUsedDetails{
		UserID:    uuid.New(),
		CodeIndex: 1,
		IP:        "203.0.113.5",
		UserAgent: "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7)",
		Route:     "/m/2fa/verify",
	}
	if err := alerter.AlertRecoveryUsed(context.Background(), details); err != nil {
		t.Fatalf("AlertRecoveryUsed: %v", err)
	}
	body := cap.Bodies()[0]
	if !strings.Contains(body, `user_agent=[Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7)]`) {
		t.Errorf("body missing bracketed user-agent with spaces: %q", body)
	}
}

func TestMFAAlerter_AlertRecoveryRegenerated(t *testing.T) {
	cap := newCaptureServer(t)
	notifier := New(cap.srv.URL)
	alerter := NewMFAAlerter(notifier)
	uid := uuid.New()
	details := mfa.RecoveryRegeneratedDetails{
		UserID:    uid,
		IP:        "203.0.113.9",
		UserAgent: "Mozilla/5.0",
		Route:     "/m/2fa/recovery/regenerate",
	}
	if err := alerter.AlertRecoveryRegenerated(context.Background(), details); err != nil {
		t.Fatalf("AlertRecoveryRegenerated: %v", err)
	}
	bodies := cap.Bodies()
	if len(bodies) != 1 {
		t.Fatalf("bodies: got %d want 1", len(bodies))
	}
	body := bodies[0]
	if !strings.Contains(body, "master_recovery_regenerated") {
		t.Errorf("body missing event tag: %q", body)
	}
	if !strings.Contains(body, uid.String()) {
		t.Errorf("body missing user_id: %q", body)
	}
	// Different emoji from used path so operators tell them apart at a glance.
	if !strings.Contains(body, ":arrows_counterclockwise:") {
		t.Errorf("body missing arrows_counterclockwise emoji: %q", body)
	}
	if strings.Contains(body, ":rotating_light:") {
		t.Errorf("regen body should NOT use rotating_light: %q", body)
	}
}

func TestMFAAlerter_AlertRecoveryRegenerated_IncludesContextFields(t *testing.T) {
	// CAVEAT-5 / SIN-62382: the regen alert carries actor + ip +
	// user_agent + route (no code_index — regenerate consumes
	// nothing). Operators use this to confirm the regen was driven
	// by the master, not by a hijacked session.
	cap := newCaptureServer(t)
	notifier := New(cap.srv.URL)
	alerter := NewMFAAlerter(notifier)
	uid := uuid.New()
	details := mfa.RecoveryRegeneratedDetails{
		UserID:    uid,
		IP:        "198.51.100.42",
		UserAgent: "curl/8.0.1",
		Route:     "/m/2fa/recovery/regenerate",
	}
	if err := alerter.AlertRecoveryRegenerated(context.Background(), details); err != nil {
		t.Fatalf("AlertRecoveryRegenerated: %v", err)
	}
	body := cap.Bodies()[0]
	for _, want := range []string{
		`ip=[198.51.100.42]`,
		`user_agent=[curl/8.0.1]`,
		`route=[/m/2fa/recovery/regenerate]`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q: %q", want, body)
		}
	}
	if strings.Contains(body, "code_index=") {
		t.Errorf("regen body should NOT include code_index: %q", body)
	}
}

// Empty webhook URL turns the underlying Notifier into a no-op; the
// adapter MUST surface that as a successful call (the iam.Service
// soft-fail path treats alert failures as non-fatal anyway, but a
// disabled webhook is the *expected* config in dev environments).
func TestMFAAlerter_EmptyWebhookIsNoop(t *testing.T) {
	alerter := NewMFAAlerter(New(""))
	uid := uuid.New()
	used := mfa.RecoveryUsedDetails{UserID: uid, CodeIndex: 0}
	regen := mfa.RecoveryRegeneratedDetails{UserID: uid}
	if err := alerter.AlertRecoveryUsed(context.Background(), used); err != nil {
		t.Errorf("AlertRecoveryUsed: %v", err)
	}
	if err := alerter.AlertRecoveryRegenerated(context.Background(), regen); err != nil {
		t.Errorf("AlertRecoveryRegenerated: %v", err)
	}
}
