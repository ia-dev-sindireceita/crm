package usecase

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/contacts"
)

// zeroTime is the time.Time zero value, used by the fake repository when
// hydrating contacts. Adapter integration tests assert real timestamps;
// the in-process fake only cares about identity invariants.
var zeroTime time.Time

// fakeRepo is an in-process Repository that mimics the Postgres
// invariants the real adapter enforces:
//
//   - tenant-scoped Find* (rows belonging to another tenant collapse to
//     ErrNotFound).
//   - global UNIQUE(channel, external_id) on identities (a Save whose
//     identity is already claimed returns ErrChannelIdentityConflict
//     regardless of tenant).
//
// We deliberately do NOT mock the database here for adapter-level tests
// (those live with the Postgres adapter). This fake is the in-process
// equivalent used by the use-case to exercise its idempotency contract
// without spinning up a cluster — explicit per the quality bar (rule 5:
// no mocking the database in tests for code that touches storage; the
// use-case does not touch storage directly).
type fakeRepo struct {
	mu sync.Mutex
	// id -> contact (deep copy on Save / Find)
	byID map[uuid.UUID]storedContact
	// "channel|externalID" -> contact id (global, not tenant-scoped)
	byIdentity map[string]uuid.UUID
	// optional hook for race-injection in concurrency tests
	beforeSave func()
	// error injectors
	findErr error
	saveErr error
}

type storedContact struct {
	id          uuid.UUID
	tenantID    uuid.UUID
	displayName string
	identities  []contacts.ChannelIdentity
}

func newFakeRepo() *fakeRepo {
	return &fakeRepo{
		byID:       map[uuid.UUID]storedContact{},
		byIdentity: map[string]uuid.UUID{},
	}
}

func identityKey(channel, externalID string) string {
	id, err := contacts.NewChannelIdentity(channel, externalID)
	if err != nil {
		// fall back to raw concat so tests that pass malformed
		// inputs still hit the validation error in the use-case
		return channel + "|" + externalID
	}
	return id.Channel + "|" + id.ExternalID
}

func (r *fakeRepo) Save(_ context.Context, c *contacts.Contact) error {
	if r.beforeSave != nil {
		r.beforeSave()
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.saveErr != nil {
		return r.saveErr
	}
	if _, dup := r.byID[c.ID]; dup {
		return fmt.Errorf("fakeRepo: duplicate id %s", c.ID)
	}
	for _, id := range c.Identities() {
		if _, claimed := r.byIdentity[id.Channel+"|"+id.ExternalID]; claimed {
			return contacts.ErrChannelIdentityConflict
		}
	}
	r.byID[c.ID] = storedContact{
		id:          c.ID,
		tenantID:    c.TenantID,
		displayName: c.DisplayName,
		identities:  c.Identities(),
	}
	for _, id := range c.Identities() {
		r.byIdentity[id.Channel+"|"+id.ExternalID] = c.ID
	}
	return nil
}

func (r *fakeRepo) FindByID(_ context.Context, tenantID, id uuid.UUID) (*contacts.Contact, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.findErr != nil {
		return nil, r.findErr
	}
	row, ok := r.byID[id]
	if !ok || row.tenantID != tenantID {
		return nil, contacts.ErrNotFound
	}
	return contacts.Hydrate(row.id, row.tenantID, row.displayName, row.identities, zeroTime, zeroTime), nil
}

func (r *fakeRepo) FindByChannelIdentity(_ context.Context, tenantID uuid.UUID, channel, externalID string) (*contacts.Contact, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.findErr != nil {
		return nil, r.findErr
	}
	id, ok := r.byIdentity[identityKey(channel, externalID)]
	if !ok {
		return nil, contacts.ErrNotFound
	}
	row := r.byID[id]
	if row.tenantID != tenantID {
		// claimed by another tenant: looks like NotFound for THIS tenant
		return nil, contacts.ErrNotFound
	}
	return contacts.Hydrate(row.id, row.tenantID, row.displayName, row.identities, zeroTime, zeroTime), nil
}

func TestNew_RejectsNilRepo(t *testing.T) {
	if u, err := New(nil); err == nil || u != nil {
		t.Errorf("New(nil) = (%v, %v), want (nil, error)", u, err)
	}
}

func TestMustNew_PanicsOnNil(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Error("MustNew(nil) did not panic")
		}
	}()
	_ = MustNew(nil)
}

func TestMustNew_ReturnsValue(t *testing.T) {
	if got := MustNew(newFakeRepo()); got == nil {
		t.Error("MustNew(repo) = nil")
	}
}

func TestExecute_CreatesNewWhenMissing(t *testing.T) {
	repo := newFakeRepo()
	u := MustNew(repo)
	tenant := uuid.New()

	res, err := u.Execute(context.Background(), Input{
		TenantID:    tenant,
		Channel:     "whatsapp",
		ExternalID:  "+5511999990001",
		DisplayName: "Alice",
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !res.Created {
		t.Errorf("Created = false, want true")
	}
	if res.Contact == nil {
		t.Fatal("Contact is nil")
	}
	if res.Contact.TenantID != tenant {
		t.Errorf("TenantID = %s, want %s", res.Contact.TenantID, tenant)
	}
	if res.Contact.DisplayName != "Alice" {
		t.Errorf("DisplayName = %q, want Alice", res.Contact.DisplayName)
	}
	ids := res.Contact.Identities()
	if len(ids) != 1 || ids[0].ExternalID != "+5511999990001" {
		t.Errorf("identities = %+v", ids)
	}
}

func TestExecute_ReturnsExistingOnSecondCall(t *testing.T) {
	repo := newFakeRepo()
	u := MustNew(repo)
	tenant := uuid.New()

	first, err := u.Execute(context.Background(), Input{
		TenantID: tenant, Channel: "whatsapp", ExternalID: "+5511999990001", DisplayName: "Alice",
	})
	if err != nil {
		t.Fatalf("first: %v", err)
	}

	second, err := u.Execute(context.Background(), Input{
		TenantID: tenant, Channel: "whatsapp", ExternalID: "+5511999990001", DisplayName: "DifferentName",
	})
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if second.Created {
		t.Errorf("second.Created = true, want false")
	}
	if second.Contact.ID != first.Contact.ID {
		t.Errorf("second contact id = %s, want %s", second.Contact.ID, first.Contact.ID)
	}
	// Confirm the second call did NOT overwrite the original display name.
	if second.Contact.DisplayName != "Alice" {
		t.Errorf("display name overwritten: got %q, want Alice", second.Contact.DisplayName)
	}
}

func TestExecute_NormalisesChannelCase(t *testing.T) {
	repo := newFakeRepo()
	u := MustNew(repo)
	tenant := uuid.New()
	first, err := u.Execute(context.Background(), Input{
		TenantID: tenant, Channel: "whatsapp", ExternalID: "+5511999990001", DisplayName: "Alice",
	})
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	second, err := u.Execute(context.Background(), Input{
		TenantID: tenant, Channel: "WhatsApp", ExternalID: "+5511999990001", DisplayName: "Alice",
	})
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if second.Contact.ID != first.Contact.ID {
		t.Errorf("case variant did not match existing identity: %s vs %s", first.Contact.ID, second.Contact.ID)
	}
	if second.Created {
		t.Errorf("Created = true on case variant, want false")
	}
}

func TestExecute_RejectsNilTenant(t *testing.T) {
	u := MustNew(newFakeRepo())
	_, err := u.Execute(context.Background(), Input{
		TenantID: uuid.Nil, Channel: "whatsapp", ExternalID: "+5511999990001", DisplayName: "Alice",
	})
	if !errors.Is(err, contacts.ErrInvalidTenant) {
		t.Errorf("err = %v, want ErrInvalidTenant", err)
	}
}

func TestExecute_PropagatesDomainConstructionError(t *testing.T) {
	u := MustNew(newFakeRepo())
	_, err := u.Execute(context.Background(), Input{
		TenantID: uuid.New(), Channel: "whatsapp", ExternalID: "+5511999990001", DisplayName: "",
	})
	if !errors.Is(err, contacts.ErrEmptyDisplayName) {
		t.Errorf("err = %v, want ErrEmptyDisplayName", err)
	}
}

func TestExecute_PropagatesIdentityValidationError(t *testing.T) {
	u := MustNew(newFakeRepo())
	_, err := u.Execute(context.Background(), Input{
		TenantID: uuid.New(), Channel: "whatsapp", ExternalID: "not-e164", DisplayName: "Alice",
	})
	if !errors.Is(err, contacts.ErrInvalidE164) {
		t.Errorf("err = %v, want ErrInvalidE164", err)
	}
}

func TestExecute_FindError_Propagates(t *testing.T) {
	repo := newFakeRepo()
	sentinel := errors.New("synthetic find failure")
	repo.findErr = sentinel
	u := MustNew(repo)
	_, err := u.Execute(context.Background(), Input{
		TenantID: uuid.New(), Channel: "whatsapp", ExternalID: "+5511999990001", DisplayName: "Alice",
	})
	if !errors.Is(err, sentinel) {
		t.Errorf("err = %v, want %v", err, sentinel)
	}
}

func TestExecute_SaveError_Propagates(t *testing.T) {
	repo := newFakeRepo()
	sentinel := errors.New("synthetic save failure")
	repo.saveErr = sentinel
	u := MustNew(repo)
	_, err := u.Execute(context.Background(), Input{
		TenantID: uuid.New(), Channel: "whatsapp", ExternalID: "+5511999990001", DisplayName: "Alice",
	})
	if !errors.Is(err, sentinel) {
		t.Errorf("err = %v, want %v", err, sentinel)
	}
}

func TestExecute_RaceCollapsesToWinner(t *testing.T) {
	repo := newFakeRepo()
	tenant := uuid.New()

	// Simulate the race: between our Find and our Save, ANOTHER caller
	// inserts the same identity. We schedule that "other caller" to
	// run inside beforeSave so our Save hits ErrChannelIdentityConflict
	// and the use-case must re-Find and return the winner.
	var injected sync.Once
	repo.beforeSave = func() {
		injected.Do(func() {
			other, err := contacts.New(tenant, "WinnerAlice")
			if err != nil {
				t.Fatalf("seed winner: %v", err)
			}
			if err := other.AddChannelIdentity("whatsapp", "+5511999990001"); err != nil {
				t.Fatalf("seed winner identity: %v", err)
			}
			repo.mu.Lock()
			repo.byID[other.ID] = storedContact{
				id: other.ID, tenantID: other.TenantID,
				displayName: other.DisplayName, identities: other.Identities(),
			}
			for _, id := range other.Identities() {
				repo.byIdentity[id.Channel+"|"+id.ExternalID] = other.ID
			}
			repo.mu.Unlock()
		})
	}

	u := MustNew(repo)
	res, err := u.Execute(context.Background(), Input{
		TenantID: tenant, Channel: "whatsapp", ExternalID: "+5511999990001", DisplayName: "LoserAlice",
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Created {
		t.Errorf("Created = true after race, want false (caller lost)")
	}
	if res.Contact == nil || res.Contact.DisplayName != "WinnerAlice" {
		t.Errorf("did not collapse to race winner: %+v", res.Contact)
	}
}

func TestExecute_RaceAcrossTenants_SurfacesConflict(t *testing.T) {
	repo := newFakeRepo()
	tenantA := uuid.New()
	tenantB := uuid.New()

	// Seed tenant B's contact with the identity.
	winner, err := contacts.New(tenantB, "TenantBOwner")
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := winner.AddChannelIdentity("whatsapp", "+5511999990001"); err != nil {
		t.Fatalf("seed identity: %v", err)
	}
	if err := repo.Save(context.Background(), winner); err != nil {
		t.Fatalf("seed save: %v", err)
	}

	// Tenant A asks: FindByChannelIdentity(A, ...) → NotFound (tenant-scoped fake).
	// Then Save fails with conflict (global). Re-Find → NotFound (still
	// claimed by B). Use-case surfaces ErrChannelIdentityConflict.
	u := MustNew(repo)
	_, err = u.Execute(context.Background(), Input{
		TenantID: tenantA, Channel: "whatsapp", ExternalID: "+5511999990001", DisplayName: "Alice",
	})
	if !errors.Is(err, contacts.ErrChannelIdentityConflict) {
		t.Errorf("err = %v, want ErrChannelIdentityConflict", err)
	}
}

func TestExecute_HighConcurrency_OneCreatorManyReaders(t *testing.T) {
	// AC #4: 100 concurrent callers with the same (tenant, channel,
	// external_id). Exactly one Created=true result; 99 Created=false;
	// all 100 resolve to the same contact id.
	repo := newFakeRepo()
	u := MustNew(repo)
	tenant := uuid.New()

	const n = 100
	results := make(chan Result, n)
	errs := make(chan error, n)
	var wg sync.WaitGroup
	start := make(chan struct{})

	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			res, err := u.Execute(context.Background(), Input{
				TenantID: tenant, Channel: "whatsapp",
				ExternalID:  "+5511999990001",
				DisplayName: fmt.Sprintf("caller-%d", i),
			})
			if err != nil {
				errs <- err
				return
			}
			results <- res
		}(i)
	}
	close(start)
	wg.Wait()
	close(results)
	close(errs)

	if e := <-errs; e != nil {
		t.Fatalf("call failed: %v", e)
	}

	created := 0
	var firstID uuid.UUID
	count := 0
	for res := range results {
		count++
		if firstID == uuid.Nil {
			firstID = res.Contact.ID
		} else if res.Contact.ID != firstID {
			t.Errorf("contact id mismatch: %s vs %s", res.Contact.ID, firstID)
		}
		if res.Created {
			created++
		}
	}
	if count != n {
		t.Errorf("got %d results, want %d", count, n)
	}
	if created != 1 {
		t.Errorf("Created=true count = %d, want 1", created)
	}
}
