package whatsapp_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/pericles-luz/crm/internal/adapter/channels/whatsapp"
	"github.com/pericles-luz/crm/internal/inbox"
)

// fakeStatusUpdater is a deterministic in-memory implementation of
// inbox.MessageStatusUpdater. Calls accumulate so the test can assert
// on the carrier replay path; the per-wamid status map mirrors the
// monotonic state machine the production use case enforces, so test
// expectations match real behaviour without spinning up Postgres.
type fakeStatusUpdater struct {
	mu     sync.Mutex
	state  map[string]inbox.MessageStatus
	known  map[string]bool
	calls  []inbox.StatusUpdate
	err    error
	failed []inbox.StatusUpdate
}

func newFakeStatusUpdater() *fakeStatusUpdater {
	return &fakeStatusUpdater{
		state: map[string]inbox.MessageStatus{},
		known: map[string]bool{},
	}
}

func (f *fakeStatusUpdater) Seed(wamid string, initial inbox.MessageStatus) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.known[wamid] = true
	f.state[wamid] = initial
}

func (f *fakeStatusUpdater) FailWith(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.err = err
}

func (f *fakeStatusUpdater) HandleStatus(_ context.Context, ev inbox.StatusUpdate) (inbox.StatusUpdateResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return inbox.StatusUpdateResult{}, f.err
	}
	f.calls = append(f.calls, ev)
	if !f.known[ev.ChannelExternalID] {
		return inbox.StatusUpdateResult{
			Outcome:   inbox.StatusOutcomeUnknownMessage,
			NewStatus: ev.NewStatus,
		}, nil
	}
	prev := f.state[ev.ChannelExternalID]
	if statusRank(prev) >= statusRank(ev.NewStatus) && prev != inbox.MessageStatusFailed && ev.NewStatus != inbox.MessageStatusFailed {
		return inbox.StatusUpdateResult{
			Outcome:        inbox.StatusOutcomeNoop,
			PreviousStatus: prev,
			NewStatus:      prev,
		}, nil
	}
	f.state[ev.ChannelExternalID] = ev.NewStatus
	if ev.NewStatus == inbox.MessageStatusFailed {
		f.failed = append(f.failed, ev)
	}
	return inbox.StatusUpdateResult{
		Outcome:        inbox.StatusOutcomeApplied,
		PreviousStatus: prev,
		NewStatus:      ev.NewStatus,
	}, nil
}

func (f *fakeStatusUpdater) Calls() []inbox.StatusUpdate {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]inbox.StatusUpdate, len(f.calls))
	copy(out, f.calls)
	return out
}

func (f *fakeStatusUpdater) State(wamid string) inbox.MessageStatus {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.state[wamid]
}

func (f *fakeStatusUpdater) Failed() []inbox.StatusUpdate {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]inbox.StatusUpdate, len(f.failed))
	copy(out, f.failed)
	return out
}

// statusRank mirrors the domain's monotonic rank so the test fake can
// reject regressions deterministically without importing internals.
func statusRank(s inbox.MessageStatus) int {
	switch s {
	case inbox.MessageStatusPending:
		return 0
	case inbox.MessageStatusSent:
		return 1
	case inbox.MessageStatusDelivered:
		return 2
	case inbox.MessageStatusRead:
		return 3
	}
	return -1
}

// statusKit extends the existing receive-side testKit with a status
// updater + a dedicated metrics registry so tests can scrape
// whatsapp_status_total / whatsapp_status_lag_seconds without
// colliding with package-level state.
type statusKit struct {
	adapter  *whatsapp.Adapter
	server   *httptest.Server
	updater  *fakeStatusUpdater
	registry *prometheus.Registry
	tenant   uuid.UUID
	clock    *fakeClock
	inbox    *fakeInbox
}

func newStatusKit(t *testing.T) *statusKit {
	t.Helper()
	cfg := whatsapp.Config{
		AppSecret:      testAppSecret,
		VerifyToken:    testVerifyToken,
		RateMaxPerMin:  100,
		MaxBodyBytes:   1 << 20,
		PastWindow:     5 * time.Minute,
		FutureSkew:     time.Minute,
		DeliverTimeout: 2 * time.Second,
	}
	in := newFakeInbox()
	res := newFakeResolver()
	fl := newFakeFlag(true)
	rl := newFakeRateLimiter(cfg.RateMaxPerMin)
	cl := newFakeClock(time.Unix(1_700_000_000, 0).UTC())
	tenantID := uuid.MustParse("22222222-2222-4222-8222-222222222222")
	res.Register(testPhoneID, tenantID)
	updater := newFakeStatusUpdater()
	reg := prometheus.NewRegistry()
	a, err := whatsapp.New(cfg, in, res, fl, rl,
		whatsapp.WithClock(cl),
		whatsapp.WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))),
		whatsapp.WithStatusUpdater(updater),
		whatsapp.WithMetricsRegistry(reg),
	)
	if err != nil {
		t.Fatalf("whatsapp.New: %v", err)
	}
	mux := http.NewServeMux()
	a.Register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &statusKit{
		adapter:  a,
		server:   srv,
		updater:  updater,
		registry: reg,
		tenant:   tenantID,
		clock:    cl,
		inbox:    in,
	}
}

// envStatus is the parallel of envMsg for statuses[] payloads. Each
// entry mirrors one Meta statuses[] block; tests build a list and
// hand it to statusEnvelopeJSON.
type envStatus struct {
	WamID     string
	Status    string
	ErrorCode int
	ErrorMsg  string
}

func statusEnvelopeJSON(t *testing.T, phoneID string, occurredAt time.Time, sts ...envStatus) []byte {
	t.Helper()
	var b strings.Builder
	for i, s := range sts {
		if i > 0 {
			b.WriteByte(',')
		}
		errFrag := ""
		if s.ErrorCode != 0 {
			errFrag = fmt.Sprintf(`,"errors":[{"code":%d,"title":%q}]`, s.ErrorCode, s.ErrorMsg)
		}
		fmt.Fprintf(&b,
			`{"id":%q,"status":%q,"timestamp":%q%s}`,
			s.WamID, s.Status, strconv.FormatInt(occurredAt.Unix(), 10), errFrag)
	}
	return []byte(fmt.Sprintf(`{
		"object":"whatsapp_business_account",
		"entry":[{
			"id":"entry-status","time":%d,
			"changes":[{"field":"messages","value":{
				"metadata":{"phone_number_id":%q,"display_phone_number":"+5511999999999"},
				"statuses":[%s]
			}}]
		}]
	}`, occurredAt.Unix(), phoneID, b.String()))
}

func postStatus(t *testing.T, k *statusKit, body []byte) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, k.server.URL+"/webhooks/whatsapp", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(whatsapp.SignatureHeader, signBody(t, body))
	resp, err := k.server.Client().Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return resp
}

// TestStatus_SequenceAdvancesMessage covers AC #1 — sent → delivered
// → read in three separate webhook posts advances the message.
func TestStatus_SequenceAdvancesMessage(t *testing.T) {
	t.Parallel()
	k := newStatusKit(t)
	k.updater.Seed("wamid.SEQ", inbox.MessageStatusPending)

	for _, raw := range []string{"sent", "delivered", "read"} {
		body := statusEnvelopeJSON(t, testPhoneID, k.clock.Now(), envStatus{
			WamID: "wamid.SEQ", Status: raw,
		})
		resp := postStatus(t, k, body)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s: status = %d", raw, resp.StatusCode)
		}
	}
	if got := k.updater.State("wamid.SEQ"); got != inbox.MessageStatusRead {
		t.Fatalf("final state = %q, want read", got)
	}
	calls := k.updater.Calls()
	if len(calls) != 3 {
		t.Fatalf("calls = %d, want 3", len(calls))
	}
}

// TestStatus_DeliveredAfterRead_NoOp covers AC #3 — a regressed
// delivered after read MUST NOT rewrite the state.
func TestStatus_DeliveredAfterRead_NoOp(t *testing.T) {
	t.Parallel()
	k := newStatusKit(t)
	k.updater.Seed("wamid.NOOP", inbox.MessageStatusRead)
	body := statusEnvelopeJSON(t, testPhoneID, k.clock.Now(), envStatus{
		WamID: "wamid.NOOP", Status: "delivered",
	})
	resp := postStatus(t, k, body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if got := k.updater.State("wamid.NOOP"); got != inbox.MessageStatusRead {
		t.Fatalf("state = %q, want read (unchanged)", got)
	}
}

// TestStatus_Failed_RecordsErrorMetadata covers AC #4 path — failed
// status reaches the use case with the Meta error metadata.
func TestStatus_Failed_RecordsErrorMetadata(t *testing.T) {
	t.Parallel()
	k := newStatusKit(t)
	k.updater.Seed("wamid.FAIL", inbox.MessageStatusSent)
	body := statusEnvelopeJSON(t, testPhoneID, k.clock.Now(), envStatus{
		WamID: "wamid.FAIL", Status: "failed", ErrorCode: 131026, ErrorMsg: "Message undeliverable",
	})
	resp := postStatus(t, k, body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	failed := k.updater.Failed()
	if len(failed) != 1 {
		t.Fatalf("failed events = %d, want 1", len(failed))
	}
	if failed[0].ErrorCode != 131026 {
		t.Errorf("error_code = %d, want 131026", failed[0].ErrorCode)
	}
	if failed[0].ErrorTitle != "Message undeliverable" {
		t.Errorf("error_title = %q, want %q", failed[0].ErrorTitle, "Message undeliverable")
	}
}

// TestStatus_MultipleEntries_AllDispatched covers the batch path
// where Meta packs several status updates in one envelope.
func TestStatus_MultipleEntries_AllDispatched(t *testing.T) {
	t.Parallel()
	k := newStatusKit(t)
	k.updater.Seed("wamid.A", inbox.MessageStatusPending)
	k.updater.Seed("wamid.B", inbox.MessageStatusPending)
	body := statusEnvelopeJSON(t, testPhoneID, k.clock.Now(),
		envStatus{WamID: "wamid.A", Status: "sent"},
		envStatus{WamID: "wamid.B", Status: "sent"},
	)
	resp := postStatus(t, k, body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if got := k.updater.State("wamid.A"); got != inbox.MessageStatusSent {
		t.Errorf("A = %q, want sent", got)
	}
	if got := k.updater.State("wamid.B"); got != inbox.MessageStatusSent {
		t.Errorf("B = %q, want sent", got)
	}
}

// TestStatus_MetricsCounter covers AC #4 — whatsapp_status_total
// counter increments per status, partitioned by status + outcome.
func TestStatus_MetricsCounter(t *testing.T) {
	t.Parallel()
	k := newStatusKit(t)
	k.updater.Seed("wamid.M", inbox.MessageStatusPending)
	body := statusEnvelopeJSON(t, testPhoneID, k.clock.Now(), envStatus{
		WamID: "wamid.M", Status: "sent",
	})
	if resp := postStatus(t, k, body); resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	const expected = `
		# HELP whatsapp_status_total WhatsApp status updates received, partitioned by carrier status and adapter outcome.
		# TYPE whatsapp_status_total counter
		whatsapp_status_total{outcome="applied",status="sent"} 1
	`
	if err := testutil.GatherAndCompare(k.registry, strings.NewReader(expected), "whatsapp_status_total"); err != nil {
		t.Fatalf("metrics counter mismatch: %v", err)
	}
}

// TestStatus_MetricsLagHistogram covers the lag histogram — Meta
// timestamp - now() difference is observed once per applied/noop
// update.
func TestStatus_MetricsLagHistogram(t *testing.T) {
	t.Parallel()
	k := newStatusKit(t)
	k.updater.Seed("wamid.LAG", inbox.MessageStatusPending)
	occurredAt := k.clock.Now().Add(-3 * time.Second)
	body := statusEnvelopeJSON(t, testPhoneID, occurredAt, envStatus{
		WamID: "wamid.LAG", Status: "sent",
	})
	if resp := postStatus(t, k, body); resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	count, err := testutil.GatherAndCount(k.registry, "whatsapp_status_lag_seconds")
	if err != nil {
		t.Fatalf("gather lag: %v", err)
	}
	if count != 1 {
		t.Errorf("lag observations = %d, want 1", count)
	}
}

// TestStatus_UnknownStatusValue_Dropped exercises forward-compatibility
// — Meta could introduce a new status (e.g. "deleted") tomorrow; the
// adapter logs + counts it under outcome="dropped" but does not crash.
func TestStatus_UnknownStatusValue_Dropped(t *testing.T) {
	t.Parallel()
	k := newStatusKit(t)
	body := statusEnvelopeJSON(t, testPhoneID, k.clock.Now(), envStatus{
		WamID: "wamid.X", Status: "deleted",
	})
	if resp := postStatus(t, k, body); resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if calls := k.updater.Calls(); len(calls) != 0 {
		t.Errorf("updater should not be called for unknown status, got %d", len(calls))
	}
}

// TestStatus_MissingWamid_Dropped covers the boundary check — a
// status with empty id is logged + counted as dropped.
func TestStatus_MissingWamid_Dropped(t *testing.T) {
	t.Parallel()
	k := newStatusKit(t)
	body := statusEnvelopeJSON(t, testPhoneID, k.clock.Now(), envStatus{
		WamID: "", Status: "sent",
	})
	if resp := postStatus(t, k, body); resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if calls := k.updater.Calls(); len(calls) != 0 {
		t.Errorf("missing wamid: updater should not be called, got %d", len(calls))
	}
}

// TestStatus_UpdaterError_AcksAndCountsError covers fail-soft: a
// downstream error from the use case MUST NOT propagate to the
// carrier — Meta still gets a 200 — and the metrics carry the
// "error" outcome.
func TestStatus_UpdaterError_AcksAndCountsError(t *testing.T) {
	t.Parallel()
	k := newStatusKit(t)
	k.updater.Seed("wamid.E", inbox.MessageStatusSent)
	k.updater.FailWith(errInjected)
	body := statusEnvelopeJSON(t, testPhoneID, k.clock.Now(), envStatus{
		WamID: "wamid.E", Status: "delivered",
	})
	if resp := postStatus(t, k, body); resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	const expected = `
		# HELP whatsapp_status_total WhatsApp status updates received, partitioned by carrier status and adapter outcome.
		# TYPE whatsapp_status_total counter
		whatsapp_status_total{outcome="error",status="delivered"} 1
	`
	if err := testutil.GatherAndCompare(k.registry, strings.NewReader(expected), "whatsapp_status_total"); err != nil {
		t.Fatalf("metrics counter mismatch: %v", err)
	}
}

// TestStatus_UnwiredUpdater_DropsCleanly verifies the option-pattern
// default: omitting WithStatusUpdater still lets the adapter ack
// statuses[] payloads without crashing — they are counted as dropped.
func TestStatus_UnwiredUpdater_DropsCleanly(t *testing.T) {
	t.Parallel()
	cfg := whatsapp.Config{
		AppSecret:      testAppSecret,
		VerifyToken:    testVerifyToken,
		RateMaxPerMin:  100,
		MaxBodyBytes:   1 << 20,
		PastWindow:     5 * time.Minute,
		FutureSkew:     time.Minute,
		DeliverTimeout: 2 * time.Second,
	}
	in := newFakeInbox()
	res := newFakeResolver()
	fl := newFakeFlag(true)
	rl := newFakeRateLimiter(cfg.RateMaxPerMin)
	cl := newFakeClock(time.Unix(1_700_000_000, 0).UTC())
	tenantID := uuid.MustParse("33333333-3333-4333-8333-333333333333")
	res.Register(testPhoneID, tenantID)
	reg := prometheus.NewRegistry()
	a, err := whatsapp.New(cfg, in, res, fl, rl,
		whatsapp.WithClock(cl),
		whatsapp.WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))),
		whatsapp.WithMetricsRegistry(reg),
	)
	if err != nil {
		t.Fatalf("whatsapp.New: %v", err)
	}
	mux := http.NewServeMux()
	a.Register(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	body := statusEnvelopeJSON(t, testPhoneID, cl.Now(), envStatus{
		WamID: "wamid.UNWIRED", Status: "sent",
	})
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/webhooks/whatsapp", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(whatsapp.SignatureHeader, signBody(t, body))
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	const expected = `
		# HELP whatsapp_status_total WhatsApp status updates received, partitioned by carrier status and adapter outcome.
		# TYPE whatsapp_status_total counter
		whatsapp_status_total{outcome="dropped",status="sent"} 1
	`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(expected), "whatsapp_status_total"); err != nil {
		t.Fatalf("metrics mismatch: %v", err)
	}
}

// Compile-time guard.
var _ inbox.MessageStatusUpdater = (*fakeStatusUpdater)(nil)
