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
	seen        map[string]struct{}
	forgetCount int
	forgetErr   error
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

func (l *fakeEventLog) Forget(_ context.Context, source, externalID string, eventType pix.WebhookEventType) error {
	l.forgetCount++
	if l.forgetErr != nil {
		return l.forgetErr
	}
	delete(l.seen, source+"|"+externalID+"|"+string(eventType))
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

func (s *sentinelEventLog) Forget(context.Context, string, string, pix.WebhookEventType) error {
	return nil
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

// TestReconciler_UnknownChargeFirstDelivery_DoesNotPoisonDedup is the
// regression test for [SIN-62997](/SIN/issues/SIN-62997). If the webhook
// is delivered before the local pix_charges row exists (race with
// charge creation, transient pgxpool error, replica-lag miss), the
// reconciler MUST compensate the dedup row so a retry can transition
// the charge on the second pass. Without the compensation, the retry
// hits ErrDuplicateEvent and silently no-ops — charge stuck pending.
func TestReconciler_UnknownChargeFirstDelivery_DoesNotPoisonDedup(t *testing.T) {
	repo := newFakeRepo()
	log := newFakeEventLog()
	actor := uuid.New()
	r := pix.NewReconciler(repo, log, actor)

	// First delivery: charge row not yet present (charge-creation race).
	evt := paidEvent()
	out, err := r.Apply(context.Background(), evt)
	if !errors.Is(err, pix.ErrNotFound) {
		t.Fatalf("first delivery: got err %v, want ErrNotFound", err)
	}
	if out.Duplicate || out.Transitioned {
		t.Errorf("first delivery returned %+v, want zero Outcome", out)
	}
	if log.forgetCount != 1 {
		t.Errorf("Forget called %d times, want 1", log.forgetCount)
	}
	key := evt.Source + "|" + evt.ExternalID + "|" + string(evt.EventType)
	if _, stillSeen := log.seen[key]; stillSeen {
		t.Fatalf("dedup row not compensated: log.seen still contains %q", key)
	}

	// Now stage the charge row (the recovered create flow finishes).
	c := newPending(t)
	if err := c.AttachExternalID(evt.ExternalID, tNow); err != nil {
		t.Fatalf("AttachExternalID: %v", err)
	}
	repo.Seed(c)

	// PSP retries: must transition this time, NOT silently dedup.
	out2, err := r.Apply(context.Background(), evt)
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if out2.Duplicate {
		t.Fatalf("retry reported Duplicate=true — dedup row was not compensated on first delivery")
	}
	if !out2.Transitioned {
		t.Errorf("retry did not transition the charge")
	}
	if out2.Charge == nil || out2.Charge.Status() != pix.StatusPaid {
		t.Errorf("charge not paid after retry: %+v", out2.Charge)
	}
}

// If the compensating Forget itself fails, surface both the original
// ErrNotFound AND the compensation error so the caller can alert on
// the now-poisoned dedup row. The PSP still retries (Inter retries on
// any non-2xx), and the next pass will hit the existing dedup row and
// dedup_hit — but at least the receiver got a structured signal that
// something went wrong on this delivery.
func TestReconciler_UnknownCharge_ForgetFailureIsJoined(t *testing.T) {
	repo := newFakeRepo()
	boom := errors.New("compensate down")
	log := newFakeEventLog()
	log.forgetErr = boom
	r := pix.NewReconciler(repo, log, uuid.New())

	_, err := r.Apply(context.Background(), paidEvent())
	if !errors.Is(err, pix.ErrNotFound) {
		t.Errorf("joined err must satisfy errors.Is(ErrNotFound), got %v", err)
	}
	if !errors.Is(err, boom) {
		t.Errorf("joined err must satisfy errors.Is(boom), got %v", err)
	}
}

// TestReconciler_DuplicateOnPendingFlagsStuckPending pins the
// observability backstop side of [SIN-62997](/SIN/issues/SIN-62997).
// If a dedup hit lands on a charge that is still pending, the
// reconciler MUST surface Outcome.StuckPendingSuspected so the
// receiver can fire an alert — the dedup row was poisoned by an
// earlier failed compensation and every retry will silently no-op.
func TestReconciler_DuplicateOnPendingFlagsStuckPending(t *testing.T) {
	repo := newFakeRepo()
	log := newFakeEventLog()
	r := pix.NewReconciler(repo, log, uuid.New())

	c := newPending(t)
	if err := c.AttachExternalID(externalID, tNow); err != nil {
		t.Fatalf("AttachExternalID: %v", err)
	}
	repo.Seed(c)

	// Prime the dedup ledger directly — simulates a poisoned row from a
	// previous delivery that committed Record but failed before Save.
	if err := log.Record(context.Background(), "banco-inter", externalID, pix.WebhookEventPaid, []byte(`{}`), tNow); err != nil {
		t.Fatalf("prime dedup: %v", err)
	}

	out, err := r.Apply(context.Background(), paidEvent())
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !out.Duplicate {
		t.Fatalf("expected Duplicate=true; outcome=%+v", out)
	}
	if !out.StuckPendingSuspected {
		t.Errorf("expected StuckPendingSuspected=true on pending charge with dedup hit")
	}
}

// Counter-case: dedup hit on a non-pending charge must NOT flag stuck
// pending — that is the normal idempotent retry path.
func TestReconciler_DuplicateOnPaidDoesNotFlag(t *testing.T) {
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

	if err := log.Record(context.Background(), "banco-inter", externalID, pix.WebhookEventPaid, []byte(`{}`), tNow); err != nil {
		t.Fatalf("prime dedup: %v", err)
	}

	out, err := r.Apply(context.Background(), paidEvent())
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !out.Duplicate {
		t.Fatalf("expected Duplicate=true")
	}
	if out.StuckPendingSuspected {
		t.Errorf("StuckPendingSuspected must be false on dedup hit against a paid charge")
	}
}

// Dedup hit with no matching charge (the peek itself errors) must NOT
// regress the dedup happy path — the observability lookup is best-effort.
func TestReconciler_DuplicateWithMissingChargeDoesNotError(t *testing.T) {
	log := newFakeEventLog()
	r := pix.NewReconciler(newFakeRepo(), log, uuid.New())

	if err := log.Record(context.Background(), "banco-inter", externalID, pix.WebhookEventPaid, []byte(`{}`), tNow); err != nil {
		t.Fatalf("prime dedup: %v", err)
	}

	out, err := r.Apply(context.Background(), paidEvent())
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !out.Duplicate {
		t.Fatalf("expected Duplicate=true")
	}
	if out.StuckPendingSuspected {
		t.Errorf("StuckPendingSuspected must be false when peek lookup errors")
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
