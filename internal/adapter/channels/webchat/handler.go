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
	ipKey := fmt.Sprintf("wc.sess.%s.%x", tenantID, sha256.Sum256([]byte(clientIP(r)+tenantID.String())))
	if ok, after, _ := a.rl.Allow(ctx, ipKey); !ok {
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
func (a *Adapter) handleStream(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	sessID := r.Header.Get(HeaderSession)
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
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "", http.StatusInternalServerError)
		return
	}

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

	sub := a.broker.Subscribe(sessID)
	defer a.broker.Unsubscribe(sessID, sub)

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

func clientIP(r *http.Request) string {
	if ip := r.Header.Get("X-Real-IP"); ip != "" {
		return ip
	}
	addr := r.RemoteAddr
	if i := strings.LastIndex(addr, ":"); i != -1 {
		return addr[:i]
	}
	return addr
}
