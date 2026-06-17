package usecase_test

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/inbox/usecase"
)

// The "unassigned" queue filter (SIN-64978) forwards UnassignedOnly to the
// read-model port. assigned_to_me and all are already covered by the
// AssignedUserID axis (SIN-64967); these tests pin the new axis and the
// mutual-exclusion guard.

func TestListConversationSummaries_UnassignedForwardsFilter(t *testing.T) {
	read := &fakeReadModel{}
	uc := usecase.MustNewListConversationSummaries(read, nil)
	tenantID := uuid.New()

	_, err := uc.Execute(context.Background(), usecase.ListConversationSummariesInput{
		TenantID:   tenantID,
		Unassigned: true,
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !read.gotFilter.UnassignedOnly {
		t.Error("UnassignedOnly = false, want true forwarded to read model")
	}
	if read.gotFilter.AssignedUserID != uuid.Nil {
		t.Errorf("AssignedUserID = %v, want Nil for the unassigned queue", read.gotFilter.AssignedUserID)
	}
}

func TestListConversationSummaries_DefaultsToAllAssignmentStates(t *testing.T) {
	read := &fakeReadModel{}
	uc := usecase.MustNewListConversationSummaries(read, nil)

	if _, err := uc.Execute(context.Background(), usecase.ListConversationSummariesInput{
		TenantID: uuid.New(),
	}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if read.gotFilter.UnassignedOnly {
		t.Error("UnassignedOnly = true, want false (the default = all)")
	}
}

func TestListConversationSummaries_AssignedToMeForwardsUser(t *testing.T) {
	read := &fakeReadModel{}
	uc := usecase.MustNewListConversationSummaries(read, nil)
	me := uuid.New()

	if _, err := uc.Execute(context.Background(), usecase.ListConversationSummariesInput{
		TenantID:       uuid.New(),
		AssignedUserID: me,
	}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if read.gotFilter.AssignedUserID != me {
		t.Errorf("AssignedUserID = %v, want %v", read.gotFilter.AssignedUserID, me)
	}
	if read.gotFilter.UnassignedOnly {
		t.Error("UnassignedOnly = true, want false when filtering by a specific user")
	}
}

func TestListConversationSummaries_RejectsUnassignedPlusUser(t *testing.T) {
	read := &fakeReadModel{}
	uc := usecase.MustNewListConversationSummaries(read, nil)

	_, err := uc.Execute(context.Background(), usecase.ListConversationSummariesInput{
		TenantID:       uuid.New(),
		AssignedUserID: uuid.New(),
		Unassigned:     true,
	})
	if err == nil {
		t.Fatal("want error for mutually-exclusive unassigned + assigned filter")
	}
	if read.gotTenant != uuid.Nil {
		t.Error("read model must not be queried when the filter is rejected at the boundary")
	}
}
