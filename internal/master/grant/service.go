package grant

import (
	"context"
	"errors"
	"fmt"
)

// Service orchestrates a courtesy-grant request: validate, evaluate
// policy, persist (or skip) the grant, audit, and alert. It is the
// single public entry point for callers (the HTTP adapter, internal jobs).
type Service struct {
	policy MasterGrantPolicy
	repo   Repo
	audit  AuditLogger
	alerts AlertNotifier
	clock  Clock
	ids    IDGenerator
}

// NewService wires the service. All dependencies are required.
func NewService(policy MasterGrantPolicy, repo Repo, audit AuditLogger, alerts AlertNotifier, clock Clock, ids IDGenerator) *Service {
	return &Service{
		policy: policy,
		repo:   repo,
		audit:  audit,
		alerts: alerts,
		clock:  clock,
		ids:    ids,
	}
}

// GrantCourtesy applies the F39 cap policy to req under principal.
//
// Successful path: the persisted Grant is returned with StatusGranted.
//
// Above-cap path: returns (zero Grant, ErrRequiresApproval). The HTTP
// adapter must surface this as 403 with body "requires approval". An
// audit entry of kind denied_cap_exceeded or pending_approval is written
// before returning, and a Slack alert is fired for any breach. No grant
// row is persisted on a denial (per AC #3 — "não cria o grant, não chama
// nenhum efeito colateral"); a pending grant IS persisted so the second
// master can ratify it.
func (s *Service) GrantCourtesy(ctx context.Context, principal Principal, req Request) (Grant, error) {
	now := s.clock.Now()

	if err := req.Validate(); err != nil {
		_ = s.audit.Log(ctx, AuditEntry{
			Kind:       AuditValidationFail,
			OccurredAt: now,
			Principal:  principal.MasterID,
			IPAddress:  principal.IPAddress,
			Request:    req,
			Note:       err.Error(),
		})
		return Grant{}, err
	}

	subSum, err := s.repo.SubscriptionWindowSum(ctx, req.SubscriptionID, now.Add(-SubscriptionWindow))
	if err != nil {
		return Grant{}, fmt.Errorf("grant: subscription window: %w", err)
	}
	masterSum, err := s.repo.MasterWindowSum(ctx, req.MasterID, now.Add(-MasterWindow))
	if err != nil {
		return Grant{}, fmt.Errorf("grant: master window: %w", err)
	}

	decision := s.policy.Evaluate(req, subSum, masterSum)

	switch decision.Status {
	case StatusGranted:
		g := Grant{
			ID:             s.ids.NewID(),
			MasterID:       req.MasterID,
			TenantID:       req.TenantID,
			SubscriptionID: req.SubscriptionID,
			Amount:         req.Amount,
			Reason:         req.Reason,
			Status:         StatusGranted,
			CreatedAt:      now,
		}
		if err := s.repo.Save(ctx, g); err != nil {
			return Grant{}, fmt.Errorf("grant: save: %w", err)
		}
		_ = s.audit.Log(ctx, AuditEntry{
			Kind:       AuditGranted,
			OccurredAt: now,
			GrantID:    g.ID,
			Principal:  principal.MasterID,
			IPAddress:  principal.IPAddress,
			Request:    req,
			Decision:   decision,
		})
		s.maybeAlert(ctx, decision, req)
		return g, nil

	case StatusPendingApproval:
		g := Grant{
			ID:             s.ids.NewID(),
			MasterID:       req.MasterID,
			TenantID:       req.TenantID,
			SubscriptionID: req.SubscriptionID,
			Amount:         req.Amount,
			Reason:         req.Reason,
			Status:         StatusPendingApproval,
			CreatedAt:      now,
		}
		if err := s.repo.Save(ctx, g); err != nil {
			return Grant{}, fmt.Errorf("grant: save: %w", err)
		}
		_ = s.audit.Log(ctx, AuditEntry{
			Kind:         AuditPending,
			OccurredAt:   now,
			GrantID:      g.ID,
			Principal:    principal.MasterID,
			IPAddress:    principal.IPAddress,
			Request:      req,
			Decision:     decision,
			BreachReason: decision.Breach.Reasons(),
		})
		s.alertOnBreach(ctx, decision, req)
		return Grant{}, ErrRequiresApproval

	case StatusDeniedCapExceeded:
		// AC #3: no side effect, no row, just audit + alert + 403.
		_ = s.audit.Log(ctx, AuditEntry{
			Kind:         AuditDeniedCap,
			OccurredAt:   now,
			Principal:    principal.MasterID,
			IPAddress:    principal.IPAddress,
			Request:      req,
			Decision:     decision,
			BreachReason: decision.Breach.Reasons(),
		})
		s.alertOnBreach(ctx, decision, req)
		return Grant{}, ErrRequiresApproval

	default:
		return Grant{}, fmt.Errorf("grant: unexpected decision status %q", decision.Status)
	}
}

// maybeAlert fires a Slack alert and audits its emission when the
// request amount strictly exceeds the alert threshold (AC #5).
func (s *Service) maybeAlert(ctx context.Context, dec Decision, req Request) {
	if !dec.AlertWorthy {
		return
	}
	s.alertNow(ctx, dec, req)
}

// alertOnBreach always alerts: a breach is itself audit-worthy, and the
// alert threshold (1M) is well below the smaller cap (10M), so any
// breach is implicitly alert-worthy too.
func (s *Service) alertOnBreach(ctx context.Context, dec Decision, req Request) {
	s.alertNow(ctx, dec, req)
}

func (s *Service) alertNow(ctx context.Context, dec Decision, req Request) {
	now := s.clock.Now()
	alert := Alert{
		MasterID:   req.MasterID,
		TenantID:   req.TenantID,
		Amount:     req.Amount,
		Reason:     req.Reason,
		Decision:   dec.Status,
		BreachOf:   dec.Breach.Reasons(),
		OccurredAt: now,
	}
	if err := s.alerts.Notify(ctx, alert); err != nil {
		// Adapter failure must not block the grant decision; record an
		// audit entry so observability still sees the attempt.
		_ = s.audit.Log(ctx, AuditEntry{
			Kind:       AuditAlertEmitted,
			OccurredAt: now,
			Principal:  req.MasterID,
			Request:    req,
			Decision:   dec,
			Note:       "slack notify failed: " + err.Error(),
		})
		return
	}
	_ = s.audit.Log(ctx, AuditEntry{
		Kind:       AuditAlertEmitted,
		OccurredAt: now,
		Principal:  req.MasterID,
		Request:    req,
		Decision:   dec,
	})
}

// Ratify applies a second-master decision (F6 4-eyes) to a pending grant.
// Approves transition pending → approved (the grant becomes effective);
// declines transition pending → cancelled.
//
// The second master must differ from the requesting master; otherwise the
// call returns ErrSelfApproval.
func (s *Service) Ratify(ctx context.Context, principal Principal, grantID string, approve bool, note string) (Grant, error) {
	if !s.policy.ApprovalEnabled() {
		return Grant{}, ErrApprovalDisabled
	}
	g, ok, err := s.repo.FindByID(ctx, grantID)
	if err != nil {
		return Grant{}, fmt.Errorf("grant: find: %w", err)
	}
	if !ok {
		return Grant{}, ErrNotFound
	}
	if g.Status != StatusPendingApproval {
		return Grant{}, ErrNotPending
	}
	if g.MasterID == principal.MasterID {
		return Grant{}, ErrSelfApproval
	}

	now := s.clock.Now()
	target := StatusCancelled
	kind := AuditCancelled
	if approve {
		target = StatusApproved
		kind = AuditApproved
	}
	if err := s.repo.UpdateDecision(ctx, grantID, target, principal.MasterID, now); err != nil {
		return Grant{}, fmt.Errorf("grant: update decision: %w", err)
	}
	g.Status = target
	g.DecidedBy = principal.MasterID
	g.DecidedAt = now

	_ = s.audit.Log(ctx, AuditEntry{
		Kind:       kind,
		OccurredAt: now,
		GrantID:    g.ID,
		Principal:  principal.MasterID,
		IPAddress:  principal.IPAddress,
		Request: Request{
			MasterID:       g.MasterID,
			TenantID:       g.TenantID,
			SubscriptionID: g.SubscriptionID,
			Amount:         g.Amount,
			Reason:         g.Reason,
		},
		Note: note,
	})
	return g, nil
}

var (
	// ErrApprovalDisabled is returned by Ratify when the F6 ratify-flow
	// is not enabled in the policy.
	ErrApprovalDisabled = errors.New("grant: approval flow disabled")
	// ErrNotFound is returned by Ratify when the grant id does not exist.
	ErrNotFound = errors.New("grant: not found")
	// ErrNotPending is returned by Ratify when the grant is not in
	// pending_approval status.
	ErrNotPending = errors.New("grant: not pending approval")
	// ErrSelfApproval is returned by Ratify when the ratifying master is
	// the same as the requesting master.
	ErrSelfApproval = errors.New("grant: self-approval forbidden")
)
