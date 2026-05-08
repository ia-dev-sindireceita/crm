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
	if err := alerter.AlertRecoveryUsed(context.Background(), uid); err != nil {
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

func TestMFAAlerter_AlertRecoveryRegenerated(t *testing.T) {
	cap := newCaptureServer(t)
	notifier := New(cap.srv.URL)
	alerter := NewMFAAlerter(notifier)
	uid := uuid.New()
	if err := alerter.AlertRecoveryRegenerated(context.Background(), uid); err != nil {
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

// Empty webhook URL turns the underlying Notifier into a no-op; the
// adapter MUST surface that as a successful call (the iam.Service
// soft-fail path treats alert failures as non-fatal anyway, but a
// disabled webhook is the *expected* config in dev environments).
func TestMFAAlerter_EmptyWebhookIsNoop(t *testing.T) {
	alerter := NewMFAAlerter(New(""))
	uid := uuid.New()
	if err := alerter.AlertRecoveryUsed(context.Background(), uid); err != nil {
		t.Errorf("AlertRecoveryUsed: %v", err)
	}
	if err := alerter.AlertRecoveryRegenerated(context.Background(), uid); err != nil {
		t.Errorf("AlertRecoveryRegenerated: %v", err)
	}
}
