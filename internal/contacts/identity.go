// Package contacts — Identity aggregate and merge-decision logic.
// Migration 0092 (SIN-62790 / F2-03) owns the identity +
// contact_identity_link tables that back this model.
package contacts

import (
	"time"

	"github.com/google/uuid"
)

// LinkReason describes why a Contact was linked to an Identity.
type LinkReason string

const (
	LinkReasonPhone      LinkReason = "phone"
	LinkReasonEmail      LinkReason = "email"
	LinkReasonExternalID LinkReason = "external_id"
	LinkReasonManual     LinkReason = "manual"
)

// IdentityLink records the bond between a Contact and an Identity.
type IdentityLink struct {
	ID         uuid.UUID
	IdentityID uuid.UUID
	ContactID  uuid.UUID
	Reason     LinkReason
	CreatedAt  time.Time
}

// Identity is the aggregate root for the merged-contact concept.
// One Identity aggregates 1..N Contacts that share a phone, email,
// or (channel, external_id). MergedIntoID is non-nil when this
// identity has been absorbed into another; callers should follow
// the chain to the surviving root.
type Identity struct {
	ID           uuid.UUID
	TenantID     uuid.UUID
	CreatedAt    time.Time
	MergedIntoID *uuid.UUID // nil → terminal / surviving identity
	Links        []IdentityLink
}

// MergeProposal is raised when two contacts map to distinct identities
// whose conversations each have a different assigned leader. The domain
// refuses to auto-merge and signals that a human must confirm via the
// merge UI (F2-13).
type MergeProposal struct {
	ID        uuid.UUID
	TenantID  uuid.UUID
	SourceID  uuid.UUID // identity to absorb (will be marked merged_into TargetID)
	TargetID  uuid.UUID // surviving identity
	Reason    string
	CreatedAt time.Time
}

// MergeAction is the outcome of a merge-decision evaluation.
type MergeAction int8

const (
	// MergeActionNew — no existing identity found; create a fresh one.
	MergeActionNew MergeAction = iota
	// MergeActionLink — single identity match; link the contact to it.
	MergeActionLink
	// MergeActionMerge — multiple identities with no conflict; auto-merge.
	MergeActionMerge
	// MergeActionPropose — two or more identities each with a distinct
	// leader; emit a MergeProposal and wait for human confirmation.
	MergeActionPropose
)

// MergeCandidate is one identity hit found during a Resolve call.
type MergeCandidate struct {
	IdentityID uuid.UUID
	Reason     LinkReason
	// HasLeader is true when this identity has a conversation with an
	// actively assigned leader in assignment_history.
	HasLeader bool
}

// MergeDecision is the outcome of DecideMerge.
type MergeDecision struct {
	Action    MergeAction
	TargetID  uuid.UUID   // surviving identity (zero for MergeActionNew/Propose)
	SourceIDs []uuid.UUID // identities to absorb (MergeActionMerge only)
	Reason    LinkReason  // reason for the link written to contact_identity_link
}

// DecideMerge applies the identity merge rules to a set of candidates
// found during a Resolve call. It is a pure function with no I/O so it
// can be exercised exhaustively in unit tests.
//
// Rules (evaluated in priority order):
//  1. No candidates → MergeActionNew (create a new identity).
//  2. All candidates share the same IdentityID → MergeActionLink
//     (idempotent re-link; no DB write needed if already linked).
//  3. Multiple distinct identities where >1 has HasLeader=true →
//     MergeActionPropose (human confirmation required — F2-13).
//  4. Multiple distinct identities where ≤1 has HasLeader=true →
//     MergeActionMerge; target is the leader's identity when one
//     exists, otherwise the lexicographically smallest UUID.
func DecideMerge(candidates []MergeCandidate) MergeDecision {
	if len(candidates) == 0 {
		return MergeDecision{Action: MergeActionNew}
	}

	type idMeta struct {
		hasLeader bool
		reason    LinkReason
	}
	// allIDs preserves first-seen insertion order so downstream
	// reason-picking is deterministic even though byID is a map.
	allIDs := make([]uuid.UUID, 0, len(candidates))
	byID := make(map[uuid.UUID]*idMeta, len(candidates))
	for _, c := range candidates {
		m, ok := byID[c.IdentityID]
		if !ok {
			cp := idMeta{hasLeader: c.HasLeader, reason: c.Reason}
			byID[c.IdentityID] = &cp
			allIDs = append(allIDs, c.IdentityID)
			continue
		}
		if c.HasLeader {
			m.hasLeader = true
		}
		if reasonPriority(c.Reason) > reasonPriority(m.reason) {
			m.reason = c.Reason
		}
	}

	if len(allIDs) == 1 {
		id := allIDs[0]
		return MergeDecision{Action: MergeActionLink, TargetID: id, Reason: byID[id].reason}
	}

	var leaderIDs []uuid.UUID
	for _, id := range allIDs {
		if byID[id].hasLeader {
			leaderIDs = append(leaderIDs, id)
		}
	}

	if len(leaderIDs) > 1 {
		return MergeDecision{Action: MergeActionPropose}
	}

	// Auto-merge: pick target, collect sources.
	var target uuid.UUID
	var reason LinkReason
	if len(leaderIDs) == 1 {
		target = leaderIDs[0]
		reason = byID[target].reason
	} else {
		target = smallestUUID(allIDs)
		for _, id := range allIDs {
			if reasonPriority(byID[id].reason) > reasonPriority(reason) {
				reason = byID[id].reason
			}
		}
	}

	sources := make([]uuid.UUID, 0, len(allIDs)-1)
	for _, id := range allIDs {
		if id != target {
			sources = append(sources, id)
		}
	}
	return MergeDecision{Action: MergeActionMerge, TargetID: target, SourceIDs: sources, Reason: reason}
}

// reasonPriority ranks LinkReasons so a more-specific match wins when
// two candidates map to the same identity via different keys.
func reasonPriority(r LinkReason) int {
	switch r {
	case LinkReasonExternalID:
		return 3
	case LinkReasonPhone, LinkReasonEmail:
		return 2
	case LinkReasonManual:
		return 1
	default:
		return 0
	}
}

// smallestUUID returns the lexicographically smallest UUID string from ids.
// Used to pick a deterministic merge target when no leader is present.
func smallestUUID(ids []uuid.UUID) uuid.UUID {
	best := ids[0]
	for _, id := range ids[1:] {
		if id.String() < best.String() {
			best = id
		}
	}
	return best
}
