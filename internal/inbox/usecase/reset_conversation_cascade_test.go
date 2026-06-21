package usecase_test

// SIN-65472 — the reset delete-cascade: deleting a training thread's
// messages must also drop it back to Unassigned AND invalidate the
// cached AI summary. These tests cover the use-case ordering and the
// nil-port tolerance with in-memory fakes; the postgres_test parent
// package covers the live read-path (ListConversationSummaries) and the
// real ai_summary invalidation.

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	inboxusecase "github.com/pericles-luz/crm/internal/inbox/usecase"
)

// recordingClearer records ClearAssignment calls and stamps a monotonic
// sequence number so the test can assert it ran after the delete and
// before the summary invalidation.
type recordingClearer struct {
	calls  int
	tenant uuid.UUID
	conv   uuid.UUID
	seq    int
	err    error
	clock  *seqClock
}

func (r *recordingClearer) ClearAssignment(_ context.Context, tenantID, conversationID uuid.UUID) error {
	r.calls++
	r.tenant = tenantID
	r.conv = conversationID
	if r.clock != nil {
		r.seq = r.clock.tick()
	}
	return r.err
}

// recordingInvalidator records InvalidateSummaries calls plus a sequence
// stamp for ordering assertions.
type recordingInvalidator struct {
	calls  int
	tenant uuid.UUID
	conv   uuid.UUID
	seq    int
	err    error
	clock  *seqClock
}

func (r *recordingInvalidator) InvalidateSummaries(_ context.Context, tenantID, conversationID uuid.UUID) error {
	r.calls++
	r.tenant = tenantID
	r.conv = conversationID
	if r.clock != nil {
		r.seq = r.clock.tick()
	}
	return r.err
}

// seqClock hands out a monotonically increasing sequence so collaborators
// can record the order in which the use case invoked them.
type seqClock struct{ n int }

func (c *seqClock) tick() int { c.n++; return c.n }

func TestResetConversation_ClearsAssignmentAndInvalidatesSummary(t *testing.T) {
	t.Parallel()
	repo := newInMemoryRepo()
	resetter := &recordingResetter{}
	clock := &seqClock{}
	clearer := &recordingClearer{clock: clock}
	invalidator := &recordingInvalidator{clock: clock}
	uc := inboxusecase.MustNewResetConversation(
		repo,
		resetter,
		inboxusecase.WithAssignmentClearer(clearer),
		inboxusecase.WithSummaryInvalidator(invalidator),
	)

	tenant := uuid.New()
	conv := seedConversationWithMessages(t, repo, tenant, uuid.New(), inboxusecase.TrainingChannel, 2)

	res, err := uc.Execute(context.Background(), inboxusecase.ResetConversationInput{TenantID: tenant, ConversationID: conv})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Deleted != 2 {
		t.Fatalf("Deleted = %d, want 2", res.Deleted)
	}

	// Both cascade steps ran exactly once for the right (tenant, conv).
	if clearer.calls != 1 || clearer.tenant != tenant || clearer.conv != conv {
		t.Fatalf("clearer = {calls:%d tenant:%v conv:%v}, want {1 %v %v}", clearer.calls, clearer.tenant, clearer.conv, tenant, conv)
	}
	if invalidator.calls != 1 || invalidator.tenant != tenant || invalidator.conv != conv {
		t.Fatalf("invalidator = {calls:%d tenant:%v conv:%v}, want {1 %v %v}", invalidator.calls, invalidator.tenant, invalidator.conv, tenant, conv)
	}
	// Ordering: clear assignment BEFORE invalidate summary BEFORE adapter reset.
	if !(clearer.seq < invalidator.seq) {
		t.Fatalf("clearer.seq (%d) should precede invalidator.seq (%d)", clearer.seq, invalidator.seq)
	}
	if resetter.calls != 1 {
		t.Fatalf("resetter calls = %d, want 1", resetter.calls)
	}
}

func TestResetConversation_NilCascadePortsAreNoOps(t *testing.T) {
	t.Parallel()
	// A deployment that wires neither the assignment store nor aiassist
	// must still run the delete-only reset without panicking — the
	// WithXxx options ignore nil ports, leaving the no-op defaults.
	repo := newInMemoryRepo()
	uc := inboxusecase.MustNewResetConversation(
		repo,
		inboxusecase.NoopConversationResetter{},
		inboxusecase.WithAssignmentClearer(nil),
		inboxusecase.WithSummaryInvalidator(nil),
	)
	tenant := uuid.New()
	conv := seedConversationWithMessages(t, repo, tenant, uuid.New(), inboxusecase.TrainingChannel, 1)
	res, err := uc.Execute(context.Background(), inboxusecase.ResetConversationInput{TenantID: tenant, ConversationID: conv})
	if err != nil {
		t.Fatalf("Execute with nil cascade ports: %v", err)
	}
	if res.Deleted != 1 {
		t.Fatalf("Deleted = %d, want 1", res.Deleted)
	}
}

func TestResetConversation_PropagatesClearerError(t *testing.T) {
	t.Parallel()
	// A failing assignment clear aborts the reset (and never reaches the
	// summary invalidation) so a retry converges. The Noop summary
	// invalidator must therefore not have been the one to fail.
	repo := newInMemoryRepo()
	invalidator := &recordingInvalidator{}
	uc := inboxusecase.MustNewResetConversation(
		repo,
		inboxusecase.NoopConversationResetter{},
		inboxusecase.WithAssignmentClearer(&recordingClearer{err: errors.New("clear boom")}),
		inboxusecase.WithSummaryInvalidator(invalidator),
	)
	tenant := uuid.New()
	conv := seedConversationWithMessages(t, repo, tenant, uuid.New(), inboxusecase.TrainingChannel, 1)
	if _, err := uc.Execute(context.Background(), inboxusecase.ResetConversationInput{TenantID: tenant, ConversationID: conv}); err == nil {
		t.Fatal("Execute err = nil, want clearer error propagated")
	}
	if invalidator.calls != 0 {
		t.Fatalf("invalidator ran despite clearer failure: calls = %d, want 0", invalidator.calls)
	}
}

func TestResetConversation_PropagatesInvalidatorError(t *testing.T) {
	t.Parallel()
	repo := newInMemoryRepo()
	resetter := &recordingResetter{}
	uc := inboxusecase.MustNewResetConversation(
		repo,
		resetter,
		inboxusecase.WithAssignmentClearer(&recordingClearer{}),
		inboxusecase.WithSummaryInvalidator(&recordingInvalidator{err: errors.New("invalidate boom")}),
	)
	tenant := uuid.New()
	conv := seedConversationWithMessages(t, repo, tenant, uuid.New(), inboxusecase.TrainingChannel, 1)
	if _, err := uc.Execute(context.Background(), inboxusecase.ResetConversationInput{TenantID: tenant, ConversationID: conv}); err == nil {
		t.Fatal("Execute err = nil, want invalidator error propagated")
	}
	// The adapter reset runs LAST, so an invalidator failure must abort
	// before it.
	if resetter.calls != 0 {
		t.Fatalf("adapter reset ran despite invalidator failure: calls = %d, want 0", resetter.calls)
	}
}
