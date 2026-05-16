package messenger

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/adapter/channels/metashared"
	"github.com/pericles-luz/crm/internal/inbox"
)

// SignatureHeader is the Meta-defined header carrying the HMAC-SHA256
// signature in lowercase hex. We re-export the shared constant so
// existing callers stay source-compatible with the metashared refactor
// (SIN-62791).
const SignatureHeader = metashared.SignatureHeader

// messengerEnvelope is the permissive subset of the Meta Messenger
// webhook payload the handler consumes. Fields we do not need (echoes,
// reactions, message_reads, etc.) are intentionally absent so an
// upstream schema change in those areas cannot break JSON parsing —
// encoding/json ignores unknown fields by default.
type messengerEnvelope struct {
	Object string       `json:"object"`
	Entry  []envelEntry `json:"entry"`
}

type envelEntry struct {
	ID        string         `json:"id"`   // page id
	Time      int64          `json:"time"` // milliseconds since epoch
	Messaging []envelMessage `json:"messaging"`
}

type envelMessage struct {
	Sender    envelActor   `json:"sender"`
	Recipient envelActor   `json:"recipient"`
	Timestamp int64        `json:"timestamp"` // milliseconds since epoch
	Message   envelMsgBody `json:"message"`
}

type envelActor struct {
	ID string `json:"id"`
}

type envelMsgBody struct {
	MID         string               `json:"mid"`
	Text        string               `json:"text"`
	Attachments []envelMsgAttachment `json:"attachments"`
}

type envelMsgAttachment struct {
	Type    string                 `json:"type"`
	Payload envelAttachmentPayload `json:"payload"`
}

type envelAttachmentPayload struct {
	URL string `json:"url"`
}

// handlePost is POST /webhooks/messenger. Steps:
//
//  1. read raw body (capped at cfg.MaxBodyBytes)
//  2. verify HMAC signature → 401 on failure (only non-200 path)
//  3. parse the envelope
//  4. drop envelopes whose Object is not "page"
//  5. for each entry: timestamp window check, tenant resolution
//     from the page id, feature flag, then per-message delivery
//     through the InboundChannel port
//
// Every drop path returns 200 OK (anti-enumeration). Errors at any
// point are logged but never propagate to the HTTP layer — Meta is
// acknowledged regardless so the carrier moves on.
func (a *Adapter) handlePost(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, a.cfg.MaxBodyBytes))
	if err != nil {
		a.logger.Warn("messenger.body_read_failed", slog.String("err", err.Error()))
		writeAck(w)
		return
	}
	if err := metashared.VerifySignature(a.cfg.AppSecret, body, r.Header.Get(SignatureHeader)); err != nil {
		a.logger.Warn("messenger.signature_invalid",
			slog.String("body_sha256", hashHex(body)),
			slog.String("err", err.Error()))
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	var env messengerEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		a.logger.Warn("messenger.parse_failed",
			slog.String("body_sha256", hashHex(body)),
			slog.String("err", err.Error()))
		writeAck(w)
		return
	}
	if env.Object != "page" {
		a.logger.Info("messenger.unexpected_object", slog.String("object", env.Object))
		writeAck(w)
		return
	}
	now := a.clock.Now()
	for _, entry := range env.Entry {
		if dir := timestampWindowDirection(entry.Time, now, a.cfg.PastWindow, a.cfg.FutureSkew); dir != "" {
			a.logger.Warn("messenger.timestamp_outside_window",
				slog.String("body_sha256", hashHex(body)),
				slog.String("direction", dir))
			continue
		}
		a.deliverEntry(r.Context(), entry)
	}
	writeAck(w)
}

// deliverEntry resolves the tenant from the entry's page id, applies
// the feature flag, and routes each messaging[] item through the
// InboundChannel port. A pre-message drop (unknown page, flag off,
// resolver error) charges nothing on the inbox use case — the use case
// is invoked once per surviving message only.
func (a *Adapter) deliverEntry(ctx context.Context, entry envelEntry) {
	pageID := strings.TrimSpace(entry.ID)
	if pageID == "" {
		a.logger.Warn("messenger.missing_page_id")
		return
	}
	tenantID, err := a.tenants.Resolve(ctx, pageID)
	if err != nil {
		// Silent drop — anti-enumeration. Info level so an unknown
		// page does not spam warn-level dashboards.
		a.logger.Info("messenger.unknown_page_id", slog.String("page_id", pageID))
		return
	}
	on, err := a.flag.Enabled(ctx, tenantID)
	if err != nil {
		a.logger.Warn("messenger.feature_flag_error",
			slog.String("tenant_id", tenantID.String()),
			slog.String("err", err.Error()))
		return
	}
	if !on {
		a.logger.Info("messenger.feature_flag_off", slog.String("tenant_id", tenantID.String()))
		return
	}
	for _, m := range entry.Messaging {
		a.deliverMessage(ctx, tenantID, pageID, m)
	}
}

// deliverMessage maps one envelope message into an inbox.InboundEvent
// and hands it to the use case. The sender's page-scoped id (PSID) is
// the external id propagated through the Identity hook
// (channel="messenger", external_id=psid) — see contacts.IdentityRepository.
//
// A duplicate mid is a dedup hit on the inbound_message_dedup ledger;
// the use case returns ErrInboundAlreadyProcessed which we log at
// debug only — that is the success path under carrier retry, not a
// failure to surface.
func (a *Adapter) deliverMessage(ctx context.Context, tenantID uuid.UUID, pageID string, m envelMessage) {
	mid := strings.TrimSpace(m.Message.MID)
	if mid == "" {
		// Statuses-only envelopes (echo, message_reads, etc.) reach
		// here with an empty mid — drop silently at debug level so
		// the warn-level signal stays meaningful for real bugs.
		a.logger.Debug("messenger.missing_mid",
			slog.String("tenant_id", tenantID.String()),
			slog.String("page_id", pageID))
		return
	}
	psid := strings.TrimSpace(m.Sender.ID)
	if psid == "" {
		a.logger.Warn("messenger.missing_sender_psid",
			slog.String("tenant_id", tenantID.String()),
			slog.String("mid", mid))
		return
	}
	ev := inbox.InboundEvent{
		TenantID:          tenantID,
		Channel:           Channel,
		ChannelExternalID: mid,
		SenderExternalID:  psid,
		Body:              extractBody(m.Message),
		OccurredAt:        msToTime(m.Timestamp),
	}
	deliverCtx, cancel := context.WithTimeout(ctx, a.cfg.DeliverTimeout)
	defer cancel()
	err := a.inbox.HandleInbound(deliverCtx, ev)
	if err != nil {
		if errors.Is(err, inbox.ErrInboundAlreadyProcessed) {
			a.logger.Debug("messenger.duplicate_mid",
				slog.String("tenant_id", tenantID.String()),
				slog.String("mid", mid))
			return
		}
		a.logger.Error("messenger.deliver_failed",
			slog.String("tenant_id", tenantID.String()),
			slog.String("page_id", pageID),
			slog.String("mid", mid),
			slog.String("err", err.Error()))
		return
	}
	a.logger.Info("messenger.delivered",
		slog.String("tenant_id", tenantID.String()),
		slog.String("page_id", pageID),
		slog.String("mid", mid))
	a.requestMediaScans(ctx, tenantID, mid, m.Message.Attachments)
}

// requestMediaScans publishes one media.scan.requested envelope per
// attachment. The message stays in the "pending" state until the
// MediaScanner worker delivers a clean verdict (F2-05). The storage key
// mirrors the Instagram adapter: tenantID/mid/index/type so retries are
// idempotent. MessageID is uuid.Nil for Fase 2 — message-id wiring
// lands in the follow-up post-clean re-materialisation PR.
func (a *Adapter) requestMediaScans(ctx context.Context, tenantID uuid.UUID, mid string, atts []envelMsgAttachment) {
	if len(atts) == 0 {
		return
	}
	if a.media == nil {
		a.logger.Warn("messenger.media_publisher_unwired",
			slog.String("tenant_id", tenantID.String()),
			slog.String("mid", mid),
			slog.Int("attachments", len(atts)))
		return
	}
	for i, att := range atts {
		key := mediaKey(tenantID, mid, i, att.Type)
		if err := a.media.PublishScanRequest(ctx, tenantID, uuid.Nil, key); err != nil {
			a.logger.Warn("messenger.media_scan_publish_failed",
				slog.String("tenant_id", tenantID.String()),
				slog.String("mid", mid),
				slog.Int("attachment_index", i),
				slog.String("err", err.Error()))
			continue
		}
		a.logger.Info("messenger.media_scan_requested",
			slog.String("tenant_id", tenantID.String()),
			slog.String("mid", mid),
			slog.Int("attachment_index", i),
			slog.String("type", att.Type))
	}
}

// mediaKey produces a deterministic storage key fragment for an inbound
// attachment. Format mirrors the Instagram adapter so the MediaScanner
// worker can process both channels uniformly.
func mediaKey(tenantID uuid.UUID, mid string, index int, attType string) string {
	t := strings.TrimSpace(attType)
	if t == "" {
		t = "attachment"
	}
	return strings.Join([]string{"messenger", tenantID.String(), mid, fmt.Sprintf("%d", index), t}, "/")
}

// timestampWindowDirection returns "" when entryTimeMs falls inside
// [now-past, now+skew], or "past" / "future" labels for breaches.
// Messenger entry timestamps are milliseconds (the WhatsApp adapter
// gets seconds via entry[].time); the function isolates that
// difference in one place.
func timestampWindowDirection(entryTimeMs int64, now time.Time, past, skew time.Duration) string {
	if entryTimeMs <= 0 {
		return ""
	}
	ts := time.UnixMilli(entryTimeMs).UTC()
	if ts.Before(now.Add(-past)) {
		return "past"
	}
	if ts.After(now.Add(skew)) {
		return "future"
	}
	return ""
}

// extractBody returns the best-effort body string for one inbound
// message. Text messages carry the body directly; attachment-only
// messages get a deterministic "[type]" placeholder so the inbox
// conversation keeps a non-empty body row (the domain rejects empty
// bodies on NewMessage). Rich media handling lives in a follow-up PR
// per the package doc-comment.
func extractBody(m envelMsgBody) string {
	if body := strings.TrimSpace(m.Text); body != "" {
		return body
	}
	if len(m.Attachments) > 0 {
		t := strings.TrimSpace(m.Attachments[0].Type)
		if t == "" {
			t = "attachment"
		}
		return "[" + t + "]"
	}
	return "[empty]"
}

// msToTime converts the Meta-encoded millisecond timestamp into a UTC
// time.Time. Non-positive inputs return a zero time, which the inbox
// use case treats as "use the receiver's now".
func msToTime(ms int64) time.Time {
	if ms <= 0 {
		return time.Time{}
	}
	return time.UnixMilli(ms).UTC()
}

// writeAck sends the empty 200 OK Meta expects. Content-Type is JSON
// for consistency with the WhatsApp handler; Meta does not inspect
// the body.
func writeAck(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"status":"ok"}`))
}

// hashHex returns a short hex digest for a log-correlation field. We
// log the digest only (never the body); the digest is non-reversible
// so it stays safe even on retention-heavy log sinks.
func hashHex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:16])
}
