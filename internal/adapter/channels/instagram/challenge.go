package instagram

import (
	"crypto/subtle"
	"log/slog"
	"net/http"
)

// handleChallenge implements the Meta subscription handshake:
// GET /webhooks/instagram?hub.mode=subscribe&hub.verify_token=...&hub.challenge=...
// On a verify-token match we echo hub.challenge back as plain text; any
// mismatch returns 403 with an empty body. Mode values other than
// "subscribe" return 400.
//
// Verify-token comparison uses subtle.ConstantTimeCompare so a
// timing-side-channel attacker cannot recover the token byte by byte.
func (a *Adapter) handleChallenge(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	mode := q.Get("hub.mode")
	if mode != "subscribe" {
		a.logger.Warn("instagram.challenge_bad_mode",
			slog.String("mode", mode))
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	token := q.Get("hub.verify_token")
	if subtle.ConstantTimeCompare([]byte(token), []byte(a.cfg.VerifyToken)) != 1 {
		a.logger.Warn("instagram.challenge_bad_token")
		w.WriteHeader(http.StatusForbidden)
		return
	}
	challenge := q.Get("hub.challenge")
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(challenge))
}
