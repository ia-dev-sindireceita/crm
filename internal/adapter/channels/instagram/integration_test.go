//go:build integration

package instagram_test

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/adapter/channels/channeltest"
	"github.com/pericles-luz/crm/internal/adapter/channels/instagram"
	pgcontacts "github.com/pericles-luz/crm/internal/adapter/db/postgres/contacts"
	pginbox "github.com/pericles-luz/crm/internal/adapter/db/postgres/inbox"
	contactsusecase "github.com/pericles-luz/crm/internal/contacts/usecase"
	inboxusecase "github.com/pericles-luz/crm/internal/inbox/usecase"
)

const testIGBusinessID = "ig-biz-1700"

func TestMain(m *testing.M) {
	code := m.Run()
	channeltest.ReleaseOnExit()()
	os.Exit(code)
}

// integrationKit wires the production stack — pgcontacts + pginbox + the
// real receive-inbound use case — behind a httptest server hosting the
// live instagram.Adapter. Resolver / flag / rate-limit ports stay on the
// fakes from fakes_test.go because the AC scopes this gap to HMAC → dedup
// → use case → Postgres; the side ports already have unit-level coverage.
type integrationKit struct {
	t       *testing.T
	server  *httptest.Server
	tenant  uuid.UUID
	clock   *fakeClock
	harness *channeltest.Harness
}

func newIntegrationKit(t *testing.T) *integrationKit {
	t.Helper()
	h := channeltest.Start(t)
	h.Truncate(t)

	tenantID := h.SeedTenant(t, uuid.Nil)

	contactsStore, err := pgcontacts.New(h.Pool())
	if err != nil {
		t.Fatalf("pgcontacts.New: %v", err)
	}
	inboxStore, err := pginbox.New(h.Pool())
	if err != nil {
		t.Fatalf("pginbox.New: %v", err)
	}
	contactsUC, err := contactsusecase.New(contactsStore)
	if err != nil {
		t.Fatalf("contactsusecase.New: %v", err)
	}
	receiveUC, err := inboxusecase.NewReceiveInbound(inboxStore, inboxStore, contactsUC)
	if err != nil {
		t.Fatalf("inboxusecase.NewReceiveInbound: %v", err)
	}

	cfg := instagram.Config{
		AppSecret:      testAppSecret,
		VerifyToken:    testVerifyToken,
		RateMaxPerMin:  100,
		MaxBodyBytes:   1 << 20,
		PastWindow:     24 * time.Hour,
		FutureSkew:     time.Minute,
		DeliverTimeout: 5 * time.Second,
	}
	res := newFakeResolver()
	res.Register(testIGBusinessID, tenantID)
	fl := newFakeFlag(true)
	rl := newFakeRateLimiter(cfg.RateMaxPerMin)
	cl := newFakeClock(time.Unix(1_700_000_000, 0).UTC())

	adapter, err := instagram.New(cfg, receiveUC, res, fl, rl,
		instagram.WithClock(cl),
		instagram.WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))),
	)
	if err != nil {
		t.Fatalf("instagram.New: %v", err)
	}
	mux := http.NewServeMux()
	adapter.Register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	return &integrationKit{t: t, server: srv, tenant: tenantID, clock: cl, harness: h}
}

func (k *integrationKit) postSigned(body []byte) int {
	k.t.Helper()
	mac := hmac.New(sha256.New, []byte(testAppSecret))
	_, _ = mac.Write(body)
	sig := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	req, err := http.NewRequest(http.MethodPost, k.server.URL+"/webhooks/instagram", bytes.NewReader(body))
	if err != nil {
		k.t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(instagram.SignatureHeader, sig)
	resp, err := k.server.Client().Do(req)
	if err != nil {
		k.t.Fatalf("do request: %v", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return resp.StatusCode
}

// igEnvelope builds an Instagram Messaging webhook payload — entry[].messaging[]
// shape — with one inbound text message addressed to testIGBusinessID at
// the harness's pinned clock. timestamp on the inner messaging block is
// unix-millis (Meta convention); entry.time is unix-seconds.
func (k *integrationKit) igEnvelope(mid, igsid, text string) []byte {
	k.t.Helper()
	now := k.clock.Now()
	msg := map[string]any{
		"sender":    map[string]string{"id": igsid},
		"recipient": map[string]string{"id": "ignored"},
		"timestamp": now.UnixMilli(),
		"message":   map[string]any{"mid": mid, "text": text},
	}
	env := map[string]any{
		"object": "instagram",
		"entry": []map[string]any{{
			"id":        testIGBusinessID,
			"time":      now.Unix(),
			"messaging": []map[string]any{msg},
		}},
	}
	out, err := json.Marshal(env)
	if err != nil {
		k.t.Fatalf("marshal envelope: %v", err)
	}
	return out
}

func (k *integrationKit) countRows(t *testing.T, sql string, args ...any) int {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var n int
	if err := k.harness.Pool().QueryRow(ctx, sql, args...).Scan(&n); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	return n
}

// TestIntegration_Instagram_Webhook_PersistsConversationAndMessage
// covers the SIN-62846 happy path for the Instagram channel: a signed
// Meta envelope reaching the live handler must traverse HMAC → dedup →
// use case → Postgres and leave exactly one row in
// inbound_message_dedup, one Conversation, one Message with the sender
// IGSID propagated as the contact's channel identity.
func TestIntegration_Instagram_Webhook_PersistsConversationAndMessage(t *testing.T) {
	k := newIntegrationKit(t)
	const (
		mid   = "ig.mid.HAPPY_001"
		igsid = "1234567890123"
		text  = "olá from instagram"
	)
	if status := k.postSigned(k.igEnvelope(mid, igsid, text)); status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}

	if got := k.countRows(t,
		`SELECT COUNT(*) FROM inbound_message_dedup
		  WHERE channel = $1 AND channel_external_id = $2`,
		instagram.Channel, mid); got != 1 {
		t.Errorf("inbound_message_dedup rows = %d, want 1", got)
	}

	if got := k.countRows(t,
		`SELECT COUNT(*) FROM conversation
		  WHERE tenant_id = $1 AND channel = $2 AND state = 'open'`,
		k.tenant, instagram.Channel); got != 1 {
		t.Errorf("conversation rows = %d, want 1", got)
	}

	if got := k.countRows(t,
		`SELECT COUNT(*) FROM message
		  WHERE tenant_id = $1 AND direction = 'in'
		    AND body = $2 AND channel_external_id = $3`,
		k.tenant, text, mid); got != 1 {
		t.Errorf("message rows = %d, want 1", got)
	}

	if got := k.countRows(t,
		`SELECT COUNT(*) FROM contact_channel_identity
		  WHERE channel = $1 AND external_id = $2`,
		instagram.Channel, igsid); got != 1 {
		t.Errorf("contact_channel_identity rows = %d, want 1", got)
	}
}

// TestIntegration_Instagram_Webhook_ReplayIsIdempotent feeds the same
// signed payload through the handler three times: the dedup ledger must
// collapse the retries and leave exactly one Conversation + Message
// behind.
func TestIntegration_Instagram_Webhook_ReplayIsIdempotent(t *testing.T) {
	k := newIntegrationKit(t)
	body := k.igEnvelope("ig.mid.REPLAY_001", "9876543210987", "olá replay")

	for i := 0; i < 3; i++ {
		if status := k.postSigned(body); status != http.StatusOK {
			t.Fatalf("post #%d status = %d, want 200", i+1, status)
		}
	}

	if got := k.countRows(t,
		`SELECT COUNT(*) FROM inbound_message_dedup
		  WHERE channel = $1`, instagram.Channel); got != 1 {
		t.Errorf("inbound_message_dedup rows = %d, want 1 (dedup must collapse replay)", got)
	}
	if got := k.countRows(t,
		`SELECT COUNT(*) FROM conversation
		  WHERE tenant_id = $1`, k.tenant); got != 1 {
		t.Errorf("conversation rows = %d, want 1 (replay must NOT open a new thread)", got)
	}
	if got := k.countRows(t,
		`SELECT COUNT(*) FROM message
		  WHERE tenant_id = $1 AND direction = 'in'`, k.tenant); got != 1 {
		t.Errorf("message rows = %d, want 1 (replay must NOT persist a duplicate)", got)
	}
}
