package postgres_test

// SIN-62964 — Postgres integration tests for the PIX webhook receiver
// stack: the EventLog ledger adapter + the Repository adapter + the
// pix.Reconciler chained end-to-end. Lives in the parent postgres_test
// package (not internal/adapter/db/postgres/pix) so it shares the
// shared-cluster TestMain bootstrap and avoids the SQLSTATE 28P01
// ALTER ROLE race documented in the testpg shared-cluster memory.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	postgresadapter "github.com/pericles-luz/crm/internal/adapter/db/postgres"
	pgpix "github.com/pericles-luz/crm/internal/adapter/db/postgres/pix"
	"github.com/pericles-luz/crm/internal/adapter/db/postgres/testpg"
	pixinter "github.com/pericles-luz/crm/internal/adapter/pix/inter"
	domainpix "github.com/pericles-luz/crm/internal/billing/pix"
)

// pixWebhookDeps bundles the seeded ids + adapter instances each test
// case needs. Tests call newPIXWebhookDeps once at the top of the
// function to share one bootstrap.
type pixWebhookDeps struct {
	db             *testpg.DB
	repo           *pgpix.Repository
	log            *pgpix.EventLogStore
	actorID        uuid.UUID
	tenantID       uuid.UUID
	chargeID       uuid.UUID
	externalID     string
	invoiceID      uuid.UUID
	subscriptionID uuid.UUID
}

func newPIXWebhookDeps(t *testing.T) *pixWebhookDeps {
	t.Helper()
	db := freshDBWithPhase4(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	tenantID, masterID := seedTenantUserMaster(t, db)
	planID := seedPlan(t, ctx, db, "pix-webhook-"+uuid.NewString()[:8], 1_000_000)
	subID := seedActiveSubscription(t, ctx, db, tenantID, planID, masterID)

	// Seed one invoice + one PIX charge attached to it. external_id
	// is set inline so the GetByExternalID lookup the reconciler
	// performs has a target row.
	invoiceID := uuid.New()
	externalID := "txid-" + uuid.NewString()[:12]
	expiresAt := time.Now().UTC().Add(30 * time.Minute)
	if err := postgresadapter.WithMasterOps(ctx, db.MasterOpsPool(), masterID, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `
			INSERT INTO invoice (id, tenant_id, subscription_id, period_start, period_end, amount_cents_brl, state)
			VALUES ($1, $2, $3, $4, $5, $6, 'pending')`,
			invoiceID, tenantID, subID,
			time.Now().UTC().Truncate(24*time.Hour),
			time.Now().UTC().Add(30*24*time.Hour).Truncate(24*time.Hour),
			4990,
		); err != nil {
			return err
		}
		return nil
	}); err != nil {
		t.Fatalf("seed invoice: %v", err)
	}

	chargeID := uuid.New()
	if err := postgresadapter.WithMasterOps(ctx, db.MasterOpsPool(), masterID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO pix_charges
			  (id, tenant_id, invoice_id, external_id, qr_code, copy_paste,
			   status, expires_at, created_at, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6, 'pending', $7, now(), now())`,
			chargeID, tenantID, invoiceID, externalID,
			"data:image/png;base64,xx", "00020126...EMVCo...",
			expiresAt,
		)
		return err
	}); err != nil {
		t.Fatalf("seed pix charge: %v", err)
	}

	actorID := uuid.New()
	repo, err := pgpix.NewRepository(db.MasterOpsPool(), actorID)
	if err != nil {
		t.Fatalf("new repository: %v", err)
	}
	logStore, err := pgpix.NewEventLogStore(db.MasterOpsPool(), actorID)
	if err != nil {
		t.Fatalf("new event log: %v", err)
	}

	return &pixWebhookDeps{
		db:             db,
		repo:           repo,
		log:            logStore,
		actorID:        actorID,
		tenantID:       tenantID,
		chargeID:       chargeID,
		externalID:     externalID,
		invoiceID:      invoiceID,
		subscriptionID: subID,
	}
}

// TestPIXWebhook_AC5_DuplicateDeliveryIdempotent is the AC #5 test:
// the same Inter webhook payload delivered twice transitions the
// pix_charge from pending → paid exactly once. webhook_events carries
// a single row for the (source, external_id, event_type) triple and
// paid_at is preserved across the second delivery.
//
// The test exercises the FULL receiver stack — pgpix.EventLog +
// pgpix.Repository + pix.NewReconciler — against a real Postgres so
// the dedup invariant is verified at the storage layer (where the
// fakes in the unit suite cannot reach).
func TestPIXWebhook_AC5_DuplicateDeliveryIdempotent(t *testing.T) {
	deps := newPIXWebhookDeps(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	reconciler := domainpix.NewReconciler(deps.repo, deps.log, deps.actorID)
	occurred := time.Now().UTC().Add(-time.Minute)
	payload := []byte(`{"txid":"` + deps.externalID + `","valor":"49.90","horario":"2026-05-17T12:00:00Z"}`)
	evt := domainpix.WebhookEvent{
		Source:     pixinter.SourceName,
		ExternalID: deps.externalID,
		EventType:  domainpix.WebhookEventPaid,
		Payload:    payload,
		OccurredAt: occurred,
	}

	// First delivery — must transition pending → paid and store one
	// webhook_events row.
	out1, err := reconciler.Apply(ctx, evt)
	if err != nil {
		t.Fatalf("first apply: %v", err)
	}
	if out1.Duplicate {
		t.Errorf("first delivery reported Duplicate=true")
	}
	if !out1.Transitioned {
		t.Errorf("first delivery did not transition; outcome=%+v", out1)
	}

	got1, err := deps.repo.GetByExternalID(ctx, deps.externalID)
	if err != nil {
		t.Fatalf("GetByExternalID after first: %v", err)
	}
	if got1.Status() != domainpix.StatusPaid {
		t.Errorf("after first delivery: status = %s, want paid", got1.Status())
	}
	if got1.PaidAt() == nil {
		t.Fatal("after first delivery: paid_at is nil")
	}
	firstPaidAt := *got1.PaidAt()

	if got := countWebhookEvents(t, ctx, deps.db, deps.externalID); got != 1 {
		t.Errorf("after first delivery: webhook_events rows = %d, want 1", got)
	}

	// Second delivery of the exact same payload — MUST be a no-op at
	// every layer: reconciler returns Duplicate, repository is not
	// re-saved (paid_at unchanged), webhook_events still has 1 row.
	out2, err := reconciler.Apply(ctx, evt)
	if err != nil {
		t.Fatalf("duplicate apply: %v", err)
	}
	if !out2.Duplicate {
		t.Errorf("duplicate delivery did not report Duplicate=true; outcome=%+v", out2)
	}
	if out2.Transitioned {
		t.Errorf("duplicate delivery reported Transitioned=true")
	}

	got2, err := deps.repo.GetByExternalID(ctx, deps.externalID)
	if err != nil {
		t.Fatalf("GetByExternalID after duplicate: %v", err)
	}
	if got2.Status() != domainpix.StatusPaid {
		t.Errorf("after duplicate: status = %s, want paid", got2.Status())
	}
	if got2.PaidAt() == nil || !got2.PaidAt().Equal(firstPaidAt) {
		t.Errorf("paid_at mutated by duplicate: got %v, want %v", got2.PaidAt(), firstPaidAt)
	}

	if got := countWebhookEvents(t, ctx, deps.db, deps.externalID); got != 1 {
		t.Errorf("after duplicate: webhook_events rows = %d, want 1 (no second insert)", got)
	}
}

// TestPIXWebhook_EventLog_DistinctEventTypesNotDeduped pins the
// "(source, external_id, event_type)" granularity: a `paid` event and
// a later `cancelled` event for the same external_id MUST both land as
// distinct webhook_events rows. The dedup is on the triple, not on the
// (source, external_id) pair.
func TestPIXWebhook_EventLog_DistinctEventTypesNotDeduped(t *testing.T) {
	deps := newPIXWebhookDeps(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	now := time.Now().UTC()
	if err := deps.log.Record(ctx, pixinter.SourceName, deps.externalID, domainpix.WebhookEventPaid, []byte(`{}`), now); err != nil {
		t.Fatalf("record paid: %v", err)
	}
	if err := deps.log.Record(ctx, pixinter.SourceName, deps.externalID, domainpix.WebhookEventCancelled, []byte(`{}`), now); err != nil {
		t.Fatalf("record cancelled: %v", err)
	}

	if got := countWebhookEvents(t, ctx, deps.db, deps.externalID); got != 2 {
		t.Errorf("webhook_events rows = %d, want 2 (paid + cancelled)", got)
	}
}

// TestPIXWebhook_EventLog_DuplicateRecordReturnsErrDuplicateEvent
// verifies the postgres adapter translates SQLSTATE 23505 on the
// dedup index to pix.ErrDuplicateEvent so the reconciler can switch
// on it via errors.Is.
func TestPIXWebhook_EventLog_DuplicateRecordReturnsErrDuplicateEvent(t *testing.T) {
	deps := newPIXWebhookDeps(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	now := time.Now().UTC()
	if err := deps.log.Record(ctx, pixinter.SourceName, deps.externalID, domainpix.WebhookEventPaid, []byte(`{}`), now); err != nil {
		t.Fatalf("first record: %v", err)
	}
	err := deps.log.Record(ctx, pixinter.SourceName, deps.externalID, domainpix.WebhookEventPaid, []byte(`{}`), now)
	if !errors.Is(err, domainpix.ErrDuplicateEvent) {
		t.Fatalf("second record err = %v, want pix.ErrDuplicateEvent", err)
	}
}

// TestPIXWebhook_Repository_GetByExternalID_NotFound exercises the
// adapter's translation of "no rows" to pix.ErrNotFound.
func TestPIXWebhook_Repository_GetByExternalID_NotFound(t *testing.T) {
	deps := newPIXWebhookDeps(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := deps.repo.GetByExternalID(ctx, "missing-txid"); !errors.Is(err, domainpix.ErrNotFound) {
		t.Errorf("got %v, want ErrNotFound", err)
	}
	// Empty externalID is a guard at the adapter (no SQL issued).
	if _, err := deps.repo.GetByExternalID(ctx, ""); !errors.Is(err, domainpix.ErrNotFound) {
		t.Errorf("empty externalID: got %v, want ErrNotFound", err)
	}
}

// TestPIXWebhook_Repository_GetByID covers both the happy path and the
// not-found translation (the GetByID code path is otherwise unused by
// the reconciler — it serves the billing UI / admin tooling).
func TestPIXWebhook_Repository_GetByID(t *testing.T) {
	deps := newPIXWebhookDeps(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	got, err := deps.repo.GetByID(ctx, deps.chargeID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.ID() != deps.chargeID {
		t.Errorf("id = %s, want %s", got.ID(), deps.chargeID)
	}
	if got.ExternalID() != deps.externalID {
		t.Errorf("externalID = %q, want %q", got.ExternalID(), deps.externalID)
	}

	if _, err := deps.repo.GetByID(ctx, uuid.Nil); !errors.Is(err, domainpix.ErrNotFound) {
		t.Errorf("uuid.Nil: got %v, want ErrNotFound", err)
	}
	if _, err := deps.repo.GetByID(ctx, uuid.New()); !errors.Is(err, domainpix.ErrNotFound) {
		t.Errorf("missing id: got %v, want ErrNotFound", err)
	}
}

// TestPIXWebhook_Repository_ListExpiredPending pins the cron-side scan
// the dunning worker uses (pending charges whose expires_at has
// elapsed) — wired through the Repository port.
func TestPIXWebhook_Repository_ListExpiredPending(t *testing.T) {
	deps := newPIXWebhookDeps(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Seed an additional charge that expired an hour ago so the scan
	// returns at least one row when called with `now` set to the
	// present.
	expiredID := uuid.New()
	expired := time.Now().UTC().Add(-time.Hour)
	if err := postgresadapter.WithMasterOps(ctx, deps.db.MasterOpsPool(), deps.actorID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO pix_charges (id, tenant_id, invoice_id, external_id, qr_code, copy_paste,
			                         status, expires_at, created_at, updated_at)
			VALUES ($1, $2, $3, $4, 'qr', 'paste', 'pending', $5, now(), now())`,
			expiredID, deps.tenantID, deps.invoiceID, "expired-"+uuid.NewString()[:6], expired)
		return err
	}); err != nil {
		t.Fatalf("seed expired charge: %v", err)
	}

	got, err := deps.repo.ListExpiredPending(ctx, time.Now().UTC(), 10)
	if err != nil {
		t.Fatalf("ListExpiredPending: %v", err)
	}
	var found bool
	for _, c := range got {
		if c.ID() == expiredID {
			found = true
		}
	}
	if !found {
		t.Errorf("expected expired charge %s in result, got %d rows", expiredID, len(got))
	}

	// Limit clamp: non-positive limit falls back to the adapter
	// default (100). Sanity test that calling with 0 returns
	// something (the expired row at minimum) and does not error out.
	got0, err := deps.repo.ListExpiredPending(ctx, time.Now().UTC(), 0)
	if err != nil {
		t.Fatalf("ListExpiredPending(limit=0): %v", err)
	}
	if len(got0) == 0 {
		t.Errorf("limit=0 fallback returned 0 rows")
	}
}

// TestPIXWebhook_Repository_Save_RoundTrip covers the upsert
// branch — second Save on the same id mutates state without
// touching the immutable columns.
func TestPIXWebhook_Repository_Save_RoundTrip(t *testing.T) {
	deps := newPIXWebhookDeps(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	c, err := deps.repo.GetByID(ctx, deps.chargeID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	transitioned, err := c.MarkPaid(time.Now().UTC())
	if err != nil {
		t.Fatalf("MarkPaid: %v", err)
	}
	if !transitioned {
		t.Fatal("MarkPaid did not transition; precondition wrong")
	}
	if err := deps.repo.Save(ctx, c, uuid.Nil); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := deps.repo.GetByID(ctx, deps.chargeID)
	if err != nil {
		t.Fatalf("GetByID after Save: %v", err)
	}
	if got.Status() != domainpix.StatusPaid {
		t.Errorf("status = %s, want paid", got.Status())
	}
	if got.PaidAt() == nil {
		t.Error("paid_at not persisted")
	}
}

// TestPIXWebhook_Repository_Save_NilCharge exercises the input-guard
// path so the adapter does not try to INSERT a nil aggregate.
func TestPIXWebhook_Repository_Save_NilCharge(t *testing.T) {
	deps := newPIXWebhookDeps(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := deps.repo.Save(ctx, nil, deps.actorID); err == nil {
		t.Error("Save(nil) returned nil error")
	}
}

// TestPIXWebhook_EventLog_RecordGuards covers the small input-guard
// branches the integration suite would otherwise miss. The reconciler
// never reaches these (the domain validates first) but the adapter
// guards defensively.
func TestPIXWebhook_EventLog_RecordGuards(t *testing.T) {
	deps := newPIXWebhookDeps(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := deps.log.Record(ctx, "", "x", domainpix.WebhookEventPaid, []byte(`{}`), time.Now()); err == nil {
		t.Error("empty source: nil error")
	}
	if err := deps.log.Record(ctx, "src", "", domainpix.WebhookEventPaid, []byte(`{}`), time.Now()); !errors.Is(err, domainpix.ErrEmptyExternalID) {
		t.Errorf("empty externalID: got %v, want ErrEmptyExternalID", err)
	}
	if err := deps.log.Record(ctx, "src", "x", domainpix.WebhookEventType("refunded"), []byte(`{}`), time.Now()); !errors.Is(err, domainpix.ErrUnknownEventType) {
		t.Errorf("unknown event type: got %v, want ErrUnknownEventType", err)
	}
	// nil/empty payload coerces to "{}" so the jsonb column still
	// accepts the row.
	if err := deps.log.Record(ctx, "src", "y", domainpix.WebhookEventPaid, nil, time.Now()); err != nil {
		t.Errorf("nil payload: got err %v, want nil", err)
	}
}

// TestPIXWebhook_Repository_Save_ExternalIDCollision exercises the
// UNIQUE-violation translation on pix_charges_external_id_uniq. The
// adapter must turn the SQLSTATE 23505 into pix.ErrExternalIDAlreadySet
// so callers can branch on errors.Is.
func TestPIXWebhook_Repository_Save_ExternalIDCollision(t *testing.T) {
	deps := newPIXWebhookDeps(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Build a brand-new charge that re-uses the seeded externalID —
	// the partial UNIQUE index should reject it on insert.
	now := time.Now().UTC()
	c, err := domainpix.NewCharge(deps.tenantID, deps.invoiceID,
		"qr", "paste", now.Add(time.Hour), now)
	if err != nil {
		t.Fatalf("NewCharge: %v", err)
	}
	if err := c.AttachExternalID(deps.externalID, now); err != nil {
		t.Fatalf("AttachExternalID: %v", err)
	}
	err = deps.repo.Save(ctx, c, deps.actorID)
	if !errors.Is(err, domainpix.ErrExternalIDAlreadySet) {
		t.Errorf("got %v, want ErrExternalIDAlreadySet", err)
	}
}

// TestPIXWebhook_Repository_Save_CheckViolation exercises the
// pix_charges_paid_at_consistency CHECK constraint. The adapter must
// translate SQLSTATE 23514 into pix.ErrInvalidTransition. We build an
// internally-inconsistent charge via HydrateCharge (the constructors
// would reject it) and force-Save it.
func TestPIXWebhook_Repository_Save_CheckViolation(t *testing.T) {
	deps := newPIXWebhookDeps(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	now := time.Now().UTC()
	bad := domainpix.HydrateCharge(
		uuid.New(), deps.tenantID, deps.invoiceID,
		"bad-extid-"+uuid.NewString()[:6], "qr", "paste",
		domainpix.StatusPaid, // status=paid …
		nil,                  // … but paid_at=nil violates the CHECK
		now.Add(time.Hour), now, now,
	)
	err := deps.repo.Save(ctx, bad, deps.actorID)
	if !errors.Is(err, domainpix.ErrInvalidTransition) {
		t.Errorf("got %v, want ErrInvalidTransition", err)
	}
}

// TestPIXWebhook_Repository_Save_PreAckCharge exercises the
// nullIfEmpty path (external_id stays NULL for a charge that has not
// yet been registered with the PSP).
func TestPIXWebhook_Repository_Save_PreAckCharge(t *testing.T) {
	deps := newPIXWebhookDeps(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	now := time.Now().UTC()
	c, err := domainpix.NewCharge(deps.tenantID, deps.invoiceID,
		"qr", "paste", now.Add(time.Hour), now)
	if err != nil {
		t.Fatalf("NewCharge: %v", err)
	}
	if err := deps.repo.Save(ctx, c, deps.actorID); err != nil {
		t.Fatalf("Save pre-ack charge: %v", err)
	}
	got, err := deps.repo.GetByID(ctx, c.ID())
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.ExternalID() != "" {
		t.Errorf("ExternalID = %q, want \"\" for pre-ack charge", got.ExternalID())
	}
}

// TestPIXWebhook_AdapterConstructors_Guards rounds out the unit-test
// coverage on the constructor guard paths (nil pool, uuid.Nil actor).
// These do not need the testpg cluster — they fail before any SQL.
func TestPIXWebhook_AdapterConstructors_Guards(t *testing.T) {
	if _, err := pgpix.NewRepository(nil, uuid.New()); !errors.Is(err, postgresadapter.ErrNilPool) {
		t.Errorf("NewRepository(nil): %v, want ErrNilPool", err)
	}
	deps := newPIXWebhookDeps(t)
	if _, err := pgpix.NewRepository(deps.db.MasterOpsPool(), uuid.Nil); !errors.Is(err, postgresadapter.ErrZeroActor) {
		t.Errorf("NewRepository(uuid.Nil actor): %v, want ErrZeroActor", err)
	}
	if _, err := pgpix.NewEventLogStore(nil, uuid.New()); !errors.Is(err, postgresadapter.ErrNilPool) {
		t.Errorf("NewEventLogStore(nil): %v, want ErrNilPool", err)
	}
	if _, err := pgpix.NewEventLogStore(deps.db.MasterOpsPool(), uuid.Nil); !errors.Is(err, postgresadapter.ErrZeroActor) {
		t.Errorf("NewEventLogStore(uuid.Nil actor): %v, want ErrZeroActor", err)
	}
}

// countWebhookEvents reads the webhook_events row count for the given
// external_id via the superuser pool (the table has no RLS, but the
// pool choice is deliberate so the assertion does not depend on the
// runtime role's grants).
func countWebhookEvents(t *testing.T, ctx context.Context, db *testpg.DB, externalID string) int {
	t.Helper()
	var n int
	if err := db.SuperuserPool().QueryRow(ctx,
		`SELECT count(*) FROM webhook_events WHERE external_id = $1`, externalID,
	).Scan(&n); err != nil {
		t.Fatalf("count webhook_events: %v", err)
	}
	return n
}
