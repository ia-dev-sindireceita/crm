package pix_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/billing/pix"
)

// fakeRepo is an in-memory Repository that satisfies the
// "documented in-memory adapter that matches production behaviour"
// requirement of quality bar rule 5. It enforces the UNIQUE constraint
// on (external_id) and translates "no rows" to ErrNotFound exactly as
// the postgres adapter will.
type fakeRepo struct {
	byID         map[uuid.UUID]*pix.PIXCharge
	byExternalID map[string]*pix.PIXCharge
	saveErr      error
	saveCount    int
	lastActor    uuid.UUID
}

func newFakeRepo() *fakeRepo {
	return &fakeRepo{
		byID:         make(map[uuid.UUID]*pix.PIXCharge),
		byExternalID: make(map[string]*pix.PIXCharge),
	}
}

func (r *fakeRepo) Seed(c *pix.PIXCharge) {
	r.byID[c.ID()] = c
	if c.ExternalID() != "" {
		r.byExternalID[c.ExternalID()] = c
	}
}

func (r *fakeRepo) GetByID(_ context.Context, id uuid.UUID) (*pix.PIXCharge, error) {
	c, ok := r.byID[id]
	if !ok {
		return nil, pix.ErrNotFound
	}
	return c, nil
}

func (r *fakeRepo) GetByExternalID(_ context.Context, externalID string) (*pix.PIXCharge, error) {
	c, ok := r.byExternalID[externalID]
	if !ok {
		return nil, pix.ErrNotFound
	}
	return c, nil
}

func (r *fakeRepo) Save(_ context.Context, c *pix.PIXCharge, actorID uuid.UUID) error {
	if r.saveErr != nil {
		return r.saveErr
	}
	r.saveCount++
	r.lastActor = actorID
	r.byID[c.ID()] = c
	if c.ExternalID() != "" {
		r.byExternalID[c.ExternalID()] = c
	}
	return nil
}

func (r *fakeRepo) ListExpiredPending(_ context.Context, before time.Time, limit int) ([]*pix.PIXCharge, error) {
	out := make([]*pix.PIXCharge, 0)
	for _, c := range r.byID {
		if c.Status() == pix.StatusPending && c.ExpiresAt().Before(before) {
			out = append(out, c)
			if limit > 0 && len(out) >= limit {
				break
			}
		}
	}
	return out, nil
}

// fakeEventLog is an in-memory EventLog that enforces the UNIQUE
// constraint on (source, external_id, event_type). It returns
// ErrDuplicateEvent on conflict, matching the postgres adapter contract.
type fakeEventLog struct {
	seen map[string]struct{}
}

func newFakeEventLog() *fakeEventLog {
	return &fakeEventLog{seen: make(map[string]struct{})}
}

func (l *fakeEventLog) Record(_ context.Context, source, externalID string, eventType pix.WebhookEventType, _ []byte, _ time.Time) error {
	k := source + "|" + externalID + "|" + string(eventType)
	if _, ok := l.seen[k]; ok {
		return pix.ErrDuplicateEvent
	}
	l.seen[k] = struct{}{}
	return nil
}

func paidEvent() pix.WebhookEvent {
	return pix.WebhookEvent{
		Source:     "banco-inter",
		ExternalID: externalID,
		EventType:  pix.WebhookEventPaid,
		Payload:    []byte(`{"event":"paid"}`),
		OccurredAt: tNow.Add(5 * time.Minute),
	}
}

func TestReconciler_Apply_Paid(t *testing.T) {
	repo := newFakeRepo()
	log := newFakeEventLog()
	actor := uuid.New()

	c := newPending(t)
	if err := c.AttachExternalID(externalID, tNow); err != nil {
		t.Fatalf("AttachExternalID: %v", err)
	}
	repo.Seed(c)

	r := pix.NewReconciler(repo, log, actor)
	out, err := r.Apply(context.Background(), paidEvent())
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if out.Duplicate {
		t.Errorf("first apply reported Duplicate=true")
	}
	if !out.Transitioned {
		t.Errorf("first apply reported Transitioned=false")
	}
	if out.Charge == nil {
		t.Fatal("Outcome.Charge is nil")
	}
	if out.Charge.Status() != pix.StatusPaid {
		t.Errorf("charge status = %s, want paid", out.Charge.Status())
	}
	if repo.saveCount != 1 {
		t.Errorf("Save called %d times, want 1", repo.saveCount)
	}
	if repo.lastActor != actor {
		t.Errorf("actor not propagated to Save: got %s, want %s", repo.lastActor, actor)
	}
}

// TestReconciler_DuplicateEvent is the AC #1 acceptance test at the
// orchestration layer: a second webhook with the same
// (source, external_id, event_type) MUST NOT transition the charge.
func TestReconciler_DuplicateEvent(t *testing.T) {
	repo := newFakeRepo()
	log := newFakeEventLog()
	r := pix.NewReconciler(repo, log, uuid.New())

	c := newPending(t)
	if err := c.AttachExternalID(externalID, tNow); err != nil {
		t.Fatalf("AttachExternalID: %v", err)
	}
	repo.Seed(c)

	out1, err := r.Apply(context.Background(), paidEvent())
	if err != nil || !out1.Transitioned {
		t.Fatalf("first apply: %+v err=%v", out1, err)
	}
	paidAtAfterFirst := *c.PaidAt()
	saveCountAfterFirst := repo.saveCount

	// Replay the exact same webhook payload.
	out2, err := r.Apply(context.Background(), paidEvent())
	if err != nil {
		t.Fatalf("duplicate apply returned error: %v", err)
	}
	if !out2.Duplicate {
		t.Errorf("duplicate apply did not report Duplicate=true")
	}
	if out2.Transitioned {
		t.Errorf("duplicate apply reported Transitioned=true")
	}
	if out2.Charge != nil {
		t.Errorf("duplicate apply returned non-nil Charge")
	}
	if !c.PaidAt().Equal(paidAtAfterFirst) {
		t.Errorf("paid_at rewritten by duplicate: got %s, want %s", c.PaidAt(), paidAtAfterFirst)
	}
	if repo.saveCount != saveCountAfterFirst {
		t.Errorf("duplicate apply called Save %d times, want 0", repo.saveCount-saveCountAfterFirst)
	}
}

func TestReconciler_Apply_Cancelled(t *testing.T) {
	repo := newFakeRepo()
	log := newFakeEventLog()
	r := pix.NewReconciler(repo, log, uuid.New())

	c := newPending(t)
	if err := c.AttachExternalID(externalID, tNow); err != nil {
		t.Fatalf("AttachExternalID: %v", err)
	}
	repo.Seed(c)

	evt := paidEvent()
	evt.EventType = pix.WebhookEventCancelled
	out, err := r.Apply(context.Background(), evt)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !out.Transitioned {
		t.Errorf("expected Transitioned=true")
	}
	if out.Charge.Status() != pix.StatusCancelled {
		t.Errorf("charge status = %s, want cancelled", out.Charge.Status())
	}
}

func TestReconciler_Apply_Expired_Webhook(t *testing.T) {
	repo := newFakeRepo()
	log := newFakeEventLog()
	r := pix.NewReconciler(repo, log, uuid.New())

	c := newPending(t)
	if err := c.AttachExternalID(externalID, tNow); err != nil {
		t.Fatalf("AttachExternalID: %v", err)
	}
	repo.Seed(c)

	evt := paidEvent()
	evt.EventType = pix.WebhookEventExpired
	// PSP-driven expiry — webhook event bypasses the TTL guard.
	evt.OccurredAt = tNow.Add(time.Minute) // before expires_at
	out, err := r.Apply(context.Background(), evt)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !out.Transitioned {
		t.Errorf("expected Transitioned=true")
	}
	if out.Charge.Status() != pix.StatusExpired {
		t.Errorf("charge status = %s, want expired", out.Charge.Status())
	}
}

func TestReconciler_Apply_Expired_Webhook_Idempotent(t *testing.T) {
	repo := newFakeRepo()
	log := newFakeEventLog()
	r := pix.NewReconciler(repo, log, uuid.New())

	c := newPending(t)
	if err := c.AttachExternalID(externalID, tNow); err != nil {
		t.Fatalf("AttachExternalID: %v", err)
	}
	if _, err := c.Expire(tExpires.Add(time.Second)); err != nil {
		t.Fatalf("Expire: %v", err)
	}
	repo.Seed(c)

	evt := paidEvent()
	evt.EventType = pix.WebhookEventExpired
	out, err := r.Apply(context.Background(), evt)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if out.Duplicate {
		t.Errorf("first delivery should not be duplicate")
	}
	if out.Transitioned {
		t.Errorf("expired charge should not transition again")
	}
	if out.Charge.Status() != pix.StatusExpired {
		t.Errorf("status = %s, want expired", out.Charge.Status())
	}
	if repo.saveCount != 0 {
		t.Errorf("Save called %d times on no-op transition, want 0", repo.saveCount)
	}
}

func TestReconciler_Apply_UnknownEventType(t *testing.T) {
	r := pix.NewReconciler(newFakeRepo(), newFakeEventLog(), uuid.New())
	evt := paidEvent()
	evt.EventType = pix.WebhookEventType("refunded")
	_, err := r.Apply(context.Background(), evt)
	if !errors.Is(err, pix.ErrUnknownEventType) {
		t.Errorf("got err %v, want ErrUnknownEventType", err)
	}
}

func TestReconciler_Apply_EmptyExternalID(t *testing.T) {
	r := pix.NewReconciler(newFakeRepo(), newFakeEventLog(), uuid.New())
	evt := paidEvent()
	evt.ExternalID = ""
	_, err := r.Apply(context.Background(), evt)
	if !errors.Is(err, pix.ErrEmptyExternalID) {
		t.Errorf("got err %v, want ErrEmptyExternalID", err)
	}
}

func TestReconciler_Apply_UnknownCharge(t *testing.T) {
	r := pix.NewReconciler(newFakeRepo(), newFakeEventLog(), uuid.New())
	_, err := r.Apply(context.Background(), paidEvent())
	if !errors.Is(err, pix.ErrNotFound) {
		t.Errorf("got err %v, want ErrNotFound", err)
	}
}

func TestReconciler_Apply_PaidOnExpiredIsError(t *testing.T) {
	repo := newFakeRepo()
	log := newFakeEventLog()
	r := pix.NewReconciler(repo, log, uuid.New())

	c := newPending(t)
	if err := c.AttachExternalID(externalID, tNow); err != nil {
		t.Fatalf("AttachExternalID: %v", err)
	}
	if _, err := c.Expire(tExpires.Add(time.Second)); err != nil {
		t.Fatalf("Expire: %v", err)
	}
	repo.Seed(c)

	_, err := r.Apply(context.Background(), paidEvent())
	if !errors.Is(err, pix.ErrInvalidTransition) {
		t.Errorf("got err %v, want ErrInvalidTransition", err)
	}
}

// An `expired` webhook delivered AFTER a `paid` webhook is a
// reconciliation inconsistency — the reconciler must refuse rather
// than walk a paid charge backwards.
func TestReconciler_Apply_ExpiredOnPaidIsInvalid(t *testing.T) {
	repo := newFakeRepo()
	log := newFakeEventLog()
	r := pix.NewReconciler(repo, log, uuid.New())

	c := newPending(t)
	if err := c.AttachExternalID(externalID, tNow); err != nil {
		t.Fatalf("AttachExternalID: %v", err)
	}
	if _, err := c.MarkPaid(tNow.Add(time.Minute)); err != nil {
		t.Fatalf("MarkPaid: %v", err)
	}
	repo.Seed(c)

	evt := paidEvent()
	evt.EventType = pix.WebhookEventExpired
	_, err := r.Apply(context.Background(), evt)
	if !errors.Is(err, pix.ErrInvalidTransition) {
		t.Errorf("got err %v, want ErrInvalidTransition", err)
	}
}

// Sentinel propagation: a non-dedup EventLog failure must surface as-is.
type sentinelEventLog struct{ err error }

func (s *sentinelEventLog) Record(context.Context, string, string, pix.WebhookEventType, []byte, time.Time) error {
	return s.err
}

func TestReconciler_Apply_EventLogError(t *testing.T) {
	boom := errors.New("network blip")
	r := pix.NewReconciler(newFakeRepo(), &sentinelEventLog{err: boom}, uuid.New())
	_, err := r.Apply(context.Background(), paidEvent())
	if !errors.Is(err, boom) {
		t.Errorf("got err %v, want %v", err, boom)
	}
}

func TestReconciler_Apply_SaveError(t *testing.T) {
	repo := newFakeRepo()
	repo.saveErr = errors.New("postgres down")
	r := pix.NewReconciler(repo, newFakeEventLog(), uuid.New())

	c := newPending(t)
	if err := c.AttachExternalID(externalID, tNow); err != nil {
		t.Fatalf("AttachExternalID: %v", err)
	}
	repo.Seed(c)

	_, err := r.Apply(context.Background(), paidEvent())
	if err == nil {
		t.Fatalf("expected Save error to surface")
	}
	if err.Error() != "postgres down" {
		t.Errorf("got err %q, want postgres down", err.Error())
	}
}

// Sanity check on the repo fake itself — its ListExpiredPending mirrors
// what the cron worker will call.
func TestFakeRepo_ListExpiredPending(t *testing.T) {
	repo := newFakeRepo()
	c1 := newPending(t)
	if err := c1.AttachExternalID("a", tNow); err != nil {
		t.Fatalf("attach a: %v", err)
	}
	c2 := newPending(t)
	if err := c2.AttachExternalID("b", tNow); err != nil {
		t.Fatalf("attach b: %v", err)
	}
	if _, err := c2.MarkPaid(tNow.Add(time.Minute)); err != nil {
		t.Fatalf("MarkPaid: %v", err)
	}
	repo.Seed(c1)
	repo.Seed(c2)

	got, err := repo.ListExpiredPending(context.Background(), tExpires.Add(time.Hour), 10)
	if err != nil {
		t.Fatalf("ListExpiredPending: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d expired-pending, want 1", len(got))
	}
	if got[0].ID() != c1.ID() {
		t.Errorf("listed wrong charge")
	}
}
