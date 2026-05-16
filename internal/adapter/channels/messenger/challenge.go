package messenger

import "net/http"

// handleChallenge implements Meta's webhook subscription verification
// handshake. Meta sends:
//
//	GET /webhooks/messenger?hub.mode=subscribe&hub.verify_token=<t>&hub.challenge=<c>
//
// We echo hub.challenge as a 200 plain-text response iff
// hub.verify_token matches cfg.VerifyToken AND hub.mode is "subscribe".
// Any mismatch returns 403 — the verify-token handshake is the only
// path where the carrier expects a non-200 on a routine probe.
func (a *Adapter) handleChallenge(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if q.Get("hub.mode") != "subscribe" || q.Get("hub.verify_token") != a.cfg.VerifyToken {
		w.WriteHeader(http.StatusForbidden)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(q.Get("hub.challenge")))
}
