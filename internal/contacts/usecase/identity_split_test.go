package usecase_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/contacts"
	"github.com/pericles-luz/crm/internal/contacts/usecase"
)

// fakeIdentityRepo is a single-tenant in-memory stub satisfying
// usecase.IdentitySplitRepository. It intentionally omits Resolve/Merge
// so the test surface stays focused on the read+write the F2-13 use
// cases actually exercise.
type fakeIdentityRepo struct {
	byContact     map[uuid.UUID]*contacts.Identity
	splitCalls    []uuid.UUID
	splitErr      error
	findErr       error
	postSplitHook func(linkID uuid.UUID)
}

func newFakeRepo() *fakeIdentityRepo {
	return &fakeIdentityRepo{byContact: map[uuid.UUID]*contacts.Identity{}}
}

func (f *fakeIdentityRepo) FindByContactID(_ context.Context, _, contactID uuid.UUID) (*contacts.Identity, error) {
	if f.findErr != nil {
		return nil, f.findErr
	}
	id, ok := f.byContact[contactID]
	if !ok {
		return nil, contacts.ErrNotFound
	}
	return id, nil
}

func (f *fakeIdentityRepo) Split(_ context.Context, _, linkID uuid.UUID) error {
	f.splitCalls = append(f.splitCalls, linkID)
	if f.splitErr != nil {
		return f.splitErr
	}
	if f.postSplitHook != nil {
		f.postSplitHook(linkID)
	}
	return nil
}

func TestNewLoadIdentityForContact_NilRepoRejected(t *testing.T) {
	if _, err := usecase.NewLoadIdentityForContact(nil); err == nil {
		t.Fatalf("nil repo accepted; want error")
	}
}

func TestNewSplitIdentityLink_NilRepoRejected(t *testing.T) {
	if _, err := usecase.NewSplitIdentityLink(nil); err == nil {
		t.Fatalf("nil repo accepted; want error")
	}
}

func TestLoadIdentityForContact_ReturnsHydratedIdentity(t *testing.T) {
	repo := newFakeRepo()
	tenant := uuid.New()
	contact := uuid.New()
	identityID := uuid.New()
	link := contacts.IdentityLink{
		ID:         uuid.New(),
		IdentityID: identityID,
		ContactID:  contact,
		Reason:     contacts.LinkReasonPhone,
		CreatedAt:  time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
	}
	repo.byContact[contact] = &contacts.Identity{
		ID: identityID, TenantID: tenant,
		Links: []contacts.IdentityLink{link},
	}

	uc, err := usecase.NewLoadIdentityForContact(repo)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	res, err := uc.Execute(context.Background(), usecase.LoadIdentityInput{
		TenantID: tenant, ContactID: contact,
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Identity == nil || res.Identity.ID != identityID {
		t.Fatalf("identity mismatch: got %+v want id %s", res.Identity, identityID)
	}
	if len(res.Identity.Links) != 1 || res.Identity.Links[0].Reason != contacts.LinkReasonPhone {
		t.Fatalf("links mismatch: %+v", res.Identity.Links)
	}
}

func TestLoadIdentityForContact_RejectsZeroIDs(t *testing.T) {
	uc, err := usecase.NewLoadIdentityForContact(newFakeRepo())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	cases := []struct {
		name string
		in   usecase.LoadIdentityInput
	}{
		{"nil tenant", usecase.LoadIdentityInput{ContactID: uuid.New()}},
		{"nil contact", usecase.LoadIdentityInput{TenantID: uuid.New()}},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			if _, err := uc.Execute(context.Background(), tc.in); err == nil {
				t.Fatalf("zero %s accepted", tc.name)
			}
		})
	}
}

func TestLoadIdentityForContact_NotFoundSurfaces(t *testing.T) {
	repo := newFakeRepo()
	uc, _ := usecase.NewLoadIdentityForContact(repo)
	_, err := uc.Execute(context.Background(), usecase.LoadIdentityInput{
		TenantID: uuid.New(), ContactID: uuid.New(),
	})
	if !errors.Is(err, contacts.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestSplitIdentityLink_HappyPath_ReturnsPostSplitIdentity(t *testing.T) {
	repo := newFakeRepo()
	tenant := uuid.New()
	survivor := uuid.New()
	splitOff := uuid.New()
	originalID := uuid.New()
	newID := uuid.New()
	linkSurvivor := contacts.IdentityLink{
		ID: uuid.New(), IdentityID: originalID, ContactID: survivor,
		Reason: contacts.LinkReasonExternalID, CreatedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
	}
	linkOrphan := contacts.IdentityLink{
		ID: uuid.New(), IdentityID: originalID, ContactID: splitOff,
		Reason: contacts.LinkReasonPhone, CreatedAt: time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC),
	}
	repo.byContact[survivor] = &contacts.Identity{
		ID: originalID, TenantID: tenant,
		Links: []contacts.IdentityLink{linkSurvivor, linkOrphan},
	}
	repo.postSplitHook = func(linkID uuid.UUID) {
		if linkID != linkOrphan.ID {
			t.Fatalf("split called with %s want %s", linkID, linkOrphan.ID)
		}
		// After split: survivor keeps its row; orphan moves to newID.
		repo.byContact[survivor] = &contacts.Identity{
			ID: originalID, TenantID: tenant,
			Links: []contacts.IdentityLink{linkSurvivor},
		}
		repo.byContact[splitOff] = &contacts.Identity{
			ID: newID, TenantID: tenant,
			Links: []contacts.IdentityLink{{
				ID: uuid.New(), IdentityID: newID, ContactID: splitOff,
				Reason: contacts.LinkReasonManual, CreatedAt: time.Date(2026, 1, 3, 0, 0, 0, 0, time.UTC),
			}},
		}
	}

	uc, err := usecase.NewSplitIdentityLink(repo)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	res, err := uc.Execute(context.Background(), usecase.SplitInput{
		TenantID: tenant, LinkID: linkOrphan.ID, SurvivorContactID: survivor,
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if got, want := res.Identity.ID, originalID; got != want {
		t.Fatalf("survivor identity id = %s, want %s", got, want)
	}
	if got, want := len(res.Identity.Links), 1; got != want {
		t.Fatalf("survivor link count = %d, want %d (orphan should be gone)", got, want)
	}
	if len(repo.splitCalls) != 1 {
		t.Fatalf("split call count = %d, want 1", len(repo.splitCalls))
	}
}

func TestSplitIdentityLink_RejectsZeroIDs(t *testing.T) {
	uc, _ := usecase.NewSplitIdentityLink(newFakeRepo())
	cases := []struct {
		name string
		in   usecase.SplitInput
	}{
		{"nil tenant", usecase.SplitInput{LinkID: uuid.New(), SurvivorContactID: uuid.New()}},
		{"nil link", usecase.SplitInput{TenantID: uuid.New(), SurvivorContactID: uuid.New()}},
		{"nil survivor", usecase.SplitInput{TenantID: uuid.New(), LinkID: uuid.New()}},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			if _, err := uc.Execute(context.Background(), tc.in); err == nil {
				t.Fatalf("zero %s accepted", tc.name)
			}
		})
	}
}

func TestSplitIdentityLink_RepoSplitError(t *testing.T) {
	repo := newFakeRepo()
	repo.splitErr = errors.New("simulated tx failure")
	uc, _ := usecase.NewSplitIdentityLink(repo)
	_, err := uc.Execute(context.Background(), usecase.SplitInput{
		TenantID: uuid.New(), LinkID: uuid.New(), SurvivorContactID: uuid.New(),
	})
	if err == nil || err.Error() == "" {
		t.Fatalf("err = %v, want non-nil", err)
	}
	if len(repo.splitCalls) != 1 {
		t.Fatalf("split call count = %d, want 1", len(repo.splitCalls))
	}
}

func TestSplitIdentityLink_PostSplitFindError(t *testing.T) {
	repo := newFakeRepo()
	repo.postSplitHook = func(_ uuid.UUID) { repo.findErr = errors.New("post-split read failure") }
	uc, _ := usecase.NewSplitIdentityLink(repo)
	_, err := uc.Execute(context.Background(), usecase.SplitInput{
		TenantID: uuid.New(), LinkID: uuid.New(), SurvivorContactID: uuid.New(),
	})
	if err == nil {
		t.Fatalf("post-split find error swallowed")
	}
}
