package pix

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"math"
	"net"
	"net/http"
	"strconv"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	pixinter "github.com/pericles-luz/crm/internal/adapter/pix/inter"
	domainpix "github.com/pericles-luz/crm/internal/billing/pix"
	domainratelimit "github.com/pericles-luz/crm/internal/iam/ratelimit"
)

// MaxBodyBytes caps inbound webhook bodies. Inter's PIX cobrança
// callbacks rarely exceed a few kilobytes; the 256 KiB cap absorbs the
// fattest plausible batch (a few hundred items) while bounding the
// memory footprint of a single misbehaving caller.
const MaxBodyBytes = 256 << 10

// Outcome enumerates the terminal classification of one webhook
// request. Names appear in structured logs; keep them stable so the
// dashboards stay queryable.
type Outcome string

const (
	OutcomeApplied          Outcome = "applied"
	OutcomeDedupHit         Outcome = "dedup_hit"
	OutcomeMixed            Outcome = "mixed"
	OutcomeSignatureFail    Outcome = "signature_fail"
	OutcomeIPFail           Outcome = "ip_fail"
	OutcomeRateLimitFail    Outcome = "rate_limit_fail"
	OutcomeParseFail        Outcome = "parse_fail"
	OutcomeReconcilerErr    Outcome = "reconciler_err"
	OutcomeBodyReadFail     Outcome = "body_read_fail"
	OutcomeMethodNotAllowed Outcome = "method_not_allowed"
)

// InterWebhookConfig wires the InterWebhookHandler. All fields with
// no default are required; New returns an error if any is missing.
type InterWebhookConfig struct {
	// Verifier is the HMAC verifier built from the wiring's
	// PIX_INTER_WEBHOOK_SECRET. Required.
	Verifier *pixinter.WebhookVerifier
	// Parser is the body normaliser. Required.
	Parser *pixinter.WebhookParser
	// Reconciler is the inbound port that applies state transitions.
	// Required.
	Reconciler domainpix.Reconciler
	// Limiter throttles inbound requests per IP and per external_id.
	// Required — the AC mandates 100/min/IP and 5/min/external_id;
	// fail-open is rejected because the dedup ledger alone does not
	// protect against a flood of unique txids.
	Limiter domainratelimit.RateLimiter

	// AllowedCIDRs is the parsed IP allowlist. Empty slice + IPCheck
	// enabled = deny every request (deny-by-default).
	AllowedCIDRs []*net.IPNet
	// IPCheckDisabled flips the allowlist into best-effort mode for
	// troubleshooting. WARN is logged on every request with this set
	// (so an operator who flipped it cannot forget).
	IPCheckDisabled bool

	// RatePerIPPerMin and RatePerExternalIDPerMin are the AC caps.
	// Zero falls back to the AC defaults (100 and 5 respectively) so
	// a misconfig defaults to the stricter posture.
	RatePerIPPerMin         int
	RatePerExternalIDPerMin int

	// Logger receives one InfoContext per request with the outcome
	// label and request id. nil falls back to slog.Default.
	Logger *slog.Logger

	// Tracer is optional; nil disables span emission. The wiring
	// passes otel.Tracer("…/transport/http/pix") so the request
	// spans line up with the existing PIX surface.
	Tracer trace.Tracer

	// Now is the clock for structured-log timestamps; nil falls back
	// to time.Now (UTC).
	Now func() time.Time

	// MetricsHook is optional. Production wires this to a Prometheus
	// counter; tests inspect the recorded labels.
	MetricsHook func(outcome Outcome)
}

// InterWebhookHandler is the HTTP boundary for POST /webhooks/pix/inter.
type InterWebhookHandler struct {
	verifier        *pixinter.WebhookVerifier
	parser          *pixinter.WebhookParser
	reconciler      domainpix.Reconciler
	limiter         domainratelimit.RateLimiter
	allowed         []*net.IPNet
	ipDisabled      bool
	ratePerIP       int
	ratePerExternal int
	logger          *slog.Logger
	tracer          trace.Tracer
	now             func() time.Time
	metrics         func(outcome Outcome)
}

// NewInterWebhookHandler validates cfg and returns a ready handler.
func NewInterWebhookHandler(cfg InterWebhookConfig) (*InterWebhookHandler, error) {
	if cfg.Verifier == nil {
		return nil, errors.New("pix/http: Verifier is required")
	}
	if cfg.Parser == nil {
		return nil, errors.New("pix/http: Parser is required")
	}
	if cfg.Reconciler == nil {
		return nil, errors.New("pix/http: Reconciler is required")
	}
	if cfg.Limiter == nil {
		return nil, errors.New("pix/http: Limiter is required")
	}
	rateIP := cfg.RatePerIPPerMin
	if rateIP <= 0 {
		rateIP = 100
	}
	rateExt := cfg.RatePerExternalIDPerMin
	if rateExt <= 0 {
		rateExt = 5
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	tracer := cfg.Tracer
	if tracer == nil {
		tracer = otel.Tracer("github.com/pericles-luz/crm/internal/adapter/transport/http/pix")
	}
	now := cfg.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	if cfg.IPCheckDisabled {
		logger.Warn("pix.inter.webhook: IP allowlist DISABLED via configuration; relying on signature + rate limit alone — restore PIX_INTER_WEBHOOK_IP_CHECK once troubleshooting is over")
	}
	return &InterWebhookHandler{
		verifier:        cfg.Verifier,
		parser:          cfg.Parser,
		reconciler:      cfg.Reconciler,
		limiter:         cfg.Limiter,
		allowed:         cfg.AllowedCIDRs,
		ipDisabled:      cfg.IPCheckDisabled,
		ratePerIP:       rateIP,
		ratePerExternal: rateExt,
		logger:          logger,
		tracer:          tracer,
		now:             now,
		metrics:         cfg.MetricsHook,
	}, nil
}

// Register mounts the handler on a Go 1.22 stdlib mux at the canonical
// path. The mux pattern is method-scoped so a stray GET / OPTIONS
// drops to 405 inside ServeHTTP rather than colliding with a sibling
// catch-all.
func (h *InterWebhookHandler) Register(mux *http.ServeMux) {
	mux.Handle("POST /webhooks/pix/inter", h)
}

// ServeHTTP enforces the AC-required defense-in-depth chain. Order is
// deliberate: every layer in front of HMAC is cheap, so they reject
// floods without paying the SHA-256 cost; HMAC keeps the limiter
// counter from being filled by unauthenticated noise.
func (h *InterWebhookHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ctx, span := h.tracer.Start(r.Context(), "pix.inter.webhook", trace.WithAttributes(
		attribute.String("psp", pixinter.SourceName),
	))
	defer span.End()

	requestID := r.Header.Get("X-Request-Id")
	if r.Method != http.MethodPost {
		h.respond(w, requestID, "", "", OutcomeMethodNotAllowed, http.StatusMethodNotAllowed, "", nil)
		return
	}

	peerIP := remotePeer(r.RemoteAddr)

	// 1) IP allowlist. Skipped when ops flipped IPCheckDisabled; the
	// constructor already logged the WARN so we do not re-log per
	// request.
	if !h.ipDisabled {
		if !pixinter.IPAllow(peerIP, h.allowed) {
			h.respond(w, requestID, peerIPString(peerIP), "", OutcomeIPFail, http.StatusForbidden, "ip not allowlisted", nil)
			return
		}
	}

	// 2) Per-IP rate limit. Uses the peer's IP literal so a single
	// flooder cannot eat the budget for the rest of the allowlist.
	if peerIP != nil {
		key := "pix:inter:ip:" + peerIP.String()
		allowed, retryAfter, err := h.limiter.Allow(ctx, key, time.Minute, h.ratePerIP)
		if err == nil && !allowed {
			h.writeRetryAfter(w, retryAfter)
			h.respond(w, requestID, peerIPString(peerIP), "", OutcomeRateLimitFail, http.StatusTooManyRequests, "ip throttled", nil)
			return
		}
		if err != nil {
			h.logger.WarnContext(ctx, "pix.inter.webhook: rate limiter error (failing open)",
				slog.String("scope", "ip"),
				slog.String("err", err.Error()),
			)
		}
	}

	// 3) Body read. Cap is enforced via http.MaxBytesReader so an
	// oversize payload short-circuits before the HMAC pays the read
	// cost.
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxBodyBytes))
	if err != nil {
		h.respond(w, requestID, peerIPString(peerIP), "", OutcomeBodyReadFail, http.StatusRequestEntityTooLarge, "body read", err)
		return
	}

	// 4) HMAC signature. The verifier returns the typed
	// missing/invalid sentinels so the log can distinguish them; the
	// HTTP outcome is the same 401 either way (AC #2).
	if vErr := h.verifier.Verify(body, r.Header); vErr != nil {
		h.respond(w, requestID, peerIPString(peerIP), "", OutcomeSignatureFail, http.StatusUnauthorized, vErr.Error(), nil)
		return
	}

	// 5) Body parse. Failures map to 400 — the signature was valid so
	// this is a misconfig (Inter sending us an unsupported shape) and
	// the operator should see it loud and clear.
	events, pErr := h.parser.Parse(body)
	if pErr != nil {
		h.respond(w, requestID, peerIPString(peerIP), "", OutcomeParseFail, http.StatusBadRequest, pErr.Error(), nil)
		return
	}

	// 6) Per-event work: external_id rate limit → reconciler.Apply.
	// We track outcomes per-event and report the aggregate at the end
	// — an entirely deduped batch logs as dedup_hit, a fully applied
	// batch as applied, anything else as mixed.
	var appliedCount, dedupCount int
	for _, evt := range events {
		extKey := "pix:inter:ext:" + evt.ExternalID
		allowed, retryAfter, lErr := h.limiter.Allow(ctx, extKey, time.Minute, h.ratePerExternal)
		if lErr == nil && !allowed {
			h.writeRetryAfter(w, retryAfter)
			h.respond(w, requestID, peerIPString(peerIP), evt.ExternalID, OutcomeRateLimitFail, http.StatusTooManyRequests, "external_id throttled", nil)
			return
		}
		if lErr != nil {
			h.logger.WarnContext(ctx, "pix.inter.webhook: rate limiter error (failing open)",
				slog.String("scope", "external_id"),
				slog.String("external_id", evt.ExternalID),
				slog.String("err", lErr.Error()),
			)
		}
		out, rErr := h.reconciler.Apply(ctx, evt)
		if rErr != nil {
			// Surface the failure as 500 so the PSP retries. The
			// dedup ledger guarantees the retry is safe.
			h.respond(w, requestID, peerIPString(peerIP), evt.ExternalID, OutcomeReconcilerErr, http.StatusInternalServerError, rErr.Error(), rErr)
			return
		}
		if out.Duplicate {
			dedupCount++
		} else {
			appliedCount++
		}
	}

	var aggregate Outcome
	switch {
	case appliedCount > 0 && dedupCount == 0:
		aggregate = OutcomeApplied
	case appliedCount == 0 && dedupCount > 0:
		aggregate = OutcomeDedupHit
	case appliedCount > 0 && dedupCount > 0:
		aggregate = OutcomeMixed
	default:
		// events is non-empty by the parser contract; this branch is
		// unreachable but keeps the switch exhaustive.
		aggregate = OutcomeApplied
	}
	h.respond(w, requestID, peerIPString(peerIP), firstExternalID(events), aggregate, http.StatusOK, "", nil)
}

// respond renders the HTTP response and the structured log line. The
// receiver responds with an empty body on every path; outcome
// classification flows through the log + the optional metrics hook.
//
// The error parameter is included in the log only — it is NEVER echoed
// to the HTTP body so a malformed signature cannot bounce off our
// /webhooks reply.
func (h *InterWebhookHandler) respond(
	w http.ResponseWriter,
	requestID, peer, externalID string,
	outcome Outcome,
	status int,
	reason string,
	err error,
) {
	if h.metrics != nil {
		h.metrics(outcome)
	}
	attrs := []any{
		slog.String("psp", pixinter.SourceName),
		slog.String("outcome", string(outcome)),
		slog.Int("status", status),
		slog.String("request_id", redactRequestID(requestID)),
	}
	if peer != "" {
		attrs = append(attrs, slog.String("peer_ip", peer))
	}
	if externalID != "" {
		attrs = append(attrs, slog.String("external_id", externalID))
	}
	if reason != "" {
		attrs = append(attrs, slog.String("reason", reason))
	}
	if err != nil {
		attrs = append(attrs, slog.String("err", err.Error()))
	}
	level := slog.LevelInfo
	if status >= 500 {
		level = slog.LevelError
	}
	h.logger.Log(context.Background(), level, "pix.inter.webhook", attrs...)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(`{}`))
}

// writeRetryAfter renders an RFC 7231 Retry-After delta-seconds header
// rounded UP. retryAfter <= 0 still emits "1" so clients see a
// positive value.
func (h *InterWebhookHandler) writeRetryAfter(w http.ResponseWriter, retryAfter time.Duration) {
	seconds := int64(math.Ceil(retryAfter.Seconds()))
	if seconds < 1 {
		seconds = 1
	}
	w.Header().Set("Retry-After", strconv.FormatInt(seconds, 10))
}

// remotePeer extracts the IP portion of r.RemoteAddr — host:port,
// [v6]:port, or a bare IP. Returns nil on parse failure so the
// allowlist check denies.
func remotePeer(addr string) net.IP {
	if addr == "" {
		return nil
	}
	host := addr
	if h, _, err := net.SplitHostPort(addr); err == nil {
		host = h
	}
	return net.ParseIP(host)
}

// peerIPString returns peer.String() for non-nil peers, "" otherwise.
// Keeps the structured log free of the literal "nil" string from
// fmt.Sprint(nil).
func peerIPString(peer net.IP) string {
	if peer == nil {
		return ""
	}
	return peer.String()
}

// redactRequestID truncates a client-supplied request id to a bounded
// length so a 64 KiB X-Request-Id header (well-formed under the
// proxy's policy but useless to operators) does not stretch the log
// line into the megabytes.
func redactRequestID(id string) string {
	const max = 128
	if len(id) > max {
		return id[:max]
	}
	return id
}

func firstExternalID(events []domainpix.WebhookEvent) string {
	if len(events) == 0 {
		return ""
	}
	return events[0].ExternalID
}
