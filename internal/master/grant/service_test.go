package grant_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/pericles-luz/crm/internal/master/grant"
	"github.com/pericles-luz/crm/internal/master/grant/memory"
)

const (
	masterA = "master-a"
	masterB = "master-b"
)

func newFixture(t *testing.T, approvalEnabled bool) (*grant.Service, *memory.Repo, *memory.AuditLogger, *memory.AlertNotifier, *memory.FixedClock) {
	t.Helper()
	repo := memory.NewRepo()
	audit := memory.NewAuditLogger()
	alerts := memory.NewAlertNotifier()
	clock := memory.NewFixedClock(time.Date(2026, 5, 2, 12, 0, 0, 0, time.UTC))
	ids := memory.NewSequenceIDs("g")
	policy := grant.NewPolicy(grant.DefaultCaps(), approvalEnabled)
	svc := grant.NewService(policy, repo, audit, alerts, clock, ids)
	return svc, repo, audit, alerts, clock
}

func principal(masterID string) grant.Principal {
	return grant.Principal{MasterID: masterID, IPAddress: "203.0.113.7"}
}

func req(amount int64, sub string) grant.Request {
	return grant.Request{
		MasterID:       masterA,
		TenantID:       "tenant-1",
		SubscriptionID: sub,
		Amount:         amount,
		Reason:         "courtesy refund",
	}
}

// AC #8 case 1: Master tenta grant 11M → 403 requires approval, sem efeito
// colateral, audit log entry tipo denied_cap_exceeded.
func TestService_GrantAboveSubscriptionCap_DeniedNoSideEffect(t *testing.T) {
	t.Parallel()
	svc, repo, audit, alerts, _ := newFixture(t, false)

	_, err := svc.GrantCourtesy(context.Background(), principal(masterA), req(11_000_000, "sub-1"))
	if !errors.Is(err, grant.ErrRequiresApproval) {
		t.Fatalf("err: want ErrRequiresApproval, got %v", err)
	}
	if len(repo.Snapshot()) != 0 {
		t.Errorf("denied grant must not persist any row: %d rows", len(repo.Snapshot()))
	}
	if got := audit.CountKind(grant.AuditDeniedCap); got != 1 {
		t.Errorf("denied_cap_exceeded entries: want 1, got %d (entries=%+v)", got, audit.Entries())
	}
	if got := len(alerts.Alerts()); got != 1 {
		t.Errorf("slack alert: want 1, got %d", got)
	}
}

// AC #8 case 2: Master tenta grant 5M → sucede, alerta Slack disparado uma
// vez, audit log entry granted + alert_emitted.
func TestService_Grant5M_Succeeds_AlertOnce(t *testing.T) {
	t.Parallel()
	svc, repo, audit, alerts, _ := newFixture(t, false)

	g, err := svc.GrantCourtesy(context.Background(), principal(masterA), req(5_000_000, "sub-1"))
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if g.Status != grant.StatusGranted {
		t.Errorf("status: want granted, got %s", g.Status)
	}
	if len(repo.Snapshot()) != 1 {
		t.Errorf("repo rows: want 1, got %d", len(repo.Snapshot()))
	}
	if got := audit.CountKind(grant.AuditGranted); got != 1 {
		t.Errorf("granted audit: want 1, got %d", got)
	}
	if got := audit.CountKind(grant.AuditAlertEmitted); got != 1 {
		t.Errorf("alert_emitted audit: want 1, got %d", got)
	}
	if got := len(alerts.Alerts()); got != 1 {
		t.Errorf("slack alerts: want 1, got %d", got)
	}
}

// AC #8 case 3: Master faz 11 grants de 10M no mesmo dia (110M cumulativo) → 11º recebe 403.
func TestService_MasterCap_BreachOn11thGrant(t *testing.T) {
	t.Parallel()
	svc, _, audit, _, _ := newFixture(t, false)

	for i := 1; i <= 10; i++ {
		_, err := svc.GrantCourtesy(context.Background(), principal(masterA), req(10_000_000, fmt.Sprintf("sub-%d", i)))
		if err != nil {
			t.Fatalf("grant %d: unexpected err %v", i, err)
		}
	}
	_, err := svc.GrantCourtesy(context.Background(), principal(masterA), req(10_000_000, "sub-11"))
	if !errors.Is(err, grant.ErrRequiresApproval) {
		t.Fatalf("11th grant: want ErrRequiresApproval, got %v", err)
	}
	if got := audit.CountKind(grant.AuditDeniedCap); got != 1 {
		t.Errorf("denied audits: want 1, got %d", got)
	}
}

// AC #8 case 4: Janela rolling — grant de 90 dias atrás não conta para o cap de 90 dias.
func TestService_RollingWindow_OldGrantsDoNotCount(t *testing.T) {
	t.Parallel()
	svc, _, _, _, clock := newFixture(t, false)

	// First 10M grant on day 0.
	_, err := svc.GrantCourtesy(context.Background(), principal(masterA), req(10_000_000, "sub-1"))
	if err != nil {
		t.Fatalf("day 0 grant: %v", err)
	}

	// 91 days later: a fresh 10M grant on the same subscription should
	// succeed because the previous grant is outside the rolling 90-day
	// window for the subscription cap.
	clock.Advance(91 * 24 * time.Hour)
	_, err = svc.GrantCourtesy(context.Background(), principal(masterA), req(10_000_000, "sub-1"))
	if err != nil {
		t.Fatalf("day 91 grant on same subscription: want success, got %v", err)
	}

	// Sanity: a same-day repeat would breach the 90-day subscription cap.
	_, err = svc.GrantCourtesy(context.Background(), principal(masterA), req(1, "sub-1"))
	if !errors.Is(err, grant.ErrRequiresApproval) {
		t.Fatalf("same-day overflow: want ErrRequiresApproval, got %v", err)
	}
}

// AC #8 case 5a: Quando F6 estiver disponível: above-cap pending → 2º master approve → grant aplica.
func TestService_Ratify_Approve(t *testing.T) {
	t.Parallel()
	svc, repo, audit, _, _ := newFixture(t, true)

	_, err := svc.GrantCourtesy(context.Background(), principal(masterA), req(11_000_000, "sub-1"))
	if !errors.Is(err, grant.ErrRequiresApproval) {
		t.Fatalf("first grant: want ErrRequiresApproval, got %v", err)
	}
	if got := audit.CountKind(grant.AuditPending); got != 1 {
		t.Errorf("pending audit: want 1, got %d", got)
	}
	rows := repo.Snapshot()
	if len(rows) != 1 || rows[0].Status != grant.StatusPendingApproval {
		t.Fatalf("expected single pending row, got %+v", rows)
	}

	g, err := svc.Ratify(context.Background(), principal(masterB), rows[0].ID, true, "looks good")
	if err != nil {
		t.Fatalf("ratify approve: %v", err)
	}
	if g.Status != grant.StatusApproved {
		t.Errorf("status: want approved, got %s", g.Status)
	}
	if g.DecidedBy != masterB {
		t.Errorf("decidedBy: want %s, got %s", masterB, g.DecidedBy)
	}
	if got := audit.CountKind(grant.AuditApproved); got != 1 {
		t.Errorf("approved audit: want 1, got %d", got)
	}
}

// AC #8 case 5b: above-cap pending → 2º master deny → grant cancelado.
func TestService_Ratify_Deny(t *testing.T) {
	t.Parallel()
	svc, repo, audit, _, _ := newFixture(t, true)

	_, err := svc.GrantCourtesy(context.Background(), principal(masterA), req(11_000_000, "sub-1"))
	if !errors.Is(err, grant.ErrRequiresApproval) {
		t.Fatalf("first grant: %v", err)
	}
	rows := repo.Snapshot()
	if len(rows) != 1 {
		t.Fatalf("expected single row, got %d", len(rows))
	}

	g, err := svc.Ratify(context.Background(), principal(masterB), rows[0].ID, false, "abuse suspected")
	if err != nil {
		t.Fatalf("ratify deny: %v", err)
	}
	if g.Status != grant.StatusCancelled {
		t.Errorf("status: want cancelled, got %s", g.Status)
	}
	if got := audit.CountKind(grant.AuditCancelled); got != 1 {
		t.Errorf("cancelled audit: want 1, got %d", got)
	}
}

func TestService_Ratify_RejectsSelfApproval(t *testing.T) {
	t.Parallel()
	svc, repo, _, _, _ := newFixture(t, true)

	_, err := svc.GrantCourtesy(context.Background(), principal(masterA), req(11_000_000, "sub-1"))
	if !errors.Is(err, grant.ErrRequiresApproval) {
		t.Fatalf("first grant: %v", err)
	}
	rows := repo.Snapshot()

	_, err = svc.Ratify(context.Background(), principal(masterA), rows[0].ID, true, "")
	if !errors.Is(err, grant.ErrSelfApproval) {
		t.Errorf("want ErrSelfApproval, got %v", err)
	}
}

func TestService_Ratify_NotFound(t *testing.T) {
	t.Parallel()
	svc, _, _, _, _ := newFixture(t, true)

	_, err := svc.Ratify(context.Background(), principal(masterB), "missing", true, "")
	if !errors.Is(err, grant.ErrNotFound) {
		t.Errorf("want ErrNotFound, got %v", err)
	}
}

func TestService_Ratify_NotPending(t *testing.T) {
	t.Parallel()
	svc, repo, _, _, _ := newFixture(t, true)

	g, err := svc.GrantCourtesy(context.Background(), principal(masterA), req(5_000_000, "sub-1"))
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	if got := repo.Snapshot()[0].Status; got != grant.StatusGranted {
		t.Fatalf("seed status: %s", got)
	}

	_, err = svc.Ratify(context.Background(), principal(masterB), g.ID, true, "")
	if !errors.Is(err, grant.ErrNotPending) {
		t.Errorf("want ErrNotPending, got %v", err)
	}
}

func TestService_Ratify_DisabledFlow(t *testing.T) {
	t.Parallel()
	svc, _, _, _, _ := newFixture(t, false)

	_, err := svc.Ratify(context.Background(), principal(masterB), "g1", true, "")
	if !errors.Is(err, grant.ErrApprovalDisabled) {
		t.Errorf("want ErrApprovalDisabled, got %v", err)
	}
}

func TestService_ValidationFailure_AuditedAndNoSideEffect(t *testing.T) {
	t.Parallel()
	svc, repo, audit, alerts, _ := newFixture(t, false)

	bad := req(10, "sub-1")
	bad.MasterID = "" // forced via service (HTTP normally fills from principal, but service must still validate)

	_, err := svc.GrantCourtesy(context.Background(), principal(""), bad)
	if !errors.Is(err, grant.ErrInvalidMaster) {
		t.Fatalf("want ErrInvalidMaster, got %v", err)
	}
	if len(repo.Snapshot()) != 0 {
		t.Errorf("validation failure must not persist")
	}
	if got := audit.CountKind(grant.AuditValidationFail); got != 1 {
		t.Errorf("validation_failed audit: want 1, got %d", got)
	}
	if len(alerts.Alerts()) != 0 {
		t.Errorf("no alert on validation failure")
	}
}

func TestService_AlertFailureAuditedNotPropagated(t *testing.T) {
	t.Parallel()
	svc, _, audit, alerts, _ := newFixture(t, false)
	alerts.SetFailure(errors.New("slack down"))

	_, err := svc.GrantCourtesy(context.Background(), principal(masterA), req(5_000_000, "sub-1"))
	if err != nil {
		t.Fatalf("grant must not fail when slack errors: %v", err)
	}
	// audit should record both granted and alert_emitted (with failure note).
	if got := audit.CountKind(grant.AuditGranted); got != 1 {
		t.Errorf("granted audit: want 1, got %d", got)
	}
	if got := audit.CountKind(grant.AuditAlertEmitted); got != 1 {
		t.Errorf("alert_emitted audit: want 1, got %d", got)
	}
	for _, e := range audit.Entries() {
		if e.Kind == grant.AuditAlertEmitted && e.Note == "" {
			t.Errorf("alert_emitted entry should carry slack failure note")
		}
	}
}

// errRepo lets tests force adapter failures.
type errRepo struct {
	memory.Repo
	subErr    error
	masterErr error
	saveErr   error
}

func (r *errRepo) SubscriptionWindowSum(ctx context.Context, sub string, since time.Time) (int64, error) {
	if r.subErr != nil {
		return 0, r.subErr
	}
	return r.Repo.SubscriptionWindowSum(ctx, sub, since)
}

func (r *errRepo) MasterWindowSum(ctx context.Context, master string, since time.Time) (int64, error) {
	if r.masterErr != nil {
		return 0, r.masterErr
	}
	return r.Repo.MasterWindowSum(ctx, master, since)
}

func (r *errRepo) Save(ctx context.Context, g grant.Grant) error {
	if r.saveErr != nil {
		return r.saveErr
	}
	return r.Repo.Save(ctx, g)
}

func TestService_RepoErrorsPropagate(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		set  func(*errRepo)
	}{
		{"subscription sum", func(r *errRepo) { r.subErr = errors.New("db down") }},
		{"master sum", func(r *errRepo) { r.masterErr = errors.New("db down") }},
		{"save", func(r *errRepo) { r.saveErr = errors.New("db down") }},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			repo := &errRepo{Repo: *memory.NewRepo()}
			tc.set(repo)
			audit := memory.NewAuditLogger()
			alerts := memory.NewAlertNotifier()
			clock := memory.NewFixedClock(time.Date(2026, 5, 2, 0, 0, 0, 0, time.UTC))
			ids := memory.NewSequenceIDs("g")
			svc := grant.NewService(grant.NewPolicy(grant.DefaultCaps(), false), repo, audit, alerts, clock, ids)

			_, err := svc.GrantCourtesy(context.Background(), principal(masterA), req(5_000_000, "sub-1"))
			if err == nil {
				t.Fatalf("want error, got nil")
			}
		})
	}
}
