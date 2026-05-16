package instagram_test

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/adapter/channels/instagram"
)

// envelope helpers ----------------------------------------------------------

const (
	testAppSecret   = "test-secret"
	testVerifyToken = "test-verify"
)

func sign(body []byte) string {
	mac := hmac.New(sha256.New, []byte(testAppSecret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func buildEnvelope(igBusinessID string, ts int64, msgs ...map[string]any) []byte {
	entries := []map[string]any{{
		"id":        igBusinessID,
		"time":      ts,
		"messaging": msgs,
	}}
	env := map[string]any{
		"object": "instagram",
		"entry":  entries,
	}
	out, _ := json.Marshal(env)
	return out
}

func msgInbound(igsid, mid, text string, ts int64, attachments []map[string]any) map[string]any {
	m := map[string]any{
		"sender":    map[string]string{"id": igsid},
		"recipient": map[string]string{"id": "ignored"},
		"timestamp": ts,
		"message": map[string]any{
			"mid":  mid,
			"text": text,
		},
	}
	if len(attachments) > 0 {
		m["message"].(map[string]any)["attachments"] = attachments
	}
	return m
}

func newAdapter(t *testing.T, opts ...buildOption) (*instagram.Adapter, *deps) {
	t.Helper()
	d := &deps{
		inbox:    newFakeInbox(),
		resolver: newFakeResolver(),
		flag:     newFakeFlag(true),
		rate:     newFakeRateLimiter(0),
		clock:    newFakeClock(time.Unix(1700000000, 0).UTC()),
		media:    newFakeMediaPublisher(),
	}
	for _, o := range opts {
		o(d)
	}
	cfg := instagram.Config{
		AppSecret:      testAppSecret,
		VerifyToken:    testVerifyToken,
		RateMaxPerMin:  600,
		MaxBodyBytes:   1 << 20,
		PastWindow:     24 * time.Hour,
		FutureSkew:     time.Minute,
		DeliverTimeout: 2 * time.Second,
	}
	a, err := instagram.New(cfg, d.inbox, d.resolver, d.flag, d.rate,
		instagram.WithClock(d.clock),
		instagram.WithMediaScanPublisher(d.media),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return a, d
}

type deps struct {
	inbox    *fakeInbox
	resolver *fakeResolver
	flag     *fakeFlag
	rate     *fakeRateLimiter
	clock    *fakeClock
	media    *fakeMediaPublisher
}

type buildOption func(*deps)

func withRateLimit(limit int) buildOption {
	return func(d *deps) { d.rate = newFakeRateLimiter(limit) }
}

// tests ---------------------------------------------------------------------

func TestHandlePost_DeliversValidMessage(t *testing.T) {
	t.Parallel()
	a, d := newAdapter(t)
	tenantID := uuid.New()
	d.resolver.Register("igb-1", tenantID)
	now := d.clock.Now()
	ts := now.Unix()
	body := buildEnvelope("igb-1", ts, msgInbound("igsid-1", "mid-1", "hello", now.UnixMilli(), nil))

	resp := postSigned(t, a, body)
	if resp.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", resp.Code)
	}
	got := d.inbox.Persisted()
	if len(got) != 1 {
		t.Fatalf("persisted: got %d, want 1", len(got))
	}
	ev := got[0]
	if ev.TenantID != tenantID {
		t.Errorf("tenant: got %s, want %s", ev.TenantID, tenantID)
	}
	if ev.Channel != "instagram" {
		t.Errorf("channel: got %q, want %q", ev.Channel, "instagram")
	}
	if ev.ChannelExternalID != "mid-1" {
		t.Errorf("mid: got %q, want %q", ev.ChannelExternalID, "mid-1")
	}
	if ev.SenderExternalID != "igsid-1" {
		t.Errorf("sender: got %q, want %q", ev.SenderExternalID, "igsid-1")
	}
	if ev.Body != "hello" {
		t.Errorf("body: got %q, want %q", ev.Body, "hello")
	}
	if ev.OccurredAt.IsZero() {
		t.Errorf("occurred_at: got zero, want non-zero")
	}
}

func TestHandlePost_BadSignatureReturns401(t *testing.T) {
	t.Parallel()
	a, _ := newAdapter(t)
	body := buildEnvelope("igb-1", time.Now().Unix(), msgInbound("u", "m", "hi", time.Now().UnixMilli(), nil))
	req := httptest.NewRequest(http.MethodPost, "/webhooks/instagram", strings.NewReader(string(body)))
	req.Header.Set(instagram.SignatureHeader, "sha256=deadbeef")
	rr := httptest.NewRecorder()
	mux := http.NewServeMux()
	a.Register(mux)
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status: got %d, want 401", rr.Code)
	}
}

func TestHandlePost_MissingSignatureReturns401(t *testing.T) {
	t.Parallel()
	a, _ := newAdapter(t)
	body := buildEnvelope("igb-1", time.Now().Unix(), msgInbound("u", "m", "hi", time.Now().UnixMilli(), nil))
	req := httptest.NewRequest(http.MethodPost, "/webhooks/instagram", strings.NewReader(string(body)))
	// no signature header
	rr := httptest.NewRecorder()
	mux := http.NewServeMux()
	a.Register(mux)
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status: got %d, want 401", rr.Code)
	}
}

func TestHandlePost_MalformedJSONReturns200Drop(t *testing.T) {
	t.Parallel()
	a, d := newAdapter(t)
	body := []byte("not-json-at-all")
	resp := postSigned(t, a, body)
	if resp.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", resp.Code)
	}
	if d.inbox.Calls() != 0 {
		t.Errorf("inbox calls: got %d, want 0", d.inbox.Calls())
	}
}

func TestHandlePost_UnknownIGBusinessIDIsSilentDrop(t *testing.T) {
	t.Parallel()
	a, d := newAdapter(t)
	body := buildEnvelope("ghost", d.clock.Now().Unix(),
		msgInbound("u", "m", "hi", d.clock.Now().UnixMilli(), nil))
	resp := postSigned(t, a, body)
	if resp.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", resp.Code)
	}
	if d.inbox.Calls() != 0 {
		t.Errorf("inbox calls: got %d, want 0", d.inbox.Calls())
	}
}

func TestHandlePost_FeatureFlagOffSkipsDelivery(t *testing.T) {
	t.Parallel()
	a, d := newAdapter(t)
	tenantID := uuid.New()
	d.resolver.Register("igb-1", tenantID)
	d.flag.Set(tenantID, false)
	body := buildEnvelope("igb-1", d.clock.Now().Unix(),
		msgInbound("u", "m", "hi", d.clock.Now().UnixMilli(), nil))
	resp := postSigned(t, a, body)
	if resp.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", resp.Code)
	}
	if d.inbox.Calls() != 0 {
		t.Errorf("inbox calls: got %d, want 0", d.inbox.Calls())
	}
}

func TestHandlePost_RateLimitedSkipsDelivery(t *testing.T) {
	t.Parallel()
	a, d := newAdapter(t, withRateLimit(1))
	tenantID := uuid.New()
	d.resolver.Register("igb-1", tenantID)
	body1 := buildEnvelope("igb-1", d.clock.Now().Unix(),
		msgInbound("u", "m1", "hi", d.clock.Now().UnixMilli(), nil))
	body2 := buildEnvelope("igb-1", d.clock.Now().Unix(),
		msgInbound("u", "m2", "hi2", d.clock.Now().UnixMilli(), nil))
	postSigned(t, a, body1)
	resp := postSigned(t, a, body2)
	if resp.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", resp.Code)
	}
	if c := d.inbox.Calls(); c != 1 {
		t.Errorf("inbox calls: got %d, want 1 (second envelope rate-limited)", c)
	}
}

func TestHandlePost_DuplicateMIDReturnsSuccess(t *testing.T) {
	t.Parallel()
	a, d := newAdapter(t)
	tenantID := uuid.New()
	d.resolver.Register("igb-1", tenantID)
	body := buildEnvelope("igb-1", d.clock.Now().Unix(),
		msgInbound("u", "dup-mid", "hi", d.clock.Now().UnixMilli(), nil))

	postSigned(t, a, body)
	postSigned(t, a, body)

	if got := len(d.inbox.Persisted()); got != 1 {
		t.Errorf("persisted: got %d, want 1 (second was duplicate)", got)
	}
}

func TestHandlePost_EchoMessagesAreDropped(t *testing.T) {
	t.Parallel()
	a, d := newAdapter(t)
	tenantID := uuid.New()
	d.resolver.Register("igb-1", tenantID)
	echo := map[string]any{
		"sender":    map[string]string{"id": "us"},
		"recipient": map[string]string{"id": "them"},
		"timestamp": d.clock.Now().UnixMilli(),
		"message": map[string]any{
			"mid":     "out-1",
			"text":    "we said this",
			"is_echo": true,
		},
	}
	body := buildEnvelope("igb-1", d.clock.Now().Unix(), echo)
	postSigned(t, a, body)
	if d.inbox.Calls() != 0 {
		t.Errorf("inbox calls on echo: got %d, want 0", d.inbox.Calls())
	}
}

func TestHandlePost_AttachmentsTriggerMediaScan(t *testing.T) {
	t.Parallel()
	a, d := newAdapter(t)
	tenantID := uuid.New()
	d.resolver.Register("igb-1", tenantID)
	att := []map[string]any{
		{"type": "image", "payload": map[string]string{"url": "https://cdn.example/x.jpg"}},
		{"type": "video", "payload": map[string]string{"url": "https://cdn.example/x.mp4"}},
	}
	body := buildEnvelope("igb-1", d.clock.Now().Unix(),
		msgInbound("u", "mid-att", "", d.clock.Now().UnixMilli(), att))
	postSigned(t, a, body)
	calls := d.media.Calls()
	if len(calls) != 2 {
		t.Fatalf("media calls: got %d, want 2", len(calls))
	}
	if !strings.Contains(calls[0].Key, "image") {
		t.Errorf("first key missing image: %q", calls[0].Key)
	}
	if !strings.Contains(calls[1].Key, "video") {
		t.Errorf("second key missing video: %q", calls[1].Key)
	}
	got := d.inbox.Persisted()
	if len(got) != 1 || got[0].Body != "[image]" {
		t.Errorf("body: got %q, want [image]", got[0].Body)
	}
}

func TestHandlePost_TimestampOutsideWindowIsDropped(t *testing.T) {
	t.Parallel()
	a, d := newAdapter(t)
	tenantID := uuid.New()
	d.resolver.Register("igb-1", tenantID)
	// time entry is 2 days old; PastWindow is 24h, so this drops.
	old := d.clock.Now().Add(-48 * time.Hour).Unix()
	body := buildEnvelope("igb-1", old,
		msgInbound("u", "stale", "hi", d.clock.Now().UnixMilli(), nil))
	postSigned(t, a, body)
	if d.inbox.Calls() != 0 {
		t.Errorf("inbox calls on stale envelope: got %d, want 0", d.inbox.Calls())
	}
}

func TestHandlePost_MissingMIDIsSkipped(t *testing.T) {
	t.Parallel()
	a, d := newAdapter(t)
	tenantID := uuid.New()
	d.resolver.Register("igb-1", tenantID)
	body := buildEnvelope("igb-1", d.clock.Now().Unix(),
		msgInbound("u", "", "hi", d.clock.Now().UnixMilli(), nil))
	postSigned(t, a, body)
	if d.inbox.Calls() != 0 {
		t.Errorf("inbox calls on missing mid: got %d, want 0", d.inbox.Calls())
	}
}

func TestHandlePost_MissingSenderIsSkipped(t *testing.T) {
	t.Parallel()
	a, d := newAdapter(t)
	tenantID := uuid.New()
	d.resolver.Register("igb-1", tenantID)
	body := buildEnvelope("igb-1", d.clock.Now().Unix(),
		msgInbound("", "m", "hi", d.clock.Now().UnixMilli(), nil))
	postSigned(t, a, body)
	if d.inbox.Calls() != 0 {
		t.Errorf("inbox calls on missing sender: got %d, want 0", d.inbox.Calls())
	}
}

func TestHandlePost_RateLimiterErrorSkipsDelivery(t *testing.T) {
	t.Parallel()
	a, d := newAdapter(t)
	tenantID := uuid.New()
	d.resolver.Register("igb-1", tenantID)
	d.rate.FailWith(errInjected)
	body := buildEnvelope("igb-1", d.clock.Now().Unix(),
		msgInbound("u", "m", "hi", d.clock.Now().UnixMilli(), nil))
	postSigned(t, a, body)
	if d.inbox.Calls() != 0 {
		t.Errorf("inbox calls on rate-limit error: got %d, want 0", d.inbox.Calls())
	}
}

func TestHandlePost_FlagErrorSkipsDelivery(t *testing.T) {
	t.Parallel()
	a, d := newAdapter(t)
	tenantID := uuid.New()
	d.resolver.Register("igb-1", tenantID)
	d.flag.FailWith(errInjected)
	body := buildEnvelope("igb-1", d.clock.Now().Unix(),
		msgInbound("u", "m", "hi", d.clock.Now().UnixMilli(), nil))
	postSigned(t, a, body)
	if d.inbox.Calls() != 0 {
		t.Errorf("inbox calls on flag error: got %d, want 0", d.inbox.Calls())
	}
}

func TestHandlePost_InboxErrorIsLoggedNot500(t *testing.T) {
	t.Parallel()
	a, d := newAdapter(t)
	tenantID := uuid.New()
	d.resolver.Register("igb-1", tenantID)
	d.inbox.FailWith(errInjected)
	body := buildEnvelope("igb-1", d.clock.Now().Unix(),
		msgInbound("u", "m", "hi", d.clock.Now().UnixMilli(), nil))
	resp := postSigned(t, a, body)
	if resp.Code != http.StatusOK {
		t.Fatalf("status on inbox failure: got %d, want 200", resp.Code)
	}
}

func TestHandlePost_AttachmentsWithoutPublisherLogsButPersists(t *testing.T) {
	t.Parallel()
	d := &deps{
		inbox:    newFakeInbox(),
		resolver: newFakeResolver(),
		flag:     newFakeFlag(true),
		rate:     newFakeRateLimiter(0),
		clock:    newFakeClock(time.Unix(1700000000, 0).UTC()),
		media:    newFakeMediaPublisher(),
	}
	cfg := instagram.Config{
		AppSecret: testAppSecret, VerifyToken: testVerifyToken,
		RateMaxPerMin: 600, MaxBodyBytes: 1 << 20,
		PastWindow: 24 * time.Hour, FutureSkew: time.Minute, DeliverTimeout: time.Second,
	}
	a, err := instagram.New(cfg, d.inbox, d.resolver, d.flag, d.rate, instagram.WithClock(d.clock))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	tenantID := uuid.New()
	d.resolver.Register("igb-1", tenantID)
	att := []map[string]any{{"type": "image", "payload": map[string]string{"url": "u"}}}
	body := buildEnvelope("igb-1", d.clock.Now().Unix(),
		msgInbound("u", "m-att", "", d.clock.Now().UnixMilli(), att))
	postSigned(t, a, body)
	if got := len(d.inbox.Persisted()); got != 1 {
		t.Errorf("persisted: got %d, want 1", got)
	}
}

func TestHandlePost_MediaPublisherErrorContinues(t *testing.T) {
	t.Parallel()
	a, d := newAdapter(t)
	tenantID := uuid.New()
	d.resolver.Register("igb-1", tenantID)
	d.media.err = errInjected
	att := []map[string]any{
		{"type": "image", "payload": map[string]string{"url": "u1"}},
	}
	body := buildEnvelope("igb-1", d.clock.Now().Unix(),
		msgInbound("u", "m-att", "", d.clock.Now().UnixMilli(), att))
	resp := postSigned(t, a, body)
	if resp.Code != http.StatusOK {
		t.Errorf("status: got %d, want 200", resp.Code)
	}
	if got := len(d.inbox.Persisted()); got != 1 {
		t.Errorf("persisted: got %d, want 1", got)
	}
}

func TestHandlePost_EmptyMessagingIsNoOp(t *testing.T) {
	t.Parallel()
	a, d := newAdapter(t)
	body := buildEnvelope("igb-1", d.clock.Now().Unix())
	resp := postSigned(t, a, body)
	if resp.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", resp.Code)
	}
	if d.inbox.Calls() != 0 {
		t.Errorf("inbox calls: got %d, want 0", d.inbox.Calls())
	}
}

func TestHandlePost_MissingEntryIDIsSkipped(t *testing.T) {
	t.Parallel()
	a, d := newAdapter(t)
	body := buildEnvelope("", d.clock.Now().Unix(),
		msgInbound("u", "m", "hi", d.clock.Now().UnixMilli(), nil))
	postSigned(t, a, body)
	if d.inbox.Calls() != 0 {
		t.Errorf("inbox calls: got %d, want 0", d.inbox.Calls())
	}
}

func TestHandlePost_BodyTooLargeIsDropped(t *testing.T) {
	t.Parallel()
	cfg := instagram.Config{
		AppSecret: testAppSecret, VerifyToken: testVerifyToken,
		RateMaxPerMin: 600, MaxBodyBytes: 16, // smaller than typical envelope
		PastWindow: 24 * time.Hour, FutureSkew: time.Minute, DeliverTimeout: time.Second,
	}
	d := &deps{
		inbox: newFakeInbox(), resolver: newFakeResolver(),
		flag: newFakeFlag(true), rate: newFakeRateLimiter(0),
		clock: newFakeClock(time.Unix(1700000000, 0).UTC()), media: newFakeMediaPublisher(),
	}
	a, err := instagram.New(cfg, d.inbox, d.resolver, d.flag, d.rate, instagram.WithClock(d.clock))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	body := buildEnvelope("igb-1", d.clock.Now().Unix(),
		msgInbound("u", "m", "hi", d.clock.Now().UnixMilli(), nil))
	resp := postSigned(t, a, body)
	// MaxBytesReader returns 200 (drop, anti-enumeration).
	if resp.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", resp.Code)
	}
	if d.inbox.Calls() != 0 {
		t.Errorf("inbox calls: got %d, want 0", d.inbox.Calls())
	}
}

// postSigned builds a signed POST against the adapter and returns the recorder.
func postSigned(t *testing.T, a *instagram.Adapter, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/webhooks/instagram", strings.NewReader(string(body)))
	req.Header.Set(instagram.SignatureHeader, sign(body))
	req.Body = io.NopCloser(strings.NewReader(string(body)))
	rr := httptest.NewRecorder()
	mux := http.NewServeMux()
	a.Register(mux)
	mux.ServeHTTP(rr, req)
	return rr
}
