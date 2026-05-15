package whatsapp

import (
	"context"
	"crypto/hmac"
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

	"github.com/pericles-luz/crm/internal/inbox"
)

// SignatureHeader is the Meta-defined header carrying the HMAC-SHA256
// signature in lowercase hex. The "sha256=" prefix is optional in the
// wire format but ubiquitous; the verifier strips it case-sensitively
// because Meta documents lowercase.
const (
	SignatureHeader = "X-Hub-Signature-256"
	signaturePrefix = "sha256="
)

// ErrUnknownPhoneNumberID is returned by TenantResolver implementations
// when no tenant_channel_associations row matches the supplied
// phone_number_id. The handler treats this as a silent drop.
var ErrUnknownPhoneNumberID = errors.New("whatsapp: unknown phone_number_id")

// metaEnvelope is the permissive subset of the Meta Cloud API webhook
// payload the handler consumes. Fields we do not need (statuses,
// contact profiles beyond the optional name fallback) are intentionally
// absent so an upstream schema change in those areas cannot break
// JSON parsing — encoding/json ignores unknown fields by default.
type metaEnvelope struct {
	Object string       `json:"object"`
	Entry  []envelEntry `json:"entry"`
}

type envelEntry struct {
	ID      string        `json:"id"`
	Time    int64         `json:"time"`
	Changes []envelChange `json:"changes"`
}

type envelChange struct {
	Field string     `json:"field"`
	Value envelValue `json:"value"`
}

type envelValue struct {
	Metadata envelMetadata  `json:"metadata"`
	Contacts []envelContact `json:"contacts"`
	Messages []envelMessage `json:"messages"`
}

type envelMetadata struct {
	PhoneNumberID string `json:"phone_number_id"`
	DisplayPhone  string `json:"display_phone_number"`
}

type envelContact struct {
	WaID    string              `json:"wa_id"`
	Profile envelContactProfile `json:"profile"`
}

type envelContactProfile struct {
	Name string `json:"name"`
}

// envelMessage carries the minimum we need for an inbound text message
// — id (wamid), from (E.164 sender), timestamp (Unix seconds as a
// string per Meta), type (we treat anything that is not "text" as a
// non-text inbound and store its sub-payload's hint as body), and the
// text body when present. Media messages route via type="image" /
// "audio" / etc.; Fase 1 stores a stub body so the conversation
// thread is unbroken, and a richer media handler ships in a later PR.
type envelMessage struct {
	ID        string            `json:"id"`
	From      string            `json:"from"`
	Timestamp string            `json:"timestamp"`
	Type      string            `json:"type"`
	Text      envelMessageText  `json:"text"`
	Errors    []envelMessageErr `json:"errors"`
}

type envelMessageText struct {
	Body string `json:"body"`
}

type envelMessageErr struct {
	Code  int    `json:"code"`
	Title string `json:"title"`
}

// handlePost is the POST /webhooks/whatsapp handler. Steps mirror the
// issue spec 1-7 in order; every drop path returns 200 OK so an
// attacker probing the endpoint cannot distinguish failure modes
// (anti-enumeration). The single non-200 response is 401 on HMAC
// verification failure: the carrier needs that to surface a
// misconfigured app secret in the Meta dashboard.
func (a *Adapter) handlePost(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, a.cfg.MaxBodyBytes))
	if err != nil {
		a.logger.Warn("whatsapp.body_read_failed", slog.String("err", err.Error()))
		writeAck(w)
		return
	}
	if !a.verifySignature(r.Header.Get(SignatureHeader), body) {
		a.logger.Warn("whatsapp.signature_invalid",
			slog.String("body_sha256", hashHex(body)))
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	var env metaEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		a.logger.Warn("whatsapp.parse_failed",
			slog.String("body_sha256", hashHex(body)),
			slog.String("err", err.Error()))
		writeAck(w)
		return
	}
	if !a.timestampInWindow(&env, a.clock.Now()) {
		a.logger.Warn("whatsapp.timestamp_outside_window",
			slog.String("body_sha256", hashHex(body)))
		writeAck(w)
		return
	}
	for _, entry := range env.Entry {
		for _, change := range entry.Changes {
			a.deliverChange(r.Context(), change)
		}
	}
	writeAck(w)
}

// deliverChange routes one entry[].changes[] block: it resolves the
// tenant from phone_number_id, applies the rate limit and feature
// flag, then delivers each inner message through the InboundChannel
// port. Errors at any point are logged but never propagate to the
// HTTP layer — Meta is acknowledged regardless.
func (a *Adapter) deliverChange(ctx context.Context, change envelChange) {
	if len(change.Value.Messages) == 0 {
		return
	}
	pnID := strings.TrimSpace(change.Value.Metadata.PhoneNumberID)
	if pnID == "" {
		a.logger.Warn("whatsapp.missing_phone_number_id")
		return
	}
	tenantID, err := a.tenants.Resolve(ctx, pnID)
	if err != nil {
		// Silent drop — anti-enumeration. The log line stays at info
		// so an unknown number does not spam warn-level dashboards.
		a.logger.Info("whatsapp.unknown_phone_number_id",
			slog.String("phone_number_id", pnID))
		return
	}
	allowed, retryAfter, err := a.rate.Allow(ctx, rateLimitKey(pnID),
		time.Minute, a.cfg.RateMaxPerMin)
	if err != nil {
		a.logger.Warn("whatsapp.rate_limiter_error",
			slog.String("phone_number_id", pnID),
			slog.String("err", err.Error()))
		return
	}
	if !allowed {
		a.logger.Warn("whatsapp.rate_limited",
			slog.String("phone_number_id", pnID),
			slog.Duration("retry_after", retryAfter))
		return
	}
	on, err := a.flag.Enabled(ctx, tenantID)
	if err != nil {
		a.logger.Warn("whatsapp.feature_flag_error",
			slog.String("tenant_id", tenantID.String()),
			slog.String("err", err.Error()))
		return
	}
	if !on {
		a.logger.Info("whatsapp.feature_flag_off",
			slog.String("tenant_id", tenantID.String()))
		return
	}
	contactName := primaryContactName(change.Value.Contacts)
	for _, msg := range change.Value.Messages {
		a.deliverMessage(ctx, tenantID, pnID, contactName, msg)
	}
}

// deliverMessage maps one envelope message into an inbox.InboundEvent
// and hands it to the use case. Empty wamid is a programming error
// from Meta's side; the use case rejects empty ChannelExternalID
// anyway but the explicit check keeps the log line crisp.
func (a *Adapter) deliverMessage(ctx context.Context, tenantID uuid.UUID, pnID, contactName string, msg envelMessage) {
	wamid := strings.TrimSpace(msg.ID)
	if wamid == "" {
		a.logger.Warn("whatsapp.missing_wamid",
			slog.String("tenant_id", tenantID.String()),
			slog.String("phone_number_id", pnID))
		return
	}
	from := strings.TrimSpace(msg.From)
	if from == "" {
		a.logger.Warn("whatsapp.missing_from",
			slog.String("tenant_id", tenantID.String()),
			slog.String("wamid", wamid))
		return
	}
	ev := inbox.InboundEvent{
		TenantID:          tenantID,
		Channel:           Channel,
		ChannelExternalID: wamid,
		SenderExternalID:  from,
		SenderDisplayName: contactName,
		Body:              extractBody(msg),
		OccurredAt:        parseMetaTimestamp(msg.Timestamp),
	}
	deliverCtx, cancel := context.WithTimeout(ctx, a.cfg.DeliverTimeout)
	defer cancel()
	if err := a.inbox.HandleInbound(deliverCtx, ev); err != nil {
		if errors.Is(err, inbox.ErrInboundAlreadyProcessed) {
			// Domain-level dedup hit: this is the success path under
			// retry, not a failure to surface. Log at debug only.
			a.logger.Debug("whatsapp.duplicate_wamid",
				slog.String("tenant_id", tenantID.String()),
				slog.String("wamid", wamid))
			return
		}
		a.logger.Error("whatsapp.deliver_failed",
			slog.String("tenant_id", tenantID.String()),
			slog.String("phone_number_id", pnID),
			slog.String("wamid", wamid),
			slog.String("err", err.Error()))
		return
	}
	a.logger.Info("whatsapp.delivered",
		slog.String("tenant_id", tenantID.String()),
		slog.String("phone_number_id", pnID),
		slog.String("wamid", wamid))
}

// verifySignature compares the supplied X-Hub-Signature-256 hex
// digest against HMAC-SHA256(body, AppSecret). The comparison is
// constant-time via hmac.Equal. Returns false for missing/blank
// headers, bad hex, or mismatched bytes.
func (a *Adapter) verifySignature(headerVal string, body []byte) bool {
	got := strings.TrimSpace(headerVal)
	if got == "" {
		return false
	}
	got = strings.TrimPrefix(got, signaturePrefix)
	gotBytes, err := hex.DecodeString(got)
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, []byte(a.cfg.AppSecret))
	_, _ = mac.Write(body)
	return hmac.Equal(gotBytes, mac.Sum(nil))
}

// timestampInWindow accepts any entry[].time that falls within
// [now-PastWindow, now+FutureSkew]. The window is the envelope-level
// freshness check from ADR 0075 §D3 — we trust Meta's clock here
// because Meta signs the envelope, including the timestamp, with the
// app secret. Empty entries return true so we still get one log line
// per parse-error envelope (rare in practice but defensive).
func (a *Adapter) timestampInWindow(env *metaEnvelope, now time.Time) bool {
	if env == nil || len(env.Entry) == 0 {
		return true
	}
	pastBound := now.Add(-a.cfg.PastWindow)
	futureBound := now.Add(a.cfg.FutureSkew)
	for _, e := range env.Entry {
		if e.Time <= 0 {
			continue
		}
		ts := time.Unix(e.Time, 0).UTC()
		if ts.Before(pastBound) || ts.After(futureBound) {
			return false
		}
	}
	return true
}

// writeAck sends the empty 200 Meta expects. Content-Type is JSON for
// consistency with the existing ADR 0075 webhook handler; Meta does
// not inspect the body.
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
	return hex.EncodeToString(sum[:16]) // 32 hex chars is enough for correlation
}

// rateLimitKey scopes the rate-limit counter by phone_number_id. We
// avoid using tenant_id here because the resolution happens AFTER the
// rate-limit check so a flood of unknown phone_number_ids cannot
// starve legitimate tenants on the resolver lookup.
func rateLimitKey(phoneNumberID string) string {
	return "whatsapp:pn:" + phoneNumberID
}

// extractBody returns the best-effort text payload for one inbound
// message. Text messages carry the body directly; media / system
// messages get a deterministic placeholder so the inbox conversation
// keeps a non-empty body row (the domain rejects empty bodies on
// NewMessage). Media handling proper lands in a follow-up PR.
func extractBody(m envelMessage) string {
	if body := strings.TrimSpace(m.Text.Body); body != "" {
		return body
	}
	switch m.Type {
	case "", "text":
		return "[empty]"
	default:
		return "[" + m.Type + "]"
	}
}

// parseMetaTimestamp turns the Meta-encoded Unix-seconds-as-string
// into a UTC time.Time. Empty / unparsable inputs return a zero time,
// which the inbox use case treats as "use the receiver's now".
func parseMetaTimestamp(s string) time.Time {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}
	}
	var n int64
	for _, c := range s {
		if c < '0' || c > '9' {
			return time.Time{}
		}
		n = n*10 + int64(c-'0')
		if n > 99999999999 {
			return time.Time{}
		}
	}
	return time.Unix(n, 0).UTC()
}

// primaryContactName returns the first non-empty contacts[].profile.name
// in the envelope, or "" when none is supplied. The fallback empty
// string lets the inbox use case derive a name from the sender phone.
func primaryContactName(cs []envelContact) string {
	for _, c := range cs {
		if name := strings.TrimSpace(c.Profile.Name); name != "" {
			return name
		}
	}
	return ""
}
