package messenger_test

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/adapter/channels/messenger"
	"github.com/pericles-luz/crm/internal/inbox"
)

const (
	testAppSecret   = "test-app-secret"
	testVerifyToken = "verify-token"
	testPageID      = "page-100"
	testPSID        = "PSID-abc"
)

// fixedNow is the wall-clock instant every handler test pins via
// WithClock; the timestamp window math is anchored to it.
var fixedNow = time.Date(2026, 5, 16, 12, 0, 0, 0, time.UTC)

func sign(t *testing.T, body []byte) string {
	t.Helper()
	mac := hmac.New(sha256.New, []byte(testAppSecret))
	_, _ = mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func newAdapter(t *testing.T, in *fakeInbox, r *fakeResolver, f *fakeFlag, c *fakeClock) *messenger.Adapter {
	t.Helper()
	cfg := messenger.Config{
		AppSecret:      testAppSecret,
		VerifyToken:    testVerifyToken,
		MaxBodyBytes:   1 << 20,
		PastWindow:     24 * time.Hour,
		FutureSkew:     time.Minute,
		DeliverTimeout: time.Second,
	}
	a, err := messenger.New(cfg, in, r, f, messenger.WithClock(c))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return a
}

func validEnvelope(t *testing.T, pageID, mid, psid, text string, tsMs int64) []byte {
	t.Helper()
	payload := map[string]any{
		"object": "page",
		"entry": []any{
			map[string]any{
				"id":   pageID,
				"time": tsMs,
				"messaging": []any{
					map[string]any{
						"sender":    map[string]any{"id": psid},
						"recipient": map[string]any{"id": pageID},
						"timestamp": tsMs,
						"message": map[string]any{
							"mid":  mid,
							"text": text,
						},
					},
				},
			},
		},
	}
	b, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}
	return b
}

func doPost(t *testing.T, a *messenger.Adapter, body []byte, headerSig string) *httptest.ResponseRecorder {
	t.Helper()
	mux := http.NewServeMux()
	a.Register(mux)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/webhooks/messenger", bytes.NewReader(body))
	if headerSig != "" {
		req.Header.Set(messenger.SignatureHeader, headerSig)
	}
	mux.ServeHTTP(rec, req)
	return rec
}

func TestHandlePost_DeliversValidMessage(t *testing.T) {
	t.Parallel()
	in := newFakeInbox()
	r := newFakeResolver()
	tenant := uuid.New()
	r.Register(testPageID, tenant)
	f := newFakeFlag(false)
	f.Set(tenant, true)
	a := newAdapter(t, in, r, f, newFakeClock(fixedNow))

	tsMs := fixedNow.Add(-10 * time.Second).UnixMilli()
	body := validEnvelope(t, testPageID, "mid-001", testPSID, "olá", tsMs)
	rec := doPost(t, a, body, sign(t, body))

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	got := in.Persisted()
	if len(got) != 1 {
		t.Fatalf("expected 1 persisted, got %d", len(got))
	}
	ev := got[0]
	if ev.TenantID != tenant {
		t.Errorf("tenant: got %s want %s", ev.TenantID, tenant)
	}
	if ev.Channel != messenger.Channel {
		t.Errorf("channel: got %q want %q", ev.Channel, messenger.Channel)
	}
	if ev.ChannelExternalID != "mid-001" {
		t.Errorf("mid: got %q", ev.ChannelExternalID)
	}
	if ev.SenderExternalID != testPSID {
		t.Errorf("psid: got %q", ev.SenderExternalID)
	}
	if ev.Body != "olá" {
		t.Errorf("body: got %q", ev.Body)
	}
	if !ev.OccurredAt.Equal(time.UnixMilli(tsMs).UTC()) {
		t.Errorf("occurredAt: got %s want %s", ev.OccurredAt, time.UnixMilli(tsMs).UTC())
	}
}

func TestHandlePost_RejectsBadSignature(t *testing.T) {
	t.Parallel()
	in := newFakeInbox()
	r := newFakeResolver()
	r.Register(testPageID, uuid.New())
	a := newAdapter(t, in, r, newFakeFlag(true), newFakeClock(fixedNow))

	body := validEnvelope(t, testPageID, "mid-001", testPSID, "hi", fixedNow.UnixMilli())
	rec := doPost(t, a, body, "sha256=deadbeef")

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 on bad signature, got %d", rec.Code)
	}
	if in.CallCount() != 0 {
		t.Fatalf("inbox should not be called: calls=%d", in.CallCount())
	}
}

func TestHandlePost_MissingSignatureIs401(t *testing.T) {
	t.Parallel()
	in := newFakeInbox()
	a := newAdapter(t, in, newFakeResolver(), newFakeFlag(true), newFakeClock(fixedNow))
	body := validEnvelope(t, testPageID, "mid", testPSID, "hi", fixedNow.UnixMilli())
	rec := doPost(t, a, body, "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rec.Code)
	}
}

func TestHandlePost_DropsUnknownObject(t *testing.T) {
	t.Parallel()
	in := newFakeInbox()
	r := newFakeResolver()
	r.Register(testPageID, uuid.New())
	a := newAdapter(t, in, r, newFakeFlag(true), newFakeClock(fixedNow))

	body := []byte(`{"object":"instagram","entry":[]}`)
	rec := doPost(t, a, body, sign(t, body))

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if in.CallCount() != 0 {
		t.Fatalf("inbox should not be called: calls=%d", in.CallCount())
	}
}

func TestHandlePost_DropsParseError(t *testing.T) {
	t.Parallel()
	in := newFakeInbox()
	a := newAdapter(t, in, newFakeResolver(), newFakeFlag(true), newFakeClock(fixedNow))
	body := []byte(`{not json`)
	rec := doPost(t, a, body, sign(t, body))
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 on parse error, got %d", rec.Code)
	}
	if in.CallCount() != 0 {
		t.Fatalf("inbox should not be called")
	}
}

func TestHandlePost_DropsUnknownPage(t *testing.T) {
	t.Parallel()
	in := newFakeInbox()
	a := newAdapter(t, in, newFakeResolver(), newFakeFlag(true), newFakeClock(fixedNow))
	body := validEnvelope(t, "unknown-page", "mid-2", testPSID, "x", fixedNow.UnixMilli())
	rec := doPost(t, a, body, sign(t, body))
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if in.CallCount() != 0 {
		t.Fatalf("inbox should not be called on unknown page")
	}
}

func TestHandlePost_DropsWhenFlagOff(t *testing.T) {
	t.Parallel()
	in := newFakeInbox()
	r := newFakeResolver()
	tenant := uuid.New()
	r.Register(testPageID, tenant)
	f := newFakeFlag(false)
	f.Set(tenant, false)
	a := newAdapter(t, in, r, f, newFakeClock(fixedNow))

	body := validEnvelope(t, testPageID, "mid-3", testPSID, "y", fixedNow.UnixMilli())
	rec := doPost(t, a, body, sign(t, body))
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d", rec.Code)
	}
	if in.CallCount() != 0 {
		t.Fatalf("inbox should not be called when flag is off")
	}
}

func TestHandlePost_FlagErrorIsSilentDrop(t *testing.T) {
	t.Parallel()
	in := newFakeInbox()
	r := newFakeResolver()
	tenant := uuid.New()
	r.Register(testPageID, tenant)
	f := newFakeFlag(true)
	f.FailWith(errInjected)
	a := newAdapter(t, in, r, f, newFakeClock(fixedNow))

	body := validEnvelope(t, testPageID, "mid-4", testPSID, "z", fixedNow.UnixMilli())
	rec := doPost(t, a, body, sign(t, body))
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d", rec.Code)
	}
	if in.CallCount() != 0 {
		t.Fatalf("inbox should not be called on flag error")
	}
}

func TestHandlePost_DedupOnSameMID(t *testing.T) {
	t.Parallel()
	in := newFakeInbox()
	r := newFakeResolver()
	tenant := uuid.New()
	r.Register(testPageID, tenant)
	a := newAdapter(t, in, r, newFakeFlag(true), newFakeClock(fixedNow))

	body := validEnvelope(t, testPageID, "mid-dedup", testPSID, "ola", fixedNow.UnixMilli())

	rec1 := doPost(t, a, body, sign(t, body))
	rec2 := doPost(t, a, body, sign(t, body))
	if rec1.Code != http.StatusOK || rec2.Code != http.StatusOK {
		t.Fatalf("both should be 200, got %d %d", rec1.Code, rec2.Code)
	}
	if got := in.CallCount(); got != 2 {
		t.Fatalf("expected 2 calls (one ack, one dedup), got %d", got)
	}
	if got := len(in.Persisted()); got != 1 {
		t.Fatalf("expected 1 persisted (idempotency), got %d", got)
	}
}

func TestHandlePost_TimestampOutsideWindowDropped(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		offset time.Duration
	}{
		{"too_old", -48 * time.Hour},
		{"too_future", 2 * time.Minute},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			in := newFakeInbox()
			r := newFakeResolver()
			r.Register(testPageID, uuid.New())
			a := newAdapter(t, in, r, newFakeFlag(true), newFakeClock(fixedNow))

			tsMs := fixedNow.Add(tc.offset).UnixMilli()
			body := validEnvelope(t, testPageID, "mid", testPSID, "x", tsMs)
			rec := doPost(t, a, body, sign(t, body))
			if rec.Code != http.StatusOK {
				t.Fatalf("got %d", rec.Code)
			}
			if in.CallCount() != 0 {
				t.Fatalf("inbox should not be called outside window")
			}
		})
	}
}

func TestHandlePost_AttachmentBodyPlaceholder(t *testing.T) {
	t.Parallel()
	in := newFakeInbox()
	r := newFakeResolver()
	tenant := uuid.New()
	r.Register(testPageID, tenant)
	a := newAdapter(t, in, r, newFakeFlag(true), newFakeClock(fixedNow))

	payload := map[string]any{
		"object": "page",
		"entry": []any{
			map[string]any{
				"id":   testPageID,
				"time": fixedNow.UnixMilli(),
				"messaging": []any{
					map[string]any{
						"sender":    map[string]any{"id": testPSID},
						"recipient": map[string]any{"id": testPageID},
						"timestamp": fixedNow.UnixMilli(),
						"message": map[string]any{
							"mid": "mid-img",
							"attachments": []any{
								map[string]any{"type": "image"},
							},
						},
					},
				},
			},
		},
	}
	body, _ := json.Marshal(payload)
	rec := doPost(t, a, body, sign(t, body))
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d", rec.Code)
	}
	got := in.Persisted()
	if len(got) != 1 {
		t.Fatalf("expected 1 persisted, got %d", len(got))
	}
	if got[0].Body != "[image]" {
		t.Errorf("body: got %q want %q", got[0].Body, "[image]")
	}
}

func TestHandlePost_MissingMIDSilentlyDropped(t *testing.T) {
	t.Parallel()
	in := newFakeInbox()
	r := newFakeResolver()
	tenant := uuid.New()
	r.Register(testPageID, tenant)
	a := newAdapter(t, in, r, newFakeFlag(true), newFakeClock(fixedNow))

	// Echo / message_reads envelope: no mid, just sender + timestamp.
	payload := map[string]any{
		"object": "page",
		"entry": []any{
			map[string]any{
				"id":   testPageID,
				"time": fixedNow.UnixMilli(),
				"messaging": []any{
					map[string]any{
						"sender":    map[string]any{"id": testPSID},
						"recipient": map[string]any{"id": testPageID},
						"timestamp": fixedNow.UnixMilli(),
						"message":   map[string]any{},
					},
				},
			},
		},
	}
	body, _ := json.Marshal(payload)
	rec := doPost(t, a, body, sign(t, body))
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d", rec.Code)
	}
	if in.CallCount() != 0 {
		t.Fatalf("inbox should not be called when mid is missing")
	}
}

func TestHandlePost_MissingSenderPSIDDropped(t *testing.T) {
	t.Parallel()
	in := newFakeInbox()
	r := newFakeResolver()
	tenant := uuid.New()
	r.Register(testPageID, tenant)
	a := newAdapter(t, in, r, newFakeFlag(true), newFakeClock(fixedNow))

	payload := map[string]any{
		"object": "page",
		"entry": []any{
			map[string]any{
				"id":   testPageID,
				"time": fixedNow.UnixMilli(),
				"messaging": []any{
					map[string]any{
						"sender":    map[string]any{"id": ""},
						"recipient": map[string]any{"id": testPageID},
						"timestamp": fixedNow.UnixMilli(),
						"message": map[string]any{
							"mid":  "mid-no-sender",
							"text": "x",
						},
					},
				},
			},
		},
	}
	body, _ := json.Marshal(payload)
	rec := doPost(t, a, body, sign(t, body))
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d", rec.Code)
	}
	if in.CallCount() != 0 {
		t.Fatalf("inbox should not be called when sender psid is empty")
	}
}

func TestHandlePost_DeliverErrorAcksMeta(t *testing.T) {
	t.Parallel()
	in := newFakeInbox()
	in.FailWith(errInjected)
	r := newFakeResolver()
	tenant := uuid.New()
	r.Register(testPageID, tenant)
	a := newAdapter(t, in, r, newFakeFlag(true), newFakeClock(fixedNow))

	body := validEnvelope(t, testPageID, "mid-err", testPSID, "x", fixedNow.UnixMilli())
	rec := doPost(t, a, body, sign(t, body))
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 ack regardless of downstream error, got %d", rec.Code)
	}
}

func TestHandlePost_MissingPageIDEntryDropped(t *testing.T) {
	t.Parallel()
	in := newFakeInbox()
	a := newAdapter(t, in, newFakeResolver(), newFakeFlag(true), newFakeClock(fixedNow))

	payload := map[string]any{
		"object": "page",
		"entry": []any{
			map[string]any{
				"id":        "",
				"time":      fixedNow.UnixMilli(),
				"messaging": []any{},
			},
		},
	}
	body, _ := json.Marshal(payload)
	rec := doPost(t, a, body, sign(t, body))
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d", rec.Code)
	}
	if in.CallCount() != 0 {
		t.Fatalf("inbox should not be called")
	}
}

func TestHandlePost_OversizedBodyDropped(t *testing.T) {
	t.Parallel()
	in := newFakeInbox()
	cfg := messenger.Config{
		AppSecret:      testAppSecret,
		VerifyToken:    testVerifyToken,
		MaxBodyBytes:   16,
		PastWindow:     time.Hour,
		FutureSkew:     time.Minute,
		DeliverTimeout: time.Second,
	}
	a, err := messenger.New(cfg, in, newFakeResolver(), newFakeFlag(true), messenger.WithClock(newFakeClock(fixedNow)))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	mux := http.NewServeMux()
	a.Register(mux)

	body := bytes.Repeat([]byte("A"), 1024)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/webhooks/messenger", bytes.NewReader(body))
	req.Header.Set(messenger.SignatureHeader, sign(t, body))
	mux.ServeHTTP(rec, req)

	// MaxBytesReader hijacks the request: io.ReadAll returns an error,
	// which the handler maps to a 200 ack (anti-enumeration). The
	// underlying transport may have already sent a 413 in some Go
	// versions; either way the inbox MUST NOT see the payload.
	if rec.Code != http.StatusOK && rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("got %d", rec.Code)
	}
	if in.CallCount() != 0 {
		t.Fatalf("inbox should not be called on oversized body")
	}
}

func TestHandleChallenge_EchoesChallenge(t *testing.T) {
	t.Parallel()
	a := newAdapter(t, newFakeInbox(), newFakeResolver(), newFakeFlag(true), newFakeClock(fixedNow))
	mux := http.NewServeMux()
	a.Register(mux)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet,
		"/webhooks/messenger?hub.mode=subscribe&hub.verify_token="+testVerifyToken+"&hub.challenge=PING",
		nil)
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	out, _ := io.ReadAll(rec.Body)
	if !bytes.Equal(out, []byte("PING")) {
		t.Errorf("body: got %q want %q", out, "PING")
	}
}

func TestHandleChallenge_RejectsBadToken(t *testing.T) {
	t.Parallel()
	a := newAdapter(t, newFakeInbox(), newFakeResolver(), newFakeFlag(true), newFakeClock(fixedNow))
	mux := http.NewServeMux()
	a.Register(mux)

	cases := []struct {
		name string
		path string
	}{
		{"wrong_token", "/webhooks/messenger?hub.mode=subscribe&hub.verify_token=wrong&hub.challenge=PING"},
		{"wrong_mode", "/webhooks/messenger?hub.mode=other&hub.verify_token=" + testVerifyToken + "&hub.challenge=PING"},
		{"missing_token", "/webhooks/messenger?hub.mode=subscribe&hub.challenge=PING"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, tc.path, nil)
			mux.ServeHTTP(rec, req)
			if rec.Code != http.StatusForbidden {
				t.Fatalf("got %d", rec.Code)
			}
		})
	}
}

// TestPersistedEventShape verifies the canonical (channel, external_id)
// the messenger handler propagates downstream — the Identity hook
// contract from F2-06: Resolve(channel="messenger", external_id=psid).
func TestPersistedEventShape(t *testing.T) {
	t.Parallel()
	in := newFakeInbox()
	r := newFakeResolver()
	tenant := uuid.New()
	r.Register(testPageID, tenant)
	a := newAdapter(t, in, r, newFakeFlag(true), newFakeClock(fixedNow))

	body := validEnvelope(t, testPageID, "mid-ident", "PSID-XYZ", "olá", fixedNow.UnixMilli())
	rec := doPost(t, a, body, sign(t, body))
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d", rec.Code)
	}
	got := in.Persisted()
	if len(got) != 1 {
		t.Fatalf("expected 1 persisted, got %d", len(got))
	}
	if got[0].Channel != "messenger" {
		t.Errorf("channel must be %q got %q", "messenger", got[0].Channel)
	}
	if got[0].SenderExternalID != "PSID-XYZ" {
		t.Errorf("sender external id must be the PSID, got %q", got[0].SenderExternalID)
	}
}

// _ pins the inbox import — the package is consumed via fakeInbox and
// the InboundEvent struct returned from Persisted(); a refactor that
// drops the direct symbol would still keep the test file compiling.
var _ = inbox.InboundEvent{}
