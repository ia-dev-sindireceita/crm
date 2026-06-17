package usecase_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/inbox"
	"github.com/pericles-luz/crm/internal/inbox/usecase"
)

// fakeReadModel is an in-memory inbox.ConversationReadModel. It records
// the last filter/limit it received so tests can assert the use case
// normalises and forwards them correctly, and returns a canned row set.
type fakeReadModel struct {
	rows      []inbox.ConversationListItem
	err       error
	gotFilter inbox.ConversationFilter
	gotLimit  int
	gotTenant uuid.UUID
}

func (f *fakeReadModel) ListConversationSummaries(_ context.Context, tenantID uuid.UUID, filter inbox.ConversationFilter, limit int) ([]inbox.ConversationListItem, error) {
	f.gotTenant = tenantID
	f.gotFilter = filter
	f.gotLimit = limit
	if f.err != nil {
		return nil, f.err
	}
	return f.rows, nil
}

// fakeDirectory is an in-memory inbox.UserDirectory. labels maps user id
// → label; ids absent from the map resolve to "no label", matching the
// adapter contract.
type fakeDirectory struct {
	labels map[uuid.UUID]string
	err    error
	gotIDs []uuid.UUID
	calls  int
}

func (f *fakeDirectory) LabelsByID(_ context.Context, _ uuid.UUID, ids []uuid.UUID) (map[uuid.UUID]string, error) {
	f.calls++
	f.gotIDs = append([]uuid.UUID(nil), ids...)
	if f.err != nil {
		return nil, f.err
	}
	out := make(map[uuid.UUID]string, len(ids))
	for _, id := range ids {
		if l, ok := f.labels[id]; ok {
			out[id] = l
		}
	}
	return out, nil
}

func ptrUUID(id uuid.UUID) *uuid.UUID { return &id }

func TestNewListConversationSummaries_RejectsNilReadModel(t *testing.T) {
	t.Parallel()
	if _, err := usecase.NewListConversationSummaries(nil, nil); err == nil {
		t.Fatalf("expected error, got nil")
	}
}

func TestMustNewListConversationSummaries_PanicsOnNil(t *testing.T) {
	t.Parallel()
	defer func() {
		if r := recover(); r == nil {
			t.Fatalf("expected panic, got none")
		}
	}()
	_ = usecase.MustNewListConversationSummaries(nil, nil)
}

func TestListConversationSummaries_Validation(t *testing.T) {
	t.Parallel()
	uc := usecase.MustNewListConversationSummaries(&fakeReadModel{}, nil)

	t.Run("nil tenant", func(t *testing.T) {
		t.Parallel()
		_, err := uc.Execute(context.Background(), usecase.ListConversationSummariesInput{})
		if !errors.Is(err, inbox.ErrInvalidTenant) {
			t.Fatalf("err = %v, want ErrInvalidTenant", err)
		}
	})

	t.Run("invalid state", func(t *testing.T) {
		t.Parallel()
		_, err := uc.Execute(context.Background(), usecase.ListConversationSummariesInput{
			TenantID: uuid.New(), State: "garbage",
		})
		if !errors.Is(err, inbox.ErrInvalidStatus) {
			t.Fatalf("err = %v, want ErrInvalidStatus", err)
		}
	})

	t.Run("invalid channel", func(t *testing.T) {
		t.Parallel()
		_, err := uc.Execute(context.Background(), usecase.ListConversationSummariesInput{
			TenantID: uuid.New(), Channel: "telegram",
		})
		if !errors.Is(err, inbox.ErrInvalidChannel) {
			t.Fatalf("err = %v, want ErrInvalidChannel", err)
		}
	})
}

func TestListConversationSummaries_NormalisesAndForwardsFilter(t *testing.T) {
	t.Parallel()
	rm := &fakeReadModel{}
	uc := usecase.MustNewListConversationSummaries(rm, nil)
	tenant := uuid.New()
	me := uuid.New()

	_, err := uc.Execute(context.Background(), usecase.ListConversationSummariesInput{
		TenantID:       tenant,
		State:          "closed",
		Channel:        "  WhatsApp ",
		AssignedUserID: me,
		Limit:          7,
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if rm.gotTenant != tenant {
		t.Errorf("tenant = %s, want %s", rm.gotTenant, tenant)
	}
	if rm.gotFilter.State != inbox.ConversationStateClosed {
		t.Errorf("state = %q, want closed", rm.gotFilter.State)
	}
	if rm.gotFilter.Channel != "whatsapp" {
		t.Errorf("channel = %q, want normalised 'whatsapp'", rm.gotFilter.Channel)
	}
	if rm.gotFilter.AssignedUserID != me {
		t.Errorf("assigned = %s, want %s", rm.gotFilter.AssignedUserID, me)
	}
	if rm.gotLimit != 7 {
		t.Errorf("limit = %d, want 7", rm.gotLimit)
	}
}

func TestListConversationSummaries_DefaultLimit(t *testing.T) {
	t.Parallel()
	rm := &fakeReadModel{}
	uc := usecase.MustNewListConversationSummaries(rm, nil)
	if _, err := uc.Execute(context.Background(), usecase.ListConversationSummariesInput{TenantID: uuid.New()}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if rm.gotLimit != 50 {
		t.Errorf("default limit = %d, want 50", rm.gotLimit)
	}
}

func TestListConversationSummaries_DerivesAwaitingReplyAndSnippet(t *testing.T) {
	t.Parallel()
	now := time.Now()
	rm := &fakeReadModel{rows: []inbox.ConversationListItem{
		{
			ID:                   uuid.New(),
			Channel:              "whatsapp",
			State:                inbox.ConversationStateOpen,
			LastMessageAt:        now,
			ContactDisplayName:   "Maria Silva",
			LastMessageSnippet:   "oi, preciso de ajuda",
			LastMessageDirection: inbox.MessageDirectionIn,
		},
		{
			ID:                   uuid.New(),
			Channel:              "webchat",
			State:                inbox.ConversationStateOpen,
			LastMessageAt:        now,
			LastMessageSnippet:   "já respondi",
			LastMessageDirection: inbox.MessageDirectionOut,
		},
		{
			ID:      uuid.New(),
			Channel: "instagram",
			State:   inbox.ConversationStateOpen,
			// no messages: empty direction => not awaiting
		},
	}}
	uc := usecase.MustNewListConversationSummaries(rm, nil)
	res, err := uc.Execute(context.Background(), usecase.ListConversationSummariesInput{TenantID: uuid.New()})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(res.Items) != 3 {
		t.Fatalf("len = %d, want 3", len(res.Items))
	}
	if !res.Items[0].AwaitingReply {
		t.Errorf("inbound last message should be AwaitingReply")
	}
	if res.Items[0].LastMessageSnippet != "oi, preciso de ajuda" {
		t.Errorf("snippet = %q", res.Items[0].LastMessageSnippet)
	}
	if res.Items[0].ContactDisplayName != "Maria Silva" {
		t.Errorf("ContactDisplayName = %q, want Maria Silva", res.Items[0].ContactDisplayName)
	}
	if res.Items[0].LastMessageDirection != "in" {
		t.Errorf("direction = %q, want in", res.Items[0].LastMessageDirection)
	}
	if res.Items[1].AwaitingReply {
		t.Errorf("outbound last message should NOT be AwaitingReply")
	}
	if res.Items[2].AwaitingReply {
		t.Errorf("no-message conversation should NOT be AwaitingReply")
	}
}

func TestListConversationSummaries_ResolvesLabels(t *testing.T) {
	t.Parallel()
	alice := uuid.New()
	bob := uuid.New()
	missing := uuid.New()
	rm := &fakeReadModel{rows: []inbox.ConversationListItem{
		{ID: uuid.New(), AssignedUserID: ptrUUID(alice)},
		{ID: uuid.New(), AssignedUserID: ptrUUID(bob)},
		{ID: uuid.New(), AssignedUserID: ptrUUID(missing)}, // not in directory
		{ID: uuid.New()}, // unassigned
		{ID: uuid.New(), AssignedUserID: ptrUUID(alice)}, // duplicate id
	}}
	dir := &fakeDirectory{labels: map[uuid.UUID]string{
		alice: "alice",
		bob:   "bob",
	}}
	uc := usecase.MustNewListConversationSummaries(rm, dir)
	res, err := uc.Execute(context.Background(), usecase.ListConversationSummariesInput{TenantID: uuid.New()})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if got := deref(res.Items[0].AssignedUserLabel); got != "alice" {
		t.Errorf("item0 label = %q, want alice", got)
	}
	if got := deref(res.Items[1].AssignedUserLabel); got != "bob" {
		t.Errorf("item1 label = %q, want bob", got)
	}
	if res.Items[2].AssignedUserLabel != nil {
		t.Errorf("unresolved id should yield nil label, got %q", *res.Items[2].AssignedUserLabel)
	}
	if res.Items[3].AssignedUserLabel != nil {
		t.Errorf("unassigned conversation should yield nil label")
	}
	// alice appears twice but should be requested only once (dedup).
	if dir.calls != 1 {
		t.Errorf("directory calls = %d, want 1 (batched)", dir.calls)
	}
	if len(dir.gotIDs) != 3 {
		t.Errorf("distinct ids requested = %d, want 3 (alice,bob,missing)", len(dir.gotIDs))
	}
}

func TestListConversationSummaries_NilDirectory_LeavesLabelsNil(t *testing.T) {
	t.Parallel()
	rm := &fakeReadModel{rows: []inbox.ConversationListItem{
		{ID: uuid.New(), AssignedUserID: ptrUUID(uuid.New())},
	}}
	uc := usecase.MustNewListConversationSummaries(rm, nil)
	res, err := uc.Execute(context.Background(), usecase.ListConversationSummariesInput{TenantID: uuid.New()})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Items[0].AssignedUserLabel != nil {
		t.Errorf("nil directory should leave label nil")
	}
}

func TestListConversationSummaries_NoAssignees_SkipsDirectory(t *testing.T) {
	t.Parallel()
	rm := &fakeReadModel{rows: []inbox.ConversationListItem{{ID: uuid.New()}}}
	dir := &fakeDirectory{}
	uc := usecase.MustNewListConversationSummaries(rm, dir)
	if _, err := uc.Execute(context.Background(), usecase.ListConversationSummariesInput{TenantID: uuid.New()}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if dir.calls != 0 {
		t.Errorf("directory should not be called when no assignees; calls = %d", dir.calls)
	}
}

func TestListConversationSummaries_PropagatesErrors(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("boom")

	t.Run("read model error", func(t *testing.T) {
		t.Parallel()
		uc := usecase.MustNewListConversationSummaries(&fakeReadModel{err: sentinel}, nil)
		_, err := uc.Execute(context.Background(), usecase.ListConversationSummariesInput{TenantID: uuid.New()})
		if !errors.Is(err, sentinel) {
			t.Fatalf("err = %v, want sentinel", err)
		}
	})

	t.Run("directory error", func(t *testing.T) {
		t.Parallel()
		rm := &fakeReadModel{rows: []inbox.ConversationListItem{{ID: uuid.New(), AssignedUserID: ptrUUID(uuid.New())}}}
		uc := usecase.MustNewListConversationSummaries(rm, &fakeDirectory{err: sentinel})
		_, err := uc.Execute(context.Background(), usecase.ListConversationSummariesInput{TenantID: uuid.New()})
		if !errors.Is(err, sentinel) {
			t.Fatalf("err = %v, want sentinel", err)
		}
	})
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
