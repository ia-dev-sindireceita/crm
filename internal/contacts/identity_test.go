package contacts

import (
	"testing"

	"github.com/google/uuid"
)

// fixed UUIDs for deterministic smallestUUID tests.
var (
	uuidA = uuid.MustParse("00000000-0000-0000-0000-000000000001")
	uuidB = uuid.MustParse("00000000-0000-0000-0000-000000000002")
	uuidC = uuid.MustParse("00000000-0000-0000-0000-000000000003")
)

func TestDecideMerge(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		candidates []MergeCandidate
		wantAction MergeAction
		wantTarget uuid.UUID
		wantSrcLen int
		wantReason LinkReason
	}{
		{
			name:       "none → new",
			candidates: nil,
			wantAction: MergeActionNew,
		},
		{
			name: "single phone match → link",
			candidates: []MergeCandidate{
				{IdentityID: uuidA, Reason: LinkReasonPhone},
			},
			wantAction: MergeActionLink,
			wantTarget: uuidA,
			wantReason: LinkReasonPhone,
		},
		{
			name: "single email match → link",
			candidates: []MergeCandidate{
				{IdentityID: uuidA, Reason: LinkReasonEmail},
			},
			wantAction: MergeActionLink,
			wantTarget: uuidA,
			wantReason: LinkReasonEmail,
		},
		{
			name: "same identity via phone and email → link (idempotent)",
			candidates: []MergeCandidate{
				{IdentityID: uuidA, Reason: LinkReasonPhone},
				{IdentityID: uuidA, Reason: LinkReasonEmail},
			},
			wantAction: MergeActionLink,
			wantTarget: uuidA,
			wantReason: LinkReasonPhone, // phone == email priority; phone wins by insertion order stability
		},
		{
			name: "phone match no leader + email match no leader → merge",
			candidates: []MergeCandidate{
				{IdentityID: uuidA, Reason: LinkReasonPhone, HasLeader: false},
				{IdentityID: uuidB, Reason: LinkReasonEmail, HasLeader: false},
			},
			wantAction: MergeActionMerge,
			wantTarget: uuidA, // smallest UUID
			wantSrcLen: 1,
			wantReason: LinkReasonPhone, // phone==email, first wins
		},
		{
			name: "phone match leader + email match no leader → merge toward leader",
			candidates: []MergeCandidate{
				{IdentityID: uuidB, Reason: LinkReasonPhone, HasLeader: true},
				{IdentityID: uuidA, Reason: LinkReasonEmail, HasLeader: false},
			},
			wantAction: MergeActionMerge,
			wantTarget: uuidB, // leader wins regardless of UUID ordering
			wantSrcLen: 1,
			wantReason: LinkReasonPhone,
		},
		{
			name: "both different identities have leaders → propose",
			candidates: []MergeCandidate{
				{IdentityID: uuidA, Reason: LinkReasonPhone, HasLeader: true},
				{IdentityID: uuidB, Reason: LinkReasonEmail, HasLeader: true},
			},
			wantAction: MergeActionPropose,
		},
		{
			name: "three identities two leaders → propose",
			candidates: []MergeCandidate{
				{IdentityID: uuidA, Reason: LinkReasonPhone, HasLeader: true},
				{IdentityID: uuidB, Reason: LinkReasonEmail, HasLeader: true},
				{IdentityID: uuidC, Reason: LinkReasonManual, HasLeader: false},
			},
			wantAction: MergeActionPropose,
		},
		{
			name: "three identities no leaders → merge to smallest",
			candidates: []MergeCandidate{
				{IdentityID: uuidB, Reason: LinkReasonPhone},
				{IdentityID: uuidC, Reason: LinkReasonEmail},
				{IdentityID: uuidA, Reason: LinkReasonManual},
			},
			wantAction: MergeActionMerge,
			wantTarget: uuidA, // lexicographically smallest
			wantSrcLen: 2,
		},
		{
			name: "external_id reason outranks phone on same identity",
			candidates: []MergeCandidate{
				{IdentityID: uuidA, Reason: LinkReasonPhone},
				{IdentityID: uuidA, Reason: LinkReasonExternalID},
			},
			wantAction: MergeActionLink,
			wantTarget: uuidA,
			wantReason: LinkReasonExternalID,
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := DecideMerge(tc.candidates)
			if got.Action != tc.wantAction {
				t.Errorf("Action: got %v, want %v", got.Action, tc.wantAction)
			}
			if tc.wantTarget != uuid.Nil && got.TargetID != tc.wantTarget {
				t.Errorf("TargetID: got %v, want %v", got.TargetID, tc.wantTarget)
			}
			if tc.wantSrcLen > 0 && len(got.SourceIDs) != tc.wantSrcLen {
				t.Errorf("SourceIDs len: got %d, want %d", len(got.SourceIDs), tc.wantSrcLen)
			}
			if tc.wantReason != "" && got.Reason != tc.wantReason {
				t.Errorf("Reason: got %v, want %v", got.Reason, tc.wantReason)
			}
		})
	}
}

func TestReasonPriority(t *testing.T) {
	t.Parallel()
	if reasonPriority(LinkReasonExternalID) <= reasonPriority(LinkReasonPhone) {
		t.Error("external_id should outrank phone")
	}
	if reasonPriority(LinkReasonPhone) <= reasonPriority(LinkReasonManual) {
		t.Error("phone should outrank manual")
	}
	if reasonPriority(LinkReasonEmail) != reasonPriority(LinkReasonPhone) {
		t.Error("email and phone should have equal priority")
	}
}

func TestSmallestUUID(t *testing.T) {
	t.Parallel()
	got := smallestUUID([]uuid.UUID{uuidC, uuidA, uuidB})
	if got != uuidA {
		t.Errorf("smallestUUID: got %v, want %v", got, uuidA)
	}
}
