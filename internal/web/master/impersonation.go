package master

// SIN-63958 / master-impersonation-spec §1.4: three master-console
// handlers that drive the impersonation envelope.
//
//   POST /master/tenants/{id}/impersonate  → Start
//   POST /master/impersonation/end         → End
//   GET  /master/impersonation/feed        → Feed (SSE)
//
// The router (internal/adapter/httpapi/router.go) wraps each with the
// appropriate gate (Start: RequireAction; End/Feed: RequireRoleMaster)
// and (where applicable) ImpersonationFromSession. The handler itself
// trusts the resolved iam.Principal and limits itself to validation,
// repository orchestration, audit write, and HTTP response shaping.

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/adapter/httpapi/middleware"
	"github.com/pericles-luz/crm/internal/adapter/httpapi/sessioncookie"
	"github.com/pericles-luz/crm/internal/iam"
	"github.com/pericles-luz/crm/internal/iam/audit"
	"github.com/pericles-luz/crm/internal/iam/impersonation"
	"github.com/pericles-luz/crm/internal/tenancy"
)

// ImpersonationDeps bundles the collaborators the impersonation handler
// needs. Kept separate from the master.Deps so a deploy that skips
// impersonation can still wire the rest of the master console.
type ImpersonationDeps struct {
	Sessions impersonation.Repo
	Auditor  audit.SplitLogger
	Tenants  tenancy.ByIDResolver
	Logger   *slog.Logger
	Clock    func() time.Time
	// FeedPollInterval is how often the Feed SSE handler re-reads the
	// audit_log_security tail. Defaults to 1s (spec §3.4) when zero.
	FeedPollInterval time.Duration
	// FeedHeartbeat is how often the Feed handler emits an SSE comment
	// line to keep proxies happy. Defaults to 15s when zero.
	FeedHeartbeat time.Duration
}

// ImpersonationHandler is the three-method handler for the Start / End
// / Feed endpoints. Construct with NewImpersonationHandler so missing
// required deps surface a clear error at boot.
type ImpersonationHandler struct {
	deps             ImpersonationDeps
	clock            func() time.Time
	feedPollInterval time.Duration
	feedHeartbeat    time.Duration
}

// NewImpersonationHandler validates inputs and returns the handler. The
// constructor uses errors (not panics) so cmd/server fails the boot
// with a useful message rather than crashing inside the wireup.
func NewImpersonationHandler(deps ImpersonationDeps) (*ImpersonationHandler, error) {
	if deps.Sessions == nil {
		return nil, errors.New("web/master: ImpersonationHandler Sessions is required")
	}
	if deps.Auditor == nil {
		return nil, errors.New("web/master: ImpersonationHandler Auditor is required")
	}
	if deps.Tenants == nil {
		return nil, errors.New("web/master: ImpersonationHandler Tenants is required")
	}
	clock := deps.Clock
	if clock == nil {
		clock = func() time.Time { return time.Now().UTC() }
	}
	logger := deps.Logger
	if logger == nil {
		logger = slog.Default()
	}
	deps.Logger = logger
	poll := deps.FeedPollInterval
	if poll <= 0 {
		poll = time.Second
	}
	hb := deps.FeedHeartbeat
	if hb <= 0 {
		hb = 15 * time.Second
	}
	return &ImpersonationHandler{
		deps:             deps,
		clock:            clock,
		feedPollInterval: poll,
		feedHeartbeat:    hb,
	}, nil
}

// Start handles POST /master/tenants/{id}/impersonate. Validates the
// `reason` form field (8..500 chars), opens an envelope via
// Sessions.Start, writes the impersonation_start audit row (audit
// failure rolls the envelope back), and 303-redirects to /master/tenants.
//
// Behaviour:
//
//   - missing principal                    → 500.
//   - missing/invalid id path param        → 400.
//   - unknown tenant                       → 404.
//   - reason < 8 or > 500                  → 422.
//   - master cookie absent                 → 503 (RequireAuth + master
//     cookie always required to
//     start impersonation).
//   - impersonation.ErrAlreadyActive       → 409.
//   - impersonation.ErrInvalidReason       → 422 (defense in depth).
//   - audit write fails                    → 500 (envelope ended back out
//     so no row outlives the
//     missing audit trail).
//   - success                              → 303 → /master/tenants.
func (h *ImpersonationHandler) Start(w http.ResponseWriter, r *http.Request) {
	p, ok := iam.PrincipalFromContext(r.Context())
	if !ok {
		http.Error(w, "principal missing", http.StatusInternalServerError)
		return
	}
	tenantID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "invalid tenant id", http.StatusBadRequest)
		return
	}
	masterSessionID, ok := h.readMasterSessionID(r)
	if !ok {
		http.Error(w, "master session required", http.StatusServiceUnavailable)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	reason := strings.TrimSpace(r.FormValue("reason"))
	if n := len(reason); n < impersonation.MinReasonLen || n > impersonation.MaxReasonLen {
		http.Error(w, fmt.Sprintf("reason must be %d..%d characters", impersonation.MinReasonLen, impersonation.MaxReasonLen), http.StatusUnprocessableEntity)
		return
	}
	if _, err := h.deps.Tenants.ResolveByID(r.Context(), tenantID); err != nil {
		if errors.Is(err, tenancy.ErrTenantNotFound) {
			http.Error(w, "tenant not found", http.StatusNotFound)
			return
		}
		h.deps.Logger.ErrorContext(r.Context(), "impersonation start: tenant resolve",
			slog.String("tenant_id", tenantID.String()),
			slog.String("error", err.Error()),
		)
		http.Error(w, "tenant resolve failed", http.StatusServiceUnavailable)
		return
	}
	now := h.clock()
	sess, err := h.deps.Sessions.Start(r.Context(), impersonation.StartInput{
		MasterUserID:    p.UserID,
		MasterSessionID: masterSessionID,
		TargetTenantID:  tenantID,
		Reason:          reason,
		StartedAt:       now,
	})
	switch {
	case errors.Is(err, impersonation.ErrAlreadyActive):
		http.Error(w, "an impersonation envelope is already active for this master session", http.StatusConflict)
		return
	case errors.Is(err, impersonation.ErrInvalidReason):
		http.Error(w, "reason failed validation", http.StatusUnprocessableEntity)
		return
	case err != nil:
		h.deps.Logger.ErrorContext(r.Context(), "impersonation start: insert",
			slog.String("user_id", p.UserID.String()),
			slog.String("tenant_id", tenantID.String()),
			slog.String("error", err.Error()),
		)
		http.Error(w, "impersonation start failed", http.StatusInternalServerError)
		return
	}
	if err := h.deps.Auditor.WriteSecurity(r.Context(), audit.SecurityAuditEvent{
		Event:         audit.SecurityEventImpersonationStart,
		ActorUserID:   p.UserID,
		TenantID:      &tenantID,
		CorrelationID: &sess.ID,
		Target: map[string]any{
			"tenant_id":         tenantID.String(),
			"master_session_id": masterSessionID.String(),
			"reason":            reason,
			"expires_at":        sess.ExpiresAt.UTC().Format(time.RFC3339Nano),
		},
		OccurredAt: now,
	}); err != nil {
		// Audit-blocks-action: undo the envelope so a session can
		// never proceed without a recorded start row. End under
		// "audit_failed" so the post-mortem trail still has the
		// impersonation_session row, just marked ended.
		_ = h.deps.Sessions.End(r.Context(), sess.ID, p.UserID, "audit_failed", now)
		h.deps.Logger.ErrorContext(r.Context(), "impersonation start: audit write",
			slog.String("session_id", sess.ID.String()),
			slog.String("error", err.Error()),
		)
		http.Error(w, "audit write failed", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, h.safeRedirect(r, "/master/tenants"), http.StatusSeeOther)
}

// End handles POST /master/impersonation/end. Always 303 → /master/tenants;
// callable even after expiry (the route MUST NOT be behind
// ImpersonationFromSession — see spec §1.4 / AC #3 from SIN-63955).
// Idempotent: no active envelope is treated as success.
func (h *ImpersonationHandler) End(w http.ResponseWriter, r *http.Request) {
	p, ok := iam.PrincipalFromContext(r.Context())
	if !ok {
		http.Error(w, "principal missing", http.StatusInternalServerError)
		return
	}
	masterSessionID, ok := h.readMasterSessionID(r)
	if !ok {
		http.Error(w, "master session required", http.StatusServiceUnavailable)
		return
	}
	active, err := h.deps.Sessions.ActiveForSession(r.Context(), masterSessionID)
	if err != nil {
		if errors.Is(err, impersonation.ErrNoActiveImpersonation) {
			// Idempotent — no envelope to end.
			http.Redirect(w, r, "/master/tenants", http.StatusSeeOther)
			return
		}
		h.deps.Logger.ErrorContext(r.Context(), "impersonation end: lookup",
			slog.String("master_session_id", masterSessionID.String()),
			slog.String("error", err.Error()),
		)
		http.Error(w, "impersonation lookup failed", http.StatusServiceUnavailable)
		return
	}
	now := h.clock()
	endedRace := false
	if err := h.deps.Sessions.End(r.Context(), active.ID, p.UserID, "manual", now); err != nil {
		if !errors.Is(err, impersonation.ErrNoActiveImpersonation) {
			h.deps.Logger.ErrorContext(r.Context(), "impersonation end: update",
				slog.String("session_id", active.ID.String()),
				slog.String("error", err.Error()),
			)
			http.Error(w, "impersonation end failed", http.StatusInternalServerError)
			return
		}
		// Already ended between our lookup and End — treat as
		// idempotent success and skip the audit write: whichever
		// branch ended the envelope (expiry middleware, concurrent
		// End) already wrote its own impersonation_stop row, so
		// emitting another one here would double-count.
		endedRace = true
	}
	if !endedRace {
		tenantID := active.TargetTenantID
		_ = h.deps.Auditor.WriteSecurity(r.Context(), audit.SecurityAuditEvent{
			Event:         audit.SecurityEventImpersonationStop,
			ActorUserID:   p.UserID,
			TenantID:      &tenantID,
			CorrelationID: &active.ID,
			Target: map[string]any{
				"reason":      "manual",
				"tenant_id":   tenantID.String(),
				"duration_ms": now.Sub(active.StartedAt).Milliseconds(),
			},
			OccurredAt: now,
		})
	}
	http.Redirect(w, r, "/master/tenants", http.StatusSeeOther)
}

// Feed handles GET /master/impersonation/feed. Streams every
// audit_log_security row tagged with the active envelope's id as SSE
// events, in occurred_at-ascending order, polled every
// FeedPollInterval. Heartbeat comments fire every FeedHeartbeat to
// keep proxies happy.
//
//   - principal not master OR not envelope owner    → 403.
//   - no active envelope                            → 204.
//   - server-side error before first frame          → 503.
//
// The stream terminates when the client disconnects OR the envelope
// ends (active row's ended_at flips non-null on the next poll).
func (h *ImpersonationHandler) Feed(w http.ResponseWriter, r *http.Request) {
	p, ok := iam.PrincipalFromContext(r.Context())
	if !ok {
		http.Error(w, "principal missing", http.StatusInternalServerError)
		return
	}
	if !p.IsMaster() {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	masterSessionID, ok := h.readMasterSessionID(r)
	if !ok {
		http.Error(w, "master session required", http.StatusServiceUnavailable)
		return
	}
	active, err := h.deps.Sessions.ActiveForSession(r.Context(), masterSessionID)
	if err != nil {
		if errors.Is(err, impersonation.ErrNoActiveImpersonation) {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		h.deps.Logger.ErrorContext(r.Context(), "impersonation feed: lookup",
			slog.String("master_session_id", masterSessionID.String()),
			slog.String("error", err.Error()),
		)
		http.Error(w, "impersonation lookup failed", http.StatusServiceUnavailable)
		return
	}
	if active.MasterUserID != p.UserID {
		// Owner check — only the master who *opened* the envelope
		// can observe its private audit stream. A different master
		// who happens to authenticate as RoleMaster MUST NOT see
		// someone else's impersonation feed (spec §1.4 / §5.5 #4
		// adjacent).
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	flusher, canFlush := w.(http.Flusher)
	if !canFlush {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	h.streamFeed(w, r, flusher, active)
}

// streamFeed is the SSE loop extracted so the surrounding
// validation/permission checks stay readable. Loops until the request
// context is cancelled, the active envelope ends, or write fails.
func (h *ImpersonationHandler) streamFeed(w http.ResponseWriter, r *http.Request, flusher http.Flusher, active *impersonation.Session) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache, no-store, no-transform")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	const initialLimit = 50
	const tailLimit = 200

	seen := map[uuid.UUID]struct{}{}

	// Initial backfill — last initialLimit rows so the operator does
	// not start with an empty feed.
	rows, err := h.deps.Sessions.ListAuditByCorrelation(r.Context(), active.ID, initialLimit)
	if err != nil {
		h.deps.Logger.ErrorContext(r.Context(), "impersonation feed: initial list",
			slog.String("session_id", active.ID.String()),
			slog.String("error", err.Error()),
		)
		return
	}
	for _, row := range rows {
		if err := writeSSEEvent(w, row); err != nil {
			return
		}
		seen[row.ID] = struct{}{}
	}
	flusher.Flush()

	poll := time.NewTicker(h.feedPollInterval)
	heartbeat := time.NewTicker(h.feedHeartbeat)
	defer poll.Stop()
	defer heartbeat.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-heartbeat.C:
			if _, err := w.Write([]byte(": keepalive\n\n")); err != nil {
				return
			}
			flusher.Flush()
		case <-poll.C:
			rows, err := h.deps.Sessions.ListAuditByCorrelation(r.Context(), active.ID, tailLimit)
			if err != nil {
				return
			}
			progressed := false
			for _, row := range rows {
				if _, dup := seen[row.ID]; dup {
					continue
				}
				if err := writeSSEEvent(w, row); err != nil {
					return
				}
				seen[row.ID] = struct{}{}
				progressed = true
			}
			if progressed {
				flusher.Flush()
			}
			// Check envelope still active. ActiveForSession is the
			// cheap check; an explicit /end or expiry makes it
			// return ErrNoActiveImpersonation.
			if _, err := h.deps.Sessions.ActiveForSession(r.Context(), active.MasterSessionID); err != nil {
				if errors.Is(err, impersonation.ErrNoActiveImpersonation) {
					return
				}
			}
		}
	}
}

// readMasterSessionID lifts the master cookie. Duplicated from the
// middleware (deliberately — same one-line semantics, but the handler
// MUST NOT import middleware).
func (h *ImpersonationHandler) readMasterSessionID(r *http.Request) (uuid.UUID, bool) {
	// First, prefer the session the middleware already resolved onto
	// the context — that proves the cookie was valid for THIS request
	// AND we get the canonical id without re-parsing.
	if sess, ok := middleware.ActiveImpersonation(r.Context()); ok {
		return sess.MasterSessionID, true
	}
	raw, err := sessioncookie.Read(r, sessioncookie.NameMaster)
	if err != nil {
		return uuid.Nil, false
	}
	id, err := uuid.Parse(raw)
	if err != nil {
		return uuid.Nil, false
	}
	return id, true
}

// safeRedirect returns the Referer if it points back inside the master
// console, else the supplied default. Prevents an open redirect via a
// hostile Referer header.
func (h *ImpersonationHandler) safeRedirect(r *http.Request, fallback string) string {
	ref := r.Header.Get("Referer")
	if ref == "" {
		return fallback
	}
	// Allow only relative paths under /master/* (no scheme, no host).
	if !strings.HasPrefix(ref, "/master/") {
		return fallback
	}
	if strings.Contains(ref, "://") || strings.HasPrefix(ref, "//") {
		return fallback
	}
	return ref
}

// writeSSEEvent serialises one audit row as an SSE "data: …" frame.
// JSON marshalling keeps the frame parser on the client side simple.
func writeSSEEvent(w http.ResponseWriter, row audit.SecurityRow) error {
	payload := map[string]any{
		"id":            row.ID.String(),
		"event":         string(row.Event),
		"actor_user_id": row.ActorUserID.String(),
		"occurred_at":   row.OccurredAt.UTC().Format(time.RFC3339Nano),
		"target":        row.Target,
	}
	if row.TenantID != nil {
		payload["tenant_id"] = row.TenantID.String()
	}
	if row.CorrelationID != nil {
		payload["correlation_id"] = row.CorrelationID.String()
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "id: %s\nevent: audit\ndata: %s\n\n", row.ID.String(), body); err != nil {
		return err
	}
	return nil
}

// HandlerFunc-shaped accessors so the wire layer (router.go) can lift
// each method onto an http.Handler without exposing the receiver's
// internals.
func (h *ImpersonationHandler) StartHandler() http.Handler { return http.HandlerFunc(h.Start) }
func (h *ImpersonationHandler) EndHandler() http.Handler   { return http.HandlerFunc(h.End) }
func (h *ImpersonationHandler) FeedHandler() http.Handler  { return http.HandlerFunc(h.Feed) }
