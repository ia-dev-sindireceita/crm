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
// payload the handler consumes. Fields we do not need (contact
// profiles beyond the optional name fallback, conversation/pricing
// blocks under statuses[]) are intentionally absent so an upstream
// schema change in those areas cannot break JSON parsing —
// encoding/json ignores unknown fields by default.
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
	Statuses []envelStatus  `json:"statuses"`
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
//
// Observability invariant (SIN-62762): every exit emits a terminal
// `whatsapp.handler_complete` slog line with `result` and
// `handler_elapsed_ms`, and observes the matching label on the
// `whatsapp_handler_elapsed_seconds` histogram. The runbook at
// docs/runbooks/whatsapp-inbound-latency.md gates the queue-and-ACK
// redesign on this signal — see SIN-62762 etapa 2.
func (a *Adapter) handlePost(w http.ResponseWriter, r *http.Request) {
	start := a.clock.Now()
	result := "dropped_other"
	defer func() {
		elapsed := a.clock.Now().Sub(start)
		if elapsed < 0 {
			elapsed = 0
		}
		a.logger.Info("whatsapp.handler_complete",
			slog.String("result", result),
			slog.Int64("handler_elapsed_ms", elapsed.Milliseconds()))
		a.handlerMetrics.observe(result, elapsed)
	}()

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, a.cfg.MaxBodyBytes))
	if err != nil {
		result = "dropped_body_read"
		a.logger.Warn("whatsapp.body_read_failed", slog.String("err", err.Error()))
		writeAck(w)
		return
	}
	if !a.verifySignature(r.Header.Get(SignatureHeader), body) {
		result = "dropped_signature"
		a.logger.Warn("whatsapp.signature_invalid",
			slog.String("body_sha256", hashHex(body)))
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	var env metaEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		result = "dropped_parse"
		a.logger.Warn("whatsapp.parse_failed",
			slog.String("body_sha256", hashHex(body)),
			slog.String("err", err.Error()))
		writeAck(w)
		return
	}
	if dir := a.timestampWindowDirection(&env, a.clock.Now()); dir != "" {
		result = "dropped_timestamp_window"
		a.timestampWindowDrop(Channel, dir)
		a.logger.Warn("whatsapp.timestamp_outside_window",
			slog.String("body_sha256", hashHex(body)),
			slog.String("direction", dir))
		writeAck(w)
		return
	}
	var agg handlerAgg
	for _, entry := range env.Entry {
		for _, change := range entry.Changes {
			a.deliverChange(r.Context(), change, &agg)
		}
	}
	result = agg.result()
	writeAck(w)
}

// handlerAgg accumulates per-message outcomes during the synchronous
// delivery loop in handlePost. It exists so the terminal result label
// is a deterministic priority-resolved string regardless of how many
// messages or changes the envelope packs.
//
// Priority order (most-productive wins): delivered > duplicate >
// dropped_deliver_error > status_processed > dropped_tenant >
// dropped_rate_limited > dropped_feature_off > dropped_other >
// dropped_empty. Rationale: histograms partitioned by terminal label
// should bucket "did this handler do any productive work" first so
// SLO dashboards can split "we processed" from "we dropped" without
// summing across labels. status_processed sits just below the
// inbound-message labels so a status-only envelope is still
// recognised as productive work; per-status outcomes ride a separate
// counter (whatsapp_status_total).
type handlerAgg struct {
	delivered       int
	duplicate       int
	deliverErrors   int
	statusesHandled int
	tenantDrops     int
	rateLimited     int
	flagOff         int
	otherDrops      int
}

func (s *handlerAgg) result() string {
	switch {
	case s.delivered > 0:
		return "delivered"
	case s.duplicate > 0:
		return "duplicate"
	case s.deliverErrors > 0:
		return "dropped_deliver_error"
	case s.statusesHandled > 0:
		return "status_processed"
	case s.tenantDrops > 0:
		return "dropped_tenant"
	case s.rateLimited > 0:
		return "dropped_rate_limited"
	case s.flagOff > 0:
		return "dropped_feature_off"
	case s.otherDrops > 0:
		return "dropped_other"
	default:
		return "dropped_empty"
	}
}

// deliverChange routes one entry[].changes[] block: it resolves the
// tenant from phone_number_id, applies the rate limit and feature
// flag, then delivers each inner message through the InboundChannel
// port and each statuses[] entry through the MessageStatusUpdater
// seam. Errors at any point are logged but never propagate to the
// HTTP layer — Meta is acknowledged regardless.
//
// agg accumulates the per-message outcomes so handlePost can resolve a
// single terminal result label for the histogram. A pre-message drop
// (unknown tenant, rate-limited, feature off) charges one count per
// inner message AND per inner status so the result label reflects
// "this many envelope entries were dropped" — keeping the
// agg.otherDrops > 0 check meaningful for status-only envelopes too.
func (a *Adapter) deliverChange(ctx context.Context, change envelChange, agg *handlerAgg) {
	entryCount := len(change.Value.Messages) + len(change.Value.Statuses)
	if entryCount == 0 {
		return
	}
	pnID := strings.TrimSpace(change.Value.Metadata.PhoneNumberID)
	if pnID == "" {
		a.logger.Warn("whatsapp.missing_phone_number_id")
		agg.otherDrops += entryCount
		return
	}
	tenantID, err := a.tenants.Resolve(ctx, pnID)
	if err != nil {
		// Silent drop — anti-enumeration. The log line stays at info
		// so an unknown number does not spam warn-level dashboards.
		a.logger.Info("whatsapp.unknown_phone_number_id",
			slog.String("phone_number_id", pnID))
		agg.tenantDrops += entryCount
		return
	}
	allowed, retryAfter, err := a.rate.Allow(ctx, rateLimitKey(pnID),
		time.Minute, a.cfg.RateMaxPerMin)
	if err != nil {
		a.logger.Warn("whatsapp.rate_limiter_error",
			slog.String("phone_number_id", pnID),
			slog.String("err", err.Error()))
		agg.otherDrops += entryCount
		return
	}
	if !allowed {
		a.logger.Warn("whatsapp.rate_limited",
			slog.String("phone_number_id", pnID),
			slog.Duration("retry_after", retryAfter))
		agg.rateLimited += entryCount
		return
	}
	on, err := a.flag.Enabled(ctx, tenantID)
	if err != nil {
		a.logger.Warn("whatsapp.feature_flag_error",
			slog.String("tenant_id", tenantID.String()),
			slog.String("err", err.Error()))
		agg.otherDrops += entryCount
		return
	}
	if !on {
		a.logger.Info("whatsapp.feature_flag_off",
			slog.String("tenant_id", tenantID.String()))
		agg.flagOff += entryCount
		return
	}
	contactName := primaryContactName(change.Value.Contacts)
	for _, msg := range change.Value.Messages {
		a.deliverMessage(ctx, tenantID, pnID, contactName, msg, agg)
	}
	for _, st := range change.Value.Statuses {
		a.deliverStatus(ctx, tenantID, pnID, st, agg)
	}
}

// deliverMessage maps one envelope message into an inbox.InboundEvent
// and hands it to the use case. Empty wamid is a programming error
// from Meta's side; the use case rejects empty ChannelExternalID
// anyway but the explicit check keeps the log line crisp.
//
// Every exit emits `deliver_elapsed_ms` on the relevant slog line
// (delivered / duplicate / failed) so log-only dashboards can derive
// per-message latency without scraping the handler-level histogram.
// agg records the outcome for the handler-level result aggregation.
func (a *Adapter) deliverMessage(ctx context.Context, tenantID uuid.UUID, pnID, contactName string, msg envelMessage, agg *handlerAgg) {
	wamid := strings.TrimSpace(msg.ID)
	if wamid == "" {
		a.logger.Warn("whatsapp.missing_wamid",
			slog.String("tenant_id", tenantID.String()),
			slog.String("phone_number_id", pnID))
		agg.otherDrops++
		return
	}
	from := strings.TrimSpace(msg.From)
	if from == "" {
		a.logger.Warn("whatsapp.missing_from",
			slog.String("tenant_id", tenantID.String()),
			slog.String("wamid", wamid))
		agg.otherDrops++
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
	deliverStart := a.clock.Now()
	err := a.inbox.HandleInbound(deliverCtx, ev)
	deliverElapsed := a.clock.Now().Sub(deliverStart)
	if deliverElapsed < 0 {
		deliverElapsed = 0
	}
	if err != nil {
		if errors.Is(err, inbox.ErrInboundAlreadyProcessed) {
			// Domain-level dedup hit: this is the success path under
			// retry, not a failure to surface. Log at debug only.
			a.logger.Debug("whatsapp.duplicate_wamid",
				slog.String("tenant_id", tenantID.String()),
				slog.String("wamid", wamid),
				slog.Int64("deliver_elapsed_ms", deliverElapsed.Milliseconds()))
			agg.duplicate++
			return
		}
		a.logger.Error("whatsapp.deliver_failed",
			slog.String("tenant_id", tenantID.String()),
			slog.String("phone_number_id", pnID),
			slog.String("wamid", wamid),
			slog.Int64("deliver_elapsed_ms", deliverElapsed.Milliseconds()),
			slog.String("err", err.Error()))
		agg.deliverErrors++
		return
	}
	a.logger.Info("whatsapp.delivered",
		slog.String("tenant_id", tenantID.String()),
		slog.String("phone_number_id", pnID),
		slog.String("wamid", wamid),
		slog.Int64("deliver_elapsed_ms", deliverElapsed.Milliseconds()))
	agg.delivered++
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

// timestampWindowDirection returns "" when every entry[].time falls
// within [now-PastWindow, now+FutureSkew], or the direction label of
// the first breach: "past" (older than PastWindow) or "future"
// (beyond FutureSkew). The window enforces the freshness check from
// ADR 0075 §D3 widened by ADR 0094 §3 — we trust Meta's clock here
// because Meta signs the envelope, including the timestamp, with the
// app secret. Empty entries return "" so we still get one log line
// per parse-error envelope (rare in practice but defensive).
//
// The direction label drives webhook_timestamp_window_drop_total
// (ADR 0094 §3.1). "past" bursts indicate Meta retry budget exceeded
// or a captured-body replay attempt at scale; "future" bursts
// indicate clock skew on our side or a future-pivoted replay.
func (a *Adapter) timestampWindowDirection(env *metaEnvelope, now time.Time) string {
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
