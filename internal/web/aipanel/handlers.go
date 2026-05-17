package aipanel

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/aipolicy"
	"github.com/pericles-luz/crm/internal/obs"
	"github.com/pericles-luz/crm/internal/tenancy"
)

// AcceptRoutePath / CancelRoutePath are the route patterns the modal
// template POSTs to. Exported so the template and the wire layer share
// one source of truth.
const (
	AcceptRoutePath = "/aipanel/consent/accept"
	CancelRoutePath = "/aipanel/consent/cancel"
)

// ConsentRecorder is the use-case port the accept handler calls.
// *aipolicy.ConsentService satisfies it structurally; the interface
// stays local so unit tests can substitute a small fake without
// dragging the consent repository in.
//
// RecordConsent receives the already-anonymized payloadPreview text —
// the handler does NOT re-anonymize at the boundary; the gate already
// did when it returned ConsentRequired. The service hashes the preview
// and persists only the digest.
type ConsentRecorder interface {
	RecordConsent(
		ctx context.Context,
		scope aipolicy.ConsentScope,
		actorUserID *uuid.UUID,
		payloadPreview, anonymizerVersion, promptVersion string,
	) error
}

// UserIDFn returns the authenticated user id for the session. uuid.Nil
// is acceptable (the consent row records actor_user_id as NULL); the
// handler does not refuse to record on a missing id.
type UserIDFn func(*http.Request) uuid.UUID

// Deps bundles the handler's collaborators. Consent + UserID are
// required; Metrics + Logger fall back to nil-safe no-ops when unset
// so unit tests don't have to wire the obs surface.
type Deps struct {
	// Consent is the aipolicy service that persists accepted consent
	// rows. Required — without it the accept route has nothing to do.
	Consent ConsentRecorder
	// UserID resolves the actor user id from the session, attached by
	// the IAM auth middleware that gates the /aipanel/* mount.
	// Required (the production wire passes a session-derived func; tests
	// substitute a constant).
	UserID UserIDFn
	// Metrics is the observability surface. nil disables emission so
	// tests can opt out of registering Prometheus counters.
	Metrics *obs.Metrics
	// Logger is the structured logger. nil falls back to slog.Default().
	Logger *slog.Logger
}

// Handler owns the consent accept/cancel endpoints.
type Handler struct {
	deps Deps
}

// New wires the Handler. Returns an error when either required
// dependency is missing; the composition root panics on that error.
func New(deps Deps) (*Handler, error) {
	if deps.Consent == nil {
		return nil, errors.New("web/aipanel: Consent is required")
	}
	if deps.UserID == nil {
		return nil, errors.New("web/aipanel: UserID is required")
	}
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	return &Handler{deps: deps}, nil
}

// Routes registers the accept + cancel endpoints on mux. Go 1.22-style
// path patterns; the mount point is the package's job — the patterns
// are absolute so the same string can live in the modal template's
// hx-post attribute.
func (h *Handler) Routes(mux *http.ServeMux) {
	mux.HandleFunc("POST "+AcceptRoutePath, h.accept)
	mux.HandleFunc("POST "+CancelRoutePath, h.cancel)
}

// accept records the operator's consent for the (tenant, scope_kind,
// scope_id) triple, then signals the client to refire the original
// aiassist request via the HX-Trigger response header.
//
// The handler enforces three boundaries before it calls the service:
//
//  1. tenant + actor come from context/session — NEVER from the body.
//  2. scope_kind is one of the closed aipolicy.ScopeType enum values.
//  3. The body's payload_hash matches sha256(payload_preview) — this
//     catches a client that swapped the preview while keeping the
//     hash, which would otherwise let it consent on a synthesized
//     payload it never showed the operator.
//
// On success the response is 200 with the cancelled-placeholder
// markup (the modal collapses) and an HX-Trigger header carrying the
// conversation id so the inbox assist button can refire its request.
func (h *Handler) accept(w http.ResponseWriter, r *http.Request) {
	tenant, err := tenancy.FromContext(r.Context())
	if err != nil {
		h.fail(w, http.StatusInternalServerError, "tenant required", err)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}

	scopeKind := strings.TrimSpace(r.PostFormValue("scope_kind"))
	scopeID := strings.TrimSpace(r.PostFormValue("scope_id"))
	anonymizerVersion := strings.TrimSpace(r.PostFormValue("anonymizer_version"))
	promptVersion := strings.TrimSpace(r.PostFormValue("prompt_version"))
	payloadHash := strings.TrimSpace(r.PostFormValue("payload_hash"))
	// payload_preview is the anonymized text the modal showed the
	// operator. The handler treats it as opaque bytes — only the
	// recomputed SHA-256 matters for the boundary check.
	payloadPreview := r.PostFormValue("payload_preview")
	conversationID := strings.TrimSpace(r.PostFormValue("conversation_id"))

	kind := aipolicy.ScopeType(scopeKind)
	if !kind.IsValid() {
		http.Error(w, "invalid scope_kind", http.StatusBadRequest)
		return
	}
	if scopeID == "" {
		http.Error(w, "scope_id required", http.StatusBadRequest)
		return
	}
	if anonymizerVersion == "" || promptVersion == "" {
		http.Error(w, "anonymizer_version and prompt_version are required", http.StatusBadRequest)
		return
	}
	if payloadHash == "" || payloadPreview == "" {
		http.Error(w, "payload_hash and payload_preview are required", http.StatusBadRequest)
		return
	}

	// Anti-tampering: recompute the digest over the supplied preview
	// and compare with the body's hash. constant-time compare keeps
	// the rejection branch indistinguishable from a timing perspective
	// (defence-in-depth — the cost of a tampering oracle here is low
	// but the constant-time pattern is free with subtle).
	got := sha256.Sum256([]byte(payloadPreview))
	wantHex := strings.ToLower(payloadHash)
	gotHex := hex.EncodeToString(got[:])
	if len(wantHex) != len(gotHex) ||
		subtle.ConstantTimeCompare([]byte(wantHex), []byte(gotHex)) != 1 {
		h.deps.Logger.Warn("web/aipanel: payload hash mismatch on consent accept",
			"tenant_id", tenant.ID, "scope_kind", scopeKind, "scope_id", scopeID)
		http.Error(w, "payload hash mismatch", http.StatusBadRequest)
		return
	}

	scope := aipolicy.ConsentScope{
		TenantID: tenant.ID,
		Kind:     kind,
		ID:       scopeID,
	}

	var actor *uuid.UUID
	if uid := h.deps.UserID(r); uid != uuid.Nil {
		copyID := uid
		actor = &copyID
	}

	if err := h.deps.Consent.RecordConsent(r.Context(), scope, actor, payloadPreview, anonymizerVersion, promptVersion); err != nil {
		h.fail(w, http.StatusInternalServerError, "record consent", err)
		return
	}

	h.deps.Metrics.AIConsent(scopeKind, obs.AIConsentOutcomeAccepted)

	// HX-Trigger lets the client refire the original aiassist request.
	// We emit a JSON-shaped trigger payload so the consumer (the inbox
	// assist button) can pick the conversation id out without parsing
	// arbitrary strings. Errors marshaling the small map are
	// impossible in practice; the empty fallback keeps the response
	// well-formed.
	if conversationID != "" {
		payload, mErr := json.Marshal(map[string]map[string]string{
			"ai-consent-accepted": {"conversation_id": conversationID},
		})
		if mErr == nil {
			w.Header().Set("HX-Trigger", string(payload))
		}
	} else {
		w.Header().Set("HX-Trigger", "ai-consent-accepted")
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	if err := cancelledPlaceholderTmpl.Execute(w, nil); err != nil {
		h.deps.Logger.Error("web/aipanel: render placeholder on accept", "err", err)
	}
}

// cancel records the cancelled outcome metric and returns the empty
// modal placeholder so HTMX's outerHTML swap collapses the dialog. No
// state crosses into aipolicy — cancelling is the no-op branch.
func (h *Handler) cancel(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	scopeKind := strings.TrimSpace(r.PostFormValue("scope_kind"))
	if scopeKind != "" {
		if !aipolicy.ScopeType(scopeKind).IsValid() {
			// Unknown scope_kind — fall through to the metric with an
			// "invalid" label is the wrong move (would inflate
			// cardinality). Drop the metric, still render the swap so
			// the modal closes for the operator.
			h.deps.Logger.Warn("web/aipanel: cancel with invalid scope_kind", "scope_kind", scopeKind)
		} else {
			h.deps.Metrics.AIConsent(scopeKind, obs.AIConsentOutcomeCancelled)
		}
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	if err := cancelledPlaceholderTmpl.Execute(w, nil); err != nil {
		h.deps.Logger.Error("web/aipanel: render placeholder on cancel", "err", err)
	}
}

// fail centralises the error log + 500 path. The response body never
// echoes err.Error() so internal detail stays out of the wire.
func (h *Handler) fail(w http.ResponseWriter, status int, msg string, err error) {
	h.deps.Logger.Error("web/aipanel: "+msg, "err", err)
	http.Error(w, http.StatusText(status), status)
}
