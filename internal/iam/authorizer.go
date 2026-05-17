package iam

import (
	"context"
	"time"
)

// Action is a stable, dotted identifier for an authorizable operation,
// e.g. "tenant.contact.read_pii" or "master.tenant.impersonate". The
// string values are part of the audit contract (deny logs in
// [SIN-62254] index by ReasonCode + Action), so do NOT rename them
// once shipped — add new constants instead.
//
// Namespace convention: "<scope>.<entity>.<verb>" where scope is
// "tenant" for in-tenant work and "master" for master-console work.
// PII reads use the "_pii" verb suffix so the master gate (ADR 0090)
// can pattern-match without an explicit PII-flag table.
type Action string

// Action values for Fase 1. The list is intentionally small — new
// actions land alongside new handlers that consume RequireAction.
const (
	ActionTenantContactRead      Action = "tenant.contact.read"
	ActionTenantContactReadPII   Action = "tenant.contact.read_pii"
	ActionTenantContactCreate    Action = "tenant.contact.create"
	ActionTenantContactUpdate    Action = "tenant.contact.update"
	ActionTenantContactDelete    Action = "tenant.contact.delete"
	ActionTenantConversationRead Action = "tenant.conversation.read"
	ActionTenantMessageSend      Action = "tenant.message.send"
	ActionTenantMessageRead      Action = "tenant.message.read"

	ActionMasterTenantCreate      Action = "master.tenant.create"
	ActionMasterTenantRead        Action = "master.tenant.read"
	ActionMasterTenantUpdate      Action = "master.tenant.update"
	ActionMasterTenantDelete      Action = "master.tenant.delete"
	ActionMasterTenantImpersonate Action = "master.tenant.impersonate"

	// Fase 2.5 — billing / wallet additions (SIN-62880). Master ops on
	// subscription plan + courtesy grant lifecycle; the wallet/billing
	// domain types live in internal/billing and internal/wallet, but
	// these action identifiers are pure strings — handlers in C9–C11
	// consume billing/wallet ports and resolve the resource separately.
	ActionMasterSubscriptionAssignPlan              Action = "master.subscription.assign_plan"
	ActionMasterSubscriptionCancel                  Action = "master.subscription.cancel"
	ActionMasterGrantCourtesyFreeSubscriptionPeriod Action = "master.grant_courtesy.free_subscription_period"
	ActionMasterGrantCourtesyExtraTokens            Action = "master.grant_courtesy.extra_tokens"
	ActionMasterGrantCourtesyRevoke                 Action = "master.grant_courtesy.revoke"

	// Tenant-scope billing/wallet reads. Restricted to gerente — atendente
	// and common do not see the wallet ledger or invoke history. RLS in
	// internal/db/postgres scopes the underlying tables by company_id;
	// this gate is the application-layer second link of defense-in-depth.
	ActionTenantBillingView      Action = "tenant.billing.view"
	ActionTenantWalletViewLedger Action = "tenant.wallet.view_ledger"

	// Fase 3 H1 — ai-policy config + audit (SIN-62353, decisão #8).
	// Write is restricted to gerente because flipping ai_enabled is a
	// privacy-sensitive control (LGPD opt-in, ADR-0041). Audit read
	// shares the role per spec ("default = mesmo papel que
	// ai-policy.write"); master operators see the cross-tenant slice
	// via the master.audit.read action below.
	ActionTenantAIPolicyWrite     Action = "tenant.ai_policy.write"
	ActionTenantAIPolicyAuditRead Action = "tenant.ai_policy.audit.read"

	// Master-scope audit read: cross-tenant slice of ai_policy_audit
	// served at /admin/audit?tenant=...&module=ai-policy. Master only;
	// the gate runs alongside the standard master.* surface.
	ActionMasterAuditRead Action = "master.audit.read"

	// Fase 3 W4C — product catalog admin (SIN-62907). One action gates
	// every catalog mutation surface (create / update / delete on
	// Product and ProductArgument) plus the read views: the gerente who
	// manages the catalog is the only role that needs to see it, mirroring
	// the W4A ai-policy pattern. Atendente / common are denied at the
	// router gate.
	ActionTenantCatalogManage Action = "tenant.catalog.manage"
)

// ReasonCode is a stable, low-cardinality classifier for the Decision.
// It is logged into the audit trail and emitted as a Prometheus label,
// so the set is closed: every Authorizer.Can result MUST pick from
// these constants. Adding a new code requires an ADR delta on 0090.
type ReasonCode string

const (
	ReasonAllowedRBAC           ReasonCode = "allowed_rbac"
	ReasonAllowedMaster         ReasonCode = "allowed_master"
	ReasonDeniedNoPrincipal     ReasonCode = "denied_no_principal"
	ReasonDeniedRBAC            ReasonCode = "denied_rbac"
	ReasonDeniedMasterPIIStepUp ReasonCode = "denied_master_pii_step_up"
	ReasonDeniedTenantMismatch  ReasonCode = "denied_tenant_mismatch"
	ReasonDeniedUnknownAction   ReasonCode = "denied_unknown_action"
)

// Resource describes the target of an action. Empty TenantID is allowed
// for master-scope actions; the per-action policy decides whether to
// require it. Kind and ID are echoed into Decision so the audit row
// has the same shape the caller emitted (e.g. Kind="contact", ID=uuid).
type Resource struct {
	TenantID string
	Kind     string
	ID       string
}

// Decision is the authorization result. ReasonCode is mandatory on
// both Allow and deny; TargetKind/TargetID mirror the Resource so the
// audit writer in [SIN-62254] does not need to re-resolve them.
type Decision struct {
	Allow      bool
	ReasonCode ReasonCode
	TargetKind string
	TargetID   string
}

// Authorizer decides whether a Principal may perform an Action against
// a Resource. The interface is intentionally minimal — Can is the only
// method; alternative policies (e.g. ABAC, OPA-shaped) implement the
// same shape so handlers depend on the seam, not the impl.
type Authorizer interface {
	Can(ctx context.Context, p Principal, a Action, r Resource) Decision
}

// RBACAuthorizer is the deterministic ADR 0090 implementation: the
// allowed-role set per Action is fixed at construction. It is the
// production default; tests replace it with stubs when needed.
//
// MasterPIIStepUpWindow controls how recent the principal's MFA
// step-up must be for the PII gate to allow a PII read while the
// master is impersonating. Zero means "always require fresh step-up";
// production wires the ADR-defined window.
//
// Now is injected so tests can pin time without mocking time.Now
// globally.
type RBACAuthorizer struct {
	rolesByAction         map[Action][]Role
	masterActions         map[Action]struct{}
	piiActions            map[Action]struct{}
	masterPIIStepUpWindow time.Duration
	now                   func() time.Time
}

// RBACConfig parameterises NewRBACAuthorizer. Empty values default to
// the ADR 0090 production matrix; tests override Now and the window.
type RBACConfig struct {
	MasterPIIStepUpWindow time.Duration
	Now                   func() time.Time
}

// NewRBACAuthorizer returns the production ADR 0090 RBAC matrix. The
// matrix is constructed at call time (not as a package-level var) so
// tests can build alternative instances without mutating shared state.
func NewRBACAuthorizer(cfg RBACConfig) *RBACAuthorizer {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &RBACAuthorizer{
		rolesByAction:         defaultRolesByAction(),
		masterActions:         defaultMasterActions(),
		piiActions:            defaultPIIActions(),
		masterPIIStepUpWindow: cfg.MasterPIIStepUpWindow,
		now:                   cfg.Now,
	}
}

// defaultRolesByAction is the ADR 0090 RBAC matrix. Each Action maps
// to the set of Role values that are permitted to invoke it. An Action
// absent from the map is implicitly denied for every role (deny by
// default) — Can returns ReasonDeniedUnknownAction.
func defaultRolesByAction() map[Action][]Role {
	return map[Action][]Role{
		ActionTenantContactRead:      {RoleTenantCommon, RoleTenantAtendente, RoleTenantGerente},
		ActionTenantContactReadPII:   {RoleTenantGerente},
		ActionTenantContactCreate:    {RoleTenantGerente},
		ActionTenantContactUpdate:    {RoleTenantGerente, RoleTenantAtendente},
		ActionTenantContactDelete:    {RoleTenantGerente},
		ActionTenantConversationRead: {RoleTenantCommon, RoleTenantAtendente, RoleTenantGerente},
		ActionTenantMessageRead:      {RoleTenantCommon, RoleTenantAtendente, RoleTenantGerente},
		ActionTenantMessageSend:      {RoleTenantAtendente, RoleTenantGerente},

		ActionMasterTenantCreate:      {RoleMaster},
		ActionMasterTenantRead:        {RoleMaster},
		ActionMasterTenantUpdate:      {RoleMaster},
		ActionMasterTenantDelete:      {RoleMaster},
		ActionMasterTenantImpersonate: {RoleMaster},

		// Fase 2.5 — master billing/courtesy ops (SIN-62880).
		ActionMasterSubscriptionAssignPlan:              {RoleMaster},
		ActionMasterSubscriptionCancel:                  {RoleMaster},
		ActionMasterGrantCourtesyFreeSubscriptionPeriod: {RoleMaster},
		ActionMasterGrantCourtesyExtraTokens:            {RoleMaster},
		ActionMasterGrantCourtesyRevoke:                 {RoleMaster},

		// Fase 2.5 — tenant billing/wallet reads (SIN-62880). Gerente
		// only: atendente and common do not see the wallet ledger or
		// invoice history.
		ActionTenantBillingView:      {RoleTenantGerente},
		ActionTenantWalletViewLedger: {RoleTenantGerente},

		// Fase 3 H1 — ai-policy + audit (SIN-62353, decisão #8).
		ActionTenantAIPolicyWrite:     {RoleTenantGerente},
		ActionTenantAIPolicyAuditRead: {RoleTenantGerente},
		ActionMasterAuditRead:         {RoleMaster},

		// Fase 3 W4C — catalog admin (SIN-62907). Gerente only.
		ActionTenantCatalogManage: {RoleTenantGerente},
	}
}

// defaultMasterActions is the set of Action values that live in the
// master scope. The master gate uses this to short-circuit: a master
// principal can perform master.* actions even without impersonating.
func defaultMasterActions() map[Action]struct{} {
	return map[Action]struct{}{
		ActionMasterTenantCreate:      {},
		ActionMasterTenantRead:        {},
		ActionMasterTenantUpdate:      {},
		ActionMasterTenantDelete:      {},
		ActionMasterTenantImpersonate: {},

		// Fase 2.5 — master billing/courtesy ops (SIN-62880). These are
		// master-scope: a master can invoke them without impersonating a
		// tenant. tenant.billing.view / tenant.wallet.view_ledger are
		// tenant-scope and do NOT belong here.
		ActionMasterSubscriptionAssignPlan:              {},
		ActionMasterSubscriptionCancel:                  {},
		ActionMasterGrantCourtesyFreeSubscriptionPeriod: {},
		ActionMasterGrantCourtesyExtraTokens:            {},
		ActionMasterGrantCourtesyRevoke:                 {},

		// Fase 3 H1 — master cross-tenant audit reader (SIN-62353).
		ActionMasterAuditRead: {},
	}
}

// defaultPIIActions is the set of tenant-scope Action values that
// expose personally-identifiable information. The master PII gate
// (ADR 0090 §M3) applies fresh-step-up enforcement to these and only
// to these — non-PII tenant reads are unaffected by the gate.
func defaultPIIActions() map[Action]struct{} {
	return map[Action]struct{}{
		ActionTenantContactReadPII: {},
	}
}

// Can is the only Authorizer entrypoint. The flow is:
//
//  1. Reject when the action is unknown (deny-by-default).
//  2. Reject when the Principal is empty (no UserID).
//  3. Master-scope actions: allow iff principal carries RoleMaster.
//  4. Tenant-scope actions while master is impersonating: enforce the
//     fresh-MFA step-up gate for PII actions, then fall through to RBAC.
//  5. Tenant-scope actions: allow iff the principal's role set
//     intersects the allowed-roles list AND the tenant matches.
func (a *RBACAuthorizer) Can(ctx context.Context, p Principal, action Action, r Resource) Decision {
	allowed, known := a.rolesByAction[action]
	if !known {
		return Decision{Allow: false, ReasonCode: ReasonDeniedUnknownAction, TargetKind: r.Kind, TargetID: r.ID}
	}
	// Empty UserID is the unauthenticated case — RequireAuth should
	// have intercepted before this point, but defense-in-depth.
	if p.UserID.String() == "00000000-0000-0000-0000-000000000000" {
		return Decision{Allow: false, ReasonCode: ReasonDeniedNoPrincipal, TargetKind: r.Kind, TargetID: r.ID}
	}

	if _, isMaster := a.masterActions[action]; isMaster {
		if p.IsMaster() {
			return Decision{Allow: true, ReasonCode: ReasonAllowedMaster, TargetKind: r.Kind, TargetID: r.ID}
		}
		return Decision{Allow: false, ReasonCode: ReasonDeniedRBAC, TargetKind: r.Kind, TargetID: r.ID}
	}

	// Tenant-scope action.
	if r.TenantID != "" && r.TenantID != p.TenantID.String() {
		return Decision{Allow: false, ReasonCode: ReasonDeniedTenantMismatch, TargetKind: r.Kind, TargetID: r.ID}
	}

	// Master tenant-PII gate: a master impersonating a tenant cannot
	// read PII without a recent MFA step-up. The window is configured
	// at construction; zero means "always require a current step-up"
	// (i.e. step-up that happened in the same instant — practically
	// always deny, useful for test fixtures).
	if p.MasterImpersonating {
		if _, isPII := a.piiActions[action]; isPII {
			if !a.stepUpFresh(p) {
				return Decision{Allow: false, ReasonCode: ReasonDeniedMasterPIIStepUp, TargetKind: r.Kind, TargetID: r.ID}
			}
		}
	}

	for _, role := range allowed {
		if p.HasRole(role) {
			return Decision{Allow: true, ReasonCode: ReasonAllowedRBAC, TargetKind: r.Kind, TargetID: r.ID}
		}
	}
	return Decision{Allow: false, ReasonCode: ReasonDeniedRBAC, TargetKind: r.Kind, TargetID: r.ID}
}

// stepUpFresh reports whether the principal's MFAVerifiedAt is within
// MasterPIIStepUpWindow of now. A nil MFAVerifiedAt is always stale.
func (a *RBACAuthorizer) stepUpFresh(p Principal) bool {
	if p.MFAVerifiedAt == nil {
		return false
	}
	if a.masterPIIStepUpWindow <= 0 {
		// Window of zero means we require step-up to coincide with
		// "now" — i.e. there is no acceptable lag. Tests pin this.
		return !a.now().After(*p.MFAVerifiedAt)
	}
	return a.now().Sub(*p.MFAVerifiedAt) <= a.masterPIIStepUpWindow
}
