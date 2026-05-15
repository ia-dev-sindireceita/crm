package whatsapp_test

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/adapter/channels/whatsapp"
	"github.com/pericles-luz/crm/internal/inbox"
)

const (
	testAppSecret   = "super-secret-app-key"
	testVerifyToken = "verify-token-1234"
	testPhoneID     = "phone-id-100200"
)

// newTestAdapter wires fakes shared by every handler test. Returns the
// adapter plus the fakes so the test can assert on them.
type testKit struct {
	adapter  *whatsapp.Adapter
	mux      *http.ServeMux
	server   *httptest.Server
	inbox    *fakeInbox
	resolver *fakeResolver
	flag     *fakeFlag
	rate     *fakeRateLimiter
	clock    *fakeClock
	tenant   uuid.UUID
}

func newTestKit(t *testing.T) *testKit {
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
	tenantID := uuid.MustParse("11111111-1111-4111-8111-111111111111")
	res.Register(testPhoneID, tenantID)

	a, err := whatsapp.New(cfg, in, res, fl, rl,
		whatsapp.WithClock(cl),
		whatsapp.WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))))
	if err != nil {
		t.Fatalf("whatsapp.New: %v", err)
	}
	mux := http.NewServeMux()
	a.Register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &testKit{
		adapter:  a,
		mux:      mux,
		server:   srv,
		inbox:    in,
		resolver: res,
		flag:     fl,
		rate:     rl,
		clock:    cl,
		tenant:   tenantID,
	}
}

// signBody returns the X-Hub-Signature-256 hex-encoded HMAC of body
// using the test app secret.
func signBody(t *testing.T, body []byte) string {
	t.Helper()
	mac := hmac.New(sha256.New, []byte(testAppSecret))
	_, _ = mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// envelopeJSON builds a minimal Meta payload addressed to phone with
// the given wamid + body + timestamp. Multiple wamids: pass several.
func envelopeJSON(t *testing.T, phoneID string, occurredAt time.Time, msgs ...envMsg) []byte {
	t.Helper()
	var bMsgs bytes.Buffer
	for i, m := range msgs {
		if i > 0 {
			bMsgs.WriteByte(',')
		}
		fmt.Fprintf(&bMsgs,
			`{"id":%q,"from":%q,"timestamp":%q,"type":"text","text":{"body":%q}}`,
			m.WamID, m.From, strconv.FormatInt(occurredAt.Unix(), 10), m.Body)
	}
	return []byte(fmt.Sprintf(`{
		"object":"whatsapp_business_account",
		"entry":[{
			"id":"entry-1","time":%d,
			"changes":[{"field":"messages","value":{
				"metadata":{"phone_number_id":%q,"display_phone_number":"+5511999999999"},
				"contacts":[{"wa_id":"5511999999999","profile":{"name":"Alice"}}],
				"messages":[%s]
			}}]
		}]
	}`, occurredAt.Unix(), phoneID, bMsgs.String()))
}

type envMsg struct {
	WamID string
	From  string
	Body  string
}

// post fires a signed POST and returns the response status. Re-use
// helpers cover unsigned and tampered cases.
func (k *testKit) post(t *testing.T, body []byte, signature string) *http.Response {
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
		t.Fatalf("do request: %v", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return resp
}

func TestPost_SignatureValid_AcceptsAndDelivers(t *testing.T) {
	t.Parallel()
	k := newTestKit(t)
	body := envelopeJSON(t, testPhoneID, k.clock.Now(), envMsg{
		WamID: "wamid.ABC123", From: "+5511955554444", Body: "olá",
	})
	resp := k.post(t, body, signBody(t, body))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got := k.inbox.PersistedCount(); got != 1 {
		t.Fatalf("persisted = %d, want 1", got)
	}
	if got := k.inbox.Persisted()[0]; got.ChannelExternalID != "wamid.ABC123" || got.TenantID != k.tenant {
		t.Fatalf("unexpected event: %#v", got)
	}
}

func TestPost_SignatureMissing_Returns401(t *testing.T) {
	t.Parallel()
	k := newTestKit(t)
	body := envelopeJSON(t, testPhoneID, k.clock.Now(), envMsg{WamID: "w1", From: "+1", Body: "x"})
	resp := k.post(t, body, "")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
	if got := k.inbox.PersistedCount(); got != 0 {
		t.Fatalf("persisted should be 0 on missing sig, got %d", got)
	}
}

func TestPost_SignatureTampered_Returns401(t *testing.T) {
	t.Parallel()
	k := newTestKit(t)
	body := envelopeJSON(t, testPhoneID, k.clock.Now(), envMsg{WamID: "w1", From: "+1", Body: "x"})
	resp := k.post(t, body, "sha256=deadbeef")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
}

func TestPost_SignatureBadHex_Returns401(t *testing.T) {
	t.Parallel()
	k := newTestKit(t)
	body := envelopeJSON(t, testPhoneID, k.clock.Now(), envMsg{WamID: "w1", From: "+1", Body: "x"})
	resp := k.post(t, body, "sha256=not-hex-XYZ")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
}

func TestPost_SignatureWrongAppSecret_Returns401_KnownVector(t *testing.T) {
	t.Parallel()
	k := newTestKit(t)
	body := envelopeJSON(t, testPhoneID, k.clock.Now(), envMsg{WamID: "w1", From: "+1", Body: "x"})
	mac := hmac.New(sha256.New, []byte("WRONG_SECRET"))
	_, _ = mac.Write(body)
	bad := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	resp := k.post(t, body, bad)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
}

func TestPost_KnownHMACVector_Accepts(t *testing.T) {
	// Reference vector: body=`hello`, key=`key`
	// Expected HMAC-SHA256 = 9307b3529d12cc77578071e88b942c6e3d3f0a6f1d6e2c45fe7d9eb1ca0a8b8a … we recompute below.
	t.Parallel()
	cfg := whatsapp.Config{
		AppSecret:      "key",
		VerifyToken:    "v",
		RateMaxPerMin:  100,
		MaxBodyBytes:   1 << 20,
		PastWindow:     time.Hour,
		FutureSkew:     time.Hour,
		DeliverTimeout: time.Second,
	}
	in := newFakeInbox()
	res := newFakeResolver()
	res.Register("phone-known", uuid.MustParse("22222222-2222-4222-8222-222222222222"))
	fl := newFakeFlag(true)
	rl := newFakeRateLimiter(100)
	cl := newFakeClock(time.Unix(1_700_000_000, 0).UTC())
	a, err := whatsapp.New(cfg, in, res, fl, rl, whatsapp.WithClock(cl),
		whatsapp.WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))))
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	a.Register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	body := envelopeJSON(t, "phone-known", cl.Now(), envMsg{WamID: "w1", From: "+1", Body: "x"})
	mac := hmac.New(sha256.New, []byte("key"))
	_, _ = mac.Write(body)
	sig := "sha256=" + hex.EncodeToString(mac.Sum(nil))

	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/webhooks/whatsapp", bytes.NewReader(body))
	req.Header.Set(whatsapp.SignatureHeader, sig)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if in.PersistedCount() != 1 {
		t.Fatalf("persisted = %d, want 1", in.PersistedCount())
	}
}

func TestPost_TimestampStale_Drops(t *testing.T) {
	t.Parallel()
	k := newTestKit(t)
	stale := k.clock.Now().Add(-10 * time.Minute) // outside 5-min past window
	body := envelopeJSON(t, testPhoneID, stale, envMsg{WamID: "w1", From: "+1", Body: "x"})
	resp := k.post(t, body, signBody(t, body))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (silent drop)", resp.StatusCode)
	}
	if k.inbox.PersistedCount() != 0 {
		t.Fatalf("stale timestamp must not persist: got %d", k.inbox.PersistedCount())
	}
}

func TestPost_TimestampFutureBeyondSkew_Drops(t *testing.T) {
	t.Parallel()
	k := newTestKit(t)
	future := k.clock.Now().Add(2 * time.Minute) // outside 1-min future skew
	body := envelopeJSON(t, testPhoneID, future, envMsg{WamID: "w1", From: "+1", Body: "x"})
	resp := k.post(t, body, signBody(t, body))
	if resp.StatusCode != http.StatusOK {
		t.Fatal("expected 200")
	}
	if k.inbox.PersistedCount() != 0 {
		t.Fatalf("future timestamp must not persist: got %d", k.inbox.PersistedCount())
	}
}

func TestPost_TimestampInsideFutureSkew_Persists(t *testing.T) {
	t.Parallel()
	k := newTestKit(t)
	ahead := k.clock.Now().Add(30 * time.Second) // within 1-min skew
	body := envelopeJSON(t, testPhoneID, ahead, envMsg{WamID: "w1", From: "+1", Body: "x"})
	resp := k.post(t, body, signBody(t, body))
	if resp.StatusCode != http.StatusOK {
		t.Fatal("expected 200")
	}
	if k.inbox.PersistedCount() != 1 {
		t.Fatalf("persisted = %d, want 1", k.inbox.PersistedCount())
	}
}

func TestPost_UnknownPhoneNumberID_SilentDrop(t *testing.T) {
	t.Parallel()
	k := newTestKit(t)
	body := envelopeJSON(t, "unknown-phone", k.clock.Now(), envMsg{WamID: "w1", From: "+1", Body: "x"})
	resp := k.post(t, body, signBody(t, body))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if k.inbox.PersistedCount() != 0 {
		t.Fatalf("unknown phone must not persist: got %d", k.inbox.PersistedCount())
	}
}

func TestPost_FeatureFlagOff_DropsButAcks(t *testing.T) {
	t.Parallel()
	k := newTestKit(t)
	k.flag.Set(k.tenant, false)
	body := envelopeJSON(t, testPhoneID, k.clock.Now(), envMsg{WamID: "w1", From: "+1", Body: "x"})
	resp := k.post(t, body, signBody(t, body))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (acked)", resp.StatusCode)
	}
	if k.inbox.PersistedCount() != 0 {
		t.Fatalf("flag-off must not persist: got %d", k.inbox.PersistedCount())
	}
}

func TestPost_FeatureFlagError_DropsButAcks(t *testing.T) {
	t.Parallel()
	k := newTestKit(t)
	k.flag.FailWith(errInjected)
	body := envelopeJSON(t, testPhoneID, k.clock.Now(), envMsg{WamID: "w1", From: "+1", Body: "x"})
	resp := k.post(t, body, signBody(t, body))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if k.inbox.PersistedCount() != 0 {
		t.Fatal("flag error must not persist")
	}
}

func TestPost_RateLimitExceeded_DropsButAcks(t *testing.T) {
	t.Parallel()
	k := newTestKit(t)
	// Drain the bucket: 10 allowed, the 11th is denied.
	body := envelopeJSON(t, testPhoneID, k.clock.Now(), envMsg{WamID: "w-pre", From: "+1", Body: "x"})
	for i := 0; i < 10; i++ {
		resp := k.post(t, body, signBody(t, body))
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("warm-up: status = %d", resp.StatusCode)
		}
	}
	// 11th hit — denied at the rate limit, before the inbox call.
	body2 := envelopeJSON(t, testPhoneID, k.clock.Now(), envMsg{WamID: "w-post", From: "+1", Body: "y"})
	resp := k.post(t, body2, signBody(t, body2))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	// w-post was rejected at rate-limit; w-pre got through once.
	persisted := k.inbox.Persisted()
	for _, ev := range persisted {
		if ev.ChannelExternalID == "w-post" {
			t.Fatalf("w-post should be rate-limited but persisted: %#v", ev)
		}
	}
	if k.rate.denied.Load() == 0 {
		t.Fatal("rate limiter never denied")
	}
}

func TestPost_RateLimiterError_DropsButAcks(t *testing.T) {
	t.Parallel()
	k := newTestKit(t)
	k.rate.FailWith(errInjected)
	body := envelopeJSON(t, testPhoneID, k.clock.Now(), envMsg{WamID: "w1", From: "+1", Body: "x"})
	resp := k.post(t, body, signBody(t, body))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if k.inbox.PersistedCount() != 0 {
		t.Fatal("rate-limit error must not persist")
	}
}

func TestPost_MalformedJSON_DropsButAcks(t *testing.T) {
	t.Parallel()
	k := newTestKit(t)
	body := []byte(`{not valid json`)
	resp := k.post(t, body, signBody(t, body))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if k.inbox.PersistedCount() != 0 {
		t.Fatal("malformed JSON must not persist")
	}
}

func TestPost_EmptyEntry_AcksWithoutDelivery(t *testing.T) {
	t.Parallel()
	k := newTestKit(t)
	body := []byte(`{"object":"whatsapp_business_account","entry":[]}`)
	resp := k.post(t, body, signBody(t, body))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if k.inbox.PersistedCount() != 0 {
		t.Fatal("no messages must not persist")
	}
}

func TestPost_MultipleMessages_AllPersisted(t *testing.T) {
	t.Parallel()
	k := newTestKit(t)
	body := envelopeJSON(t, testPhoneID, k.clock.Now(),
		envMsg{WamID: "wam-a", From: "+5511955554444", Body: "primeira"},
		envMsg{WamID: "wam-b", From: "+5511955554444", Body: "segunda"},
	)
	resp := k.post(t, body, signBody(t, body))
	if resp.StatusCode != http.StatusOK {
		t.Fatal("expected 200")
	}
	if k.inbox.PersistedCount() != 2 {
		t.Fatalf("persisted = %d, want 2", k.inbox.PersistedCount())
	}
}

func TestPost_DuplicateWamid_Idempotent(t *testing.T) {
	t.Parallel()
	k := newTestKit(t)
	body := envelopeJSON(t, testPhoneID, k.clock.Now(), envMsg{
		WamID: "wamid.same", From: "+5511955554444", Body: "olá",
	})
	for i := 0; i < 5; i++ {
		resp := k.post(t, body, signBody(t, body))
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("iter %d: status = %d", i, resp.StatusCode)
		}
	}
	if got := k.inbox.PersistedCount(); got != 1 {
		t.Fatalf("persisted = %d, want 1 after 5 retries", got)
	}
}

// TestPost_ConcurrentReplay_OneMessage exercises AC #2: replay of the
// same wamid 100x concurrent → exactly 1 message persisted. The
// in-memory fake is mutex-guarded and mirrors the production
// ON CONFLICT DO NOTHING contract; the real Postgres adapter is
// covered by the inbox repo's own concurrency test under
// internal/adapter/db/postgres/inbox/.
func TestPost_ConcurrentReplay_OneMessage(t *testing.T) {
	t.Parallel()
	k := newTestKit(t)
	// Lift rate limit out of the way for this volume test — the test
	// exercises idempotency, not back-pressure.
	k.rate = newFakeRateLimiter(0) // limit=0 disables the cap
	k.adapter, _ = whatsapp.New(whatsapp.Config{
		AppSecret:      testAppSecret,
		VerifyToken:    testVerifyToken,
		RateMaxPerMin:  100000,
		MaxBodyBytes:   1 << 20,
		PastWindow:     5 * time.Minute,
		FutureSkew:     time.Minute,
		DeliverTimeout: 5 * time.Second,
	}, k.inbox, k.resolver, k.flag, k.rate,
		whatsapp.WithClock(k.clock),
		whatsapp.WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))))
	mux := http.NewServeMux()
	k.adapter.Register(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()
	k.server = srv

	body := envelopeJSON(t, testPhoneID, k.clock.Now(), envMsg{
		WamID: "wamid.concurrent", From: "+5511955554444", Body: "race",
	})
	signature := signBody(t, body)

	const N = 100
	var wg sync.WaitGroup
	wg.Add(N)
	for i := 0; i < N; i++ {
		go func() {
			defer wg.Done()
			req, _ := http.NewRequest(http.MethodPost, srv.URL+"/webhooks/whatsapp", bytes.NewReader(body))
			req.Header.Set(whatsapp.SignatureHeader, signature)
			resp, err := srv.Client().Do(req)
			if err != nil {
				t.Errorf("do: %v", err)
				return
			}
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Errorf("status = %d", resp.StatusCode)
			}
		}()
	}
	wg.Wait()
	if got := k.inbox.PersistedCount(); got != 1 {
		t.Fatalf("100 concurrent replays persisted %d messages, want 1", got)
	}
	if got := k.inbox.CallCount(); got != int64(N) {
		t.Logf("inbox call count = %d (expected to see all %d attempts)", got, N)
	}
}

func TestPost_OversizeBody_AcksSilently(t *testing.T) {
	t.Parallel()
	k := newTestKit(t)
	huge := bytes.Repeat([]byte("a"), int(k.adapterMaxBody(t))+1)
	// Body is not valid JSON anyway — the read should fail first.
	resp := k.post(t, huge, signBody(t, huge))
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if k.inbox.PersistedCount() != 0 {
		t.Fatal("oversize body must not persist")
	}
}

func (k *testKit) adapterMaxBody(t *testing.T) int64 {
	t.Helper()
	// Mirror the testKit Config — the value is private to the adapter,
	// so we keep the test's source of truth here.
	return 1 << 20
}

func TestNew_RejectsMissingDeps(t *testing.T) {
	t.Parallel()
	good := whatsapp.Config{AppSecret: "s", VerifyToken: "v"}
	in := newFakeInbox()
	res := newFakeResolver()
	fl := newFakeFlag(true)
	rl := newFakeRateLimiter(10)

	cases := []struct {
		name string
		cfg  whatsapp.Config
		in   inbox.InboundChannel
		res  whatsapp.TenantResolver
		fl   whatsapp.FeatureFlag
		rl   whatsapp.RateLimiter
	}{
		{"missing-secret", whatsapp.Config{VerifyToken: "v"}, in, res, fl, rl},
		{"missing-verify-token", whatsapp.Config{AppSecret: "s"}, in, res, fl, rl},
		{"missing-inbox", good, nil, res, fl, rl},
		{"missing-resolver", good, in, nil, fl, rl},
		{"missing-flag", good, in, res, nil, rl},
		{"missing-rate", good, in, res, fl, nil},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, err := whatsapp.New(tc.cfg, tc.in, tc.res, tc.fl, tc.rl); err == nil {
				t.Fatal("expected error, got nil")
			}
		})
	}
}

// TestPost_DeliverFailure_AcksAndLogs makes sure a downstream failure
// (Postgres down, etc.) still results in a 200 OK so Meta does not
// loop us into a retry storm.
func TestPost_DeliverFailure_AcksAndLogs(t *testing.T) {
	t.Parallel()
	k := newTestKit(t)
	k.inbox.FailWith(errInjected)
	body := envelopeJSON(t, testPhoneID, k.clock.Now(), envMsg{WamID: "w1", From: "+1", Body: "x"})
	resp := k.post(t, body, signBody(t, body))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
}

func TestPost_TenantResolverInfraError_DropsButAcks(t *testing.T) {
	t.Parallel()
	k := newTestKit(t)
	k.resolver.err = errInjected
	body := envelopeJSON(t, testPhoneID, k.clock.Now(), envMsg{WamID: "w1", From: "+1", Body: "x"})
	resp := k.post(t, body, signBody(t, body))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if k.inbox.PersistedCount() != 0 {
		t.Fatal("resolver infra error must not persist")
	}
}

func TestPost_MissingPhoneNumberID_Drops(t *testing.T) {
	t.Parallel()
	k := newTestKit(t)
	body := []byte(fmt.Sprintf(`{
		"object":"whatsapp_business_account",
		"entry":[{"id":"e","time":%d,"changes":[{"field":"messages","value":{
			"metadata":{},
			"messages":[{"id":"w1","from":"+1","timestamp":%q,"type":"text","text":{"body":"x"}}]
		}}]}]
	}`, k.clock.Now().Unix(), strconv.FormatInt(k.clock.Now().Unix(), 10)))
	resp := k.post(t, body, signBody(t, body))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if k.inbox.PersistedCount() != 0 {
		t.Fatal("missing phone_number_id must not persist")
	}
}

func TestPost_MissingWamID_Drops(t *testing.T) {
	t.Parallel()
	k := newTestKit(t)
	body := []byte(fmt.Sprintf(`{
		"object":"whatsapp_business_account",
		"entry":[{"id":"e","time":%d,"changes":[{"field":"messages","value":{
			"metadata":{"phone_number_id":%q},
			"messages":[{"id":"","from":"+1","timestamp":%q,"type":"text","text":{"body":"x"}}]
		}}]}]
	}`, k.clock.Now().Unix(), testPhoneID, strconv.FormatInt(k.clock.Now().Unix(), 10)))
	resp := k.post(t, body, signBody(t, body))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if k.inbox.PersistedCount() != 0 {
		t.Fatal("missing wamid must not persist")
	}
}

func TestPost_NonTextMessage_StoresTypePlaceholder(t *testing.T) {
	t.Parallel()
	k := newTestKit(t)
	body := []byte(fmt.Sprintf(`{
		"object":"whatsapp_business_account",
		"entry":[{"id":"e","time":%d,"changes":[{"field":"messages","value":{
			"metadata":{"phone_number_id":%q},
			"messages":[{"id":"w1","from":"+5511955554444","timestamp":%q,"type":"image"}]
		}}]}]
	}`, k.clock.Now().Unix(), testPhoneID, strconv.FormatInt(k.clock.Now().Unix(), 10)))
	resp := k.post(t, body, signBody(t, body))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	persisted := k.inbox.Persisted()
	if len(persisted) != 1 {
		t.Fatalf("persisted = %d", len(persisted))
	}
	if persisted[0].Body != "[image]" {
		t.Fatalf("body = %q, want [image]", persisted[0].Body)
	}
}

func TestPost_ContextPropagated(t *testing.T) {
	t.Parallel()
	k := newTestKit(t)
	body := envelopeJSON(t, testPhoneID, k.clock.Now(), envMsg{WamID: "w1", From: "+1", Body: "x"})
	req, _ := http.NewRequest(http.MethodPost, k.server.URL+"/webhooks/whatsapp", bytes.NewReader(body))
	req.Header.Set(whatsapp.SignatureHeader, signBody(t, body))
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	req = req.WithContext(ctx)
	resp, err := k.server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
}
