//go:build integration

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
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/adapter/channels/channeltest"
	"github.com/pericles-luz/crm/internal/adapter/channels/whatsapp"
	pgcontacts "github.com/pericles-luz/crm/internal/adapter/db/postgres/contacts"
	pginbox "github.com/pericles-luz/crm/internal/adapter/db/postgres/inbox"
	contactsusecase "github.com/pericles-luz/crm/internal/contacts/usecase"
	inboxusecase "github.com/pericles-luz/crm/internal/inbox/usecase"
)

// TestMain releases the shared channeltest harness on process exit. The
// container (or external DSN) outlives every TestXxx run inside this
// package so the migration cost is paid once.
func TestMain(m *testing.M) {
	code := m.Run()
	channeltest.ReleaseOnExit()()
	os.Exit(code)
}

// integrationKit wires the production stack — pgcontacts.Store +
// pginbox.Store fronted by the real receive-inbound use case — behind a
// httptest server hosting the whatsapp.Adapter. The resolver / flag /
// rate-limiter ports keep the same fakes the unit suite uses because
// they cover no DB-state invariant the AC asks us to exercise here; the
// E2E gap closed by SIN-62846 is "did inbound HMAC → dedup → use case →
// Postgres actually wire end-to-end?", not "do the side ports also have
// adapters?".
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

	cfg := whatsapp.Config{
		AppSecret:      testAppSecret,
		VerifyToken:    testVerifyToken,
		RateMaxPerMin:  100,
		MaxBodyBytes:   1 << 20,
		PastWindow:     24 * time.Hour,
		FutureSkew:     time.Minute,
		DeliverTimeout: 5 * time.Second,
	}
	res := newFakeResolver()
	res.Register(testPhoneID, tenantID)
	fl := newFakeFlag(true)
	rl := newFakeRateLimiter(cfg.RateMaxPerMin)
	cl := newFakeClock(time.Unix(1_700_000_000, 0).UTC())

	adapter, err := whatsapp.New(cfg, receiveUC, res, fl, rl,
		whatsapp.WithClock(cl),
		whatsapp.WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))),
	)
	if err != nil {
		t.Fatalf("whatsapp.New: %v", err)
	}
	mux := http.NewServeMux()
	adapter.Register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	return &integrationKit{t: t, server: srv, tenant: tenantID, clock: cl, harness: h}
}

// postSigned fires a signed POST against the live whatsapp.Adapter and
// returns the resulting status. The full body is signed; the underlying
// metashared.VerifySignature rejects any tampering with timestamps or
// payload.
func (k *integrationKit) postSigned(body []byte) int {
	k.t.Helper()
	mac := hmac.New(sha256.New, []byte(testAppSecret))
	_, _ = mac.Write(body)
	sig := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	req, err := http.NewRequest(http.MethodPost, k.server.URL+"/webhooks/whatsapp", bytes.NewReader(body))
	if err != nil {
		k.t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(whatsapp.SignatureHeader, sig)
	resp, err := k.server.Client().Do(req)
	if err != nil {
		k.t.Fatalf("do request: %v", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return resp.StatusCode
}

// envelope builds a WhatsApp Cloud API webhook payload with one inbound
// text message addressed to testPhoneID at the harness's pinned clock.
func (k *integrationKit) envelope(wamid, from, body string) []byte {
	k.t.Helper()
	now := k.clock.Now()
	msg := fmt.Sprintf(
		`{"id":%q,"from":%q,"timestamp":%q,"type":"text","text":{"body":%q}}`,
		wamid, from, strconv.FormatInt(now.Unix(), 10), body,
	)
	return []byte(fmt.Sprintf(`{
		"object":"whatsapp_business_account",
		"entry":[{
			"id":"entry-1","time":%d,
			"changes":[{"field":"messages","value":{
				"metadata":{"phone_number_id":%q,"display_phone_number":"+5511999999999"},
				"contacts":[{"wa_id":"5511955554444","profile":{"name":"Alice"}}],
				"messages":[%s]
			}}]
		}]
	}`, now.Unix(), testPhoneID, msg))
}

// countRows is a tiny helper that runs SELECT COUNT(*) against the
// supplied predicate. Used to drive the dedup / conversation / message
// row-count assertions without hand-rolling pgx boilerplate in each test.
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

// TestIntegration_WhatsApp_Webhook_PersistsConversationAndMessage covers
// the SIN-62846 happy path: a signed Meta envelope arriving at the live
// HTTP handler must traverse HMAC → dedup → use case → Postgres and
// leave exactly one row in inbound_message_dedup, one Conversation and
// one Message with the expected body + sender mapping.
func TestIntegration_WhatsApp_Webhook_PersistsConversationAndMessage(t *testing.T) {
	k := newIntegrationKit(t)
	const (
		wamid = "wamid.HBgLNTUxMTk1NTU1NDQ0NA=="
		from  = "+5511955554444"
		body  = "olá integration"
	)
	if status := k.postSigned(k.envelope(wamid, from, body)); status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}

	// Dedup ledger: exactly one row for (whatsapp, wamid).
	if got := k.countRows(t,
		`SELECT COUNT(*) FROM inbound_message_dedup
		  WHERE channel = $1 AND channel_external_id = $2`,
		whatsapp.Channel, wamid); got != 1 {
		t.Errorf("inbound_message_dedup rows = %d, want 1", got)
	}

	// Conversation: exactly one open conversation for the seeded tenant
	// on the whatsapp channel.
	if got := k.countRows(t,
		`SELECT COUNT(*) FROM conversation
		  WHERE tenant_id = $1 AND channel = $2 AND state = 'open'`,
		k.tenant, whatsapp.Channel); got != 1 {
		t.Errorf("conversation rows = %d, want 1", got)
	}

	// Message: exactly one inbound message with the expected body +
	// channel_external_id propagated from the carrier wamid.
	if got := k.countRows(t,
		`SELECT COUNT(*) FROM message
		  WHERE tenant_id = $1 AND direction = 'in'
		    AND body = $2 AND channel_external_id = $3`,
		k.tenant, body, wamid); got != 1 {
		t.Errorf("message rows = %d, want 1", got)
	}

	// Contact: the carrier-supplied sender phone is the contact's
	// channel identity; the contact should belong to our tenant.
	if got := k.countRows(t,
		`SELECT COUNT(*) FROM contact_channel_identity
		  WHERE channel = $1 AND external_id = $2`,
		whatsapp.Channel, from); got != 1 {
		t.Errorf("contact_channel_identity rows = %d, want 1", got)
	}
}

// TestIntegration_WhatsApp_Webhook_ReplayIsIdempotent feeds the same
// signed envelope back through the live handler and asserts the dedup
// ledger collapses the retry — exactly one Message and one Conversation
// remain. This is the SIN-62846 AC #6 replay invariant.
func TestIntegration_WhatsApp_Webhook_ReplayIsIdempotent(t *testing.T) {
	k := newIntegrationKit(t)
	body := k.envelope(
		"wamid.HBgLNTUxMTk1NTU1NDQ0NA==-replay",
		"+5511955554444",
		"olá replay",
	)

	for i := 0; i < 3; i++ {
		if status := k.postSigned(body); status != http.StatusOK {
			t.Fatalf("post #%d status = %d, want 200", i+1, status)
		}
	}

	if got := k.countRows(t,
		`SELECT COUNT(*) FROM inbound_message_dedup
		  WHERE channel = $1`, whatsapp.Channel); got != 1 {
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
