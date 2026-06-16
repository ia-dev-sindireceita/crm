package webchat

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/pericles-luz/crm/internal/inbox"
	"github.com/pericles-luz/crm/internal/tenancy"
)

type sessionResp struct {
	SessionID string    `json:"session_id"`
	CSRFToken string    `json:"csrf_token"`
	ExpiresAt time.Time `json:"expires_at"`
}

// handleSession implements POST /widget/v1/session.
// Steps: feature flag → rate limit → CORS/origin allowlist →
// origin signature → generate session + CSRF token → respond.
func (a *Adapter) handleSession(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	tenant, err := tenancy.FromContext(ctx)
	if err != nil {
		http.Error(w, "", http.StatusBadRequest)
		return
	}
	tenantID := tenant.ID

	on, err := a.flag.Enabled(ctx, tenantID)
	if err != nil || !on {
		http.NotFound(w, r)
		return
	}

	origin := strings.TrimSpace(r.Header.Get("Origin"))
	if origin == "" {
		http.Error(w, "", http.StatusForbidden)
		return
	}

	// Rate limit: 10 new sessions / min / ip+tenant (ADR-0021 D5).
	// ipHash = sha256(ip || tenant_id); the plaintext IP never leaves
	// this scope, so the value persisted on the session row (and used
	// as the rate-limit key) is LGPD-safe.
	ip := clientIP(r)
	ipHash := fmt.Sprintf("%x", sha256.Sum256([]byte(ip+tenantID.String())))
	ipKey := "wc.sess." + tenantID.String() + "." + ipHash
	if ok, after, _ := a.rl.Allow(ctx, ipKey); !ok {
		w.Header().Set("Retry-After", fmt.Sprintf("%d", int(after.Seconds())))
		http.Error(w, "", http.StatusTooManyRequests)
		return
	}

	// /24 anti-sybil bucket (ADR-0021 D5): 200 session-creates / min per
	// (tenant × IPv4 /24). Stops a single subnet from rotating IPs to
	// dodge the per-IP cap above. net_hash = sha256(network || tenant_id)
	// keeps it LGPD-safe — the network prefix never persists in plaintext.
	netHash := fmt.Sprintf("%x", sha256.Sum256([]byte(networkBucket(ip)+tenantID.String())))
	netKey := "wc.s24." + tenantID.String() + "." + netHash
	if ok, after, _ := a.rl.Allow(ctx, netKey); !ok {
		w.Header().Set("Retry-After", fmt.Sprintf("%d", int(after.Seconds())))
		http.Error(w, "", http.StatusTooManyRequests)
		return
	}

	valid, err := a.origins.Valid(ctx, tenantID, origin)
	if err != nil || !valid {
		http.Error(w, "", http.StatusForbidden)
		return
	}
	originSig, err := a.origins.HMAC(ctx, tenantID, origin)
	if err != nil {
		http.Error(w, "", http.StatusForbidden)
		return
	}

	sessID := uuid.Must(uuid.NewV7()).String()
	csrfRaw := make([]byte, 32)
	if _, err := rand.Read(csrfRaw); err != nil {
		http.Error(w, "", http.StatusInternalServerError)
		return
	}
	csrfToken := base64.RawURLEncoding.EncodeToString(csrfRaw)
	now := time.Now().UTC()
	sess := Session{
		ID:            sessID,
		TenantID:      tenantID,
		CSRFTokenHash: hashToken(csrfToken),
		OriginSig:     originSig,
		IPHash:        ipHash,
		ExpiresAt:     now.Add(sessionTTL),
	}
	if err := a.sessions.Create(ctx, sess); err != nil {
		http.Error(w, "", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", origin)
	w.Header().Set("Vary", "Origin")
	w.Header().Set("Content-Security-Policy", "default-src 'none'")
	_ = json.NewEncoder(w).Encode(sessionResp{
		SessionID: sessID,
		CSRFToken: csrfToken,
		ExpiresAt: sess.ExpiresAt,
	})
}

type messageReq struct {
	Body        string `json:"body"`
	ClientMsgID string `json:"client_msg_id"`
	Email       string `json:"email,omitempty"`
	Phone       string `json:"phone,omitempty"`
}

// handleMessage implements POST /widget/v1/message.
// Steps: read headers → load session → CSRF check → rate limit →
// parse body → deliver to inbox → optional identity signal update.
func (a *Adapter) handleMessage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	sessID := r.Header.Get(HeaderSession)
	csrfPresented := r.Header.Get(HeaderCSRF)
	if sessID == "" || csrfPresented == "" {
		http.Error(w, "", http.StatusUnauthorized)
		return
	}

	sess, err := a.sessions.Get(ctx, sessID)
	if err != nil || time.Now().UTC().After(sess.ExpiresAt) {
		http.Error(w, "", http.StatusUnauthorized)
		return
	}

	// Constant-time CSRF verification (double-submit, no cookies).
	if !hmac.Equal([]byte(hashToken(csrfPresented)), []byte(sess.CSRFTokenHash)) {
		http.Error(w, "", http.StatusForbidden)
		return
	}

	on, err := a.flag.Enabled(ctx, sess.TenantID)
	if err != nil || !on {
		http.NotFound(w, r)
		return
	}

	// Rate limit: 60 msgs / min / session (ADR-0021 D5).
	if ok, after, _ := a.rl.Allow(ctx, "wc.msg."+sessID); !ok {
		w.Header().Set("Retry-After", fmt.Sprintf("%d", int(after.Seconds())))
		http.Error(w, "", http.StatusTooManyRequests)
		return
	}

	raw, _ := io.ReadAll(io.LimitReader(r.Body, 4096))
	var req messageReq
	if err := json.Unmarshal(raw, &req); err != nil || strings.TrimSpace(req.Body) == "" || req.ClientMsgID == "" {
		http.Error(w, "", http.StatusBadRequest)
		return
	}

	deliverCtx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	ev := inbox.InboundEvent{
		TenantID:          sess.TenantID,
		Channel:           Channel,
		ChannelExternalID: req.ClientMsgID,
		SenderExternalID:  sessID,
		Body:              req.Body,
		OccurredAt:        time.Now().UTC(),
	}
	if err := a.inbox.HandleInbound(deliverCtx, ev); err != nil &&
		!errors.Is(err, inbox.ErrInboundAlreadyProcessed) {
		a.logger.Error("webchat.deliver_failed", "session_id", sessID, "err", err)
		http.Error(w, "", http.StatusInternalServerError)
		return
	}

	if a.contacts != nil && (req.Email != "" || req.Phone != "") {
		_ = a.contacts.UpdateSignals(ctx, sess.TenantID, sessID, req.Phone, req.Email)
	}
	_ = a.sessions.Touch(ctx, sessID)
	w.WriteHeader(http.StatusNoContent)
}

// handleStream implements GET /widget/v1/stream (SSE).
// Reconnection via Last-Event-ID is acknowledged but replay requires
// the Postgres session store (follow-up).
//
// The session id may arrive via the X-Webchat-Session header (preferred
// for server-to-server callers and Go integration tests) or via the
// session_id query parameter (required for browser EventSource clients,
// which cannot set custom request headers). The CSRF token is not
// required for the read-only stream — it gates POSTs only.
func (a *Adapter) handleStream(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	sessID := r.Header.Get(HeaderSession)
	if sessID == "" {
		sessID = r.URL.Query().Get("session_id")
	}
	if sessID == "" {
		http.Error(w, "", http.StatusUnauthorized)
		return
	}
	sess, err := a.sessions.Get(ctx, sessID)
	if err != nil || time.Now().UTC().After(sess.ExpiresAt) {
		http.Error(w, "", http.StatusUnauthorized)
		return
	}
	on, err := a.flag.Enabled(ctx, sess.TenantID)
	if err != nil || !on {
		http.NotFound(w, r)
		return
	}

	// Stream-entry rate limit (ADR-0021 D5): bound the connect/reconnect
	// rate per session. The concurrency caps below stop simultaneous
	// streams; this throttles an open→close loop that never holds more
	// than one stream at a time and so would dodge the concurrency cap.
	if ok, after, _ := a.rl.Allow(ctx, "wc.stream."+sessID); !ok {
		w.Header().Set("Retry-After", fmt.Sprintf("%d", int(after.Seconds())))
		http.Error(w, "", http.StatusTooManyRequests)
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "", http.StatusInternalServerError)
		return
	}

	// Concurrency caps (ADR-0021 D5, threat T5): 1 stream / session_id,
	// 5 / (tenant × IP). Reject excess BEFORE writing the 200 so the
	// client sees a clean 429 instead of an aborted event-stream. The
	// per-IP bucket reuses the session's LGPD-safe ip_hash.
	sub, ok := a.broker.Subscribe(sessID, sess.IPHash)
	if !ok {
		w.Header().Set("Retry-After", "5")
		http.Error(w, "", http.StatusTooManyRequests)
		return
	}
	defer a.broker.Unsubscribe(sessID, sess.IPHash, sub)

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("Content-Security-Policy", "default-src 'none'")
	// Flush headers now so the client's Do() unblocks before we enter
	// the subscription loop. Without this explicit flush the client
	// blocks waiting for response headers until the first event arrives,
	// causing a deadlock in tests.
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	for {
		select {
		case <-ctx.Done():
			return
		case payload, ok := <-sub:
			if !ok {
				return
			}
			fmt.Fprintf(w, "data: %s\n\n", payload)
			flusher.Flush()
		}
	}
}

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// networkBucket reduces an IP to its anti-sybil network prefix: the /24
// for IPv4, the /48 for IPv6 (a single allocation in practice). The
// result feeds the LGPD-safe net_hash, never persisting in plaintext.
// An unparseable address falls back to itself so it is still bucketed.
func networkBucket(ipStr string) string {
	ip := net.ParseIP(ipStr)
	if ip == nil {
		return ipStr
	}
	if v4 := ip.To4(); v4 != nil {
		return v4.Mask(net.CIDRMask(24, 32)).String()
	}
	return ip.Mask(net.CIDRMask(48, 128)).String()
}

// clientIP returns the caller's IP as the rate-limit + ip_hash key
// source. It trusts ONLY r.RemoteAddr and never reads the client-supplied
// X-Real-IP / X-Forwarded-For / True-Client-IP headers directly.
//
// SIN-64991 (trust-boundary follow-up of SIN-64986 / OWASP A05): the
// widget routes are mounted in the tenanted group, which inherits the
// router-root trusted-proxy RealIP wrapper (httpapi.NewTrustedRealIP,
// SIN-62978). That wrapper has already rewritten r.RemoteAddr from the
// forwarded-identity headers IFF the immediate TCP peer is inside the
// trusted-proxy CIDR allowlist (Caddy edge), and stripped those headers
// otherwise. Re-reading X-Real-IP here would reintroduce the per-IP
// rate-limit bypass (D5): a caller not behind the Caddy edge could spoof
// X-Real-IP per request to partition the rate-limit bucket and the
// ip_hash session key. By keying off r.RemoteAddr alone, an untrusted
// peer is always seen as its raw TCP source.
//
// net.SplitHostPort strips the port for the common "ip:port" RemoteAddr
// (incl. bracketed IPv6 "[::1]:5555"); a bare IP — which chimw.RealIP
// writes when it honours a trusted header — has no port to split, so we
// return it unchanged instead of truncating an IPv6 address at the last
// colon.
func clientIP(r *http.Request) string {
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}
