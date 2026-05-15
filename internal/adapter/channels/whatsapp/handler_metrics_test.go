package whatsapp_test

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/pericles-luz/crm/internal/adapter/channels/whatsapp"
)

// SIN-62762 — handler-latency observability tests. The terminal label
// set on whatsapp_handler_elapsed_seconds drives the runbook
// thresholds in docs/runbooks/whatsapp-inbound-latency.md, so a
// regression on the label string (a typo, a missing case, a renamed
// drop class) is a real production risk: alerts would silently stop
// firing. Each test below pins the exact label string for one path
// through handlePost.

// handlerKit extends testKit with a Prometheus registry so the tests
// can scrape whatsapp_handler_elapsed_seconds. The registry is per-test
// (NewRegistry) to keep parallel tests isolated.
type handlerKit struct {
	adapter  *whatsapp.Adapter
	server   *httptest.Server
	inbox    *fakeInbox
	resolver *fakeResolver
	flag     *fakeFlag
	rate     *fakeRateLimiter
	clock    *fakeClock
	registry *prometheus.Registry
	tenant   uuid.UUID
	logBuf   *bytes.Buffer
}

func newHandlerKit(t *testing.T) *handlerKit {
	t.Helper()
	cfg := whatsapp.Config{
		AppSecret:      testAppSecret,
		VerifyToken:    testVerifyToken,
		RateMaxPerMin:  10,
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
	tenantID := uuid.MustParse("44444444-4444-4444-8444-444444444444")
	res.Register(testPhoneID, tenantID)
	reg := prometheus.NewRegistry()
	logBuf := &bytes.Buffer{}
	a, err := whatsapp.New(cfg, in, res, fl, rl,
		whatsapp.WithClock(cl),
		whatsapp.WithLogger(slog.New(slog.NewJSONHandler(logBuf, nil))),
		whatsapp.WithMetricsRegistry(reg),
	)
	if err != nil {
		t.Fatalf("whatsapp.New: %v", err)
	}
	mux := http.NewServeMux()
	a.Register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &handlerKit{
		adapter:  a,
		server:   srv,
		inbox:    in,
		resolver: res,
		flag:     fl,
		rate:     rl,
		clock:    cl,
		registry: reg,
		tenant:   tenantID,
		logBuf:   logBuf,
	}
}

func (k *handlerKit) post(t *testing.T, body []byte, signature string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, k.server.URL+"/webhooks/whatsapp", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if signature != "" {
		req.Header.Set(whatsapp.SignatureHeader, signature)
	}
	resp, err := k.server.Client().Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return resp
}

// assertResultBucketHas asserts the histogram recorded a single
// observation for the given result label. We use CollectAndCount
// because elapsed-time inside a fakeClock-driven test is always 0,
// and 0 is a valid sample; we only need to know "did the observation
// land on the right label".
func assertResultBucketHas(t *testing.T, reg *prometheus.Registry, result string, want int) {
	t.Helper()
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, mf := range mfs {
		if mf.GetName() != "whatsapp_handler_elapsed_seconds" {
			continue
		}
		for _, m := range mf.Metric {
			got := ""
			for _, l := range m.Label {
				if l.GetName() == "result" {
					got = l.GetValue()
				}
			}
			if got != result {
				continue
			}
			if c := int(m.GetHistogram().GetSampleCount()); c != want {
				t.Fatalf("result=%q sample count = %d, want %d", result, c, want)
			}
			return
		}
	}
	t.Fatalf("no observation found for result=%q", result)
}

// assertHandlerCompleteLogged scans the JSON-encoded slog buffer for
// the terminal handler_complete line and confirms result + presence of
// handler_elapsed_ms. We deliberately do not assert a non-zero value
// for elapsed because the fakeClock returns a fixed instant; the
// observation contract is "field present, label correct".
func assertHandlerCompleteLogged(t *testing.T, logBuf *bytes.Buffer, wantResult string) {
	t.Helper()
	lines := bytes.Split(logBuf.Bytes(), []byte("\n"))
	for _, line := range lines {
		if !bytes.Contains(line, []byte(`"whatsapp.handler_complete"`)) {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal(line, &rec); err != nil {
			t.Fatalf("parse log line: %v (%s)", err, line)
		}
		if got := rec["result"]; got != wantResult {
			t.Fatalf("log result = %v, want %v", got, wantResult)
		}
		if _, ok := rec["handler_elapsed_ms"]; !ok {
			t.Fatal("handler_complete missing handler_elapsed_ms field")
		}
		return
	}
	t.Fatalf("no whatsapp.handler_complete line found; log = %s", logBuf.String())
}

// signHandlerBody mirrors signBody but uses the handlerKit-local app
// secret (identical at the moment, but keeps the call site explicit).
func signHandlerBody(t *testing.T, body []byte) string {
	t.Helper()
	mac := hmac.New(sha256.New, []byte(testAppSecret))
	_, _ = mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// handlerEnvelope reuses the shared envelopeJSON shape; kept for
// readability at the call sites below.
func handlerEnvelope(t *testing.T, phoneID string, occurredAt time.Time, msgs ...envMsg) []byte {
	t.Helper()
	return envelopeJSON(t, phoneID, occurredAt, msgs...)
}

func TestHandlerMetrics_DeliveredHappyPath(t *testing.T) {
	t.Parallel()
	k := newHandlerKit(t)
	body := handlerEnvelope(t, testPhoneID, k.clock.Now(), envMsg{
		WamID: "wamid.metric.ok", From: "+5511955554444", Body: "hi",
	})
	if resp := k.post(t, body, signHandlerBody(t, body)); resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	assertResultBucketHas(t, k.registry, "delivered", 1)
	assertHandlerCompleteLogged(t, k.logBuf, "delivered")
}

func TestHandlerMetrics_DuplicateRetry(t *testing.T) {
	t.Parallel()
	k := newHandlerKit(t)
	body := handlerEnvelope(t, testPhoneID, k.clock.Now(), envMsg{
		WamID: "wamid.metric.dup", From: "+5511955554444", Body: "retry",
	})
	sig := signHandlerBody(t, body)
	// First post: delivered. Second + third: duplicates.
	if resp := k.post(t, body, sig); resp.StatusCode != http.StatusOK {
		t.Fatalf("warm-up status = %d", resp.StatusCode)
	}
	for i := 0; i < 2; i++ {
		if resp := k.post(t, body, sig); resp.StatusCode != http.StatusOK {
			t.Fatalf("retry %d status = %d", i, resp.StatusCode)
		}
	}
	assertResultBucketHas(t, k.registry, "delivered", 1)
	assertResultBucketHas(t, k.registry, "duplicate", 2)
}

func TestHandlerMetrics_DroppedSignature(t *testing.T) {
	t.Parallel()
	k := newHandlerKit(t)
	body := handlerEnvelope(t, testPhoneID, k.clock.Now(), envMsg{
		WamID: "wamid.sig.bad", From: "+1", Body: "x",
	})
	if resp := k.post(t, body, "sha256=deadbeef"); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
	assertResultBucketHas(t, k.registry, "dropped_signature", 1)
	assertHandlerCompleteLogged(t, k.logBuf, "dropped_signature")
}

func TestHandlerMetrics_DroppedParse(t *testing.T) {
	t.Parallel()
	k := newHandlerKit(t)
	body := []byte(`{not json`)
	if resp := k.post(t, body, signHandlerBody(t, body)); resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	assertResultBucketHas(t, k.registry, "dropped_parse", 1)
}

func TestHandlerMetrics_DroppedTimestampWindow(t *testing.T) {
	t.Parallel()
	k := newHandlerKit(t)
	stale := k.clock.Now().Add(-10 * time.Minute)
	body := handlerEnvelope(t, testPhoneID, stale, envMsg{
		WamID: "wamid.stale", From: "+1", Body: "x",
	})
	if resp := k.post(t, body, signHandlerBody(t, body)); resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	assertResultBucketHas(t, k.registry, "dropped_timestamp_window", 1)
}

func TestHandlerMetrics_DroppedEmptyEnvelope(t *testing.T) {
	t.Parallel()
	k := newHandlerKit(t)
	body := []byte(`{"object":"whatsapp_business_account","entry":[]}`)
	if resp := k.post(t, body, signHandlerBody(t, body)); resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	assertResultBucketHas(t, k.registry, "dropped_empty", 1)
}

func TestHandlerMetrics_DroppedTenantResolverError(t *testing.T) {
	t.Parallel()
	k := newHandlerKit(t)
	body := handlerEnvelope(t, "phone-unknown", k.clock.Now(), envMsg{
		WamID: "wamid.x", From: "+1", Body: "x",
	})
	if resp := k.post(t, body, signHandlerBody(t, body)); resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	assertResultBucketHas(t, k.registry, "dropped_tenant", 1)
}

func TestHandlerMetrics_DroppedRateLimited(t *testing.T) {
	t.Parallel()
	k := newHandlerKit(t)
	body := handlerEnvelope(t, testPhoneID, k.clock.Now(), envMsg{
		WamID: "wamid.rl.warm", From: "+1", Body: "x",
	})
	sig := signHandlerBody(t, body)
	for i := 0; i < 10; i++ {
		if resp := k.post(t, body, sig); resp.StatusCode != http.StatusOK {
			t.Fatalf("warm %d status = %d", i, resp.StatusCode)
		}
	}
	body2 := handlerEnvelope(t, testPhoneID, k.clock.Now(), envMsg{
		WamID: "wamid.rl.denied", From: "+1", Body: "y",
	})
	if resp := k.post(t, body2, signHandlerBody(t, body2)); resp.StatusCode != http.StatusOK {
		t.Fatalf("denied status = %d", resp.StatusCode)
	}
	assertResultBucketHas(t, k.registry, "dropped_rate_limited", 1)
}

func TestHandlerMetrics_DroppedFeatureOff(t *testing.T) {
	t.Parallel()
	k := newHandlerKit(t)
	k.flag.Set(k.tenant, false)
	body := handlerEnvelope(t, testPhoneID, k.clock.Now(), envMsg{
		WamID: "wamid.flag", From: "+1", Body: "x",
	})
	if resp := k.post(t, body, signHandlerBody(t, body)); resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	assertResultBucketHas(t, k.registry, "dropped_feature_off", 1)
}

func TestHandlerMetrics_DroppedDeliverError(t *testing.T) {
	t.Parallel()
	k := newHandlerKit(t)
	k.inbox.FailWith(errInjected)
	body := handlerEnvelope(t, testPhoneID, k.clock.Now(), envMsg{
		WamID: "wamid.err", From: "+1", Body: "x",
	})
	if resp := k.post(t, body, signHandlerBody(t, body)); resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	assertResultBucketHas(t, k.registry, "dropped_deliver_error", 1)
}

func TestHandlerMetrics_DroppedOther_MissingWamid(t *testing.T) {
	t.Parallel()
	k := newHandlerKit(t)
	// Build an envelope with an empty wamid — passes signature + parse
	// + timestamp + tenant + rate + flag, then trips deliverMessage's
	// missing-wamid guard.
	body := []byte(fmt.Sprintf(`{
		"object":"whatsapp_business_account",
		"entry":[{"id":"e","time":%d,"changes":[{"field":"messages","value":{
			"metadata":{"phone_number_id":%q},
			"messages":[{"id":"","from":"+1","timestamp":%q,"type":"text","text":{"body":"x"}}]
		}}]}]
	}`, k.clock.Now().Unix(), testPhoneID, strconv.FormatInt(k.clock.Now().Unix(), 10)))
	if resp := k.post(t, body, signHandlerBody(t, body)); resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	assertResultBucketHas(t, k.registry, "dropped_other", 1)
}

// TestHandlerMetrics_MultipleMessages_DeliveredDominates pins the
// aggregation rule from handler.go: when an envelope mixes a delivered
// message with a duplicate replay, the handler-level result is
// "delivered" (productive work). Without this the histogram would
// rotate between labels on every replay and SLO dashboards would lose
// signal.
func TestHandlerMetrics_MultipleMessages_DeliveredDominates(t *testing.T) {
	t.Parallel()
	k := newHandlerKit(t)
	// First post: prime "wamid.A" so the second post sees it as duplicate.
	prime := handlerEnvelope(t, testPhoneID, k.clock.Now(), envMsg{
		WamID: "wamid.A", From: "+1", Body: "x",
	})
	if resp := k.post(t, prime, signHandlerBody(t, prime)); resp.StatusCode != http.StatusOK {
		t.Fatal("warm-up failed")
	}
	// Second post: A duplicates, B delivers.
	mixed := handlerEnvelope(t, testPhoneID, k.clock.Now(),
		envMsg{WamID: "wamid.A", From: "+1", Body: "x"},
		envMsg{WamID: "wamid.B", From: "+1", Body: "y"},
	)
	if resp := k.post(t, mixed, signHandlerBody(t, mixed)); resp.StatusCode != http.StatusOK {
		t.Fatal("mixed post failed")
	}
	// First post = 1 delivered; mixed = 1 delivered (because A duplicate
	// + B delivered → "delivered" wins by priority).
	assertResultBucketHas(t, k.registry, "delivered", 2)
}

// TestHandlerMetrics_DeliverElapsedMS_LoggedOnDelivered formalises
// the contract that whatsapp.delivered carries deliver_elapsed_ms.
// Together with handler_elapsed_ms on the terminal line, this lets
// Loki dashboards split per-message vs total time without scraping
// Prometheus.
func TestHandlerMetrics_DeliverElapsedMS_LoggedOnDelivered(t *testing.T) {
	t.Parallel()
	k := newHandlerKit(t)
	body := handlerEnvelope(t, testPhoneID, k.clock.Now(), envMsg{
		WamID: "wamid.dem", From: "+1", Body: "x",
	})
	if resp := k.post(t, body, signHandlerBody(t, body)); resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	found := false
	for _, line := range bytes.Split(k.logBuf.Bytes(), []byte("\n")) {
		if !bytes.Contains(line, []byte(`"whatsapp.delivered"`)) {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal(line, &rec); err != nil {
			t.Fatalf("parse log: %v", err)
		}
		if _, ok := rec["deliver_elapsed_ms"]; !ok {
			t.Fatalf("whatsapp.delivered missing deliver_elapsed_ms: %s", line)
		}
		found = true
	}
	if !found {
		t.Fatal("no whatsapp.delivered line emitted")
	}
}

// TestHandlerMetrics_OnlyOneObservationPerHandler keeps the histogram
// from drifting into per-message double-counts: handlePost MUST emit
// exactly one observation per request regardless of how many messages
// the envelope carries.
func TestHandlerMetrics_OnlyOneObservationPerHandler(t *testing.T) {
	t.Parallel()
	k := newHandlerKit(t)
	body := handlerEnvelope(t, testPhoneID, k.clock.Now(),
		envMsg{WamID: "wamid.one.A", From: "+1", Body: "x"},
		envMsg{WamID: "wamid.one.B", From: "+1", Body: "y"},
		envMsg{WamID: "wamid.one.C", From: "+1", Body: "z"},
	)
	if resp := k.post(t, body, signHandlerBody(t, body)); resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	// Exactly one delivered observation despite three messages.
	count, err := testutil.GatherAndCount(k.registry, "whatsapp_handler_elapsed_seconds")
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	if count != 1 {
		t.Fatalf("histogram time series count = %d, want 1 (one result label)", count)
	}
	assertResultBucketHas(t, k.registry, "delivered", 1)
	// Inbox got all three messages.
	if k.inbox.PersistedCount() != 3 {
		t.Fatalf("persisted = %d, want 3", k.inbox.PersistedCount())
	}
}

// TestHandlerMetrics_UnwiredRegistry_Skips covers the option-pattern
// default: omitting WithMetricsRegistry leaves the handler functional
// but with no metric registration. handlePost MUST NOT panic on the
// nil receiver, and the terminal slog line MUST still fire.
func TestHandlerMetrics_UnwiredRegistry_Skips(t *testing.T) {
	t.Parallel()
	cfg := whatsapp.Config{
		AppSecret:      testAppSecret,
		VerifyToken:    testVerifyToken,
		RateMaxPerMin:  10,
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
	tenantID := uuid.MustParse("55555555-5555-4555-8555-555555555555")
	res.Register(testPhoneID, tenantID)
	logBuf := &bytes.Buffer{}
	// NOTE: WithMetricsRegistry deliberately omitted.
	a, err := whatsapp.New(cfg, in, res, fl, rl,
		whatsapp.WithClock(cl),
		whatsapp.WithLogger(slog.New(slog.NewJSONHandler(logBuf, nil))),
	)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	a.Register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	body := envelopeJSON(t, testPhoneID, cl.Now(), envMsg{
		WamID: "wamid.noregistry", From: "+1", Body: "x",
	})
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/webhooks/whatsapp", bytes.NewReader(body))
	req.Header.Set(whatsapp.SignatureHeader, signBody(t, body))
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if !strings.Contains(logBuf.String(), `"whatsapp.handler_complete"`) {
		t.Fatal("terminal handler_complete log not emitted")
	}
	if !strings.Contains(logBuf.String(), `"result":"delivered"`) {
		t.Fatal("handler_complete missing result=delivered")
	}
}
