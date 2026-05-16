package instagram

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/adapter/channels/metashared"
	"github.com/pericles-luz/crm/internal/inbox"
)

// SignatureHeader is the Meta HMAC-SHA256 header. Re-exported from
// metashared so existing IG callers stay source-compatible.
const SignatureHeader = metashared.SignatureHeader

// igEnvelope is the permissive subset of Meta's Instagram Messaging
// webhook payload the handler consumes. Unknown fields are ignored by
// encoding/json so a future addition cannot break parsing.
//
// The wire format mirrors the Messenger Platform's `entry[].messaging[]`
// shape — NOT the WhatsApp Cloud `entry[].changes[]` shape. The two
// shapes are intentionally not aliased to a shared struct because IG
// has channel-specific fields (sender.id == IGSID, no contacts[]
// profile array, etc.) and a shared struct would invite drift.
type igEnvelope struct {
	Object string    `json:"object"`
	Entry  []igEntry `json:"entry"`
}

type igEntry struct {
	ID        string        `json:"id"` // ig business account id (the recipient)
	Time      int64         `json:"time"`
	Messaging []igMessaging `json:"messaging"`
}

type igMessaging struct {
	Sender    igParty   `json:"sender"`
	Recipient igParty   `json:"recipient"`
	Timestamp int64     `json:"timestamp"` // unix millis
	Message   igMessage `json:"message"`
}

type igParty struct {
	ID string `json:"id"`
}

type igMessage struct {
	MID         string         `json:"mid"`
	Text        string         `json:"text"`
	Attachments []igAttachment `json:"attachments"`
	IsEcho      bool           `json:"is_echo"`
}

type igAttachment struct {
	Type    string              `json:"type"`
	Payload igAttachmentPayload `json:"payload"`
}

type igAttachmentPayload struct {
	URL string `json:"url"`
}

// handlePost is the POST /webhooks/instagram handler. Steps mirror the
// whatsapp adapter (HMAC → parse → window → tenant → rate → flag →
// deliver) so the security posture is uniform across Meta channels.
// Every drop path returns 200 OK (anti-enumeration); the single non-200
// reply is 401 on HMAC failure.
func (a *Adapter) handlePost(w http.ResponseWriter, r *http.Request) {
	start := a.clock.Now()
	defer func() {
		elapsed := a.clock.Now().Sub(start)
		if elapsed < 0 {
			elapsed = 0
		}
		a.logger.Debug("instagram.handler_complete",
			slog.Int64("handler_elapsed_ms", elapsed.Milliseconds()))
	}()

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, a.cfg.MaxBodyBytes))
	if err != nil {
		a.logger.Warn("instagram.body_read_failed", slog.String("err", err.Error()))
		writeAck(w)
		return
	}
	if err := metashared.VerifySignature(a.cfg.AppSecret, body, r.Header.Get(SignatureHeader)); err != nil {
		a.logger.Warn("instagram.signature_invalid",
			slog.String("body_sha256", hashHex(body)),
			slog.String("err", err.Error()))
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	var env igEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		a.logger.Warn("instagram.parse_failed",
			slog.String("body_sha256", hashHex(body)),
			slog.String("err", err.Error()))
		writeAck(w)
		return
	}
	if dir := a.timestampWindowDirection(&env, a.clock.Now()); dir != "" {
		a.logger.Warn("instagram.timestamp_outside_window",
			slog.String("body_sha256", hashHex(body)),
			slog.String("direction", dir))
		writeAck(w)
		return
	}
	for _, entry := range env.Entry {
		a.deliverEntry(r.Context(), entry)
	}
	writeAck(w)
}

// deliverEntry resolves the tenant for one entry[] block and dispatches
// each messaging[] item. The entry id is the IG Business Account id and
// the tenant resolution key. Pre-message drops (unknown tenant,
// rate-limited, feature off) log and return; the HTTP layer still acks
// 200.
func (a *Adapter) deliverEntry(ctx context.Context, entry igEntry) {
	if len(entry.Messaging) == 0 {
		return
	}
	igBusinessID := strings.TrimSpace(entry.ID)
	if igBusinessID == "" {
		a.logger.Warn("instagram.missing_ig_business_id")
		return
	}
	tenantID, err := a.tenants.Resolve(ctx, igBusinessID)
	if err != nil {
		a.logger.Info("instagram.unknown_ig_business_id",
			slog.String("ig_business_id", igBusinessID))
		return
	}
	allowed, retryAfter, err := a.rate.Allow(ctx, rateLimitKey(igBusinessID),
		time.Minute, a.cfg.RateMaxPerMin)
	if err != nil {
		a.logger.Warn("instagram.rate_limiter_error",
			slog.String("ig_business_id", igBusinessID),
			slog.String("err", err.Error()))
		return
	}
	if !allowed {
		a.logger.Warn("instagram.rate_limited",
			slog.String("ig_business_id", igBusinessID),
			slog.Duration("retry_after", retryAfter))
		return
	}
	on, err := a.flag.Enabled(ctx, tenantID)
	if err != nil {
		a.logger.Warn("instagram.feature_flag_error",
			slog.String("tenant_id", tenantID.String()),
			slog.String("err", err.Error()))
		return
	}
	if !on {
		a.logger.Info("instagram.feature_flag_off",
			slog.String("tenant_id", tenantID.String()))
		return
	}
	for _, m := range entry.Messaging {
		a.deliverMessage(ctx, tenantID, igBusinessID, m)
	}
}

// deliverMessage maps one messaging[] item to inbox.InboundEvent and
// hands it to the use case. Echo messages (outbound originated by us,
// looped back by Meta) are dropped early; the inbox already records
// outbound from the sender path.
func (a *Adapter) deliverMessage(ctx context.Context, tenantID uuid.UUID, igBusinessID string, m igMessaging) {
	if m.Message.IsEcho {
		a.logger.Debug("instagram.echo_dropped",
			slog.String("tenant_id", tenantID.String()),
			slog.String("mid", m.Message.MID))
		return
	}
	mid := strings.TrimSpace(m.Message.MID)
	if mid == "" {
		a.logger.Warn("instagram.missing_mid",
			slog.String("tenant_id", tenantID.String()),
			slog.String("ig_business_id", igBusinessID))
		return
	}
	igsid := strings.TrimSpace(m.Sender.ID)
	if igsid == "" {
		a.logger.Warn("instagram.missing_sender",
			slog.String("tenant_id", tenantID.String()),
			slog.String("mid", mid))
		return
	}
	ev := inbox.InboundEvent{
		TenantID:          tenantID,
		Channel:           Channel,
		ChannelExternalID: mid,
		SenderExternalID:  igsid,
		Body:              extractBody(m.Message),
		OccurredAt:        parseMillis(m.Timestamp),
	}
	deliverCtx, cancel := context.WithTimeout(ctx, a.cfg.DeliverTimeout)
	defer cancel()
	if err := a.inbox.HandleInbound(deliverCtx, ev); err != nil {
		if errors.Is(err, inbox.ErrInboundAlreadyProcessed) {
			a.logger.Debug("instagram.duplicate_mid",
				slog.String("tenant_id", tenantID.String()),
				slog.String("mid", mid))
			return
		}
		a.logger.Error("instagram.deliver_failed",
			slog.String("tenant_id", tenantID.String()),
			slog.String("ig_business_id", igBusinessID),
			slog.String("mid", mid),
			slog.String("err", err.Error()))
		return
	}
	a.logger.Info("instagram.delivered",
		slog.String("tenant_id", tenantID.String()),
		slog.String("ig_business_id", igBusinessID),
		slog.String("mid", mid))
	a.requestMediaScans(ctx, tenantID, mid, m.Message.Attachments)
}

// requestMediaScans publishes one media.scan.requested envelope per
// attachment. We use mid (deterministic per attachment row index) as
// the storage key fragment so retries land on the same row when the
// inbox use case later re-materialises the attachment list. The
// MessageID passed to the publisher is uuid.Nil for Fase 2 — message-id
// wiring lands in the follow-up "post-clean re-materialisation" PR
// (the inbox use case does not yet return ids on HandleInbound).
func (a *Adapter) requestMediaScans(ctx context.Context, tenantID uuid.UUID, mid string, atts []igAttachment) {
	if len(atts) == 0 {
		return
	}
	if a.media == nil {
		a.logger.Warn("instagram.media_publisher_unwired",
			slog.String("tenant_id", tenantID.String()),
			slog.String("mid", mid),
			slog.Int("attachments", len(atts)))
		return
	}
	for i, att := range atts {
		key := mediaKey(tenantID, mid, i, att.Type)
		if err := a.media.PublishScanRequest(ctx, tenantID, uuid.Nil, key); err != nil {
			a.logger.Warn("instagram.media_scan_publish_failed",
				slog.String("tenant_id", tenantID.String()),
				slog.String("mid", mid),
				slog.Int("attachment_index", i),
				slog.String("err", err.Error()))
			continue
		}
		a.logger.Info("instagram.media_scan_requested",
			slog.String("tenant_id", tenantID.String()),
			slog.String("mid", mid),
			slog.Int("attachment_index", i),
			slog.String("type", att.Type))
	}
}

// timestampWindowDirection returns "" when every entry[].time falls
// inside [now-PastWindow, now+FutureSkew], or the direction label
// ("past" / "future") of the first breach. Meta sets entry[].time in
// unix seconds.
func (a *Adapter) timestampWindowDirection(env *igEnvelope, now time.Time) string {
	if env == nil || len(env.Entry) == 0 {
		return ""
	}
	pastBound := now.Add(-a.cfg.PastWindow)
	futureBound := now.Add(a.cfg.FutureSkew)
	for _, e := range env.Entry {
		if e.Time <= 0 {
			continue
		}
		ts := time.Unix(e.Time, 0).UTC()
		if ts.Before(pastBound) {
			return "past"
		}
		if ts.After(futureBound) {
			return "future"
		}
	}
	return ""
}

// writeAck sends the empty 200 Meta expects.
func writeAck(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"status":"ok"}`))
}

// hashHex returns a short hex digest for a log-correlation field.
func hashHex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:16])
}

// rateLimitKey scopes the rate-limit counter by ig_business_id so a
// flood against an unknown account cannot starve resolved tenants.
func rateLimitKey(igBusinessID string) string {
	return "instagram:ig:" + igBusinessID
}

// extractBody returns the best-effort text payload for one inbound
// message. Text wins; attachment-only messages get a deterministic
// placeholder (one per attachment type) so the inbox conversation keeps
// a non-empty body row.
func extractBody(m igMessage) string {
	if body := strings.TrimSpace(m.Text); body != "" {
		return body
	}
	if len(m.Attachments) == 0 {
		return "[empty]"
	}
	t := strings.TrimSpace(m.Attachments[0].Type)
	if t == "" {
		t = "media"
	}
	return "[" + t + "]"
}

// parseMillis converts Meta's unix-millis timestamp into UTC. Zero / negative
// returns a zero time which the inbox use case treats as "use now".
func parseMillis(ms int64) time.Time {
	if ms <= 0 {
		return time.Time{}
	}
	return time.Unix(0, ms*int64(time.Millisecond)).UTC()
}

// mediaKey builds the deterministic storage-key fragment used in the
// media.scan.requested envelope. Keying on (tenant, mid, attachment
// index, type) keeps retries idempotent in the scanner ledger.
func mediaKey(tenantID uuid.UUID, mid string, idx int, typ string) string {
	cleanType := strings.TrimSpace(typ)
	if cleanType == "" {
		cleanType = "media"
	}
	return tenantID.String() + "/instagram/" + mid + "/" + cleanType + "/" + itoa(idx)
}

// itoa is a tiny zero-alloc int-to-string for non-negative attachment
// indices. We avoid strconv to keep this file's import list flat
// (every import here is a hot loop concern; we already have strings).
func itoa(n int) string {
	if n < 0 {
		n = 0
	}
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
