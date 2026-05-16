package messenger_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/adapter/channels/messenger"
)

// newAdapterWithMedia constructs an Adapter with an optional media publisher wired in.
func newAdapterWithMedia(t *testing.T, in *fakeInbox, r *fakeResolver, f *fakeFlag, c *fakeClock, media *fakeMediaPublisher) *messenger.Adapter {
	t.Helper()
	cfg := messenger.Config{
		AppSecret:      testAppSecret,
		VerifyToken:    testVerifyToken,
		MaxBodyBytes:   1 << 20,
		PastWindow:     0, // disable window so all timestamps are accepted
		FutureSkew:     1<<62 - 1,
		DeliverTimeout: 0,
	}
	opts := []messenger.Option{
		messenger.WithClock(c),
	}
	if media != nil {
		opts = append(opts, messenger.WithMediaScanPublisher(media))
	}
	a, err := messenger.New(cfg, in, r, f, opts...)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return a
}

func attachmentEnvelope(t *testing.T, pageID, mid, psid string, atts []map[string]any) ([]byte, string) {
	t.Helper()
	payload := map[string]any{
		"object": "page",
		"entry": []any{
			map[string]any{
				"id":   pageID,
				"time": fixedNow.UnixMilli(),
				"messaging": []any{
					map[string]any{
						"sender":    map[string]any{"id": psid},
						"recipient": map[string]any{"id": pageID},
						"timestamp": fixedNow.UnixMilli(),
						"message": map[string]any{
							"mid":         mid,
							"attachments": atts,
						},
					},
				},
			},
		},
	}
	b, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b, sign(t, b)
}

func TestHandlePost_SingleAttachment_PublishesScanRequest(t *testing.T) {
	t.Parallel()

	in := newFakeInbox()
	r := newFakeResolver()
	tenant := uuid.New()
	r.Register(testPageID, tenant)
	media := newFakeMediaPublisher()
	a := newAdapterWithMedia(t, in, r, newFakeFlag(true), newFakeClock(fixedNow), media)

	atts := []map[string]any{{"type": "image", "payload": map[string]any{"url": "https://example.com/img.jpg"}}}
	body, sig := attachmentEnvelope(t, testPageID, "mid-img1", testPSID, atts)
	rec := doPost(t, a, body, sig)
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d", rec.Code)
	}

	calls := media.Calls()
	if len(calls) != 1 {
		t.Fatalf("expected 1 scan request, got %d", len(calls))
	}
	if calls[0].TenantID != tenant {
		t.Errorf("tenant mismatch: got %s want %s", calls[0].TenantID, tenant)
	}
	if !strings.Contains(calls[0].Key, "mid-img1") {
		t.Errorf("key should contain mid-img1, got %q", calls[0].Key)
	}
	if !strings.Contains(calls[0].Key, "image") {
		t.Errorf("key should contain attachment type, got %q", calls[0].Key)
	}
}

func TestHandlePost_MultipleAttachments_PublishesOnePerAttachment(t *testing.T) {
	t.Parallel()

	in := newFakeInbox()
	r := newFakeResolver()
	tenant := uuid.New()
	r.Register(testPageID, tenant)
	media := newFakeMediaPublisher()
	a := newAdapterWithMedia(t, in, r, newFakeFlag(true), newFakeClock(fixedNow), media)

	atts := []map[string]any{
		{"type": "image", "payload": map[string]any{"url": "https://example.com/a.jpg"}},
		{"type": "video", "payload": map[string]any{"url": "https://example.com/b.mp4"}},
		{"type": "file", "payload": map[string]any{"url": "https://example.com/c.pdf"}},
	}
	body, sig := attachmentEnvelope(t, testPageID, "mid-multi", testPSID, atts)
	rec := doPost(t, a, body, sig)
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d", rec.Code)
	}
	calls := media.Calls()
	if len(calls) != 3 {
		t.Fatalf("expected 3 scan requests, got %d", len(calls))
	}
}

func TestHandlePost_NoAttachments_NoScanPublish(t *testing.T) {
	t.Parallel()

	in := newFakeInbox()
	r := newFakeResolver()
	tenant := uuid.New()
	r.Register(testPageID, tenant)
	media := newFakeMediaPublisher()
	a := newAdapterWithMedia(t, in, r, newFakeFlag(true), newFakeClock(fixedNow), media)

	body := validEnvelope(t, testPageID, "mid-text", testPSID, "hello", fixedNow.UnixMilli())
	rec := doPost(t, a, body, sign(t, body))
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d", rec.Code)
	}
	if len(media.Calls()) != 0 {
		t.Errorf("expected no scan requests for text-only message, got %d", len(media.Calls()))
	}
}

func TestHandlePost_MediaPublisherUnwired_StillDelivers(t *testing.T) {
	t.Parallel()

	in := newFakeInbox()
	r := newFakeResolver()
	tenant := uuid.New()
	r.Register(testPageID, tenant)
	// no media publisher — nil is the graceful-degradation path
	a := newAdapterWithMedia(t, in, r, newFakeFlag(true), newFakeClock(fixedNow), nil)

	atts := []map[string]any{{"type": "image"}}
	body, sig := attachmentEnvelope(t, testPageID, "mid-unwired", testPSID, atts)
	rec := doPost(t, a, body, sig)
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d", rec.Code)
	}
	// Message still delivered even when media publisher is absent.
	if len(in.Persisted()) != 1 {
		t.Fatalf("expected 1 inbox delivery, got %d", len(in.Persisted()))
	}
}

func TestHandlePost_MediaPublishError_StillAcks(t *testing.T) {
	t.Parallel()

	in := newFakeInbox()
	r := newFakeResolver()
	tenant := uuid.New()
	r.Register(testPageID, tenant)
	media := newFakeMediaPublisher()
	media.FailWith(errInjected)
	a := newAdapterWithMedia(t, in, r, newFakeFlag(true), newFakeClock(fixedNow), media)

	atts := []map[string]any{{"type": "video"}}
	body, sig := attachmentEnvelope(t, testPageID, "mid-pub-err", testPSID, atts)
	rec := doPost(t, a, body, sig)
	// Meta always gets 200 OK even if media publish failed.
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d, want 200", rec.Code)
	}
	// Message still delivered (inbound handling succeeded).
	if len(in.Persisted()) != 1 {
		t.Fatalf("expected 1 inbox delivery, got %d", len(in.Persisted()))
	}
}

// TestHandlePost_PublishesWithPersistedMessageID is the SIN-62848 AC #2
// anchor: PublishScanRequest MUST receive the persisted MessageID (not
// uuid.Nil) so the MediaScanner worker can patch `message.media` once
// the verdict lands. Without it, the worker drops the envelope as poison
// and the scan_status stays "pending" forever.
func TestHandlePost_PublishesWithPersistedMessageID(t *testing.T) {
	t.Parallel()

	in := newFakeInbox()
	r := newFakeResolver()
	tenant := uuid.New()
	r.Register(testPageID, tenant)
	media := newFakeMediaPublisher()
	a := newAdapterWithMedia(t, in, r, newFakeFlag(true), newFakeClock(fixedNow), media)

	atts := []map[string]any{
		{"type": "image", "payload": map[string]any{"url": "https://example.com/a.jpg"}},
		{"type": "video", "payload": map[string]any{"url": "https://example.com/b.mp4"}},
	}
	body, sig := attachmentEnvelope(t, testPageID, "mid-id", testPSID, atts)
	rec := doPost(t, a, body, sig)
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d", rec.Code)
	}

	records := in.PersistedRecords()
	if len(records) != 1 {
		t.Fatalf("expected 1 persisted, got %d", len(records))
	}
	want := records[0].MessageID
	if want == uuid.Nil {
		t.Fatal("fake materialiser returned uuid.Nil — bug in fake")
	}
	// HasAttachments must flow through to the materialiser so the use
	// case sets media.scan_status="pending" at persist time (AC #1).
	if !records[0].Event.HasAttachments {
		t.Error("HasAttachments=false on persisted event for an attachment envelope")
	}

	calls := media.Calls()
	if len(calls) != 2 {
		t.Fatalf("expected 2 publish calls, got %d", len(calls))
	}
	for i, c := range calls {
		if c.MessageID != want {
			t.Errorf("call[%d].MessageID = %s, want %s", i, c.MessageID, want)
		}
	}
}

// TestHandlePost_DuplicateMID_DoesNotRepublishScan is the SIN-62848
// AC #4 anchor at the adapter layer: a redelivered envelope (same mid)
// MUST NOT republish media.scan.requested. The first delivery persists
// the row in scan_status="pending"; republishing with the same
// MessageID would race the in-flight scan and risk overwriting the
// terminal verdict the worker is about to write.
func TestHandlePost_DuplicateMID_DoesNotRepublishScan(t *testing.T) {
	t.Parallel()

	in := newFakeInbox()
	r := newFakeResolver()
	tenant := uuid.New()
	r.Register(testPageID, tenant)
	media := newFakeMediaPublisher()
	a := newAdapterWithMedia(t, in, r, newFakeFlag(true), newFakeClock(fixedNow), media)

	atts := []map[string]any{{"type": "image"}}
	body, sig := attachmentEnvelope(t, testPageID, "mid-dup-media", testPSID, atts)
	// First delivery publishes one scan.
	if rec := doPost(t, a, body, sig); rec.Code != http.StatusOK {
		t.Fatalf("first delivery got %d", rec.Code)
	}
	if got := len(media.Calls()); got != 1 {
		t.Fatalf("after first delivery: publish calls = %d, want 1", got)
	}
	// Second (duplicate) delivery must not republish.
	if rec := doPost(t, a, body, sig); rec.Code != http.StatusOK {
		t.Fatalf("second delivery got %d", rec.Code)
	}
	if got := len(media.Calls()); got != 1 {
		t.Fatalf("after duplicate delivery: publish calls = %d, want still 1", got)
	}
}
