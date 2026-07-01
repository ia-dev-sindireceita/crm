package channels

import (
	"context"
	"net/http"

	"github.com/google/uuid"
)

// AccessAuditor is the write-only port the handler uses to record a
// channel access-change privilege event (SIN-66405, from the SIN-66392 P3
// security review). Each mutation the management surface performs —
// granting a user access, revoking it, or flipping the channel's
// open↔restricted mode — is a privilege change that OWASP A09
// (logging/monitoring) requires be audit-logged before GA.
//
// The concrete adapter (composition root) routes each call into the same
// audit_log_security ledger as the other privilege events via
// audit.SplitLogger; the domain-facing port stays free of that vocabulary
// so the web layer only speaks channel/user ids. Implementations are
// best-effort by contract: an audit-write failure is logged by the
// adapter and never surfaces back here, mirroring the authz recorder —
// the mutation has already committed and observability must not fail the
// operator's request. A nil Auditor on Deps disables emission (fail-soft
// wiring), so every method must tolerate being called on the no-op path.
//
// Actor is the authenticated gerente performing the change (the route is
// ActionTenantChannelsManage-gated); tenant is the channel's tenant. The
// context carries the request deadline + any impersonation correlation id
// the adapter forwards to the ledger.
type AccessAuditor interface {
	// ChannelAccessGranted records that user was added to channel's
	// access roster.
	ChannelAccessGranted(ctx context.Context, actor, tenant, channelID, user uuid.UUID)
	// ChannelAccessRevoked records that user was removed from channel's
	// access roster.
	ChannelAccessRevoked(ctx context.Context, actor, tenant, channelID, user uuid.UUID)
	// ChannelRestrictedChanged records that channel's restricted flag
	// flipped from `from` to `to`.
	ChannelRestrictedChanged(ctx context.Context, actor, tenant, channelID uuid.UUID, from, to bool)
}

// auditAccessReplace emits one grant line per newly-added user and one
// revoke line per removed user, diffing the before/after roster of a
// full-replace write. It is a no-op when the handler has no Auditor wired
// or no authenticated actor is resolvable (the audit_log_security actor
// column is NOT NULL — a missing actor is skipped, never written as nil).
// Order of the emitted lines is deterministic (before-set drives revokes,
// after-set drives grants) so tests can assert without sorting.
func (h *Handler) auditAccessReplace(ctx context.Context, actor, tenantID, channelID uuid.UUID, before, after []uuid.UUID) {
	if h.deps.Audit == nil || actor == uuid.Nil {
		return
	}
	beforeSet := make(map[uuid.UUID]struct{}, len(before))
	for _, id := range before {
		beforeSet[id] = struct{}{}
	}
	afterSet := make(map[uuid.UUID]struct{}, len(after))
	for _, id := range after {
		afterSet[id] = struct{}{}
	}
	for _, id := range before {
		if _, keep := afterSet[id]; !keep {
			h.deps.Audit.ChannelAccessRevoked(ctx, actor, tenantID, channelID, id)
		}
	}
	for _, id := range after {
		if _, had := beforeSet[id]; !had {
			h.deps.Audit.ChannelAccessGranted(ctx, actor, tenantID, channelID, id)
		}
	}
}

// auditRestrictedChange emits one restricted-changed line when the flag
// actually flipped. A no-op flip (from == to) writes nothing so the trail
// records privilege *changes*, not idempotent re-submits of the edit form.
func (h *Handler) auditRestrictedChange(ctx context.Context, actor, tenantID, channelID uuid.UUID, from, to bool) {
	if h.deps.Audit == nil || actor == uuid.Nil || from == to {
		return
	}
	h.deps.Audit.ChannelRestrictedChanged(ctx, actor, tenantID, channelID, from, to)
}

// beforeGrants reads the channel's current roster for the audit diff,
// returning ok=false (and warn-logging) when the read fails so the caller
// skips the grant/revoke lines rather than emitting a bogus all-granted
// diff. When no Auditor is wired it short-circuits without a DB round-trip
// — there is nothing to diff for.
func (h *Handler) beforeGrants(ctx context.Context, tenantID, channelID uuid.UUID) ([]uuid.UUID, bool) {
	if h.deps.Audit == nil {
		return nil, false
	}
	ids, err := h.deps.Access.ChannelUserIDs(ctx, tenantID, channelID)
	if err != nil {
		h.deps.Logger.Warn("web/channels: audit before-roster read failed", "err", err)
		return nil, false
	}
	return ids, true
}

// actorID resolves the authenticated actor for an audit line, returning
// uuid.Nil when the UserID collaborator is unwired (the emit helpers then
// skip). It mirrors userDisplayName's nil-guard so the audit path degrades
// exactly like the chrome path.
func (h *Handler) actorID(r *http.Request) uuid.UUID {
	if h.deps.UserID == nil {
		return uuid.Nil
	}
	return h.deps.UserID(r)
}
